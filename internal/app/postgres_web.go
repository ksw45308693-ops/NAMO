package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/mail"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"namo/internal/auth"
	"namo/internal/matcher"
	"namo/internal/model"
	"namo/internal/report"
	appweb "namo/internal/web"
)

type WebService struct {
	Repository      *PostgresRepository
	QueueCollection func() error
	TestMail        func(context.Context, string) error
	ReportStore     *report.FileStore
	ReportDir       string
	Now             func() time.Time
	RunManualReport ManualReportRunner
}

var _ appweb.Backend = (*WebService)(nil)
var _ appweb.Actions = (*WebService)(nil)

func (s *WebService) MapRequest(r *http.Request) (appweb.RequestContext, error) {
	requestContext := appweb.RequestContext{CSRFToken: CSRFTokenFromContext(r.Context())}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		if r.URL.Path == "/login" || r.URL.Path == "/signup" || r.URL.Path == "/accept-invite" {
			return requestContext, nil
		}
		return appweb.RequestContext{}, ErrUnauthenticated
	}
	requestContext.UserID = principal.UserID
	requestContext.UserName = principal.Email
	requestContext.Email = principal.Email
	requestContext.TenantID = principal.TenantID
	requestContext.Role = string(principal.Role)
	if principal.TenantID == "" {
		return requestContext, nil
	}
	if s == nil || s.Repository == nil {
		return appweb.RequestContext{}, errors.New("web repository is not configured")
	}
	err := s.Repository.withTenant(r.Context(), principal.TenantID, func(tx pgx.Tx) error {
		var displayName, tenantName string
		if err := tx.QueryRow(r.Context(), `SELECT u.display_name, t.name
FROM public.users u JOIN public.tenants t ON t.id=u.tenant_id
WHERE u.id=$1::uuid AND u.tenant_id=$2::uuid`, principal.UserID, principal.TenantID).Scan(&displayName, &tenantName); err != nil {
			return fmt.Errorf("load web identity: %w", err)
		}
		if strings.TrimSpace(displayName) != "" {
			requestContext.UserName = displayName
		}
		requestContext.TenantName = tenantName
		return nil
	})
	return requestContext, err
}

func (s *WebService) Load(ctx context.Context, requestContext appweb.RequestContext, page appweb.PageRequest) (appweb.AppData, error) {
	if s == nil || s.Repository == nil || s.Repository.Pool == nil {
		return appweb.AppData{}, errors.New("web repository is not configured")
	}
	data, state, err := s.loadGlobalState(ctx)
	if err != nil {
		return appweb.AppData{}, err
	}
	if requestContext.Role == "platform_admin" {
		data.Admin.ReportDir = s.ReportDir
		if err := s.loadPlatformData(ctx, &data, state, requestContext.UserID); err != nil {
			return appweb.AppData{}, err
		}
		return data, nil
	}
	if requestContext.TenantID == "" {
		return appweb.AppData{}, ErrUnauthenticated
	}
	err = s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		return loadTenantWebData(ctx, tx, requestContext.TenantID, &data, noticesNeeded(page.Path))
	})
	return data, err
}

func noticesNeeded(path string) bool {
	return path == "/notices" || strings.HasPrefix(path, "/notices/") || path == "/filters"
}

func (s *WebService) loadGlobalState(ctx context.Context) (appweb.AppData, CollectionResult, error) {
	var lastSuccess *time.Time
	var resultJSON []byte
	var lastError *string
	if err := s.Repository.Pool.QueryRow(ctx, `SELECT last_success_at, last_result, last_error
FROM public.collection_state WHERE singleton`).Scan(&lastSuccess, &resultJSON, &lastError); err != nil {
		return appweb.AppData{}, CollectionResult{}, fmt.Errorf("load collection dashboard: %w", err)
	}
	var result CollectionResult
	if len(resultJSON) != 0 {
		_ = json.Unmarshal(resultJSON, &result)
	}
	data := appweb.AppData{Demo: false}
	data.Dashboard.NewNotices = result.Changed
	data.Dashboard.Collected = result.Fetched
	data.Dashboard.Healthy = lastError == nil || *lastError == ""
	data.Dashboard.LastCollected = "아직 없음"
	if lastSuccess != nil {
		data.Dashboard.LastCollected = formatKoreanTime(*lastSuccess)
	}
	data.Admin = appweb.AdminView{
		Healthy: data.Dashboard.Healthy, LastCollected: data.Dashboard.LastCollected,
		CollectedCount: result.Fetched,
	}
	return data, result, nil
}

func loadTenantWebData(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData, withNotices bool) error {
	if err := tx.QueryRow(ctx, `SELECT contact_email FROM public.tenants WHERE id=$1::uuid`, tenantID).Scan(&data.ContactEmail); err != nil {
		return fmt.Errorf("load tenant settings: %w", err)
	}
	if err := loadTenantSchedule(ctx, tx, tenantID, data); err != nil {
		return err
	}
	if err := loadTenantReports(ctx, tx, tenantID, data); err != nil {
		return err
	}
	filters, err := loadTenantFilters(ctx, tx, tenantID, data)
	if err != nil {
		return err
	}
	if withNotices {
		if err := loadTenantNotices(ctx, tx, data, filters); err != nil {
			return err
		}
	}
	if err := loadTenantRecipients(ctx, tx, tenantID, data); err != nil {
		return err
	}
	if err := loadTenantMembers(ctx, tx, tenantID, data); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.matches
WHERE tenant_id=$1::uuid AND created_at >= now()-interval '24 hours'`, tenantID).Scan(&data.Dashboard.Matches); err != nil {
		return fmt.Errorf("count recent matches: %w", err)
	}
	return nil
}

func loadTenantSchedule(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData) error {
	data.DeliveryTime, data.Timezone = "07:00", "Asia/Seoul"
	data.DeliveryDays = []int{0, 1, 2, 3, 4, 5, 6}
	var hour, minute int
	var weekdays []int16
	err := tx.QueryRow(ctx, `SELECT hour, minute, timezone, weekdays FROM public.schedules
WHERE tenant_id=$1::uuid AND enabled ORDER BY created_at LIMIT 1`, tenantID).Scan(&hour, &minute, &data.Timezone, &weekdays)
	if errors.Is(err, pgx.ErrNoRows) {
		data.Dashboard.RunTime = data.DeliveryTime
		if next, ok := nextDeliveryAt(time.Now(), 7, 0, data.DeliveryDays); ok {
			data.Dashboard.NextDelivery = next.Format("01.02 15:04")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load report schedule: %w", err)
	}
	data.DeliveryTime = fmt.Sprintf("%02d:%02d", hour, minute)
	data.Dashboard.RunTime = data.DeliveryTime
	data.DeliveryDays = make([]int, len(weekdays))
	for i, day := range weekdays {
		data.DeliveryDays[i] = int(day)
	}
	if next, ok := nextDeliveryAt(time.Now(), hour, minute, data.DeliveryDays); ok {
		data.Dashboard.NextDelivery = next.Format("01.02 15:04")
	}
	return nil
}

const tenantReportsSQL = `SELECT id::text,relative_path,trigger,status,due_at,generated_at,notice_count,attempts
FROM public.reports
WHERE tenant_id=$1::uuid AND deleted_at IS NULL
ORDER BY due_at DESC
LIMIT 50`

func loadTenantReports(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData) error {
	rows, err := tx.Query(ctx, tenantReportsSQL, tenantID)
	if err != nil {
		return fmt.Errorf("load tenant reports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, relativePath, trigger, status string
		var dueAt time.Time
		var generatedAt *time.Time
		var noticeCount, attempts int
		if err := rows.Scan(&id, &relativePath, &trigger, &status, &dueAt, &generatedAt, &noticeCount, &attempts); err != nil {
			return fmt.Errorf("scan tenant report: %w", err)
		}
		data.Reports = append(data.Reports, reportViewFromRow(id, relativePath, trigger, status, dueAt, generatedAt, noticeCount, attempts))
	}
	return rows.Err()
}

func reportViewFromRow(id, relativePath, trigger, status string, dueAt time.Time, generatedAt *time.Time, noticeCount, attempts int) appweb.ReportView {
	view := appweb.ReportView{
		ID: id, Trigger: map[string]string{"scheduled": "예약", "manual": "수동"}[trigger],
		DueAt: formatKoreanTime(dueAt), GeneratedAt: "-", NoticeCount: noticeCount,
	}
	if relativePath != "" {
		view.FileName = path.Base(relativePath)
	}
	if generatedAt != nil {
		view.GeneratedAt = formatKoreanTime(*generatedAt)
	}
	switch status {
	case "generated":
		view.Status = "생성 완료"
		view.Downloadable = view.FileName != "" && generatedAt != nil
	case "generating":
		view.Status = "생성 중"
	case "failed":
		if attempts >= 3 {
			view.Status = "생성 실패"
		} else {
			view.Status = "재시도 대기"
		}
	default:
		view.Status = status
	}
	return view
}

const tenantNoticesSQL = `SELECT id::text,payload,collected_at
FROM public.notices
WHERE deadline_at IS NULL OR deadline_at >= now()
ORDER BY published_at DESC NULLS LAST,id`

type activeWebFilter struct {
	ID   string
	Rule matcher.Rule
}

func loadTenantNotices(ctx context.Context, tx pgx.Tx, data *appweb.AppData, filters []activeWebFilter) error {
	rows, err := tx.Query(ctx, tenantNoticesSQL)
	if err != nil {
		return fmt.Errorf("load notices: %w", err)
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var id string
		var noticeJSON []byte
		var collectedAt time.Time
		if err := rows.Scan(&id, &noticeJSON, &collectedAt); err != nil {
			return fmt.Errorf("scan notice: %w", err)
		}
		var notice model.Notice
		if err := json.Unmarshal(noticeJSON, &notice); err != nil {
			return fmt.Errorf("decode notice: %w", err)
		}
		view := noticeViewFromModel(now, id, notice, collectedAt, filters)
		data.Notices = append(data.Notices, view)
		applyNoticeFilterCounts(data, view)
	}
	return rows.Err()
}

func applyNoticeFilterCounts(data *appweb.AppData, notice appweb.NoticeView) {
	for i := range data.Filters {
		if _, matched := notice.FilterReasons[data.Filters[i].ID]; matched {
			data.Filters[i].Matches++
		}
	}
}

func noticeViewFromModel(now time.Time, id string, notice model.Notice, collectedAt time.Time, filters []activeWebFilter) appweb.NoticeView {
	view := appweb.NoticeView{
		ID: id, Title: notice.Title, Category: categoryLabel(notice.Category), Agency: notice.Agency,
		Region: notice.Region, Amount: formatWon(notice.Amount), Deadline: formatKoreanTime(notice.Deadline), SourceURL: notice.SourceURL,
		SourceKind: "입찰공고목록-입찰공고", Trade: "-",
		CollectedDate: formatKoreanDate(collectedAt, "20060102"), CollectedClock: formatKoreanDate(collectedAt, "1504"),
		PostedAt:      formatKoreanDate(notice.PostedAt, "2006-01-02"),
		FilterReasons: make(map[string][]string), FilterKeywords: make(map[string]string),
	}
	switch notice.Category {
	case model.CategoryConstruction, model.CategoryService, model.CategoryGoods:
		view.Trade = "내자"
	case model.CategoryForeign:
		view.Trade = "외자"
	}
	var keywords []string
	for _, filter := range filters {
		matched := matcher.MatchAt(now, notice, filter.Rule)
		if !matched.Matched {
			continue
		}
		view.FilterReasons[filter.ID] = nil
		var filterKeywords []string
		for _, detail := range matched.Details {
			if detail.Code == matcher.ReasonIncludeAny || detail.Code == matcher.ReasonIncludeAll {
				keywords = appendUnique(keywords, detail.RuleValue)
				filterKeywords = appendUnique(filterKeywords, detail.RuleValue)
			}
			reason := reasonText(detail)
			view.Reasons = appendUnique(view.Reasons, reason)
			view.FilterReasons[filter.ID] = appendUnique(view.FilterReasons[filter.ID], reason)
		}
		if len(matched.Details) == 0 {
			for _, reason := range matched.Reasons {
				text := reasonText(matcher.Detail{Code: reason})
				view.Reasons = appendUnique(view.Reasons, text)
				view.FilterReasons[filter.ID] = appendUnique(view.FilterReasons[filter.ID], text)
			}
		}
		view.FilterKeywords[filter.ID] = strings.Join(filterKeywords, ", ")
	}
	view.Keyword = strings.Join(keywords, ", ")
	return view
}

func formatKoreanDate(value time.Time, layout string) string {
	if value.IsZero() {
		return "-"
	}
	return value.In(time.FixedZone("KST", 9*60*60)).Format(layout)
}

const tenantFiltersSQL = `SELECT id::text, name, rules, enabled
FROM public.filters
WHERE tenant_id=$1::uuid ORDER BY created_at`

func loadTenantFilters(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData) ([]activeWebFilter, error) {
	rows, err := tx.Query(ctx, tenantFiltersSQL, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load filters: %w", err)
	}
	defer rows.Close()
	var active []activeWebFilter
	for rows.Next() {
		var view appweb.FilterView
		var raw []byte
		if err := rows.Scan(&view.ID, &view.Name, &raw, &view.Enabled); err != nil {
			return nil, err
		}
		var rule matcher.Rule
		if err := json.Unmarshal(raw, &rule); err != nil {
			return nil, fmt.Errorf("decode filter rule: %w", err)
		}
		view.Summary = filterSummary(rule)
		data.Filters = append(data.Filters, view)
		if view.Enabled {
			data.Dashboard.ActiveFilters++
			active = append(active, activeWebFilter{ID: view.ID, Rule: rule})
		}
	}
	return active, rows.Err()
}

func loadTenantRecipients(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData) error {
	rows, err := tx.Query(ctx, `SELECT name, email, enabled FROM public.recipients WHERE tenant_id=$1::uuid ORDER BY created_at`, tenantID)
	if err != nil {
		return fmt.Errorf("load recipients: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var view appweb.RecipientView
		var enabled bool
		if err := rows.Scan(&view.Name, &view.Email, &enabled); err != nil {
			return err
		}
		if enabled {
			view.State = "수신"
		} else {
			view.State = "중지"
		}
		data.Recipients = append(data.Recipients, view)
	}
	return rows.Err()
}

func loadTenantMembers(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData) error {
	rows, err := tx.Query(ctx, `SELECT id::text, display_name, email, role FROM public.users WHERE tenant_id=$1::uuid ORDER BY created_at`, tenantID)
	if err != nil {
		return fmt.Errorf("load members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var view appweb.MemberView
		var role string
		if err := rows.Scan(&view.UserID, &view.Name, &view.Email, &role); err != nil {
			return err
		}
		view.Role = accountRoleLabel(auth.Role(role))
		data.Members = append(data.Members, view)
	}
	return rows.Err()
}

func (s *WebService) loadPlatformData(ctx context.Context, data *appweb.AppData, _ CollectionResult, actorUserID string) error {
	tenants, err := s.Repository.tenantCatalog(ctx)
	if err != nil {
		return err
	}
	if err := s.loadAccountAssignments(ctx, data, tenants, actorUserID); err != nil {
		return err
	}
	registry, err := s.Repository.TenantRegistry(ctx, actorUserID)
	if err != nil {
		return fmt.Errorf("load tenant registry: %w", err)
	}
	contacts := make(map[string]TenantRegistryEntry, len(registry))
	for _, entry := range registry {
		contacts[entry.ID] = entry
	}
	for _, tenant := range tenants {
		view := appweb.TenantView{Name: tenant.Name, LastDigest: "생성 전", State: "정상"}
		if entry, ok := contacts[tenant.ID]; ok {
			view.AdminName, view.AdminEmail, view.ContactMail = entry.AdminName, entry.AdminEmail, entry.ContactEmail
		}
		err := s.Repository.withTenant(ctx, tenant.ID, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.users WHERE tenant_id=$1::uuid`, tenant.ID).Scan(&view.Members); err != nil {
				return err
			}
			var sentAt *time.Time
			if err := tx.QueryRow(ctx, `SELECT max(generated_at) FROM public.reports WHERE tenant_id=$1::uuid AND status='generated'`, tenant.ID).Scan(&sentAt); err != nil {
				return err
			}
			if sentAt != nil {
				view.LastDigest = formatKoreanTime(*sentAt)
			}
			failures, err := loadTenantFailureCount(ctx, tx, tenant.ID)
			if err != nil {
				return err
			}
			applyTenantFailures(data, &view, failures)
			return nil
		})
		if err != nil {
			return fmt.Errorf("load platform tenant %s: %w", tenant.ID, err)
		}
		data.Tenants = append(data.Tenants, view)
	}
	return nil
}

func applyTenantFailures(data *appweb.AppData, view *appweb.TenantView, failures int) {
	data.Admin.FailedJobs += failures
	if failures > 0 {
		data.Admin.Healthy = false
		view.State = "점검"
	}
}

type failureCountQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadTenantFailureCount(ctx context.Context, queryer failureCountQueryer, tenantID string) (int, error) {
	var failures int
	err := queryer.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM public.job_runs
   WHERE tenant_id=$1::uuid AND status='failed' AND started_at >= now()-interval '24 hours')
  +
  (SELECT count(*) FROM public.deliveries
   WHERE tenant_id=$1::uuid AND status='failed' AND claimed_at >= now()-interval '24 hours'
     AND position($2 in COALESCE(last_error,'')) = 0)`, tenantID, expiredDigestTerminalReason).Scan(&failures)
	if err != nil {
		return 0, fmt.Errorf("count tenant job and delivery failures: %w", err)
	}
	return failures, nil
}

func (s *WebService) loadAccountAssignments(ctx context.Context, data *appweb.AppData, tenants []tenantCatalogEntry, actorUserID string) error {
	for _, tenant := range tenants {
		data.TenantOptions = append(data.TenantOptions, appweb.TenantOption{ID: tenant.ID, Name: tenant.Name})
	}
	accounts, err := s.Repository.MemberAccounts(ctx, actorUserID)
	if err != nil {
		return fmt.Errorf("load member accounts: %w", err)
	}
	for _, account := range accounts {
		data.Accounts = append(data.Accounts, appweb.AccountView{
			UserID: account.UserID, Email: account.Email, DisplayName: account.DisplayName,
			TenantName: account.TenantName, Created: formatKoreanTime(account.Created),
			Assigned: account.TenantID != "", TenantID: account.TenantID,
			Role: string(account.Role), RoleLabel: accountRoleLabel(account.Role),
		})
	}
	return nil
}

// CreateTenant registers one company. Accounts join it through tenant
// assignment, so no invitation or mail delivery is involved.
func (s *WebService) CreateTenant(ctx context.Context, requestContext appweb.RequestContext, command appweb.TenantCommand) error {
	if s == nil || s.Repository == nil {
		return errors.New("web repository is not configured")
	}
	if requestContext.Role != "platform_admin" || requestContext.UserID == "" {
		return ErrSignupPrivileges
	}
	_, err := s.Repository.RegisterTenant(ctx, requestContext.UserID, TenantRegistration{
		Name: command.Name, ContactEmail: command.ContactEmail,
		AdminName: command.AdminName, AdminEmail: command.AdminEmail,
	})
	if errors.Is(err, ErrTenantRegistered) {
		return appweb.ErrTenantExists
	}
	return err
}

// AssignAccountTenant grants or removes company access and the role inside the
// company. The change takes effect on the account's next request because the
// session lookup reads the assignment live.
func (s *WebService) AssignAccountTenant(ctx context.Context, requestContext appweb.RequestContext, command appweb.AssignAccountCommand) error {
	if s == nil || s.Repository == nil {
		return errors.New("web repository is not configured")
	}
	if requestContext.Role != "platform_admin" || requestContext.UserID == "" {
		return ErrSignupPrivileges
	}
	role := auth.Role(command.Role)
	if command.TenantID == "" {
		role = auth.Member
	}
	err := s.Repository.SetAccountAccess(ctx, requestContext.UserID, command.UserID, command.TenantID, role)
	if errors.Is(err, ErrAccountRole) {
		return appweb.ErrAccountRole
	}
	return err
}

// RemoveMember drops one account from the caller's company. The account stays
// and returns to the unassigned state.
func (s *WebService) RemoveMember(ctx context.Context, requestContext appweb.RequestContext, command appweb.AccountCommand) error {
	if s == nil || s.Repository == nil {
		return errors.New("web repository is not configured")
	}
	if requestContext.Role != "tenant_admin" || requestContext.UserID == "" || requestContext.TenantID == "" {
		return errors.New("company administrator role is required")
	}
	return s.Repository.RemoveTenantMember(ctx, requestContext.UserID, requestContext.TenantID, command.UserID)
}

// DeleteAccount removes the account itself, together with its sessions.
func (s *WebService) DeleteAccount(ctx context.Context, requestContext appweb.RequestContext, command appweb.AccountCommand) error {
	if s == nil || s.Repository == nil {
		return errors.New("web repository is not configured")
	}
	if requestContext.Role != "platform_admin" || requestContext.UserID == "" {
		return ErrSignupPrivileges
	}
	return s.Repository.DeleteAccount(ctx, requestContext.UserID, command.UserID)
}

func accountRoleLabel(role auth.Role) string {
	if role == auth.TenantAdmin {
		return "회사 관리자"
	}
	return "일반 사용자"
}

func (s *WebService) SaveFilter(ctx context.Context, requestContext appweb.RequestContext, command appweb.FilterCommand) error {
	if err := requireTenantAdmin(requestContext); err != nil {
		return err
	}
	rule := filterRuleFromWebCommand(command)
	raw, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	return s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		if err := tryCollectionLock(ctx, tx); err != nil {
			return fmt.Errorf("wait for collection before saving filter: %w", err)
		}
		now := s.now()
		notices, err := loadActiveNotices(ctx, tx, now)
		if err != nil {
			return err
		}
		if err := saveFilter(ctx, tx, requestContext.TenantID, command.Name, raw); err != nil {
			return err
		}
		filter := StoredFilter{TenantID: requestContext.TenantID, Rule: rule}
		if err := tx.QueryRow(ctx, `SELECT id::text, updated_at FROM public.filters
WHERE tenant_id=$1::uuid AND name=$2 AND enabled`, requestContext.TenantID, command.Name).Scan(&filter.ID, &filter.Revision); err != nil {
			return fmt.Errorf("load saved filter revision: %w", err)
		}
		return refreshFilterMatches(ctx, tx, now, filter, notices)
	})
}

type filterMatchBatcher interface {
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

func refreshFilterMatches(ctx context.Context, tx filterMatchBatcher, now time.Time, filter StoredFilter, notices []ActiveNotice) error {
	batch := &pgx.Batch{}
	for _, current := range notices {
		result := matcher.MatchAt(now, current.Notice, filter.Rule)
		if !result.Matched {
			batch.Queue(`WITH eligible_filter AS MATERIALIZED (
    SELECT id FROM public.filters
    WHERE tenant_id=$1::uuid AND id=$2::uuid AND enabled AND updated_at=$4
    FOR UPDATE
)
DELETE FROM public.matches m USING eligible_filter f
WHERE m.tenant_id=$1::uuid AND m.filter_id=f.id AND m.notice_id=$3::uuid`,
				filter.TenantID, filter.ID, current.ID, filter.Revision)
			continue
		}
		payload, err := json.Marshal(struct {
			Reasons []matcher.Reason `json:"reasons"`
			Details []matcher.Detail `json:"details"`
		}{result.Reasons, result.Details})
		if err != nil {
			return fmt.Errorf("encode refreshed filter match: %w", err)
		}
		batch.Queue(`WITH eligible_filter AS MATERIALIZED (
    SELECT id FROM public.filters
    WHERE tenant_id=$1::uuid AND id=$2::uuid AND enabled AND updated_at=$5
    FOR UPDATE
)
INSERT INTO public.matches (tenant_id, filter_id, notice_id, reasons)
SELECT $1::uuid, $2::uuid, $3::uuid, $4 FROM eligible_filter
ON CONFLICT (tenant_id, filter_id, notice_id) DO UPDATE SET reasons=EXCLUDED.reasons`,
			filter.TenantID, filter.ID, current.ID, payload, filter.Revision)
	}
	if batch.Len() == 0 {
		return nil
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("refresh filter matches: %w", err)
	}
	return nil
}

type webQueryExecer interface {
	webExecer
	QueryRow(context.Context, string, ...any) pgx.Row
}

func saveFilter(ctx context.Context, tx webQueryExecer, tenantID, name string, rules []byte) error {
	var filterID string
	err := tx.QueryRow(ctx, `INSERT INTO public.filters (tenant_id, name, rules)
VALUES ($1::uuid, $2, $3::jsonb)
ON CONFLICT (tenant_id, name) DO NOTHING
RETURNING id::text`, tenantID, name, rules).Scan(&filterID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("insert filter: %w", err)
	}

	var rulesChanged, enabled bool
	err = tx.QueryRow(ctx, `SELECT id::text, rules IS DISTINCT FROM $3::jsonb, enabled
FROM public.filters
WHERE tenant_id=$1::uuid AND name=$2
FOR UPDATE`, tenantID, name, rules).Scan(&filterID, &rulesChanged, &enabled)
	if err != nil {
		return fmt.Errorf("lock existing filter: %w", err)
	}
	if !rulesChanged && enabled {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE public.filters
SET rules=$3::jsonb, enabled=true, updated_at=now()
WHERE tenant_id=$1::uuid AND id=$2::uuid`, tenantID, filterID, rules)
	if err != nil {
		return fmt.Errorf("update filter: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	if !rulesChanged {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.matches
WHERE tenant_id=$1::uuid AND filter_id=$2::uuid`, tenantID, filterID); err != nil {
		return fmt.Errorf("delete stale filter matches: %w", err)
	}
	return nil
}

func (s *WebService) ToggleFilter(ctx context.Context, requestContext appweb.RequestContext, command appweb.ToggleFilterCommand) error {
	if err := requireTenantAdmin(requestContext); err != nil {
		return err
	}
	var notices []ActiveNotice
	var err error
	var now time.Time
	return s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		if command.Enabled {
			if err := tryCollectionLock(ctx, tx); err != nil {
				return fmt.Errorf("wait for collection before enabling filter: %w", err)
			}
			now = s.now()
			notices, err = loadActiveNotices(ctx, tx, now)
			if err != nil {
				return err
			}
		}
		if err := toggleFilter(ctx, tx, requestContext.TenantID, command); err != nil || !command.Enabled {
			return err
		}
		filter := StoredFilter{ID: command.FilterID, TenantID: requestContext.TenantID}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT rules, updated_at FROM public.filters
WHERE tenant_id=$1::uuid AND id=$2::uuid AND enabled`, requestContext.TenantID, command.FilterID).Scan(&raw, &filter.Revision); err != nil {
			return fmt.Errorf("load enabled filter revision: %w", err)
		}
		if err := json.Unmarshal(raw, &filter.Rule); err != nil {
			return fmt.Errorf("decode enabled filter rule: %w", err)
		}
		return refreshFilterMatches(ctx, tx, now, filter, notices)
	})
}

type webExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func toggleFilter(ctx context.Context, tx webExecer, tenantID string, command appweb.ToggleFilterCommand) error {
	tag, err := tx.Exec(ctx, `UPDATE public.filters SET enabled=$3, updated_at=now()
WHERE tenant_id=$1::uuid AND id=$2::uuid`, tenantID, command.FilterID, command.Enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	if command.Enabled {
		return nil
	}
	_, err = tx.Exec(ctx, `DELETE FROM public.matches
WHERE tenant_id=$1::uuid AND filter_id=$2::uuid`, tenantID, command.FilterID)
	return err
}

func (s *WebService) DeleteFilter(ctx context.Context, requestContext appweb.RequestContext, command appweb.DeleteFilterCommand) error {
	if requestContext.TenantID == "" || requestContext.Role != "tenant_admin" {
		return errors.New("tenant administrator role is required")
	}
	return s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		return deleteFilter(ctx, tx, requestContext.TenantID, command)
	})
}

func deleteFilter(ctx context.Context, tx webExecer, tenantID string, command appweb.DeleteFilterCommand) error {
	tag, err := tx.Exec(ctx, `DELETE FROM public.filters
WHERE tenant_id=$1::uuid AND id=$2::uuid`, tenantID, command.FilterID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *WebService) SaveNotification(ctx context.Context, requestContext appweb.RequestContext, command appweb.NotificationCommand) error {
	if err := requireTenantAdmin(requestContext); err != nil {
		return err
	}
	parsed, err := time.Parse("15:04", command.DeliveryTime)
	if err != nil || command.Timezone != "Asia/Seoul" || len(command.DeliveryDays) == 0 {
		return errors.New("invalid notification schedule")
	}
	days := make([]int16, len(command.DeliveryDays))
	for i, day := range command.DeliveryDays {
		if day < 0 || day > 6 {
			return errors.New("invalid notification weekday")
		}
		days[i] = int16(day)
	}
	return s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.schedules (tenant_id, name, hour, minute, timezone, weekdays)
VALUES ($1::uuid, '기본 알림', $2, $3, $4, $5)
ON CONFLICT (tenant_id, name) DO UPDATE SET hour=EXCLUDED.hour, minute=EXCLUDED.minute,
timezone=EXCLUDED.timezone, weekdays=EXCLUDED.weekdays, enabled=true`,
			requestContext.TenantID, parsed.Hour(), parsed.Minute(), command.Timezone, days)
		return err
	})
}

func (s *WebService) SaveReportSchedule(ctx context.Context, requestContext appweb.RequestContext, command appweb.NotificationCommand) error {
	if err := requireReportAdmin(requestContext); err != nil {
		return err
	}
	return s.SaveNotification(ctx, requestContext, command)
}

func (s *WebService) GenerateReport(ctx context.Context, requestContext appweb.RequestContext) error {
	if err := requireReportAdmin(requestContext); err != nil {
		return err
	}
	if s == nil {
		return errors.New("report generation is unavailable")
	}
	run := s.RunManualReport
	if run == nil {
		if s.Repository == nil || s.ReportStore == nil {
			return errors.New("report generation is unavailable")
		}
		runner := ReportRunner{Repository: s.Repository, Writer: s.ReportStore, Now: s.now}
		run = runner.RunManual
	}
	outcome, err := run(ctx, requestContext.TenantID)
	if err != nil {
		return err
	}
	if !outcome.Created {
		return appweb.ErrNoReportMatches
	}
	return nil
}

func (s *WebService) RetryReport(ctx context.Context, requestContext appweb.RequestContext, reportID string) error {
	if err := requireReportAdmin(requestContext); err != nil {
		return err
	}
	if s == nil || s.Repository == nil || s.ReportStore == nil {
		return errors.New("report retry is unavailable")
	}
	runner := ReportRunner{Repository: s.Repository, Writer: s.ReportStore, Now: s.now}
	outcome, err := runner.Retry(ctx, requestContext.TenantID, reportID)
	if err != nil {
		return err
	}
	if !outcome.Created {
		return appweb.ErrReportNotFound
	}
	return nil
}

func (s *WebService) OpenReport(ctx context.Context, requestContext appweb.RequestContext, reportID string) (appweb.ReportDownload, error) {
	if requestContext.TenantID == "" || !allowedReportReader(requestContext.Role) {
		return appweb.ReportDownload{}, appweb.ErrReportNotFound
	}
	if s == nil || s.Repository == nil || s.ReportStore == nil {
		return appweb.ReportDownload{}, errors.New("report download is unavailable")
	}
	return openReportAfterMetadata(func() (string, error) {
		var relativePath string
		err := s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
			var err error
			relativePath, err = reportDownloadPath(ctx, tx, requestContext.TenantID, reportID)
			return err
		})
		return relativePath, err
	}, s.ReportStore.Open)
}

type reportDownloadQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type reportFileOpen func(string) (*os.File, os.FileInfo, error)

type reportMetadataLoad func() (string, error)

func reportDownloadPath(ctx context.Context, queryer reportDownloadQueryer, tenantID, reportID string) (string, error) {
	if queryer == nil || tenantID == "" || reportID == "" {
		return "", errors.New("report download metadata dependencies are required")
	}
	var relativePath string
	err := queryer.QueryRow(ctx, `SELECT relative_path
FROM public.reports
WHERE tenant_id=$1::uuid AND id=$2::uuid AND status='generated' AND deleted_at IS NULL AND relative_path<>''`, tenantID, reportID).Scan(&relativePath)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", appweb.ErrReportNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load report download metadata: %w", err)
	}
	return relativePath, nil
}

func openReportAfterMetadata(load reportMetadataLoad, open reportFileOpen) (appweb.ReportDownload, error) {
	if load == nil || open == nil {
		return appweb.ReportDownload{}, errors.New("report download dependencies are required")
	}
	relativePath, err := load()
	if err != nil {
		return appweb.ReportDownload{}, err
	}
	if relativePath == "" {
		return appweb.ReportDownload{}, errors.New("report download path is required")
	}
	file, info, err := open(relativePath)
	if errors.Is(err, fs.ErrNotExist) {
		return appweb.ReportDownload{}, appweb.ErrReportNotFound
	}
	if err != nil {
		return appweb.ReportDownload{}, fmt.Errorf("open stored report: %w", err)
	}
	if file == nil || info == nil {
		if file != nil {
			_ = file.Close()
		}
		return appweb.ReportDownload{}, errors.New("stored report is unavailable")
	}
	return appweb.ReportDownload{Name: path.Base(relativePath), Modified: info.ModTime(), Body: file}, nil
}

func allowedReportReader(role string) bool {
	return role == "member" || role == "tenant_admin" || role == "platform_admin"
}

func (s *WebService) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *WebService) AddRecipient(ctx context.Context, requestContext appweb.RequestContext, command appweb.RecipientCommand) error {
	if err := requireTenantAdmin(requestContext); err != nil {
		return err
	}
	command.Name = strings.TrimSpace(command.Name)
	command.Email = strings.ToLower(strings.TrimSpace(command.Email))
	address, err := mail.ParseAddress(command.Email)
	if command.Name == "" || err != nil || address.Address != command.Email {
		return errors.New("invalid recipient")
	}
	return s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.recipients (tenant_id, name, email)
VALUES ($1::uuid, $2, $3) ON CONFLICT (tenant_id, (lower(email)))
DO UPDATE SET name=EXCLUDED.name, enabled=true`, requestContext.TenantID, command.Name, command.Email)
		return err
	})
}

func (s *WebService) SaveSettings(ctx context.Context, requestContext appweb.RequestContext, command appweb.SettingsCommand) error {
	if err := requireTenantAdmin(requestContext); err != nil {
		return err
	}
	return s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.tenants SET name=$2, contact_email=$3 WHERE id=$1::uuid`,
			requestContext.TenantID, command.TenantName, command.ContactEmail)
		return err
	})
}

func (s *WebService) RunCollection(_ context.Context, requestContext appweb.RequestContext) error {
	if requestContext.Role != "platform_admin" || s.QueueCollection == nil {
		return errors.New("platform collection action is unavailable")
	}
	return s.QueueCollection()
}

func (s *WebService) SendTestMail(ctx context.Context, requestContext appweb.RequestContext) error {
	if requestContext.Role != "platform_admin" || s.TestMail == nil || requestContext.Email == "" {
		return errors.New("platform test-mail action is unavailable")
	}
	return s.TestMail(ctx, requestContext.Email)
}

func requireTenantAdmin(requestContext appweb.RequestContext) error {
	if requestContext.TenantID == "" || (requestContext.Role != "tenant_admin" && requestContext.Role != "platform_admin") {
		return errors.New("tenant administrator role is required")
	}
	return nil
}

func requireReportAdmin(requestContext appweb.RequestContext) error {
	if requestContext.TenantID == "" || requestContext.Role != "tenant_admin" {
		return errors.New("tenant administrator role is required")
	}
	return nil
}

func filterRuleFromWebCommand(command appweb.FilterCommand) matcher.Rule {
	rule := matcher.Rule{
		Exclude:            splitTerms(command.ExcludeKeywords),
		Agencies:           splitTerms(command.Agency),
		Regions:            splitTerms(command.Region),
		MinAmount:          command.MinimumAmount,
		DeadlineWithinDays: command.DeadlineDays,
	}
	terms := splitTerms(command.IncludeKeywords)
	if command.IncludeMode == "all" {
		rule.IncludeAll = terms
	} else {
		rule.IncludeAny = terms
	}
	if category := categoryFromLabel(command.Category); category != "" {
		rule.Categories = []model.Category{category}
	}
	return rule
}

func splitTerms(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if value := model.NormalizeText(field); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func categoryFromLabel(value string) model.Category {
	return map[string]model.Category{"공사": model.CategoryConstruction, "용역": model.CategoryService, "물품": model.CategoryGoods, "외자": model.CategoryForeign}[value]
}

func categoryLabel(value model.Category) string {
	return map[model.Category]string{model.CategoryConstruction: "공사", model.CategoryService: "용역", model.CategoryGoods: "물품", model.CategoryForeign: "외자"}[value]
}

func formatKoreanTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	location, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		location = time.FixedZone("KST", 9*60*60)
	}
	return value.In(location).Format("2006.01.02 15:04")
}

func nextDeliveryAt(now time.Time, hour, minute int, weekdays []int) (time.Time, bool) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || len(weekdays) == 0 {
		return time.Time{}, false
	}
	location, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		location = time.FixedZone("KST", 9*60*60)
	}
	allowed := make(map[time.Weekday]bool, len(weekdays))
	for _, day := range weekdays {
		if day < 0 || day > 6 {
			return time.Time{}, false
		}
		allowed[time.Weekday(day)] = true
	}
	localNow := now.In(location)
	for offset := 0; offset <= 7; offset++ {
		date := localNow.AddDate(0, 0, offset)
		candidate := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, location)
		if allowed[candidate.Weekday()] && candidate.After(localNow) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

func formatWon(value int64) string {
	digits := strconv.FormatInt(value, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits + "원"
}

func reasonText(detail matcher.Detail) string {
	label := map[matcher.Reason]string{
		matcher.ReasonIncludeAny: "포함 키워드", matcher.ReasonIncludeAll: "필수 키워드",
		matcher.ReasonCategory: "업무구분", matcher.ReasonAgency: "기관", matcher.ReasonRegion: "지역",
		matcher.ReasonMinAmount: "최소 금액", matcher.ReasonMaxAmount: "최대 금액",
		matcher.ReasonDeadlineWeekday: "마감 요일", matcher.ReasonDeadlineWithinDays: "마감 잔여일",
	}[detail.Code]
	if label == "" {
		label = string(detail.Code)
	}
	if detail.RuleValue == "" {
		return label + " 일치"
	}
	text := fmt.Sprintf("%s ‘%s’ 일치", label, detail.RuleValue)
	if detail.NoticeValue != "" {
		text += fmt.Sprintf(" (공고: %s)", detail.NoticeValue)
	}
	return text
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func filterSummary(rule matcher.Rule) string {
	parts := make([]string, 0, 8)
	if len(rule.IncludeAny) > 0 {
		parts = append(parts, "ANY: "+strings.Join(rule.IncludeAny, ", "))
	}
	if len(rule.IncludeAll) > 0 {
		parts = append(parts, "ALL: "+strings.Join(rule.IncludeAll, ", "))
	}
	if len(rule.Exclude) > 0 {
		parts = append(parts, "제외: "+strings.Join(rule.Exclude, ", "))
	}
	if len(rule.Regions) > 0 {
		parts = append(parts, strings.Join(rule.Regions, ", "))
	}
	if len(rule.Categories) > 0 {
		labels := make([]string, 0, len(rule.Categories))
		for _, category := range rule.Categories {
			labels = append(labels, categoryLabel(category))
		}
		parts = append(parts, "업종: "+strings.Join(labels, ", "))
	}
	if len(rule.Agencies) > 0 {
		parts = append(parts, "기관: "+strings.Join(rule.Agencies, ", "))
	}
	if rule.MinAmount != nil {
		parts = append(parts, formatWon(*rule.MinAmount)+" 이상")
	}
	if rule.DeadlineWithinDays != nil {
		parts = append(parts, "마감 "+strconv.Itoa(*rule.DeadlineWithinDays)+"일 이내")
	}
	if len(parts) == 0 {
		return "전체 공고"
	}
	return strings.Join(parts, " · ")
}
