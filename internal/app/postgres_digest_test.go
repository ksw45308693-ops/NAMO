package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type digestRowStub struct {
	value time.Time
	err   error
}

func (r digestRowStub) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*time.Time)) = r.value
	return nil
}

type digestRowsStub struct{ pgx.Rows }

func (digestRowsStub) Close()     {}
func (digestRowsStub) Err() error { return nil }
func (digestRowsStub) Next() bool { return false }

type digestSnapshotStub struct {
	cutoff time.Time
	busy   bool
	calls  []string
	args   [][]any
}

func (s *digestSnapshotStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.calls = append(s.calls, sql)
	s.args = append(s.args, args)
	if s.busy {
		return pgconn.CommandTag{}, context.DeadlineExceeded
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (s *digestSnapshotStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.calls = append(s.calls, sql)
	s.args = append(s.args, args)
	if strings.Contains(sql, "pg_try_advisory_xact_lock") {
		return lockRow{locked: !s.busy}
	}
	return digestRowStub{value: s.cutoff}
}

type digestQueryStub struct {
	query, rowQuery string
	queries         []string
	rowQueries      []string
	args, rowArgs   []any
	rowResults      []digestRowStub
	execQueries     []string
	execArgs        [][]any
}

type digestExecStub struct {
	query   string
	args    []any
	queries []string
	rows    []int64
}

func (s *digestExecStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.query, s.args = sql, args
	s.queries = append(s.queries, sql)
	rows := int64(1)
	if len(s.rows) > 0 {
		rows, s.rows = s.rows[0], s.rows[1:]
	}
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", rows)), nil
}

func (s *digestQueryStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.rowQuery, s.rowArgs = sql, args
	s.rowQueries = append(s.rowQueries, sql)
	if len(s.rowResults) == 0 {
		return digestRowStub{err: pgx.ErrNoRows}
	}
	result := s.rowResults[0]
	s.rowResults = s.rowResults[1:]
	return result
}

func (s *digestQueryStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execQueries = append(s.execQueries, sql)
	s.execArgs = append(s.execArgs, args)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (s *digestQueryStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.query, s.args = sql, args
	s.queries = append(s.queries, sql)
	return digestRowsStub{}, nil
}

func TestDigestWindowReusesPersistedSnapshotAndMatchQueryUsesThatExactBoundary(t *testing.T) {
	dueAt := time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	proposed := dueAt.Add(3 * time.Hour)
	fixed := dueAt.Add(2 * time.Hour)
	lastSuccess := dueAt.Add(-24 * time.Hour)
	stub := &digestQueryStub{rowResults: []digestRowStub{{value: fixed}}}

	got, err := getOrCreateDigestWindow(context.Background(), stub, "tenant-1", "schedule-1", dueAt, proposed, lastSuccess)
	if err != nil || !got.Equal(fixed) {
		t.Fatalf("window=%v err=%v", got, err)
	}
	if !strings.Contains(stub.rowQuery, "ON CONFLICT (tenant_id, schedule_id, due_at) DO NOTHING") || stub.rowArgs[3] != proposed || len(stub.execQueries) != 2 || !strings.Contains(stub.execQueries[0], "public.digest_window_items") || !strings.Contains(stub.execQueries[0], "public.matches") || !strings.Contains(stub.execQueries[0], "JOIN public.filters f") || !strings.Contains(stub.execQueries[0], "f.tenant_id = m.tenant_id") || !strings.Contains(stub.execQueries[0], "f.id = m.filter_id") || !strings.Contains(stub.execQueries[0], "f.enabled") || !strings.Contains(stub.execQueries[0], "m.created_at > COALESCE") || !strings.Contains(stub.execQueries[0], "payload->>'SourceURL'") || !strings.Contains(stub.execQueries[0], "n.deadline_at IS NULL OR n.deadline_at >= $4") || !strings.Contains(stub.execQueries[1], "public.digest_window_recipients") || !strings.Contains(stub.execQueries[1], "public.recipients") {
		t.Fatalf("window query=%q args=%#v", stub.rowQuery, stub.rowArgs)
	}

	if _, err := loadDigestRecipients(context.Background(), stub, "tenant-1", "schedule-1", dueAt, got); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDigestNoticeRows(context.Background(), stub, "tenant-1", "schedule-1", dueAt, got, proposed); err != nil {
		t.Fatal(err)
	}
	if len(stub.queries) != 2 || !strings.Contains(stub.queries[0], "public.digest_window_recipients") || !strings.Contains(stub.queries[1], "public.digest_window_items") || !strings.Contains(stub.queries[1], "JOIN public.notices") || !strings.Contains(stub.queries[1], "deadline_at IS NULL OR n.deadline_at >= $5") || strings.Contains(stub.queries[1], "public.matches") {
		t.Fatalf("snapshot read queries=%q", stub.queries)
	}
	if stub.args[2] != dueAt || stub.args[3] != fixed || stub.args[4] != proposed {
		t.Fatalf("match boundary args=%#v", stub.args)
	}

	retry := &digestQueryStub{rowResults: []digestRowStub{{err: pgx.ErrNoRows}, {value: fixed}}}
	if _, err := getOrCreateDigestWindow(context.Background(), retry, "tenant-1", "schedule-1", dueAt, proposed.Add(time.Hour), lastSuccess); err != nil {
		t.Fatal(err)
	}
	if len(retry.rowQueries) != 2 || len(retry.execQueries) != 0 {
		t.Fatalf("retry did not reuse fixed snapshot: rows=%q writes=%q", retry.rowQueries, retry.execQueries)
	}
}

func TestDigestSnapshotLocksBeforeReadingDatabaseCutoff(t *testing.T) {
	cutoff := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	stub := &digestSnapshotStub{cutoff: cutoff}

	got, err := lockDigestSnapshot(context.Background(), stub)
	if err != nil || !got.Equal(cutoff) {
		t.Fatalf("cutoff=%v err=%v", got, err)
	}
	if len(stub.calls) != 2 || !strings.Contains(stub.calls[0], "pg_try_advisory_xact_lock") || !strings.Contains(stub.calls[1], "clock_timestamp") {
		t.Fatalf("snapshot lock/cutoff order=%q", stub.calls)
	}
	if len(stub.args[0]) != 1 || stub.args[0][0] != collectionAdvisoryLock {
		t.Fatalf("snapshot lock key=%#v, collection key=%d", stub.args[0], collectionAdvisoryLock)
	}
}

func TestDigestSnapshotReturnsBusyWithoutWaitingOrReadingCutoff(t *testing.T) {
	stub := &digestSnapshotStub{busy: true}
	cutoff, err := lockDigestSnapshot(context.Background(), stub)
	if !errors.Is(err, ErrCollectionRunning) || !cutoff.IsZero() {
		t.Fatalf("cutoff=%v err=%v; want retryable busy result", cutoff, err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("busy snapshot read data before acquiring lock: %q", stub.calls)
	}
}

func TestCompleteNoopDigestWindowTerminalizesExpiredPartialDeliveryIdempotently(t *testing.T) {
	dueAt := time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	windowEnd := dueAt.Add(3 * time.Hour)
	stub := &digestExecStub{rows: []int64{1, 1}}
	if err := completeNoopDigestWindow(context.Background(), stub, "tenant-1", "schedule-1", dueAt, windowEnd); err != nil {
		t.Fatal(err)
	}
	if err := completeNoopDigestWindow(context.Background(), stub, "tenant-1", "schedule-1", dueAt, windowEnd); err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
	if len(stub.queries) != 2 {
		t.Fatalf("completion queries=%q", stub.queries)
	}
	for _, query := range stub.queries {
		for _, want := range []string{
			"FOR UPDATE", "active_lease", "d.status = 'sending'", "clock_timestamp() - interval '15 minutes'",
			"NOT EXISTS (SELECT 1 FROM active_lease)", "UPDATE public.deliveries", "d.status <> 'sent'", "last_error",
			"UPDATE public.digest_windows", "status = 'completed'", "UPDATE public.schedules",
			"target.status = 'completed'", "SELECT 1 FROM active_lease",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("terminal completion query missing %q: %s", want, query)
			}
		}
	}
	if len(stub.args) != 5 || stub.args[2] != dueAt || stub.args[3] != windowEnd || stub.args[4] != expiredDigestTerminalReason {
		t.Fatalf("completion args=%#v", stub.args)
	}
}

func TestCompleteNoopActiveLeaseDispositionIsANonErrorSkip(t *testing.T) {
	dueAt := time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	stub := &digestExecStub{rows: []int64{1}}
	if err := completeNoopDigestWindow(context.Background(), stub, "tenant-1", "schedule-1", dueAt, dueAt.Add(time.Hour)); err != nil {
		t.Fatalf("active lease deferral must be a non-error skip: %v", err)
	}
	if len(stub.queries) != 1 || !strings.Contains(stub.query, "SELECT 1 FROM active_lease") {
		t.Fatalf("active lease disposition is not represented in terminal query: %s", stub.query)
	}
}

func TestCompleteNoopRejectsMissingOrFailedWindow(t *testing.T) {
	dueAt := time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	stub := &digestExecStub{rows: []int64{0}}
	err := completeNoopDigestWindow(context.Background(), stub, "tenant-1", "schedule-1", dueAt, dueAt.Add(time.Hour))
	if err == nil || len(stub.queries) != 1 {
		t.Fatalf("err=%v queries=%q", err, stub.queries)
	}
}

func TestZeroRecipientDigestRunsOneAtomicNoop(t *testing.T) {
	dueAt := time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	windowEnd := dueAt.Add(3 * time.Hour)
	works, err := buildDigestWorks("tenant-1", digestScheduleRow{ID: "schedule-1"}, dueAt, windowEnd, nil, []digestNoticeRow{{
		ID: "notice-1", Title: "회계감사", ReasonsJSON: []byte(`{"reasons":["include_any"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	repository := &digestRepoStub{work: works}
	result, err := (DigestRunner{Repository: repository, Mailer: &mailerStub{}, From: "monitor@example.test"}).Run(context.Background())
	if err != nil || result.Skipped != 1 || repository.noopCount != 1 || repository.claimCount != 0 || !repository.noopWindowEnd.Equal(windowEnd) {
		t.Fatalf("result=%+v err=%v noop=%d claims=%d end=%v", result, err, repository.noopCount, repository.claimCount, repository.noopWindowEnd)
	}
}

func TestLatestDigestDueUsesSeoulWeekdaysAndReturnsOnlyNewestMissedWindow(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, seoul) // Wednesday.
	last := time.Date(2026, 8, 31, 7, 0, 0, 0, seoul)
	due, ok := latestDigestDue(now, last, 7, 0, []int16{1, 3})
	if !ok || !due.Equal(time.Date(2026, 9, 2, 7, 0, 0, 0, seoul)) {
		t.Fatalf("due=%v ok=%t", due, ok)
	}

	beforeWednesdayRun := time.Date(2026, 9, 2, 6, 59, 0, 0, seoul)
	due, ok = latestDigestDue(beforeWednesdayRun, time.Time{}, 7, 0, []int16{1, 3})
	if !ok || !due.Equal(time.Date(2026, 8, 31, 7, 0, 0, 0, seoul)) {
		t.Fatalf("pre-run due=%v ok=%t", due, ok)
	}

	if _, ok := latestDigestDue(now, due, 7, 0, []int16{1, 3}); !ok {
		t.Fatal("a newer Wednesday window should remain due after Monday succeeded")
	}
	if _, ok := latestDigestDue(now, time.Date(2026, 9, 2, 7, 0, 0, 0, seoul), 7, 0, []int16{1, 3}); ok {
		t.Fatal("the already completed newest window was returned again")
	}
}

func TestPendingDigestWindowTakesPriorityOverNewerSchedule(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatal(err)
	}
	pendingDue := time.Date(2026, 9, 1, 7, 0, 0, 0, seoul)
	pendingEnd := pendingDue.Add(time.Hour)
	schedule := digestScheduleRow{
		ID: "schedule-1", Hour: 7, Minute: 0, Weekdays: []int16{1, 2, 3, 4, 5},
		PendingDue: pendingDue, PendingWindowEnd: pendingEnd,
	}
	dueAt, windowEnd, existing, ok := selectDigestWindow(time.Date(2026, 9, 2, 10, 0, 0, 0, seoul), schedule)
	if !ok || !existing || !dueAt.Equal(pendingDue) || !windowEnd.Equal(pendingEnd) {
		t.Fatalf("due=%v end=%v existing=%t ok=%t", dueAt, windowEnd, existing, ok)
	}
}

func TestDigestNoticeRowsDeduplicateNoticeAndMergeReasons(t *testing.T) {
	rows := []digestNoticeRow{
		{ID: "notice-1", Title: "회계감사 용역", SourceURL: "https://example.test/notice/1", ReasonsJSON: []byte(`{"reasons":["include_any"],"details":[{"Code":"include_any","Field":"title","RuleValue":"회계감사","NoticeValue":"회계감사 용역"}]}`)},
		{ID: "notice-1", Title: "회계감사 용역", SourceURL: "https://example.test/notice/1", ReasonsJSON: []byte(`{"reasons":["region"],"details":[{"Code":"region","Field":"region","RuleValue":"서울","NoticeValue":"서울특별시"}]}`)},
		{ID: "notice-2", Title: "보안 장비", SourceURL: "https://example.test/notice/2", ReasonsJSON: []byte(`{"reasons":["category"],"details":[{"Code":"category","Field":"category","RuleValue":"goods","NoticeValue":"goods"}]}`)},
	}
	notices, err := mergeDigestNoticeRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 {
		t.Fatalf("notices=%+v", notices)
	}
	if notices[0].URL != "https://example.test/notice/1" || !strings.Contains(notices[0].Reason, "회계감사") || !strings.Contains(notices[0].Reason, "서울") {
		t.Fatalf("first notice lost its source URL or match reasons: %+v", notices[0])
	}
}

func TestBuildDigestWorksUsesOneNoopOrOneWorkPerRecipient(t *testing.T) {
	due := time.Date(2026, 9, 2, 7, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	windowEnd := due.Add(3 * time.Hour)
	schedule := digestScheduleRow{ID: "schedule-1"}
	recipients := []digestRecipientRow{{ID: "r1", Email: "a@example.test"}, {ID: "r2", Email: "b@example.test"}}

	empty, err := buildDigestWorks("tenant-1", schedule, due, windowEnd, recipients, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 1 || empty[0].RecipientID != "" || len(empty[0].Notices) != 0 || !empty[0].WindowEnd.Equal(windowEnd) {
		t.Fatalf("empty digest should be one no-op: %+v", empty)
	}

	withoutRecipients, err := buildDigestWorks("tenant-1", schedule, due, windowEnd, nil, []digestNoticeRow{{
		ID: "notice-1", Title: "공고", ReasonsJSON: []byte(`{"reasons":["include_any"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutRecipients) != 1 || withoutRecipients[0].RecipientID != "" {
		t.Fatalf("recipient-free digest should be one no-op: %+v", withoutRecipients)
	}

	works, err := buildDigestWorks("tenant-1", schedule, due, windowEnd, recipients, []digestNoticeRow{{
		ID: "notice-1", Title: "공고", SourceURL: "https://example.test/notice/1", ReasonsJSON: []byte(`{"reasons":["include_any"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 2 || works[0].RecipientID != "r1" || works[1].RecipientID != "r2" || len(works[0].Notices) != 1 || !works[0].WindowEnd.Equal(windowEnd) {
		t.Fatalf("recipient works=%+v", works)
	}
}

func TestMergeDigestNoticeRowsRejectsMalformedReasonData(t *testing.T) {
	_, err := mergeDigestNoticeRows([]digestNoticeRow{{ID: "notice-1", Title: "공고", ReasonsJSON: []byte(`{`)}})
	if err == nil {
		t.Fatal("malformed match reasons were silently emailed")
	}
}
