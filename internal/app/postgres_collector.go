package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"namo/internal/matcher"
	"namo/internal/model"
)

func noticeHashes(notice model.Notice) ([]byte, []byte, error) {
	if err := notice.ValidateSource(); err != nil {
		return nil, nil, err
	}
	identity, err := hex.DecodeString(notice.Identity())
	if err != nil || len(identity) != sha256Size {
		return nil, nil, errors.New("invalid notice identity hash")
	}
	revision, err := hex.DecodeString(notice.Revision())
	if err != nil || len(revision) != sha256Size {
		return nil, nil, errors.New("invalid notice revision hash")
	}
	return identity, revision, nil
}

const sha256Size = 32

var ErrDailyAPICallBudget = errors.New("daily 나라장터 API call budget exhausted")

const defaultDailyAPICallLimit = 900

type budgetQueryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PostgresDailyCallBudget atomically shares the free API allowance across
// retries, commands, restarts, and concurrently running service instances.
type PostgresDailyCallBudget struct {
	DB    budgetQueryRower
	Limit int
	Now   func() time.Time
}

func (b PostgresDailyCallBudget) Take(ctx context.Context) error {
	if b.DB == nil {
		return errors.New("daily API call budget database is required")
	}
	limit := b.Limit
	if limit <= 0 {
		limit = defaultDailyAPICallLimit
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	day := now.In(time.FixedZone("Asia/Seoul", 9*60*60)).Format("2006-01-02")
	var calls int
	err := b.DB.QueryRow(ctx, `INSERT INTO public.api_daily_usage (usage_day, calls, updated_at)
		VALUES ($1::date, 1, now())
		ON CONFLICT (usage_day) DO UPDATE
		SET calls = public.api_daily_usage.calls + 1, updated_at = now()
		WHERE public.api_daily_usage.calls < $2
		RETURNING calls`, day, limit).Scan(&calls)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDailyAPICallBudget
	}
	if err != nil {
		return fmt.Errorf("consume daily API call budget: %w", err)
	}
	return nil
}

func (r *PostgresRepository) LastSuccessfulCollection(ctx context.Context) (time.Time, error) {
	var last *time.Time
	if err := r.Pool.QueryRow(ctx, `SELECT last_success_at FROM public.collection_state WHERE singleton`).Scan(&last); err != nil {
		return time.Time{}, fmt.Errorf("read collection state: %w", err)
	}
	if last == nil {
		return time.Time{}, nil
	}
	return *last, nil
}

func (r *PostgresRepository) StoreNotice(ctx context.Context, notice model.Notice) (StoredNotice, error) {
	identity, revision, err := noticeHashes(notice)
	if err != nil {
		return StoredNotice{}, err
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		return StoredNotice{}, fmt.Errorf("encode notice: %w", err)
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return StoredNotice{}, fmt.Errorf("begin notice transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	var existingRevision []byte
	var existingPayload []byte
	regionLookupComplete := notice.Region != ""
	var existingRegionLookupComplete bool
	err = tx.QueryRow(ctx, `SELECT id::text, revision_hash, payload, region_lookup_complete
		FROM public.notices WHERE identity_hash = $1 FOR UPDATE`, identity).Scan(&id, &existingRevision, &existingPayload, &existingRegionLookupComplete)
	if err == nil {
		notice, regionLookupComplete, err = mergeStoredRegion(notice, existingPayload, existingRegionLookupComplete)
		if err != nil {
			return StoredNotice{}, err
		}
		identity, revision, err = noticeHashes(notice)
		if err != nil {
			return StoredNotice{}, err
		}
		payload, err = json.Marshal(notice)
		if err != nil {
			return StoredNotice{}, fmt.Errorf("encode merged notice: %w", err)
		}
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, `INSERT INTO public.notices
			(identity_hash, revision_hash, source_id, title, published_at, deadline_at, payload, region_lookup_complete)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
			identity, revision, notice.BidNumber, notice.Title, nullableTime(notice.PostedAt), nullableTime(notice.Deadline), payload, regionLookupComplete,
		).Scan(&id); err != nil {
			return StoredNotice{}, fmt.Errorf("insert notice: %w", err)
		}
	case err != nil:
		return StoredNotice{}, fmt.Errorf("read notice revision: %w", err)
	case bytes.Equal(existingRevision, revision):
		if _, err := tx.Exec(ctx, `UPDATE public.notices SET collected_at = now() WHERE id = $1::uuid`, id); err != nil {
			return StoredNotice{}, fmt.Errorf("touch notice: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return StoredNotice{}, fmt.Errorf("commit unchanged notice: %w", err)
		}
		return StoredNotice{ID: id, Changed: false, Region: notice.Region, RegionLookupComplete: regionLookupComplete}, nil
	default:
		if _, err := tx.Exec(ctx, `UPDATE public.notices SET revision_hash=$2, source_id=$3, title=$4,
			published_at=$5, deadline_at=$6, payload=$7, region_lookup_complete=$8, collected_at=now() WHERE id=$1::uuid`,
			id, revision, notice.BidNumber, notice.Title, nullableTime(notice.PostedAt), nullableTime(notice.Deadline), payload, regionLookupComplete,
		); err != nil {
			return StoredNotice{}, fmt.Errorf("update notice: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.notice_revisions (notice_id, revision_hash, payload)
        VALUES ($1::uuid, $2, $3) ON CONFLICT (notice_id, revision_hash) DO NOTHING`, id, revision, payload); err != nil {
		return StoredNotice{}, fmt.Errorf("record notice revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredNotice{}, fmt.Errorf("commit notice: %w", err)
	}
	return StoredNotice{ID: id, Changed: true, Region: notice.Region, RegionLookupComplete: regionLookupComplete}, nil
}

func mergeStoredRegion(notice model.Notice, payload []byte, lookupComplete bool) (model.Notice, bool, error) {
	if notice.Region != "" || len(payload) == 0 {
		return notice, notice.Region != "", nil
	}
	var stored model.Notice
	if err := json.Unmarshal(payload, &stored); err != nil {
		return model.Notice{}, false, fmt.Errorf("decode stored notice for region preservation: %w", err)
	}
	if stored.Identity() != notice.Identity() {
		return model.Notice{}, false, errors.New("stored notice identity does not match incoming notice")
	}
	candidate := notice
	candidate.Region = stored.Region
	if candidate.Revision() == stored.Revision() {
		return candidate, lookupComplete, nil
	}
	// Other source fields changed while the list response omitted region. The
	// previous region can now be stale, so persist blank/pending and re-enrich.
	return notice, false, nil
}

func (r *PostgresRepository) ActiveNotices(ctx context.Context, now time.Time) ([]ActiveNotice, error) {
	return loadActiveNotices(ctx, r.Pool, now)
}

func loadActiveNotices(ctx context.Context, queryer filterRowsQuerier, now time.Time) ([]ActiveNotice, error) {
	rows, err := queryer.Query(ctx, `SELECT id::text, payload, region_lookup_complete
		FROM public.notices
		WHERE deadline_at IS NULL OR deadline_at >= $1
		ORDER BY published_at DESC NULLS LAST, id`, now)
	if err != nil {
		return nil, fmt.Errorf("query active notices: %w", err)
	}
	defer rows.Close()
	var notices []ActiveNotice
	for rows.Next() {
		var current ActiveNotice
		var payload []byte
		if err := rows.Scan(&current.ID, &payload, &current.RegionLookupComplete); err != nil {
			return nil, fmt.Errorf("scan active notice: %w", err)
		}
		if err := json.Unmarshal(payload, &current.Notice); err != nil {
			return nil, fmt.Errorf("decode active notice %s: %w", current.ID, err)
		}
		if err := current.Notice.ValidateSource(); err != nil {
			return nil, fmt.Errorf("validate active notice %s: %w", current.ID, err)
		}
		notices = append(notices, current)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active notices: %w", err)
	}
	return notices, nil
}

func (r *PostgresRepository) MarkRegionLookupComplete(ctx context.Context, noticeID string) error {
	tag, err := r.Pool.Exec(ctx, `UPDATE public.notices SET region_lookup_complete=true, collected_at=now()
		WHERE id=$1::uuid`, noticeID)
	if err != nil {
		return fmt.Errorf("mark region lookup complete: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("notice not found while completing region lookup")
	}
	return nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (r *PostgresRepository) StoreWarning(ctx context.Context, warning SourceWarning) error {
	detail, _ := json.Marshal(warning)
	_, err := r.Pool.Exec(ctx, `INSERT INTO public.source_warnings (category, page, item, field, code, detail)
        VALUES ($1, $2, $3, $4, $5, $6)`, warning.Category, warning.Page, warning.Item, warning.Field, warning.Code, detail)
	if err != nil {
		return fmt.Errorf("insert source warning: %w", err)
	}
	return nil
}

func (r *PostgresRepository) EnabledFilters(ctx context.Context) ([]StoredFilter, error) {
	tenants, err := r.tenantCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var filters []StoredFilter
	for _, tenant := range tenants {
		err := r.withTenant(ctx, tenant.ID, func(tx pgx.Tx) error {
			tenantFilters, err := loadEnabledFilters(ctx, tx, tenant.ID)
			if err == nil {
				filters = append(filters, tenantFilters...)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return filters, nil
}

type filterRowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadEnabledFilters(ctx context.Context, queryer filterRowsQuerier, tenantID string) ([]StoredFilter, error) {
	rows, err := queryer.Query(ctx, `SELECT id::text, rules, updated_at
FROM public.filters
WHERE tenant_id=$1::uuid AND enabled
ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query filters: %w", err)
	}
	defer rows.Close()
	var filters []StoredFilter
	for rows.Next() {
		var filter StoredFilter
		var raw []byte
		if err := rows.Scan(&filter.ID, &raw, &filter.Revision); err != nil {
			return nil, err
		}
		filter.TenantID = tenantID
		if err := json.Unmarshal(raw, &filter.Rule); err != nil {
			return nil, fmt.Errorf("decode filter %s: %w", filter.ID, err)
		}
		filters = append(filters, filter)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filters: %w", err)
	}
	return filters, nil
}

type revisionMatchStore interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type matchWriteResult struct {
	Created bool
	Applied bool
}

func (r *PostgresRepository) UpsertMatch(ctx context.Context, match StoredMatch) (bool, error) {
	payload, err := json.Marshal(struct {
		Reasons []matcher.Reason `json:"reasons"`
		Details []matcher.Detail `json:"details"`
	}{match.Reasons, match.Details})
	if err != nil {
		return false, fmt.Errorf("encode match reasons: %w", err)
	}
	created := false
	err = r.withTenant(ctx, match.TenantID, func(tx pgx.Tx) error {
		result, err := upsertMatchRevision(ctx, tx, match, payload)
		created = result.Created
		return err
	})
	return created, err
}

func upsertMatchRevision(ctx context.Context, tx revisionMatchStore, match StoredMatch, payload []byte) (matchWriteResult, error) {
	var created bool
	err := tx.QueryRow(ctx, `WITH eligible_filter AS MATERIALIZED (
    SELECT id FROM public.filters
    WHERE tenant_id=$1::uuid AND id=$2::uuid AND enabled AND updated_at=$5
    FOR UPDATE
)
INSERT INTO public.matches (tenant_id, filter_id, notice_id, reasons)
SELECT $1::uuid, $2::uuid, $3::uuid, $4 FROM eligible_filter
ON CONFLICT (tenant_id, filter_id, notice_id) DO NOTHING
RETURNING true`, match.TenantID, match.FilterID, match.NoticeID, payload, match.FilterRevision).Scan(&created)
	if err == nil {
		return matchWriteResult{Created: created, Applied: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return matchWriteResult{}, err
	}
	tag, err := tx.Exec(ctx, `WITH eligible_filter AS MATERIALIZED (
    SELECT id FROM public.filters
    WHERE tenant_id=$1::uuid AND id=$2::uuid AND enabled AND updated_at=$5
    FOR UPDATE
)
UPDATE public.matches m SET reasons=$4
FROM eligible_filter f
WHERE m.tenant_id=$1::uuid AND m.filter_id=f.id AND m.notice_id=$3::uuid`,
		match.TenantID, match.FilterID, match.NoticeID, payload, match.FilterRevision)
	if err != nil {
		return matchWriteResult{}, err
	}
	return matchWriteResult{Applied: tag.RowsAffected() == 1}, nil
}

func (r *PostgresRepository) DeleteMatch(ctx context.Context, match StoredMatch) error {
	return r.withTenant(ctx, match.TenantID, func(tx pgx.Tx) error {
		_, err := deleteMatchRevision(ctx, tx, match)
		return err
	})
}

func deleteMatchRevision(ctx context.Context, tx revisionMatchStore, match StoredMatch) (bool, error) {
	tag, err := tx.Exec(ctx, `WITH eligible_filter AS MATERIALIZED (
    SELECT id FROM public.filters
    WHERE tenant_id=$1::uuid AND id=$2::uuid AND enabled AND updated_at=$4
    FOR UPDATE
)
DELETE FROM public.matches m USING eligible_filter f
WHERE m.tenant_id=$1::uuid AND m.filter_id=f.id AND m.notice_id=$3::uuid`,
		match.TenantID, match.FilterID, match.NoticeID, match.FilterRevision)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *PostgresRepository) FinishCollection(ctx context.Context, at time.Time, result CollectionResult, runErr error) error {
	payload, _ := json.Marshal(result)
	var successAt any
	if !at.IsZero() && runErr == nil {
		successAt = at
	}
	var errorText any
	status := "succeeded"
	if runErr != nil {
		errorText = runErr.Error()
		status = "failed"
	}
	tenants, err := r.tenantCatalog(ctx)
	if err != nil {
		return err
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin collection completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE public.collection_state
		SET last_success_at=COALESCE($1, last_success_at), last_result=$2, last_error=$3, updated_at=now()
		WHERE singleton`, successAt, payload, errorText); err != nil {
		return fmt.Errorf("update collection state: %w", err)
	}
	for _, tenant := range tenants {
		if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('app.tenant_id', $1, true)`, tenant.ID); err != nil {
			return fmt.Errorf("set collection tenant context: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.job_runs (tenant_id, kind, status, started_at, finished_at, detail)
			VALUES ($1::uuid, 'collection', $2, now(), now(), $3)`, tenant.ID, status, payload); err != nil {
			return fmt.Errorf("record tenant collection run: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit collection completion: %w", err)
	}
	return nil
}
