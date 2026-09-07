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

	"namo/internal/matcher"
	"namo/internal/report"
)

var _ ReportRepository = (*PostgresRepository)(nil)

// WithReportClaim serializes file publication with reclaim and deletion. The
// claim is checked under the same row lock deletion takes before removing files.
func (r *PostgresRepository) WithReportClaim(ctx context.Context, work ReportWork, publish func() error) error {
	return r.withTenant(ctx, work.TenantID, func(tx pgx.Tx) error {
		return withReportClaim(ctx, tx, work, publish)
	})
}

func withReportClaim(ctx context.Context, tx reportStore, work ReportWork, publish func() error) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM public.reports
WHERE tenant_id=$1::uuid AND id=$2::uuid AND claim_token=$3::uuid
  AND status='generating' AND deleted_at IS NULL
FOR UPDATE`, work.TenantID, work.ReportID, work.ClaimToken).Scan(&id)
	if err != nil {
		return fmt.Errorf("lock active report claim: %w", err)
	}
	return publish()
}

type reportStore interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type reportSchedule struct {
	ID, Name, TimeZone      string
	Hour, Minute            int
	Weekdays                []int16
	LastSuccess, PendingDue time.Time
	PendingWindowEnd        time.Time
}

func (r *PostgresRepository) ClaimDueReports(ctx context.Context, now time.Time) ([]ReportWork, error) {
	if now.IsZero() {
		return nil, errors.New("report claim time is required")
	}
	tenants, err := r.tenantCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var works []ReportWork
	for _, tenant := range tenants {
		err := r.withTenant(ctx, tenant.ID, func(tx pgx.Tx) error {
			if err := tryCollectionLock(ctx, tx); err != nil {
				return fmt.Errorf("wait for collection before report snapshot: %w", err)
			}
			schedules, err := loadReportSchedules(ctx, tx, tenant.ID)
			if err != nil {
				return err
			}
			for _, schedule := range schedules {
				if schedule.TimeZone != "Asia/Seoul" {
					return fmt.Errorf("schedule %s has unsupported timezone %q", schedule.ID, schedule.TimeZone)
				}
				digestSchedule := digestScheduleRow{
					ID: schedule.ID, Hour: schedule.Hour, Minute: schedule.Minute, TimeZone: schedule.TimeZone,
					Weekdays: schedule.Weekdays, LastSuccess: schedule.LastSuccess,
					PendingDue: schedule.PendingDue, PendingWindowEnd: schedule.PendingWindowEnd,
				}
				dueAt, windowEnd, existing, ok := selectDigestWindow(now, digestSchedule)
				if !ok {
					continue
				}
				if !existing {
					windowEnd, err = getOrCreateDigestWindow(ctx, tx, tenant.ID, schedule.ID, dueAt, now, schedule.LastSuccess)
					if err != nil {
						return fmt.Errorf("fix schedule %s report window: %w", schedule.ID, err)
					}
				}
				work, claimed, err := claimScheduledReport(ctx, tx, tenant.ID, tenant.Name, schedule, dueAt, windowEnd, now)
				if err != nil {
					return fmt.Errorf("claim schedule %s report: %w", schedule.ID, err)
				}
				if claimed {
					works = append(works, work)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("claim tenant %s reports: %w", tenant.ID, err)
		}
	}
	return works, nil
}

func loadReportSchedules(ctx context.Context, tx reportStore, tenantID string) ([]reportSchedule, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, name, hour, minute, timezone, weekdays, last_success_at
FROM public.schedules
WHERE tenant_id=$1::uuid AND enabled
ORDER BY id
FOR UPDATE`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query report schedules: %w", err)
	}
	defer rows.Close()
	var schedules []reportSchedule
	for rows.Next() {
		var schedule reportSchedule
		var lastSuccess *time.Time
		if err := rows.Scan(&schedule.ID, &schedule.Name, &schedule.Hour, &schedule.Minute, &schedule.TimeZone, &schedule.Weekdays, &lastSuccess); err != nil {
			return nil, fmt.Errorf("scan report schedule: %w", err)
		}
		if lastSuccess != nil {
			schedule.LastSuccess = *lastSuccess
		}
		schedules = append(schedules, schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate report schedules: %w", err)
	}
	rows.Close()
	for index := range schedules {
		err := tx.QueryRow(ctx, `SELECT w.due_at,w.window_end_at
FROM public.digest_windows w
LEFT JOIN public.reports r
  ON r.tenant_id=w.tenant_id AND r.schedule_id=w.schedule_id AND r.due_at=w.due_at AND r.trigger='scheduled'
WHERE w.tenant_id=$1::uuid AND w.schedule_id=$2::uuid AND w.status = 'completed'
  AND EXISTS (
    SELECT 1 FROM public.digest_window_items i
    WHERE i.tenant_id=w.tenant_id AND i.schedule_id=w.schedule_id
      AND i.due_at=w.due_at AND i.window_end_at=w.window_end_at
  )
  AND (r.id IS NULL OR (r.deleted_at IS NULL AND r.attempts < 3 AND
    (r.status = 'failed' OR (r.status = 'generating' AND r.claimed_at < pg_catalog.clock_timestamp() - interval '15 minutes'))))
ORDER BY w.due_at DESC
LIMIT 1`, tenantID, schedules[index].ID).Scan(&schedules[index].PendingDue, &schedules[index].PendingWindowEnd)
		if err == nil {
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("query completed report window for schedule %s: %w", schedules[index].ID, err)
		}
		err = tx.QueryRow(ctx, `SELECT due_at, window_end_at
FROM public.digest_windows
WHERE tenant_id=$1::uuid AND schedule_id=$2::uuid AND status='pending'`, tenantID, schedules[index].ID).Scan(&schedules[index].PendingDue, &schedules[index].PendingWindowEnd)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("query pending report window for schedule %s: %w", schedules[index].ID, err)
		}
	}
	return schedules, nil
}

func claimScheduledReport(ctx context.Context, tx reportStore, tenantID, tenantName string, schedule reportSchedule, dueAt, windowEnd, now time.Time) (ReportWork, bool, error) {
	var hasItems, deleted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM public.digest_window_items
  WHERE tenant_id=$1::uuid AND schedule_id=$2::uuid AND due_at=$3 AND window_end_at=$4
), EXISTS (
  SELECT 1 FROM public.reports
  WHERE tenant_id=$1::uuid AND schedule_id=$2::uuid AND due_at=$3
    AND trigger='scheduled' AND deleted_at IS NOT NULL
)`, tenantID, schedule.ID, dueAt, windowEnd).Scan(&hasItems, &deleted); err != nil {
		return ReportWork{}, false, fmt.Errorf("check scheduled report snapshot: %w", err)
	}
	// A deleted report keeps its window identity. Use the existing terminal
	// window handling, which waits for active delivery leases before advancing.
	if deleted || !hasItems {
		if err := completeNoopDigestWindow(ctx, tx, tenantID, schedule.ID, dueAt, windowEnd); err != nil {
			return ReportWork{}, false, err
		}
		return ReportWork{}, false, nil
	}
	var windowStart any
	if !schedule.LastSuccess.IsZero() {
		windowStart = schedule.LastSuccess
	}
	row := tx.QueryRow(ctx, `WITH candidate AS (
  SELECT pg_catalog.gen_random_uuid() AS id, pg_catalog.gen_random_uuid() AS token
), claimed AS (
  INSERT INTO public.reports
      (id,tenant_id,schedule_id,due_at,window_start_at,window_end_at,tenant_name,schedule_name,trigger,status,relative_path,attempts,claim_token,claimed_at)
  SELECT candidate.id,$1::uuid,$2::uuid,$3,$4,$5,$7,$8,'scheduled','generating',
         'reports/' || $1 || '/' || candidate.id::text || '.html',1,candidate.token,$6
  FROM candidate
  ON CONFLICT (tenant_id,schedule_id,due_at) WHERE trigger='scheduled'
  DO UPDATE SET status = 'generating', attempts = public.reports.attempts + 1,
      claim_token = pg_catalog.gen_random_uuid(), claimed_at = EXCLUDED.claimed_at, last_error = NULL
  WHERE public.reports.deleted_at IS NULL AND public.reports.attempts < 3
    AND ((public.reports.status = 'generating' AND public.reports.claimed_at < EXCLUDED.claimed_at - interval '15 minutes')
      OR public.reports.status = 'failed')
  RETURNING id,tenant_id,schedule_id,due_at,window_start_at,window_end_at,tenant_name,schedule_name,trigger,relative_path,claim_token,attempts
)
SELECT id::text,tenant_id::text,tenant_name,schedule_id::text,schedule_name,trigger,relative_path,claim_token::text,
       due_at,window_start_at,window_end_at,attempts
FROM claimed`, tenantID, schedule.ID, dueAt, windowStart, windowEnd, now, tenantName, schedule.Name)
	work, ok, err := scanReportWork(row)
	if err != nil || !ok {
		return ReportWork{}, ok, err
	}
	if _, err := tx.Exec(ctx, scheduledReportItemsSQL, tenantID, work.ReportID, schedule.ID, dueAt, windowEnd); err != nil {
		return ReportWork{}, false, fmt.Errorf("snapshot scheduled report items: %w", err)
	}
	if err := loadReportNotices(ctx, tx, &work); err != nil {
		return ReportWork{}, false, err
	}
	return work, true, nil
}

const scheduledReportItemsSQL = `INSERT INTO public.report_items
    (tenant_id,report_id,ordinal,match_id,notice_id,title,category,agency,region,amount,deadline_at,source_url,rule_name,reasons,
     source_kind,posted_at,collected_at,recorded_at)
SELECT $1::uuid,$2::uuid,row_number() OVER (ORDER BY n.published_at DESC NULLS LAST,i.title,i.notice_id,i.matched_at,i.match_id),
       i.match_id,i.notice_id,i.title,
       n.payload->>'Category',COALESCE(n.payload->>'Agency',''),COALESCE(n.payload->>'Region',''),
       COALESCE(NULLIF(n.payload->>'Amount',''),'0')::bigint,
       COALESCE(n.deadline_at,TIMESTAMPTZ '0001-01-01 00:00:00+00'),i.source_url,
       COALESCE(f.name,''),i.reasons,
       '입찰공고목록-입찰공고',n.published_at,n.collected_at,i.matched_at
FROM public.digest_window_items i
JOIN public.notices n ON n.id=i.notice_id
LEFT JOIN public.matches m ON m.tenant_id=i.tenant_id AND m.id=i.match_id
LEFT JOIN public.filters f ON f.tenant_id=m.tenant_id AND f.id=m.filter_id
WHERE i.tenant_id=$1::uuid AND i.schedule_id=$3::uuid AND i.due_at=$4 AND i.window_end_at=$5
ORDER BY n.published_at DESC NULLS LAST,i.title,i.notice_id,i.matched_at,i.match_id
ON CONFLICT (tenant_id,report_id,match_id) DO NOTHING`

func (r *PostgresRepository) ClaimManualReport(ctx context.Context, tenantID string, now time.Time) (ReportWork, bool, error) {
	if tenantID == "" || now.IsZero() {
		return ReportWork{}, false, errors.New("manual report requires tenant and claim time")
	}
	var work ReportWork
	var claimed bool
	err := r.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Evaluate and snapshot at one database time after acquiring the lock.
		snapshotAt, err := lockDigestSnapshot(ctx, tx)
		if err != nil {
			return err
		}
		work, claimed, err = claimManualReport(ctx, tx, tenantID, snapshotAt)
		return err
	})
	return work, claimed, err
}

func claimManualReport(ctx context.Context, tx interface {
	reportStore
	filterMatchBatcher
}, tenantID string, now time.Time) (ReportWork, bool, error) {
	// Every entry point (web and CLI) refreshes the same active filter set in
	// the claim transaction, before inspecting or copying stored matches.
	notices, err := loadActiveNotices(ctx, tx, now)
	if err != nil {
		return ReportWork{}, false, err
	}
	filters, err := loadEnabledFilters(ctx, tx, tenantID)
	if err != nil {
		return ReportWork{}, false, err
	}
	for _, filter := range filters {
		if err := refreshFilterMatches(ctx, tx, now, filter, notices); err != nil {
			return ReportWork{}, false, err
		}
	}
	var tenantName string
	if err := tx.QueryRow(ctx, `SELECT name FROM public.tenants WHERE id=$1::uuid`, tenantID).Scan(&tenantName); err != nil {
		return ReportWork{}, false, fmt.Errorf("load manual report tenant: %w", err)
	}
	var hasMatches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM public.matches m
  JOIN public.filters f ON f.tenant_id=m.tenant_id AND f.id=m.filter_id AND f.enabled
  JOIN public.notices n ON n.id=m.notice_id
  WHERE m.tenant_id=$1::uuid AND (n.deadline_at IS NULL OR n.deadline_at >= $2)
)`, tenantID, now).Scan(&hasMatches); err != nil {
		return ReportWork{}, false, fmt.Errorf("check manual report matches: %w", err)
	}
	if !hasMatches {
		return ReportWork{}, false, nil
	}
	row := tx.QueryRow(ctx, `WITH candidate AS (
  SELECT pg_catalog.gen_random_uuid() AS id,pg_catalog.gen_random_uuid() AS token
), claimed AS (
  INSERT INTO public.reports
      (id,tenant_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status,relative_path,attempts,claim_token,claimed_at)
  SELECT candidate.id,$1::uuid,$2,$2,$3,'수동','manual','generating',
         'reports/' || $1 || '/' || candidate.id::text || '.html',1,candidate.token,$2
  FROM candidate
  RETURNING id,tenant_id,due_at,window_start_at,window_end_at,tenant_name,schedule_name,trigger,relative_path,claim_token,attempts
)
SELECT id::text,tenant_id::text,tenant_name,''::text,schedule_name,trigger,relative_path,claim_token::text,
       due_at,window_start_at,window_end_at,attempts
FROM claimed`, tenantID, now, tenantName)
	work, ok, err := scanReportWork(row)
	if err != nil || !ok {
		return ReportWork{}, ok, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO public.report_items
    (tenant_id,report_id,ordinal,match_id,notice_id,title,category,agency,region,amount,deadline_at,source_url,rule_name,reasons,
     source_kind,posted_at,collected_at,recorded_at)
SELECT $1::uuid,$2::uuid,row_number() OVER (ORDER BY n.published_at DESC NULLS LAST,n.title,n.id,m.created_at,m.id),
       m.id,n.id,n.title,n.payload->>'Category',COALESCE(n.payload->>'Agency',''),
       COALESCE(n.payload->>'Region',''),COALESCE(NULLIF(n.payload->>'Amount',''),'0')::bigint,
       COALESCE(n.deadline_at,TIMESTAMPTZ '0001-01-01 00:00:00+00'),
       COALESCE(NULLIF(n.payload->>'SourceURL',''),n.payload->>'source_url',''),f.name,m.reasons,
       '입찰공고목록-입찰공고',n.published_at,n.collected_at,m.created_at
FROM public.matches m
JOIN public.filters f ON f.tenant_id=m.tenant_id AND f.id=m.filter_id AND f.enabled
JOIN public.notices n ON n.id=m.notice_id
WHERE m.tenant_id=$1::uuid AND (n.deadline_at IS NULL OR n.deadline_at >= $3)
ORDER BY n.published_at DESC NULLS LAST,n.title,n.id,m.created_at,m.id`, tenantID, work.ReportID, now)
	if err != nil {
		return ReportWork{}, false, fmt.Errorf("snapshot manual report items: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ReportWork{}, false, errors.New("manual report snapshot disappeared during claim")
	}
	if err := loadReportNotices(ctx, tx, &work); err != nil {
		return ReportWork{}, false, err
	}
	return work, true, nil
}

func (r *PostgresRepository) ReclaimReport(ctx context.Context, tenantID, reportID string) (ReportWork, bool, error) {
	var work ReportWork
	var claimed bool
	err := r.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		work, claimed, err = reclaimReport(ctx, tx, tenantID, reportID)
		return err
	})
	return work, claimed, err
}

func reclaimReport(ctx context.Context, tx reportStore, tenantID, reportID string) (ReportWork, bool, error) {
	row := tx.QueryRow(ctx, `WITH claimed AS (
  UPDATE public.reports
  SET status = 'generating',attempts = attempts + 1,claim_token = pg_catalog.gen_random_uuid(),claimed_at=pg_catalog.clock_timestamp(),last_error=NULL
  WHERE tenant_id=$1::uuid AND id=$2::uuid AND deleted_at IS NULL AND attempts < 3
    AND ((status = 'generating' AND claimed_at < pg_catalog.clock_timestamp() - interval '15 minutes') OR status = 'failed')
  RETURNING *
)
SELECT c.id::text,c.tenant_id::text,c.tenant_name,COALESCE(c.schedule_id::text,''),c.schedule_name,
       c.trigger,c.relative_path,c.claim_token::text,c.due_at,c.window_start_at,c.window_end_at,c.attempts
FROM claimed c`, tenantID, reportID)
	return scanAndLoadReport(ctx, tx, row)
}

func (r *PostgresRepository) RetryReport(ctx context.Context, tenantID, reportID string, now time.Time) (ReportWork, bool, error) {
	if now.IsZero() {
		return ReportWork{}, false, errors.New("report retry time is required")
	}
	var work ReportWork
	var claimed bool
	err := r.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		work, claimed, err = retryReport(ctx, tx, tenantID, reportID, now)
		return err
	})
	return work, claimed, err
}

func retryReport(ctx context.Context, tx reportStore, tenantID, reportID string, now time.Time) (ReportWork, bool, error) {
	row := tx.QueryRow(ctx, `WITH claimed AS (
  UPDATE public.reports
  SET status = 'generating',attempts = 1,claim_token = pg_catalog.gen_random_uuid(),claimed_at=$3,last_error=NULL
  WHERE tenant_id=$1::uuid AND id=$2::uuid AND deleted_at IS NULL AND status = 'failed'
  RETURNING *
)
SELECT c.id::text,c.tenant_id::text,c.tenant_name,COALESCE(c.schedule_id::text,''),c.schedule_name,
       c.trigger,c.relative_path,c.claim_token::text,c.due_at,c.window_start_at,c.window_end_at,c.attempts
FROM claimed c`, tenantID, reportID, now)
	return scanAndLoadReport(ctx, tx, row)
}

func scanAndLoadReport(ctx context.Context, tx reportStore, row pgx.Row) (ReportWork, bool, error) {
	work, ok, err := scanReportWork(row)
	if err != nil || !ok {
		return ReportWork{}, ok, err
	}
	if err := loadReportNotices(ctx, tx, &work); err != nil {
		return ReportWork{}, false, err
	}
	return work, true, nil
}

func scanReportWork(row pgx.Row) (ReportWork, bool, error) {
	var work ReportWork
	err := row.Scan(&work.ReportID, &work.TenantID, &work.TenantName, &work.ScheduleID, &work.ScheduleName,
		&work.Trigger, &work.RelativePath, &work.ClaimToken, &work.DueAt, &work.WindowStart, &work.WindowEnd, &work.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReportWork{}, false, nil
	}
	if err != nil {
		return ReportWork{}, false, fmt.Errorf("scan claimed report: %w", err)
	}
	return work, true, nil
}

func loadReportNotices(ctx context.Context, tx reportStore, work *ReportWork) error {
	rows, err := tx.Query(ctx, `SELECT notice_id::text,title,category,agency,region,amount,deadline_at,source_url,rule_name,reasons,
source_kind,posted_at,collected_at,recorded_at
FROM public.report_items
WHERE tenant_id=$1::uuid AND report_id=$2::uuid
ORDER BY ordinal`, work.TenantID, work.ReportID)
	if err != nil {
		return fmt.Errorf("query report items: %w", err)
	}
	defer rows.Close()
	byNotice := make(map[string]int)
	for rows.Next() {
		var noticeID, title, category, agency, region, sourceURL, ruleName string
		var amount int64
		var deadline time.Time
		var sourceKind *string
		var postedAt, collectedAt, recordedAt *time.Time
		var raw []byte
		if err := rows.Scan(&noticeID, &title, &category, &agency, &region, &amount, &deadline, &sourceURL, &ruleName, &raw,
			&sourceKind, &postedAt, &collectedAt, &recordedAt); err != nil {
			return fmt.Errorf("scan report item: %w", err)
		}
		var reasons storedMatchReasons
		if err := json.Unmarshal(raw, &reasons); err != nil {
			return fmt.Errorf("decode report item reasons: %w", err)
		}
		index, exists := byNotice[noticeID]
		if !exists {
			index = len(work.Notices)
			byNotice[noticeID] = index
			notice := report.Notice{
				ID: noticeID, Title: title, Category: category, Agency: agency, Region: region,
				Amount: amount, Deadline: deadline, SourceURL: sourceURL,
			}
			if sourceKind != nil {
				notice.SourceKind = *sourceKind
			}
			if postedAt != nil {
				notice.PostedAt = *postedAt
			}
			if collectedAt != nil {
				notice.CollectedAt = *collectedAt
			}
			if recordedAt != nil {
				notice.RecordedAt = *recordedAt
			}
			work.Notices = append(work.Notices, notice)
		}
		for _, keyword := range matchedKeywords(reasons) {
			work.Notices[index].Keywords = appendUnique(work.Notices[index].Keywords, keyword)
		}
		work.Notices[index].Matches = append(work.Notices[index].Matches, report.Match{RuleName: ruleName, Reasons: readableMatchReasons(reasons)})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate report items: %w", err)
	}
	return nil
}

func matchedKeywords(payload storedMatchReasons) []string {
	var keywords []string
	for _, detail := range payload.Details {
		switch matcher.Reason(detail.Code) {
		case matcher.ReasonIncludeAny, matcher.ReasonIncludeAll:
			keywords = appendUnique(keywords, detail.RuleValue)
		}
	}
	return keywords
}

func (r *PostgresRepository) FinalizeReport(ctx context.Context, work ReportWork, artifact ReportArtifact, generatedAt time.Time) error {
	if err := validateReportArtifact(work, artifact, generatedAt); err != nil {
		return err
	}
	return r.withTenant(ctx, work.TenantID, func(tx pgx.Tx) error {
		return finalizeReport(ctx, tx, work, artifact, generatedAt)
	})
}

func validateReportArtifact(work ReportWork, artifact ReportArtifact, generatedAt time.Time) error {
	if work.ReportID == "" || work.TenantID == "" || work.ClaimToken == "" || work.Attempts < 1 || generatedAt.IsZero() {
		return errors.New("claimed report and generation time are required")
	}
	if artifact.RelativePath == "" || artifact.RelativePath != work.RelativePath || strings.TrimSpace(artifact.SHA256) == "" || artifact.NoticeCount < 0 {
		return errors.New("report artifact does not match the claimed path")
	}
	return nil
}

func finalizeReport(ctx context.Context, tx reportStore, work ReportWork, artifact ReportArtifact, generatedAt time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE public.reports
SET status = 'generated',relative_path=$4,sha256=$5,notice_count=$6,generated_at=$7,last_error=NULL
WHERE tenant_id=$1::uuid AND id=$2::uuid AND status = 'generating' AND claim_token = $3::uuid AND attempts = $8`,
		work.TenantID, work.ReportID, work.ClaimToken, artifact.RelativePath, artifact.SHA256, artifact.NoticeCount, generatedAt, work.Attempts)
	if err != nil {
		return fmt.Errorf("finalize report: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("report claim is stale or already finalized")
	}
	if work.Trigger != "scheduled" {
		return nil
	}
	tag, err = tx.Exec(ctx, `WITH target_window AS MATERIALIZED (
  SELECT tenant_id,schedule_id,due_at,window_end_at,status
  FROM public.digest_windows
  WHERE tenant_id=$1::uuid AND schedule_id=$2::uuid AND due_at=$3 AND window_end_at=$4
  FOR UPDATE
), completed_window AS (
  UPDATE public.digest_windows w
  SET status='completed',completed_at=COALESCE(w.completed_at,$6)
  FROM target_window target
  WHERE target.status = 'pending'
    AND w.tenant_id=target.tenant_id AND w.schedule_id=target.schedule_id
    AND w.due_at=target.due_at AND w.window_end_at=target.window_end_at
  RETURNING w.window_end_at
), eligible_window AS (
  SELECT window_end_at FROM completed_window
  UNION ALL
  SELECT target.window_end_at FROM target_window target WHERE target.status = 'completed'
), advanced_schedule AS (
  UPDATE public.schedules
  SET last_success_at=GREATEST(COALESCE(last_success_at,'-infinity'::timestamptz),eligible_window.window_end_at)
  FROM eligible_window
  WHERE tenant_id=$1::uuid AND id=$2::uuid
  RETURNING id
)
SELECT 1 FROM advanced_schedule`, work.TenantID, work.ScheduleID, work.DueAt, work.WindowEnd, work.ReportID, generatedAt)
	if err != nil {
		return fmt.Errorf("complete scheduled report window: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("scheduled report window is missing or already completed")
	}
	return nil
}

func (r *PostgresRepository) FinalizeReportFailure(ctx context.Context, work ReportWork, reportErr error) error {
	if work.ReportID == "" || work.TenantID == "" || work.ClaimToken == "" || work.Attempts < 1 || reportErr == nil {
		return errors.New("claimed report and failure are required")
	}
	return r.withTenant(ctx, work.TenantID, func(tx pgx.Tx) error {
		return finalizeReportFailure(ctx, tx, work, reportErr)
	})
}

func finalizeReportFailure(ctx context.Context, tx reportStore, work ReportWork, reportErr error) error {
	tag, err := tx.Exec(ctx, `UPDATE public.reports
SET status = 'failed',last_error=$4
WHERE tenant_id=$1::uuid AND id=$2::uuid AND status = 'generating' AND claim_token = $3::uuid AND attempts = $5`,
		work.TenantID, work.ReportID, work.ClaimToken, reportErr.Error(), work.Attempts)
	if err != nil {
		return fmt.Errorf("finalize report failure: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("report claim is stale or already finalized")
	}
	return nil
}
