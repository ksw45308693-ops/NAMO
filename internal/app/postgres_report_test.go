package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"namo/internal/model"
)

type reportRowFunc func(...any) error

func (f reportRowFunc) Scan(dest ...any) error { return f(dest...) }

type reportRowsStub struct {
	pgx.Rows
	rows  [][]any
	index int
}

func (r *reportRowsStub) Close()     {}
func (r *reportRowsStub) Err() error { return nil }
func (r *reportRowsStub) Next() bool { return r.index < len(r.rows) }
func (r *reportRowsStub) Scan(dest ...any) error {
	row := r.rows[r.index]
	r.index++
	if len(dest) != len(row) {
		return fmt.Errorf("report row columns: got %d destinations, want %d", len(dest), len(row))
	}
	for index := range dest {
		switch target := dest[index].(type) {
		case *string:
			*target = row[index].(string)
		case **string:
			if row[index] == nil {
				*target = nil
			} else {
				value := row[index].(string)
				*target = &value
			}
		case *int:
			*target = row[index].(int)
		case *bool:
			*target = row[index].(bool)
		case *int64:
			*target = row[index].(int64)
		case *time.Time:
			*target = row[index].(time.Time)
		case **time.Time:
			if row[index] == nil {
				*target = nil
			} else {
				value := row[index].(time.Time)
				*target = &value
			}
		case *[]int16:
			*target = row[index].([]int16)
		case *[]byte:
			*target = row[index].([]byte)
		default:
			return errors.New("unsupported report row destination")
		}
	}
	return nil
}

type reportStoreStub struct {
	batches    []*pgx.Batch
	rowQueries []string
	rowArgs    [][]any
	rowResults []reportRowFunc
	queries    []string
	queryArgs  [][]any
	queryRows  []*reportRowsStub
	execs      []string
	execArgs   [][]any
	execRows   []int64
}

func (s *reportStoreStub) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	s.batches = append(s.batches, batch)
	return refreshBatchResultsStub{}
}

func TestManualReportReevaluatesTimeDependentFilterBeforeSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	deadline := now.Add(72*time.Hour - time.Minute)
	payload, err := json.Marshal(model.Notice{Category: model.CategoryGoods, BidNumber: "newly-eligible", BidSequence: "00", Title: "데이터 구매", Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	stub := &reportStoreStub{queryRows: []*reportRowsStub{
		{rows: [][]any{{"notice-new", payload, false}}},
		{rows: [][]any{{"filter-1", []byte(`{"DeadlineWithinDays":3}`), now.Add(-time.Hour)}}},
		{rows: [][]any{{"notice-new", "데이터 구매", "goods", "기관", "", int64(0), deadline, "", "3일 이내", []byte(`{}`), nil, nil, nil, nil}}},
	}}
	stub.rowResults = []reportRowFunc{
		func(dest ...any) error { *(dest[0].(*string)) = "테넌트"; return nil },
		func(dest ...any) error {
			// Stored matches start empty and become visible only after refresh.
			*(dest[0].(*bool)) = len(stub.batches) == 1 && len(stub.batches[0].QueuedQueries) == 1 && strings.Contains(stub.batches[0].QueuedQueries[0].SQL, "INSERT INTO public.matches")
			return nil
		},
		workRow("report-1", "tenant-1", "테넌트", "", "수동", "manual", "reports/tenant-1/report-1.html", "token-1", now, nil, now, 1),
	}
	work, created, err := claimManualReport(context.Background(), stub, "tenant-1", now)
	if err != nil || !created || len(work.Notices) != 1 || work.Notices[0].Title != "데이터 구매" {
		t.Fatalf("newly eligible notice missing: work=%+v created=%t err=%v", work, created, err)
	}
}

func (s *reportStoreStub) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	s.rowQueries = append(s.rowQueries, query)
	s.rowArgs = append(s.rowArgs, args)
	if len(s.rowResults) == 0 {
		return reportRowFunc(func(...any) error { return pgx.ErrNoRows })
	}
	result := s.rowResults[0]
	s.rowResults = s.rowResults[1:]
	return result
}

func (s *reportStoreStub) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	s.queries = append(s.queries, query)
	s.queryArgs = append(s.queryArgs, args)
	if len(s.queryRows) == 0 {
		return &reportRowsStub{}, nil
	}
	rows := s.queryRows[0]
	s.queryRows = s.queryRows[1:]
	return rows, nil
}

func (s *reportStoreStub) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	s.execs = append(s.execs, query)
	s.execArgs = append(s.execArgs, args)
	rows := int64(1)
	if len(s.execRows) > 0 {
		rows, s.execRows = s.execRows[0], s.execRows[1:]
	}
	return pgconn.NewCommandTag("UPDATE " + string(rune('0'+rows))), nil
}

func TestPostgresReportScheduledClaimUsesSnapshotLeaseAndFencing(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	dueAt := now.Add(-3 * time.Hour)
	stub := &reportStoreStub{
		rowResults: []reportRowFunc{
			func(dest ...any) error { *(dest[0].(*bool)) = true; return nil },
			workRow("report-1", "tenant-1", "테넌트", "schedule-1", "매일", "scheduled", "reports/tenant-1/report-1.html", "token-1", dueAt, nil, now, 1),
		},
		queryRows: []*reportRowsStub{{rows: [][]any{{"notice-1", "공고", "goods", "기관", "서울", int64(100), now.Add(time.Hour), "https://example.test/1", "필터", []byte(`{"reasons":["region"]}`), nil, nil, nil, nil}}}},
	}
	work, created, err := claimScheduledReport(context.Background(), stub, "tenant-1", "테넌트", reportSchedule{ID: "schedule-1", Name: "매일"}, dueAt, now, now)
	if err != nil || !created || work.ReportID != "report-1" || len(work.Notices) != 1 {
		t.Fatalf("work=%+v created=%t err=%v", work, created, err)
	}
	claimSQL := stub.rowQueries[1]
	for _, want := range []string{"public.reports", "tenant_name", "schedule_name", "trigger", "'scheduled'", "status = 'generating'", "attempts < 3", "interval '15 minutes'", "claim_token", "gen_random_uuid()", "reports/", "ON CONFLICT"} {
		if !strings.Contains(claimSQL, want) {
			t.Fatalf("scheduled claim SQL missing %q: %s", want, claimSQL)
		}
	}
	if len(stub.execs) != 1 || !strings.Contains(stub.execs[0], "public.report_items") || !strings.Contains(stub.execs[0], "public.digest_window_items") || !strings.Contains(stub.execs[0], "ON CONFLICT") {
		t.Fatalf("snapshot SQL=%q", stub.execs)
	}
}

func TestPostgresReportSchedulesRecoverCompletedDigestWindowWithoutReport(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	dueAt := now.Add(-3 * time.Hour)
	stub := &reportStoreStub{
		queryRows: []*reportRowsStub{{rows: [][]any{{"schedule-1", "매일", 7, 0, "Asia/Seoul", []int16{1, 2, 3}, nil}}}},
		rowResults: []reportRowFunc{func(dest ...any) error {
			*(dest[0].(*time.Time)) = dueAt
			*(dest[1].(*time.Time)) = now
			return nil
		}},
	}
	schedules, err := loadReportSchedules(context.Background(), stub, "tenant-1")
	if err != nil || len(schedules) != 1 || !schedules[0].PendingDue.Equal(dueAt) || !schedules[0].PendingWindowEnd.Equal(now) {
		t.Fatalf("schedules=%+v err=%v", schedules, err)
	}
	if len(stub.rowQueries) != 1 {
		t.Fatalf("window queries=%q", stub.rowQueries)
	}
	query := stub.rowQueries[0]
	for _, want := range []string{"w.status = 'completed'", "public.digest_window_items", "public.reports", "r.id IS NULL", "r.attempts < 3", "interval '15 minutes'"} {
		if !strings.Contains(query, want) {
			t.Fatalf("completed report recovery query missing %q: %s", want, query)
		}
	}
}

func TestPostgresReportEmptyScheduledWindowCompletesWithoutReport(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	stub := &reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error { *(dest[0].(*bool)) = false; return nil }}}
	work, created, err := claimScheduledReport(context.Background(), stub, "tenant-1", "테넌트", reportSchedule{ID: "schedule-1", Name: "매일"}, now.Add(-time.Hour), now, now)
	if err != nil || created || work.ReportID != "" {
		t.Fatalf("work=%+v created=%t err=%v", work, created, err)
	}
	if len(stub.rowQueries) != 1 || len(stub.execs) != 1 || strings.Contains(stub.execs[0], "INSERT INTO public.reports") {
		t.Fatalf("row queries=%q execs=%q", stub.rowQueries, stub.execs)
	}
	for _, want := range []string{"UPDATE public.digest_windows", "status = 'completed'", "UPDATE public.schedules", "last_success_at"} {
		if !strings.Contains(stub.execs[0], want) {
			t.Fatalf("empty completion SQL missing %q: %s", want, stub.execs[0])
		}
	}
}

func TestPostgresReportManualClaimSnapshotsOpenMatchesWithoutScheduleAdvance(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	stub := &reportStoreStub{
		rowResults: []reportRowFunc{
			func(dest ...any) error { *(dest[0].(*string)) = "테넌트"; return nil },
			func(dest ...any) error { *(dest[0].(*bool)) = true; return nil },
			workRow("report-2", "tenant-1", "테넌트", "", "수동", "manual", "reports/tenant-1/report-2.html", "token-2", now, nil, now, 1),
		},
		queryRows: []*reportRowsStub{{}},
	}
	work, created, err := claimManualReport(context.Background(), stub, "tenant-1", now)
	if err != nil || !created || work.Trigger != "manual" {
		t.Fatalf("work=%+v created=%t err=%v", work, created, err)
	}
	joined := strings.Join(append(append([]string{}, stub.rowQueries...), stub.execs...), "\n")
	for _, want := range []string{"tenant_name", "schedule_name", "public.matches", "JOIN public.filters", "f.enabled", "public.report_items", "deadline_at"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("manual snapshot SQL missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "UPDATE public.schedules") {
		t.Fatalf("manual claim advanced a schedule: %s", joined)
	}
}

func TestPostgresReportReclaimAndRetryKeepSnapshotWithFreshFence(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		retry bool
		want  []string
	}{
		{"automatic reclaim", false, []string{"interval '15 minutes'", "attempts < 3", "attempts = attempts + 1", "claim_token = pg_catalog.gen_random_uuid()"}},
		{"operator retry", true, []string{"status = 'failed'", "attempts = 1", "claim_token = pg_catalog.gen_random_uuid()"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &reportStoreStub{rowResults: []reportRowFunc{workRow("report-1", "tenant-1", "테넌트", "", "수동", "manual", "reports/tenant-1/report-1.html", "new-token", now, nil, now, 1)}, queryRows: []*reportRowsStub{{}}}
			var ok bool
			var err error
			if test.retry {
				_, ok, err = retryReport(context.Background(), stub, "tenant-1", "report-1", now)
			} else {
				_, ok, err = reclaimReport(context.Background(), stub, "tenant-1", "report-1")
			}
			if err != nil || !ok {
				t.Fatalf("ok=%t err=%v", ok, err)
			}
			for _, want := range test.want {
				if !strings.Contains(stub.rowQueries[0], want) {
					t.Fatalf("claim SQL missing %q: %s", want, stub.rowQueries[0])
				}
			}
			if !strings.Contains(stub.rowQueries[0], "c.tenant_name") || !strings.Contains(stub.rowQueries[0], "c.schedule_name") || strings.Contains(stub.rowQueries[0], "JOIN public.tenants") || strings.Contains(stub.rowQueries[0], "JOIN public.schedules") {
				t.Fatalf("claim reloaded mutable display names: %s", stub.rowQueries[0])
			}
			if len(stub.execs) != 0 {
				t.Fatalf("existing snapshot was rewritten: %q", stub.execs)
			}
		})
	}
}

func TestPostgresReportFinalizeFencesSuccessAndFailure(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	work := ReportWork{ReportID: "report-1", TenantID: "tenant-1", ScheduleID: "schedule-1", Trigger: "scheduled", RelativePath: "reports/tenant-1/report-1.html", ClaimToken: "token-1", DueAt: now.Add(-time.Hour), WindowEnd: now, Attempts: 2}
	artifact := ReportArtifact{RelativePath: work.RelativePath, SHA256: strings.Repeat("a", 64), NoticeCount: 1}

	success := &reportStoreStub{}
	if err := finalizeReport(context.Background(), success, work, artifact, now); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(success.execs, "\n")
	// Every bound argument must occur in the SQL. An unused parameter between
	// $4 and $6 leaves PostgreSQL unable to infer its type in prepared queries.
	for i, query := range success.execs {
		for j := range success.execArgs[i] {
			parameter := regexp.MustCompile(fmt.Sprintf(`\$%d\b`, j+1))
			if !parameter.MatchString(query) {
				t.Fatalf("finalize query %d has unused parameter $%d", i+1, j+1)
			}
		}
	}
	for _, want := range []string{"claim_token = $", "attempts = $", "status = 'generating'", "status = 'generated'", "UPDATE public.digest_windows", "UPDATE public.schedules", "last_success_at"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("success finalize missing %q: %s", want, joined)
		}
	}

	failure := &reportStoreStub{}
	if err := finalizeReportFailure(context.Background(), failure, work, errors.New("render failed")); err != nil {
		t.Fatal(err)
	}
	if len(failure.execs) != 1 || !strings.Contains(failure.execs[0], "claim_token = $") || !strings.Contains(failure.execs[0], "attempts = $") || !strings.Contains(failure.execs[0], "status = 'failed'") {
		t.Fatalf("failure finalize SQL=%q", failure.execs)
	}
}

func TestPostgresReportFinalizeAcceptsTheSameWindowAlreadyCompletedByDigest(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	work := ReportWork{ReportID: "report-1", TenantID: "tenant-1", ScheduleID: "schedule-1", Trigger: "scheduled", RelativePath: "reports/tenant-1/report-1.html", ClaimToken: "token-1", DueAt: now.Add(-time.Hour), WindowEnd: now, Attempts: 1}
	artifact := ReportArtifact{RelativePath: work.RelativePath, SHA256: strings.Repeat("a", 64), NoticeCount: 1}
	stub := &reportStoreStub{execRows: []int64{1, 1}}
	if err := finalizeReport(context.Background(), stub, work, artifact, now); err != nil {
		t.Fatal(err)
	}
	if len(stub.execs) != 2 {
		t.Fatalf("finalize queries=%q", stub.execs)
	}
	completion := stub.execs[1]
	for _, want := range []string{"target_window", "FOR UPDATE", "target.status = 'completed'", "eligible_window"} {
		if !strings.Contains(completion, want) {
			t.Fatalf("idempotent completion query missing %q: %s", want, completion)
		}
	}
}

func TestPostgresReportSnapshotCombinesRulesPerNotice(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	stub := &reportStoreStub{queryRows: []*reportRowsStub{{rows: [][]any{
		{"notice-1", "공고", "service", "기관", "서울", int64(100), now, "https://example.test/1", "지역 필터", []byte(`{"details":[{"Code":"region","RuleValue":"서울"}]}`), nil, nil, nil, nil},
		{"notice-1", "공고", "service", "기관", "서울", int64(100), now, "https://example.test/1", "금액 필터", []byte(`{"reasons":["min_amount"]}`), nil, nil, nil, nil},
	}}}}
	work := ReportWork{ReportID: "report-1", TenantID: "tenant-1"}
	if err := loadReportNotices(context.Background(), stub, &work); err != nil {
		t.Fatal(err)
	}
	if len(work.Notices) != 1 || len(work.Notices[0].Matches) != 2 || work.Notices[0].Matches[0].RuleName != "지역 필터" || len(work.Notices[0].Matches[0].Reasons) != 1 {
		t.Fatalf("notices=%+v", work.Notices)
	}
}

func TestSnapshotRecordsSearchColumns(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	for _, trigger := range []string{"scheduled", "manual"} {
		t.Run(trigger, func(t *testing.T) {
			stub := &reportStoreStub{rowResults: []reportRowFunc{
				func(dest ...any) error { *(dest[0].(*bool)) = true; return nil },
				workRow("report-1", "tenant-1", "테넌트", "schedule-1", "매일", trigger, "reports/tenant-1/report-1.html", "token-1", now, nil, now, 1),
			}}
			order := "n.published_at DESC NULLS LAST,i.title,i.notice_id,i.matched_at,i.match_id"
			recorded := "i.matched_at"
			where := "WHERE i.tenant_id=$1::uuid AND i.schedule_id=$3::uuid AND i.due_at=$4 AND i.window_end_at=$5"
			wantArgs := []any{"tenant-1", "report-1", "schedule-1", now, now}
			var err error
			if trigger == "scheduled" {
				_, _, err = claimScheduledReport(context.Background(), stub, "tenant-1", "테넌트", reportSchedule{ID: "schedule-1"}, now, now, now)
			} else {
				stub.rowResults = append([]reportRowFunc{func(dest ...any) error { *(dest[0].(*string)) = "테넌트"; return nil }}, stub.rowResults...)
				_, _, err = claimManualReport(context.Background(), stub, "tenant-1", now)
				order = "n.published_at DESC NULLS LAST,n.title,n.id,m.created_at,m.id"
				recorded = "m.created_at"
				where = "WHERE m.tenant_id=$1::uuid AND (n.deadline_at IS NULL OR n.deadline_at >= $3)"
				wantArgs = []any{"tenant-1", "report-1", now}
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(stub.execs) != 1 {
				t.Fatalf("snapshot count=%d", len(stub.execs))
			}
			query := strings.Join(strings.Fields(stub.execs[0]), " ")
			for _, want := range []string{
				"reasons, source_kind,posted_at,collected_at,recorded_at)",
				"'입찰공고목록-입찰공고',n.published_at,n.collected_at," + recorded,
				"row_number() OVER (ORDER BY " + order + ")",
				"ORDER BY " + order, where,
				"COALESCE(n.payload->>'Agency','')", "COALESCE(NULLIF(n.payload->>'Amount',''),'0')::bigint",
			} {
				if !strings.Contains(query, want) {
					t.Errorf("snapshot missing %q: %s", want, query)
				}
			}
			if strings.Count(query, "ORDER BY "+order) != 2 {
				t.Errorf("ordinal and output order differ: %s", query)
			}
			if strings.Contains(query, "LIMIT") {
				t.Errorf("snapshot must not be limited: %s", query)
			}
			if !reflect.DeepEqual(stub.execArgs[0], wantArgs) {
				t.Errorf("snapshot args=%v want=%v", stub.execArgs[0], wantArgs)
			}
		})
	}
}

func TestMatchedKeywordsKeepsOnlyIncludeTerms(t *testing.T) {
	var payload storedMatchReasons
	if err := json.Unmarshal([]byte(`{"reasons":["include_any"],"details":[{"Code":"include_any","RuleValue":"경관조명"},{"Code":"include_all","RuleValue":"스마트폴"},{"Code":"include_any","RuleValue":"경관조명"},{"Code":"category","RuleValue":"goods"},{"Code":"deadline_within_days","RuleValue":"3"},{"Code":"exclude_any","RuleValue":"제외"},{"Code":"include_any","RuleValue":""}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	if got := matchedKeywords(payload); !reflect.DeepEqual(got, []string{"경관조명", "스마트폴"}) {
		t.Fatalf("keywords=%v", got)
	}
	if got := matchedKeywords(storedMatchReasons{}); len(got) != 0 {
		t.Fatalf("empty reasons produced keywords=%v", got)
	}
}

func TestLoadReportNoticesMergesKeywordsPerNotice(t *testing.T) {
	posted := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	collected, recorded := posted.Add(time.Hour), posted.Add(2*time.Hour)
	later := posted.Add(24 * time.Hour)
	stub := &reportStoreStub{queryRows: []*reportRowsStub{{rows: [][]any{
		{"notice-1", "공고", "goods", "기관", "서울", int64(100), later, "https://example.test/1", "첫 필터", []byte(`{"details":[{"Code":"include_any","RuleValue":"경관조명"},{"Code":"include_all","RuleValue":"경관조명"},{"Code":"region","RuleValue":"서울"}]}`), "입찰공고목록-입찰공고", posted, collected, recorded},
		{"notice-2", "둘째", "service", "기관2", "부산", int64(200), later, "https://example.test/2", "둘째 필터", []byte(`{"details":[{"Code":"include_any","RuleValue":"별도"}]}`), nil, nil, nil, nil},
		{"notice-1", "변경 공고", "service", "변경 기관", "부산", int64(999), later, "https://example.test/changed", "다음 필터", []byte(`{"details":[{"Code":"include_all","RuleValue":"스마트폴"},{"Code":"include_any","RuleValue":"경관조명"},{"Code":"category","RuleValue":"goods"},{"Code":"deadline_within_days","RuleValue":"3"}]}`), "변경 구분", later, later, later},
	}}}}
	work := ReportWork{TenantID: "tenant-1", ReportID: "report-1"}
	if err := loadReportNotices(context.Background(), stub, &work); err != nil {
		t.Fatal(err)
	}
	if len(work.Notices) != 2 {
		t.Fatalf("notices=%+v", work.Notices)
	}
	first := work.Notices[0]
	if first.ID != "notice-1" || work.Notices[1].ID != "notice-2" || first.Title != "공고" || first.Agency != "기관" || first.Amount != 100 {
		t.Errorf("first-seen notice data/order lost: %+v", work.Notices)
	}
	if !reflect.DeepEqual(first.Keywords, []string{"경관조명", "스마트폴"}) || !reflect.DeepEqual(work.Notices[1].Keywords, []string{"별도"}) {
		t.Errorf("keywords=%+v", work.Notices)
	}
	if first.SourceKind != "입찰공고목록-입찰공고" || !first.PostedAt.Equal(posted) || !first.CollectedAt.Equal(collected) || !first.RecordedAt.Equal(recorded) {
		t.Errorf("first-row snapshot fields=%+v", first)
	}
	if len(first.Matches) != 2 || first.Matches[0].RuleName != "첫 필터" || first.Matches[1].RuleName != "다음 필터" || !reflect.DeepEqual(first.Matches[0].Reasons, []string{"포함 키워드: 경관조명", "필수 키워드: 경관조명", "지역: 서울"}) {
		t.Errorf("matches=%+v", first.Matches)
	}
	for _, want := range []string{"source_kind,posted_at,collected_at,recorded_at", "WHERE tenant_id=$1::uuid AND report_id=$2::uuid", "ORDER BY ordinal"} {
		if !strings.Contains(stub.queries[0], want) {
			t.Errorf("loader missing %q: %s", want, stub.queries[0])
		}
	}
	if !reflect.DeepEqual(stub.queryArgs[0], []any{"tenant-1", "report-1"}) {
		t.Errorf("loader args=%v", stub.queryArgs[0])
	}
}

func TestLoadReportNoticesOldNullSearchColumns(t *testing.T) {
	stub := &reportStoreStub{queryRows: []*reportRowsStub{{rows: [][]any{
		{"old", "과거 공고", "goods", "기관", "서울", int64(100), time.Time{}, "https://example.test/old", "과거 필터", []byte(`{"reasons":["include_any"]}`), nil, nil, nil, nil},
	}}}}
	work := ReportWork{TenantID: "tenant-1", ReportID: "old-report"}
	if err := loadReportNotices(context.Background(), stub, &work); err != nil {
		t.Fatal(err)
	}
	if len(work.Notices) != 1 {
		t.Fatalf("notices=%+v", work.Notices)
	}
	notice := work.Notices[0]
	if notice.SourceKind != "" || !notice.PostedAt.IsZero() || !notice.CollectedAt.IsZero() || !notice.RecordedAt.IsZero() || len(notice.Keywords) != 0 {
		t.Errorf("old snapshot invented data: %+v", notice)
	}
	if len(notice.Matches) != 1 || !reflect.DeepEqual(notice.Matches[0].Reasons, []string{"포함 키워드"}) {
		t.Errorf("old reasons lost: %+v", notice.Matches)
	}
}

func workRow(reportID, tenantID, tenantName, scheduleID, scheduleName, trigger, path, token string, dueAt time.Time, windowStart *time.Time, windowEnd time.Time, attempts int) reportRowFunc {
	return func(dest ...any) error {
		*(dest[0].(*string)) = reportID
		*(dest[1].(*string)) = tenantID
		*(dest[2].(*string)) = tenantName
		*(dest[3].(*string)) = scheduleID
		*(dest[4].(*string)) = scheduleName
		*(dest[5].(*string)) = trigger
		*(dest[6].(*string)) = path
		*(dest[7].(*string)) = token
		*(dest[8].(*time.Time)) = dueAt
		*(dest[9].(**time.Time)) = windowStart
		*(dest[10].(*time.Time)) = windowEnd
		*(dest[11].(*int)) = attempts
		return nil
	}
}
