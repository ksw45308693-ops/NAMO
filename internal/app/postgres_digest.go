package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"namo/internal/digest"
	"namo/internal/store"
)

var _ DigestRepository = (*PostgresRepository)(nil)

type digestScheduleRow struct {
	ID               string
	Hour             int
	Minute           int
	TimeZone         string
	Weekdays         []int16
	LastSuccess      time.Time
	PendingDue       time.Time
	PendingWindowEnd time.Time
}

type digestRecipientRow struct{ ID, Email string }

type digestNoticeRow struct {
	ID, Title, SourceURL string
	ReasonsJSON          []byte
}

type digestRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type digestRowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type digestExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

const expiredDigestTerminalReason = "delivery cancelled: all matched notices expired before delivery"

type digestWindowStore interface {
	digestRowQuerier
	digestExecer
}

// DueDigests returns at most the newest missed window for every enabled
// schedule. Each tenant is read inside a transaction with its RLS context set.
func (r *PostgresRepository) DueDigests(ctx context.Context, _ time.Time) ([]DigestWork, error) {
	tenants, err := r.tenantCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var work []DigestWork
	for _, tenant := range tenants {
		err := r.withTenant(ctx, tenant.ID, func(tx pgx.Tx) error {
			snapshotAt, err := lockDigestSnapshot(ctx, tx)
			if err != nil {
				return err
			}
			schedules, err := loadDigestSchedules(ctx, tx, tenant.ID)
			if err != nil {
				return err
			}
			for _, schedule := range schedules {
				if schedule.TimeZone != "Asia/Seoul" {
					return fmt.Errorf("schedule %s has unsupported timezone %q", schedule.ID, schedule.TimeZone)
				}
				dueAt, windowEnd, existing, ok := selectDigestWindow(snapshotAt, schedule)
				if !ok {
					continue
				}
				if !existing {
					windowEnd, err = getOrCreateDigestWindow(ctx, tx, tenant.ID, schedule.ID, dueAt, snapshotAt, schedule.LastSuccess)
					if err != nil {
						return fmt.Errorf("fix schedule %s digest window: %w", schedule.ID, err)
					}
				}
				recipients, err := loadDigestRecipients(ctx, tx, tenant.ID, schedule.ID, dueAt, windowEnd)
				if err != nil {
					return fmt.Errorf("load schedule %s recipients: %w", schedule.ID, err)
				}
				notices, err := loadDigestNoticeRows(ctx, tx, tenant.ID, schedule.ID, dueAt, windowEnd, snapshotAt)
				if err != nil {
					return fmt.Errorf("load schedule %s matches: %w", schedule.ID, err)
				}
				items, err := buildDigestWorks(tenant.ID, schedule, dueAt, windowEnd, recipients, notices)
				if err != nil {
					return fmt.Errorf("build schedule %s digest: %w", schedule.ID, err)
				}
				work = append(work, items...)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("load tenant %s digests: %w", tenant.ID, err)
		}
	}
	return work, nil
}

func lockDigestSnapshot(ctx context.Context, tx digestWindowStore) (time.Time, error) {
	if err := tryCollectionLock(ctx, tx); err != nil {
		return time.Time{}, fmt.Errorf("wait for collection before digest snapshot: %w", err)
	}
	var cutoff time.Time
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&cutoff); err != nil {
		return time.Time{}, fmt.Errorf("read digest snapshot cutoff: %w", err)
	}
	return cutoff, nil
}

func getOrCreateDigestWindow(ctx context.Context, tx digestWindowStore, tenantID, scheduleID string, dueAt, windowEnd, lastSuccess time.Time) (time.Time, error) {
	var fixedWindowEnd time.Time
	err := tx.QueryRow(ctx, `INSERT INTO public.digest_windows
    (tenant_id, schedule_id, due_at, window_end_at, status)
VALUES ($1::uuid, $2::uuid, $3, $4, 'pending')
ON CONFLICT (tenant_id, schedule_id, due_at) DO NOTHING
RETURNING window_end_at`, tenantID, scheduleID, dueAt, windowEnd).Scan(&fixedWindowEnd)
	created := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT window_end_at FROM public.digest_windows
WHERE tenant_id = $1::uuid AND schedule_id = $2::uuid AND due_at = $3`, tenantID, scheduleID, dueAt).Scan(&fixedWindowEnd)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("create digest window: %w", err)
	}
	if !created {
		return fixedWindowEnd, nil
	}
	var since any
	if !lastSuccess.IsZero() {
		since = lastSuccess
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.digest_window_items
    (tenant_id, schedule_id, due_at, window_end_at, match_id, notice_id, title, source_url, reasons, matched_at)
SELECT $1::uuid, $2::uuid, $3, $4, m.id, n.id, n.title,
       COALESCE(NULLIF(n.payload->>'SourceURL', ''), n.payload->>'source_url', ''),
       m.reasons, m.created_at
FROM public.matches m
JOIN public.filters f ON f.tenant_id = m.tenant_id AND f.id = m.filter_id AND f.enabled
JOIN public.notices n ON n.id = m.notice_id
WHERE m.tenant_id = $1::uuid
  AND m.created_at > COALESCE($5::timestamptz, '-infinity'::timestamptz)
  AND m.created_at <= $4
  AND (n.deadline_at IS NULL OR n.deadline_at >= $4)
ORDER BY n.title, n.id, m.created_at, m.id`, tenantID, scheduleID, dueAt, fixedWindowEnd, since); err != nil {
		return time.Time{}, fmt.Errorf("snapshot digest window items: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.digest_window_recipients
    (tenant_id, schedule_id, due_at, window_end_at, recipient_id, email)
SELECT $1::uuid, $2::uuid, $3, $4, r.id, r.email
FROM public.recipients r
WHERE r.tenant_id = $1::uuid AND r.enabled
ORDER BY r.email, r.id`, tenantID, scheduleID, dueAt, fixedWindowEnd); err != nil {
		return time.Time{}, fmt.Errorf("snapshot digest window recipients: %w", err)
	}
	return fixedWindowEnd, nil
}

func loadDigestSchedules(ctx context.Context, tx pgx.Tx, tenantID string) ([]digestScheduleRow, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, hour, minute, timezone, weekdays, last_success_at
FROM public.schedules
WHERE tenant_id = $1::uuid AND enabled
ORDER BY id
FOR UPDATE`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query digest schedules: %w", err)
	}
	defer rows.Close()
	var schedules []digestScheduleRow
	for rows.Next() {
		var schedule digestScheduleRow
		var lastSuccess *time.Time
		if err := rows.Scan(&schedule.ID, &schedule.Hour, &schedule.Minute, &schedule.TimeZone, &schedule.Weekdays, &lastSuccess); err != nil {
			return nil, fmt.Errorf("scan digest schedule: %w", err)
		}
		if lastSuccess != nil {
			schedule.LastSuccess = *lastSuccess
		}
		schedules = append(schedules, schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate digest schedules: %w", err)
	}
	rows.Close()
	for i := range schedules {
		err := tx.QueryRow(ctx, `SELECT due_at, window_end_at
FROM public.digest_windows
WHERE tenant_id = $1::uuid AND schedule_id = $2::uuid AND status = 'pending'`, tenantID, schedules[i].ID).Scan(&schedules[i].PendingDue, &schedules[i].PendingWindowEnd)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("query pending digest window for schedule %s: %w", schedules[i].ID, err)
		}
	}
	return schedules, nil
}

func selectDigestWindow(now time.Time, schedule digestScheduleRow) (dueAt, windowEnd time.Time, existing, ok bool) {
	if !schedule.PendingDue.IsZero() {
		if schedule.PendingWindowEnd.Before(schedule.PendingDue) {
			return time.Time{}, time.Time{}, false, false
		}
		return schedule.PendingDue, schedule.PendingWindowEnd, true, true
	}
	dueAt, ok = latestDigestDue(now, schedule.LastSuccess, schedule.Hour, schedule.Minute, schedule.Weekdays)
	return dueAt, time.Time{}, false, ok
}

func loadDigestRecipients(ctx context.Context, tx digestRowsQuerier, tenantID, scheduleID string, dueAt, windowEnd time.Time) ([]digestRecipientRow, error) {
	rows, err := tx.Query(ctx, `SELECT recipient_id::text, email
FROM public.digest_window_recipients
WHERE tenant_id = $1::uuid AND schedule_id = $2::uuid AND due_at = $3 AND window_end_at = $4
ORDER BY email, recipient_id`, tenantID, scheduleID, dueAt, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("query digest recipients: %w", err)
	}
	defer rows.Close()
	var recipients []digestRecipientRow
	for rows.Next() {
		var recipient digestRecipientRow
		if err := rows.Scan(&recipient.ID, &recipient.Email); err != nil {
			return nil, fmt.Errorf("scan digest recipient: %w", err)
		}
		recipients = append(recipients, recipient)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate digest recipients: %w", err)
	}
	return recipients, nil
}

func loadDigestNoticeRows(ctx context.Context, tx digestRowsQuerier, tenantID, scheduleID string, dueAt, windowEnd, cutoff time.Time) ([]digestNoticeRow, error) {
	rows, err := tx.Query(ctx, `SELECT i.notice_id::text, i.title, i.source_url, i.reasons
FROM public.digest_window_items i
JOIN public.notices n ON n.id = i.notice_id
WHERE i.tenant_id = $1::uuid AND i.schedule_id = $2::uuid AND i.due_at = $3 AND i.window_end_at = $4
  AND (n.deadline_at IS NULL OR n.deadline_at >= $5)
ORDER BY i.title, i.notice_id, i.matched_at, i.match_id`, tenantID, scheduleID, dueAt, windowEnd, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query digest matches: %w", err)
	}
	defer rows.Close()
	var notices []digestNoticeRow
	for rows.Next() {
		var notice digestNoticeRow
		if err := rows.Scan(&notice.ID, &notice.Title, &notice.SourceURL, &notice.ReasonsJSON); err != nil {
			return nil, fmt.Errorf("scan digest match: %w", err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate digest matches: %w", err)
	}
	return notices, nil
}

func latestDigestDue(now, lastSuccess time.Time, hour, minute int, weekdays []int16) (time.Time, bool) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || len(weekdays) == 0 {
		return time.Time{}, false
	}
	allowed := make(map[time.Weekday]bool, len(weekdays))
	for _, weekday := range weekdays {
		if weekday < 0 || weekday > 6 {
			return time.Time{}, false
		}
		allowed[time.Weekday(weekday)] = true
	}
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return time.Time{}, false
	}
	localNow := now.In(seoul)
	for daysAgo := 0; daysAgo <= 7; daysAgo++ {
		date := localNow.AddDate(0, 0, -daysAgo)
		candidate := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, seoul)
		if candidate.After(localNow) || !allowed[candidate.Weekday()] {
			continue
		}
		if !lastSuccess.IsZero() && !candidate.After(lastSuccess) {
			return time.Time{}, false
		}
		return candidate, true
	}
	return time.Time{}, false
}

func buildDigestWorks(tenantID string, schedule digestScheduleRow, dueAt, windowEnd time.Time, recipients []digestRecipientRow, rows []digestNoticeRow) ([]DigestWork, error) {
	notices, err := mergeDigestNoticeRows(rows)
	if err != nil {
		return nil, err
	}
	if len(notices) == 0 || len(recipients) == 0 {
		return []DigestWork{{TenantID: tenantID, ScheduleID: schedule.ID, DueAt: dueAt, WindowEnd: windowEnd}}, nil
	}
	work := make([]DigestWork, 0, len(recipients))
	for _, recipient := range recipients {
		work = append(work, DigestWork{
			TenantID: tenantID, ScheduleID: schedule.ID, RecipientID: recipient.ID,
			Recipient: recipient.Email, DueAt: dueAt, WindowEnd: windowEnd, Notices: notices,
		})
	}
	return work, nil
}

type storedMatchReasons struct {
	Reasons []string `json:"reasons"`
	Details []struct {
		Code, Field, RuleValue, NoticeValue string
	} `json:"details"`
}

func mergeDigestNoticeRows(rows []digestNoticeRow) ([]digest.Notice, error) {
	type accumulatedNotice struct {
		notice  digest.Notice
		reasons []string
		seen    map[string]bool
	}
	ordered := make([]*accumulatedNotice, 0, len(rows))
	byID := make(map[string]*accumulatedNotice, len(rows))
	for _, row := range rows {
		if row.ID == "" || strings.TrimSpace(row.Title) == "" {
			return nil, errors.New("digest notice requires an id and title")
		}
		var payload storedMatchReasons
		if err := json.Unmarshal(row.ReasonsJSON, &payload); err != nil {
			return nil, fmt.Errorf("decode reasons for notice %s: %w", row.ID, err)
		}
		item := byID[row.ID]
		if item == nil {
			item = &accumulatedNotice{
				notice: digest.Notice{Title: row.Title, URL: row.SourceURL},
				seen:   make(map[string]bool),
			}
			byID[row.ID] = item
			ordered = append(ordered, item)
		} else if item.notice.URL == "" {
			item.notice.URL = row.SourceURL
		}
		for _, reason := range readableMatchReasons(payload) {
			if !item.seen[reason] {
				item.seen[reason] = true
				item.reasons = append(item.reasons, reason)
			}
		}
	}
	notices := make([]digest.Notice, 0, len(ordered))
	for _, item := range ordered {
		if len(item.reasons) == 0 {
			item.reasons = []string{"등록된 조건과 일치"}
		}
		item.notice.Reason = strings.Join(item.reasons, ", ")
		notices = append(notices, item.notice)
	}
	return notices, nil
}

func readableMatchReasons(payload storedMatchReasons) []string {
	labels := map[string]string{
		"include_any":          "포함 키워드",
		"include_all":          "필수 키워드",
		"category":             "업무구분",
		"agency":               "기관",
		"region":               "지역",
		"min_amount":           "최소금액",
		"max_amount":           "최대금액",
		"deadline_weekday":     "마감 요일",
		"deadline_within_days": "마감 잔여일",
	}
	var reasons []string
	seenCode := make(map[string]bool)
	for _, detail := range payload.Details {
		label := labels[detail.Code]
		if label == "" {
			label = detail.Code
		}
		value := strings.TrimSpace(detail.RuleValue)
		if value != "" {
			label += ": " + value
		}
		if label != "" {
			reasons = append(reasons, label)
			seenCode[detail.Code] = true
		}
	}
	for _, code := range payload.Reasons {
		if seenCode[code] {
			continue
		}
		label := labels[code]
		if label == "" {
			label = code
		}
		if label != "" {
			reasons = append(reasons, label)
		}
	}
	return reasons
}

func (r *PostgresRepository) deliveryRepository() (store.DeliveryRepository, error) {
	if r == nil || r.Pool == nil {
		return store.DeliveryRepository{}, errors.New("database pool is not configured")
	}
	return store.DeliveryRepository{DB: store.PgxDeliveryBeginner{DB: r.Pool}}, nil
}

func (r *PostgresRepository) ClaimDelivery(ctx context.Context, claim DeliveryClaim) (DeliveryReservation, error) {
	repository, err := r.deliveryRepository()
	if err != nil {
		return DeliveryReservation{}, err
	}
	reservation, err := repository.Claim(ctx, store.DeliveryClaim{
		TenantID: claim.TenantID, ScheduleID: claim.ScheduleID, RecipientID: claim.RecipientID,
		IdempotencyKey: claim.IdempotencyKey, DueAt: claim.DueAt, WindowEnd: claim.WindowEnd,
	})
	if err != nil {
		return DeliveryReservation{}, err
	}
	return DeliveryReservation{Claimed: reservation.Claimed, Attempts: reservation.Attempts, ClaimToken: reservation.ClaimToken}, nil
}

func (r *PostgresRepository) FinalizeSent(ctx context.Context, claim DeliveryClaim, attempts int, sentAt time.Time) error {
	repository, err := r.deliveryRepository()
	if err != nil {
		return err
	}
	return repository.FinalizeSent(ctx, claim.TenantID, claim.ScheduleID, claim.RecipientID, claim.IdempotencyKey, claim.ClaimToken, claim.DueAt, claim.WindowEnd, attempts, sentAt)
}

func (r *PostgresRepository) FinalizeFailure(ctx context.Context, claim DeliveryClaim, attempts int, sendErr error) error {
	repository, err := r.deliveryRepository()
	if err != nil {
		return err
	}
	return repository.FinalizeFailure(ctx, claim.TenantID, claim.ScheduleID, claim.RecipientID, claim.IdempotencyKey, claim.ClaimToken, claim.DueAt, claim.WindowEnd, attempts, sendErr)
}

func (r *PostgresRepository) CompleteNoop(ctx context.Context, tenantID, scheduleID string, dueAt, windowEnd time.Time) error {
	if dueAt.IsZero() || windowEnd.Before(dueAt) {
		return errors.New("digest requires a valid fixed window")
	}
	return r.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return completeNoopDigestWindow(ctx, tx, tenantID, scheduleID, dueAt, windowEnd)
	})
}

func completeNoopDigestWindow(ctx context.Context, tx digestExecer, tenantID, scheduleID string, dueAt, windowEnd time.Time) error {
	tag, err := tx.Exec(ctx, `WITH target_window AS MATERIALIZED (
  SELECT w.tenant_id, w.schedule_id, w.due_at, w.window_end_at, w.status
  FROM public.digest_windows w
  WHERE w.tenant_id = $1::uuid AND w.schedule_id = $2::uuid
    AND w.due_at = $3 AND w.window_end_at = $4
  FOR UPDATE
), active_lease AS MATERIALIZED (
  SELECT 1
  FROM public.deliveries d
  JOIN target_window target
    ON d.tenant_id = target.tenant_id AND d.schedule_id = target.schedule_id
   AND d.due_at = target.due_at AND d.window_end_at = target.window_end_at
  WHERE target.status = 'pending'
    AND d.status = 'sending'
    AND d.claimed_at >= clock_timestamp() - interval '15 minutes'
  LIMIT 1
), cancelled_deliveries AS (
  UPDATE public.deliveries d
  SET status = 'failed',
      last_error = CASE
        WHEN position($5 in COALESCE(d.last_error, '')) > 0 THEN d.last_error
        ELSE concat_ws('; ', NULLIF(d.last_error, ''), $5)
      END
  FROM target_window target
  WHERE target.status = 'pending'
    AND NOT EXISTS (SELECT 1 FROM active_lease)
    AND d.tenant_id = target.tenant_id AND d.schedule_id = target.schedule_id
    AND d.due_at = target.due_at AND d.window_end_at = target.window_end_at
    AND d.status <> 'sent'
  RETURNING d.id
), completed_window AS (
  UPDATE public.digest_windows w
  SET status = 'completed', completed_at = COALESCE(w.completed_at, now())
  FROM target_window target
  WHERE target.status = 'pending'
    AND NOT EXISTS (SELECT 1 FROM active_lease)
    AND w.tenant_id = target.tenant_id AND w.schedule_id = target.schedule_id
    AND w.due_at = target.due_at AND w.window_end_at = target.window_end_at
    AND w.status = 'pending'
  RETURNING w.window_end_at
), advanced_schedule AS (
  UPDATE public.schedules s
  SET last_success_at = GREATEST(COALESCE(s.last_success_at, '-infinity'::timestamptz), completed_window.window_end_at)
  FROM completed_window
  WHERE s.tenant_id = $1::uuid AND s.id = $2::uuid
  RETURNING s.id
)
SELECT 1 FROM advanced_schedule
UNION ALL
SELECT 1 FROM target_window target WHERE target.status = 'completed'
UNION ALL
SELECT 1 FROM active_lease`,
		tenantID, scheduleID, dueAt, windowEnd, expiredDigestTerminalReason)
	if err != nil {
		return fmt.Errorf("terminalize empty digest: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("digest window is missing, failed, or has no schedule")
	}
	return nil
}
