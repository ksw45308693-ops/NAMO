package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"namo/internal/matcher"
	"namo/internal/model"
	appweb "namo/internal/web"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const testWebReportID = "123e4567-e89b-12d3-a456-426614174000"

type noticeQueryTx struct {
	pgx.Tx
	rows  pgx.Rows
	query string
}

func (s *noticeQueryTx) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	s.query = query
	return s.rows, nil
}

func TestLoadNoticesSearchHistoryReadsCollectedAt(t *testing.T) {
	payload, err := json.Marshal(model.Notice{Title: "경관조명", PostedAt: time.Date(2026, 1, 2, 16, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	tx := &noticeQueryTx{rows: &reportRowsStub{rows: [][]any{{"notice-1", payload, time.Date(2026, 1, 7, 14, 54, 0, 0, time.UTC)}}}}
	data := appweb.AppData{}
	if err := loadTenantNotices(context.Background(), tx, &data, nil); err != nil {
		t.Fatal(err)
	}
	wantQuery := "SELECT id::text,payload,collected_at FROM public.notices WHERE deadline_at IS NULL OR deadline_at >= now() ORDER BY published_at DESC NULLS LAST,id"
	if got := strings.Join(strings.Fields(tx.query), " "); got != wantQuery {
		t.Fatalf("notice query=%s", got)
	}
	if len(data.Notices) != 1 || data.Notices[0].Title != "경관조명" {
		t.Fatalf("notices=%+v", data.Notices)
	}
	view := data.Notices[0]
	if view.CollectedDate != "20260107" || view.CollectedClock != "2354" || view.PostedAt != "2026-01-03" {
		t.Fatalf("load lost separate collection/posting times: %+v", view)
	}
}

func TestNoticeViewSearchHistoryMatchedKeywords(t *testing.T) {
	now := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	view := noticeViewFromModel(now, "notice-1", model.Notice{
		Title: "경관조명 스마트폴", Category: model.CategoryGoods, Agency: "만원복지재단", Region: "서울", Amount: 599995000,
	}, now, []activeWebFilter{
		{ID: "any", Rule: matcher.Rule{IncludeAny: []string{"경관조명", "없는단어"}}},
		{ID: "all", Rule: matcher.Rule{IncludeAll: []string{"경관조명", "스마트폴", "경관조명"}}},
		{ID: "agency-keyword", Rule: matcher.Rule{IncludeAny: []string{"복지"}}},
		{ID: "metadata", Rule: matcher.Rule{Agencies: []string{"만원"}, Regions: []string{"서울"}}},
		{ID: "miss", Rule: matcher.Rule{IncludeAny: []string{"스마트폴"}, Categories: []model.Category{model.CategoryService}}},
	})
	if view.Keyword != "경관조명, 스마트폴, 복지" {
		t.Fatalf("actual matched keyword union=%q", view.Keyword)
	}
	want := map[string]string{"any": "경관조명", "all": "경관조명, 스마트폴", "agency-keyword": "복지", "metadata": ""}
	if !reflect.DeepEqual(view.FilterKeywords, want) {
		t.Fatalf("per-filter keywords=%#v", view.FilterKeywords)
	}
	if len(view.FilterReasons) != 4 || len(view.FilterReasons["metadata"]) != 2 {
		t.Fatalf("reasons changed: %#v", view.FilterReasons)
	}
	if view.SourceKind != "입찰공고목록-입찰공고" || view.Trade != "내자" || view.Category != "물품" || view.Agency != "만원복지재단" || view.Amount != "599,995,000원" {
		t.Fatalf("history fields=%+v", view)
	}
}

func TestNoticeViewSearchHistoryDatesAndMissingValues(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		collected, posted    time.Time
		date, clock, posting string
	}{
		{"missing", time.Time{}, time.Time{}, "-", "-", "-"},
		{"kst rollover", time.Date(2026, 1, 7, 15, 4, 0, 0, time.UTC), time.Date(2026, 1, 2, 16, 0, 0, 0, time.UTC), "20260108", "0004", "2026-01-03"},
		{"non UTC input", time.Date(2026, 1, 7, 10, 4, 0, 0, time.FixedZone("west", -5*3600)), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "20260108", "0004", "2026-01-01"},
		{"fixed KST historical", time.Date(1955, 1, 1, 15, 0, 0, 0, time.UTC), time.Date(1955, 1, 1, 15, 0, 0, 0, time.UTC), "19550102", "0000", "1955-01-02"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			view := noticeViewFromModel(time.Now(), "n", model.Notice{PostedAt: tt.posted}, tt.collected, nil)
			if view.CollectedDate != tt.date || view.CollectedClock != tt.clock || view.PostedAt != tt.posting {
				t.Fatalf("times=%+v", view)
			}
			if view.Trade != "-" || view.Keyword != "" {
				t.Fatalf("missing values=%+v", view)
			}
		})
	}
	for category, want := range map[model.Category]string{model.CategoryGoods: "내자", model.CategoryService: "내자", model.CategoryConstruction: "내자", model.CategoryForeign: "외자", "unknown": "-"} {
		if view := noticeViewFromModel(time.Now(), "n", model.Notice{Category: category}, time.Time{}, nil); view.Trade != want {
			t.Errorf("%s trade=%q want %q", category, view.Trade, want)
		}
	}
}

type toggleExecCall struct {
	query string
	args  []any
}

type toggleExecStub struct {
	calls []toggleExecCall
	rows  []int64
}

type saveFilterRow struct {
	id                    string
	rulesChanged, enabled bool
	err                   error
}

func (r saveFilterRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.id
	if len(dest) > 1 {
		*(dest[1].(*bool)) = r.rulesChanged
		*(dest[2].(*bool)) = r.enabled
	}
	return nil
}

type saveFilterStub struct {
	toggleExecStub
	rowCalls []toggleExecCall
	rows     []saveFilterRow
}

func (s *saveFilterStub) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	s.rowCalls = append(s.rowCalls, toggleExecCall{query: query, args: args})
	if len(s.rows) == 0 {
		return saveFilterRow{err: pgx.ErrNoRows}
	}
	row := s.rows[0]
	s.rows = s.rows[1:]
	return row
}

func TestSaveFilterRuleEditDeletesOnlyItsTenantMatches(t *testing.T) {
	stub := &saveFilterStub{rows: []saveFilterRow{{err: pgx.ErrNoRows}, {id: "filter-1", rulesChanged: true, enabled: true}}}
	err := saveFilter(context.Background(), stub, "tenant-a", "감사", []byte(`{"IncludeAny":["신규"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.rowCalls) != 2 || !strings.Contains(stub.rowCalls[0].query, "ON CONFLICT (tenant_id, name) DO NOTHING") || !strings.Contains(stub.rowCalls[1].query, "rules IS DISTINCT FROM $3::jsonb") || !strings.Contains(stub.rowCalls[1].query, "FOR UPDATE") {
		t.Fatalf("insert/lock queries=%#v", stub.rowCalls)
	}
	for _, call := range stub.rowCalls {
		if !strings.Contains(call.query, "tenant_id") || call.args[0] != "tenant-a" {
			t.Fatalf("cross-tenant unsafe insert/lock=%#v", call)
		}
	}
	if len(stub.calls) != 2 || !strings.Contains(stub.calls[0].query, "UPDATE public.filters") || !strings.Contains(stub.calls[1].query, "DELETE FROM public.matches") {
		t.Fatalf("rule edit writes=%#v", stub.calls)
	}
	for _, call := range stub.calls {
		if !strings.Contains(call.query, "tenant_id=$1::uuid") || call.args[0] != "tenant-a" || call.args[1] != "filter-1" {
			t.Fatalf("cross-tenant unsafe write=%#v", call)
		}
	}
}

func TestSaveFilterNewAndIdenticalRulesPreserveMatches(t *testing.T) {
	tests := []struct {
		name      string
		rows      []saveFilterRow
		wantExecs int
	}{
		{name: "new", rows: []saveFilterRow{{id: "new-filter"}}, wantExecs: 0},
		{name: "identical", rows: []saveFilterRow{{err: pgx.ErrNoRows}, {id: "filter-1", enabled: true}}, wantExecs: 0},
		{name: "reenable", rows: []saveFilterRow{{err: pgx.ErrNoRows}, {id: "filter-1", enabled: false}}, wantExecs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &saveFilterStub{rows: tt.rows}
			if err := saveFilter(context.Background(), stub, "tenant-a", "감사", []byte(`{"IncludeAny":["감사"]}`)); err != nil {
				t.Fatal(err)
			}
			if len(stub.calls) != tt.wantExecs {
				t.Fatalf("writes=%#v, want %d", stub.calls, tt.wantExecs)
			}
			for _, call := range stub.calls {
				if strings.Contains(call.query, "DELETE FROM public.matches") {
					t.Fatalf("%s save deleted matches: %s", tt.name, call.query)
				}
			}
		})
	}
}

func (s *toggleExecStub) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	s.calls = append(s.calls, toggleExecCall{query: query, args: args})
	rows := int64(1)
	if len(s.rows) > 0 {
		rows, s.rows = s.rows[0], s.rows[1:]
	}
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", rows)), nil
}

func TestToggleFilterDisableDeletesOnlySameTenantMatches(t *testing.T) {
	stub := &toggleExecStub{rows: []int64{1, 3}}
	err := toggleFilter(context.Background(), stub, "tenant-a", appweb.ToggleFilterCommand{FilterID: "filter-1", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 2 {
		t.Fatalf("calls=%#v", stub.calls)
	}
	if !strings.Contains(stub.calls[0].query, "UPDATE public.filters") || !strings.Contains(stub.calls[0].query, "tenant_id=$1::uuid AND id=$2::uuid") {
		t.Fatalf("tenant-scoped toggle query=%s", stub.calls[0].query)
	}
	if !strings.Contains(stub.calls[1].query, "DELETE FROM public.matches") || !strings.Contains(stub.calls[1].query, "tenant_id=$1::uuid AND filter_id=$2::uuid") {
		t.Fatalf("tenant-scoped match deletion query=%s", stub.calls[1].query)
	}
	for _, call := range stub.calls {
		if call.args[0] != "tenant-a" || call.args[1] != "filter-1" {
			t.Fatalf("cross-tenant unsafe args=%#v", call.args)
		}
	}
}

func TestToggleFilterEnableLeavesMatchRefreshToCaller(t *testing.T) {
	stub := &toggleExecStub{rows: []int64{1}}
	if err := toggleFilter(context.Background(), stub, "tenant-a", appweb.ToggleFilterCommand{FilterID: "filter-1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("enable unexpectedly deleted matches: %#v", stub.calls)
	}
}

func TestDeleteFilterIsTenantScoped(t *testing.T) {
	stub := &toggleExecStub{rows: []int64{1}}
	if err := deleteFilter(context.Background(), stub, "tenant-a", appweb.DeleteFilterCommand{FilterID: "filter-1"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("delete calls=%#v", stub.calls)
	}
	call := stub.calls[0]
	if !strings.Contains(call.query, "DELETE FROM public.filters") || !strings.Contains(call.query, "tenant_id=$1::uuid AND id=$2::uuid") {
		t.Fatalf("tenant-scoped delete query=%s", call.query)
	}
	if call.args[0] != "tenant-a" || call.args[1] != "filter-1" {
		t.Fatalf("delete args=%#v", call.args)
	}
}

func TestDeleteFilterRequiresTenantAdministratorRole(t *testing.T) {
	service := &WebService{}
	err := service.DeleteFilter(context.Background(), appweb.RequestContext{TenantID: "tenant-a", Role: "platform_admin"}, appweb.DeleteFilterCommand{FilterID: "filter-1"})
	if err == nil {
		t.Fatal("platform administrator with tenant context deleted a filter")
	}
}

func TestWebNoticeMatchingAppliesCurrentFilterRulesImmediately(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	view := noticeViewFromModel(now, "notice-1", model.Notice{Title: "회계감사 용역"}, time.Time{}, []activeWebFilter{
		{ID: "filter-new", Rule: matcher.Rule{IncludeAny: []string{"회계감사"}}},
		{ID: "filter-other", Rule: matcher.Rule{IncludeAny: []string{"건설"}}},
	})
	if _, ok := view.FilterReasons["filter-new"]; !ok {
		t.Fatalf("new filter did not match immediately: %+v", view.FilterReasons)
	}
	if _, ok := view.FilterReasons["filter-other"]; ok {
		t.Fatalf("unmatched filter was attached: %+v", view.FilterReasons)
	}
}

type refreshMatchStoreStub struct {
	batch *pgx.Batch
}

type refreshBatchResultsStub struct{ pgx.BatchResults }

func (refreshBatchResultsStub) Close() error { return nil }

func (s *refreshMatchStoreStub) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	s.batch = batch
	return refreshBatchResultsStub{}
}

func TestRefreshFilterMatchesPersistsCurrentRuleResults(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	revision := now.Add(-time.Minute)
	filter := StoredFilter{ID: "filter-1", TenantID: "tenant-a", Revision: revision, Rule: matcher.Rule{IncludeAny: []string{"데이터"}}}
	notices := []ActiveNotice{
		{ID: "notice-match", Notice: model.Notice{Title: "데이터 분석 용역"}},
		{ID: "notice-miss", Notice: model.Notice{Title: "청소 용역"}},
	}
	stub := &refreshMatchStoreStub{}

	if err := refreshFilterMatches(context.Background(), stub, now, filter, notices); err != nil {
		t.Fatal(err)
	}
	if stub.batch == nil || stub.batch.Len() != 2 {
		t.Fatalf("match refresh batch=%#v", stub.batch)
	}
	upsert, deleteCall := stub.batch.QueuedQueries[0], stub.batch.QueuedQueries[1]
	if !strings.Contains(upsert.SQL, "ON CONFLICT") || upsert.Arguments[0] != "tenant-a" || upsert.Arguments[1] != "filter-1" || upsert.Arguments[2] != "notice-match" || upsert.Arguments[4] != revision {
		t.Fatalf("matched notice upsert=%#v", upsert)
	}
	if !strings.Contains(deleteCall.SQL, "DELETE FROM public.matches") || deleteCall.Arguments[2] != "notice-miss" {
		t.Fatalf("unmatched notice delete=%#v", deleteCall)
	}
	if payload, ok := upsert.Arguments[3].([]byte); !ok || !strings.Contains(string(payload), `"include_any"`) {
		t.Fatalf("match reasons payload=%q", payload)
	}
}

func TestFilterManagementCountsSameActiveMatchesAsNoticeList(t *testing.T) {
	data := appweb.AppData{Filters: []appweb.FilterView{{ID: "filter-1"}, {ID: "filter-2"}}}
	applyNoticeFilterCounts(&data, appweb.NoticeView{FilterReasons: map[string][]string{"filter-1": {"키워드 일치"}}})
	applyNoticeFilterCounts(&data, appweb.NoticeView{FilterReasons: map[string][]string{"filter-1": {"키워드 일치"}, "filter-2": {"지역 일치"}}})
	if data.Filters[0].Matches != 2 || data.Filters[1].Matches != 1 {
		t.Fatalf("filter counts=%+v", data.Filters)
	}
}

func TestFilterSummaryShowsHiddenNarrowingRules(t *testing.T) {
	days := 3
	summary := filterSummary(matcher.Rule{
		IncludeAny:         []string{"회계"},
		Categories:         []model.Category{model.CategoryConstruction},
		Agencies:           []string{"한국철도공사"},
		DeadlineWithinDays: &days,
	})
	for _, want := range []string{"ANY: 회계", "업종: 공사", "기관: 한국철도공사", "마감 3일 이내"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("filter summary %q missing %q", summary, want)
		}
	}
}

func TestFilterSummaryShowsUnlimitedDeadline(t *testing.T) {
	if summary := filterSummary(matcher.Rule{IncludeAny: []string{"데이터"}}); strings.Contains(summary, "마감") {
		t.Fatalf("unlimited deadline should not print a window: %q", summary)
	}
}

func TestTenantNoticeQueryLoadsAllActiveNoticesWithoutStoredMatches(t *testing.T) {
	for _, want := range []string{"FROM public.notices", "deadline_at IS NULL OR deadline_at >= now()"} {
		if !strings.Contains(tenantNoticesSQL, want) {
			t.Fatalf("tenant notice query missing %q: %s", want, tenantNoticesSQL)
		}
	}
	if strings.Contains(tenantNoticesSQL, "public.matches") {
		t.Fatalf("web notice query still depends on delayed stored matches: %s", tenantNoticesSQL)
	}
}

func TestTenantNoticeQueryHasNoRowCap(t *testing.T) {
	if strings.Contains(tenantNoticesSQL, "LIMIT") {
		t.Fatalf("web notice query still caps rows and undercounts filters: %s", tenantNoticesSQL)
	}
	for _, want := range []string{"FROM public.notices", "deadline_at IS NULL OR deadline_at >= now()"} {
		if !strings.Contains(tenantNoticesSQL, want) {
			t.Fatalf("tenant notice query missing %q: %s", want, tenantNoticesSQL)
		}
	}
}

func TestNoticesLoadOnlyForPagesThatShowThem(t *testing.T) {
	for _, path := range []string{"/notices", "/notices/abc", "/filters"} {
		if !noticesNeeded(path) {
			t.Fatalf("%s needs notices but load was skipped", path)
		}
	}
	for _, path := range []string{"/dashboard", "/settings", "/reports", "/admin"} {
		if noticesNeeded(path) {
			t.Fatalf("%s does not render notices but still loads them", path)
		}
	}
}

type failureCountQueryStub struct {
	query string
	args  []any
}

func (s *failureCountQueryStub) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	s.query = query
	s.args = args
	return failureCountRow(4)
}

type failureCountRow int

func (r failureCountRow) Scan(dest ...any) error {
	*(dest[0].(*int)) = int(r)
	return nil
}

func TestLoadTenantFailureCountIncludesJobsAndDeliveries(t *testing.T) {
	stub := &failureCountQueryStub{}
	count, err := loadTenantFailureCount(context.Background(), stub, "tenant-1")
	if err != nil || count != 4 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	for _, want := range []string{"public.job_runs", "public.deliveries", "status='failed'", "interval '24 hours'"} {
		if !strings.Contains(stub.query, want) {
			t.Fatalf("failure query missing %q: %s", want, stub.query)
		}
	}
	if !strings.Contains(stub.query, "claimed_at >= now()-interval '24 hours'") || strings.Contains(stub.query, "due_at >= now()-interval '24 hours'") {
		t.Fatalf("delivery failures must use observed claim time, not old due time: %s", stub.query)
	}
	if !strings.Contains(stub.query, "position($2 in COALESCE(last_error,'')) = 0") || len(stub.args) != 2 || stub.args[1] != expiredDigestTerminalReason {
		t.Fatalf("expected expired-delivery cancellation exclusion: query=%s args=%#v", stub.query, stub.args)
	}
}

func TestApplyTenantFailuresMarksPlatformHealthForDigestJobErrors(t *testing.T) {
	data := appweb.AppData{Admin: appweb.AdminView{Healthy: true}}
	view := appweb.TenantView{State: "정상"}
	applyTenantFailures(&data, &view, 2)
	if data.Admin.Healthy || data.Admin.FailedJobs != 2 || view.State != "점검" {
		t.Fatalf("admin=%+v tenant=%+v", data.Admin, view)
	}
}

func TestWebServiceAllowsOnlyKnownAnonymousPagesWithoutPrincipal(t *testing.T) {
	service := &WebService{}
	for _, path := range []string{"/login", "/accept-invite"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if _, err := service.MapRequest(request); err != nil {
			t.Fatalf("%s anonymous mapping error = %v", path, err)
		}
	}
	if _, err := service.MapRequest(httptest.NewRequest(http.MethodGet, "/dashboard", nil)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("protected anonymous mapping error = %v", err)
	}
}

func TestFilterRuleFromWebCommandSupportsAnyAndAll(t *testing.T) {
	amount := int64(50_000_000)
	days := 7
	command := appweb.FilterCommand{
		IncludeKeywords: "회계, 감사", IncludeMode: "all", ExcludeKeywords: "상주, 파견",
		Category: "용역", Region: "서울", MinimumAmount: &amount, DeadlineDays: &days, Agency: "공공기관",
	}
	rule := filterRuleFromWebCommand(command)
	if !reflect.DeepEqual(rule.IncludeAll, []string{"회계", "감사"}) || len(rule.IncludeAny) != 0 {
		t.Fatalf("include rule = %+v", rule)
	}
	if !reflect.DeepEqual(rule.Exclude, []string{"상주", "파견"}) ||
		!reflect.DeepEqual(rule.Categories, []model.Category{model.CategoryService}) ||
		!reflect.DeepEqual(rule.Regions, []string{"서울"}) ||
		!reflect.DeepEqual(rule.Agencies, []string{"공공기관"}) ||
		rule.MinAmount == nil || *rule.MinAmount != amount || rule.DeadlineWithinDays == nil || *rule.DeadlineWithinDays != 7 {
		t.Fatalf("rule = %+v", rule)
	}

	command.IncludeMode = "any"
	rule = filterRuleFromWebCommand(command)
	if !reflect.DeepEqual(rule.IncludeAny, []string{"회계", "감사"}) || len(rule.IncludeAll) != 0 {
		t.Fatalf("ANY rule = %+v", rule)
	}
}

func TestFilterRuleOmitsDeadlineWhenUnlimited(t *testing.T) {
	rule := filterRuleFromWebCommand(appweb.FilterCommand{Name: "데이터", IncludeKeywords: "데이터"})
	if rule.DeadlineWithinDays != nil {
		t.Fatalf("unlimited filter still carries a deadline window: %d", *rule.DeadlineWithinDays)
	}
}

func TestFilterRuleKeepsRequestedDeadlineWindow(t *testing.T) {
	days := 3
	rule := filterRuleFromWebCommand(appweb.FilterCommand{Name: "03", IncludeKeywords: "회계", DeadlineDays: &days})
	if rule.DeadlineWithinDays == nil || *rule.DeadlineWithinDays != 3 {
		t.Fatalf("deadline window lost: %+v", rule.DeadlineWithinDays)
	}
}

func TestWebLabelsExposeMatchReasonAndKoreanCategory(t *testing.T) {
	detail := matcher.Detail{Code: matcher.ReasonRegion, RuleValue: "서울", NoticeValue: "서울특별시"}
	if got := reasonText(detail); got != "지역 ‘서울’ 일치 (공고: 서울특별시)" {
		t.Fatalf("reason = %q", got)
	}
	if got := categoryLabel(model.CategoryConstruction); got != "공사" {
		t.Fatalf("category = %q", got)
	}
	when := time.Date(2026, 9, 1, 7, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	if got := formatKoreanTime(when); got != "2026.09.01 07:00" {
		t.Fatalf("time = %q", got)
	}
}

func TestNextDeliveryUsesSelectedSeoulWeekday(t *testing.T) {
	seoul := time.FixedZone("KST", 9*60*60)
	fridayAfterRun := time.Date(2026, 9, 4, 8, 0, 0, 0, seoul)
	next, ok := nextDeliveryAt(fridayAfterRun, 7, 0, []int{1, 3, 5})
	if !ok || next.Weekday() != time.Monday || next.Hour() != 7 || next.Minute() != 0 {
		t.Fatalf("next delivery = %s ok=%t", next, ok)
	}
}

func TestReportViewUsesStoredMetadataAndOnlyExhaustedFailuresAreRetryable(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 9, 2, 7, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	generated := due.Add(time.Minute)
	tests := []struct {
		name, relativePath, trigger, status, wantStatus string
		attempts                                        int
		wantDownload                                    bool
	}{
		{"generated", "tenant/2026/09/namo-20260902-070000.html", "scheduled", "generated", "생성 완료", 1, true},
		{"retry pending", "", "scheduled", "failed", "재시도 대기", 2, false},
		{"retry exhausted", "", "manual", "failed", "생성 실패", 3, false},
		{"generating", "", "manual", "generating", "생성 중", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := reportViewFromRow(testWebReportID, tt.relativePath, tt.trigger, tt.status, due, &generated, 7, tt.attempts)
			if view.Status != tt.wantStatus || view.Downloadable != tt.wantDownload || view.NoticeCount != 7 {
				t.Fatalf("view=%+v", view)
			}
			if tt.relativePath != "" && view.FileName != "namo-20260902-070000.html" {
				t.Errorf("filename=%q", view.FileName)
			}
		})
	}
	for _, want := range []string{"FROM public.reports", "tenant_id=$1::uuid", "ORDER BY due_at DESC", "LIMIT 50"} {
		if !strings.Contains(tenantReportsSQL, want) {
			t.Errorf("tenant report query missing %q", want)
		}
	}
}

type reportDownloadQueryStub struct {
	query, tenantID, reportID, relativePath string
	err                                     error
	queried                                 bool
}

func (s *reportDownloadQueryStub) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	s.queried = true
	s.query = query
	s.tenantID, _ = args[0].(string)
	s.reportID, _ = args[1].(string)
	return reportDownloadRow{s: s}
}

type reportDownloadRow struct{ s *reportDownloadQueryStub }

func (r reportDownloadRow) Scan(dest ...any) error {
	if r.s.err != nil {
		return r.s.err
	}
	*(dest[0].(*string)) = r.s.relativePath
	return nil
}

func TestOpenReportDownloadQueriesTenantMetadataBeforeStoredRelativePath(t *testing.T) {
	t.Parallel()

	temp := t.TempDir()
	filePath := temp + string(os.PathSeparator) + "stored.html"
	if err := os.WriteFile(filePath, []byte("<html>tenant report</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	query := &reportDownloadQueryStub{relativePath: "tenant/2026/09/namo-20260902-070000.html"}
	opened := ""
	download, err := openReportAfterMetadata(func() (string, error) {
		return reportDownloadPath(context.Background(), query, "tenant-a", testWebReportID)
	}, func(relativePath string) (*os.File, os.FileInfo, error) {
		if !query.queried {
			t.Fatal("file opened before tenant metadata query")
		}
		opened = relativePath
		file, err := os.Open(filePath)
		if err != nil {
			return nil, nil, err
		}
		info, err := file.Stat()
		return file, info, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	if query.tenantID != "tenant-a" || query.reportID != testWebReportID || opened != query.relativePath {
		t.Fatalf("query tenant=%q report=%q opened=%q", query.tenantID, query.reportID, opened)
	}
	for _, want := range []string{"FROM public.reports", "tenant_id=$1::uuid", "id=$2::uuid", "status='generated'", "relative_path<>''"} {
		if !strings.Contains(query.query, want) {
			t.Errorf("download metadata query missing %q: %s", want, query.query)
		}
	}
	if download.Name != "namo-20260902-070000.html" {
		t.Errorf("download name=%q", download.Name)
	}
}

func TestOpenReportDownloadWaitsForTenantTransactionCommit(t *testing.T) {
	t.Parallel()

	query := &reportDownloadQueryStub{relativePath: "tenant/report.html"}
	commitErr := errors.New("commit failed")
	openCalls := 0
	_, err := openReportAfterMetadata(func() (string, error) {
		relativePath, err := reportDownloadPath(context.Background(), query, "tenant-a", testWebReportID)
		if err != nil {
			return "", err
		}
		return relativePath, commitErr
	}, func(string) (*os.File, os.FileInfo, error) {
		openCalls++
		return nil, nil, nil
	})
	if !errors.Is(err, commitErr) {
		t.Fatalf("error=%v, want commit failure", err)
	}
	if !query.queried {
		t.Fatal("tenant metadata was not queried")
	}
	if openCalls != 0 {
		t.Errorf("commit failure opened file %d times", openCalls)
	}
}

func TestOpenReportDownloadMapsRLSNoRowAndMissingFileToNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   *reportDownloadQueryStub
		openErr error
	}{
		{"RLS or missing row", &reportDownloadQueryStub{err: pgx.ErrNoRows}, nil},
		{"missing stored file", &reportDownloadQueryStub{relativePath: "tenant/report.html"}, fs.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			openCalls := 0
			_, err := openReportAfterMetadata(func() (string, error) {
				return reportDownloadPath(context.Background(), tt.query, "tenant-a", testWebReportID)
			}, func(string) (*os.File, os.FileInfo, error) {
				openCalls++
				return nil, nil, tt.openErr
			})
			if !errors.Is(err, appweb.ErrReportNotFound) {
				t.Fatalf("error=%v, want report not found", err)
			}
			if tt.query.err != nil && openCalls != 0 {
				t.Errorf("metadata miss opened file %d times", openCalls)
			}
		})
	}
}

func TestReportMutationsRequireExactTenantAdminRoleBeforeDependencies(t *testing.T) {
	t.Parallel()

	service := &WebService{}
	for _, requestContext := range []appweb.RequestContext{
		{Role: "member", TenantID: "tenant-a"},
		{Role: "platform_admin", TenantID: "tenant-a"},
		{Role: "tenant_admin"},
	} {
		if err := service.GenerateReport(context.Background(), requestContext); err == nil || !strings.Contains(err.Error(), "tenant administrator") {
			t.Errorf("GenerateReport(%+v) error=%v", requestContext, err)
		}
		if err := service.RetryReport(context.Background(), requestContext, testWebReportID); err == nil || !strings.Contains(err.Error(), "tenant administrator") {
			t.Errorf("RetryReport(%+v) error=%v", requestContext, err)
		}
		if err := service.SaveReportSchedule(context.Background(), requestContext, appweb.NotificationCommand{}); err == nil || !strings.Contains(err.Error(), "tenant administrator") {
			t.Errorf("SaveReportSchedule(%+v) error=%v", requestContext, err)
		}
	}
}

func TestGenerateReportDistinguishesEmptyOutcomeFromCreatedAndFailed(t *testing.T) {
	t.Parallel()

	runErr := errors.New("manual report failed")
	tests := []struct {
		name    string
		outcome ReportOutcome
		runErr  error
		wantErr error
	}{
		{"created", ReportOutcome{Created: true}, nil, nil},
		{"no eligible matches", ReportOutcome{}, nil, appweb.ErrNoReportMatches},
		{"runner failure", ReportOutcome{}, runErr, runErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calledTenant := ""
			service := &WebService{RunManualReport: func(_ context.Context, tenantID string) (ReportOutcome, error) {
				calledTenant = tenantID
				return tt.outcome, tt.runErr
			}}
			err := service.GenerateReport(context.Background(), appweb.RequestContext{TenantID: "tenant-a", Role: "tenant_admin"})
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("GenerateReport error=%v, want %v", err, tt.wantErr)
			}
			if calledTenant != "tenant-a" {
				t.Errorf("manual report tenant=%q", calledTenant)
			}
		})
	}
}
