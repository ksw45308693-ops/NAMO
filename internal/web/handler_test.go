package web

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAgencyLabelsDescribeAnnouncementAgency(t *testing.T) {
	for _, path := range []string{"/filters", "/notices/2026-sample-001"} {
		body := serve(t, http.MethodGet, path).Body.String()
		if !strings.Contains(body, "공고기관") || strings.Contains(body, "수요기관") {
			t.Errorf("%s must label the stored announcement agency as 공고기관", path)
		}
	}
}

func TestPagesRenderExpectedLandmarks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path       string
		wantTitle  string
		wantMarker string
	}{
		{"/login", "로그인", `<main class="login-main"`},
		{"/dashboard", "입찰공고 모니터링", `aria-label="모니터링 자동화 흐름"`},
		{"/notices", "공고 목록", `aria-current="page"`},
		{"/notices/2026-sample-001", "공고 상세", `aria-label="선정 사유"`},
		{"/filters", "필터 관리", `name="include_keywords"`},
		{"/reports", "리포트", `name="delivery_time"`},
		{"/settings", "환경 설정", `name="tenant_name"`},
		{"/admin", "플랫폼 관리", `aria-label="수집 상태"`},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			response := serve(t, http.MethodGet, tt.path)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			body := response.Body.String()
			for _, want := range []string{"<!doctype html>", `lang="ko"`, "<title>" + tt.wantTitle, tt.wantMarker} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
		})
	}
}

func TestDashboardRendersApprovedProcessFlow(t *testing.T) {
	t.Parallel()

	response := serve(t, http.MethodGet, "/dashboard")
	body := response.Body.String()
	for _, want := range []string{
		"나라장터 공고", "키워드 필터", "신규 공고 요약", "HTML 리포트 저장",
		"매일 07:00 자동 실행", "준비 중", "공고 → 양식 자동 추천", "관련 업무이력 자동 리스트업",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if !strings.Contains(body, `<ol class="process-flow" aria-label="모니터링 자동화 흐름">`) {
		t.Error("process flow is not exposed as an ordered list")
	}
	last := -1
	for _, stage := range []string{"나라장터 공고", "키워드 필터", "신규 공고 요약", "HTML 리포트 저장"} {
		index := strings.Index(body, stage)
		if index <= last {
			t.Fatalf("process stage %q is out of order", stage)
		}
		last = index
	}
}

func TestMobileDrawerControlComesBeforeNavigation(t *testing.T) {
	t.Parallel()

	body := serve(t, http.MethodGet, "/dashboard").Body.String()
	button := strings.Index(body, `data-nav-toggle`)
	nav := strings.Index(body, `id="primary-nav"`)
	if button < 0 || nav < 0 || button >= nav {
		t.Fatalf("mobile menu button index = %d, navigation index = %d; button must come first", button, nav)
	}
	for _, want := range []string{`aria-expanded="false"`, `aria-controls="primary-nav"`, `data-drawer`} {
		if !strings.Contains(body, want) {
			t.Errorf("drawer markup missing %q", want)
		}
	}
}

func TestMobileDrawerKeepsKeyboardFocusInsideAtPhoneWidth(t *testing.T) {
	t.Parallel()

	markup := serve(t, http.MethodGet, "/dashboard").Body.String()
	if !strings.Contains(markup, `data-drawer-background`) {
		t.Error("drawer background is not marked for inert state")
	}
	javascript := serve(t, http.MethodGet, "/assets/app.js").Body.String()
	for _, want := range []string{
		`window.matchMedia('(max-width: 820px)')`,
		`background.inert = expanded`,
		`event.key !== 'Tab'`,
		`document.activeElement === first`,
		`document.activeElement === last`,
		`event.preventDefault()`,
		`button.focus()`,
	} {
		if !strings.Contains(javascript, want) {
			t.Errorf("390px drawer keyboard contract missing %q", want)
		}
	}
}

func TestSkipLinkTargetKeepsVisibleFocusIndicator(t *testing.T) {
	t.Parallel()

	stylesheet := serve(t, http.MethodGet, "/assets/app.css").Body.String()
	if strings.Contains(stylesheet, `.main-content:focus { outline: none; }`) {
		t.Error("skip-link target removes its focus indicator")
	}
	if !strings.Contains(stylesheet, `.main-content:focus { outline: 3px solid var(--focus);`) {
		t.Error("skip-link target has no visible focus indicator")
	}
}

func TestNoticeStatesExplainRecovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want string
	}{
		{"/notices?state=empty", "필터를 조정하거나 수집 상태를 확인해 주세요."},
		{"/notices?state=error", "공고를 불러오지 못했습니다."},
		{"/notices?state=loading", `aria-busy="true"`},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if body := serve(t, http.MethodGet, tt.path).Body.String(); !strings.Contains(body, tt.want) {
				t.Errorf("body missing %q", tt.want)
			}
		})
	}
}

func TestEmbeddedAssetsAreServed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path        string
		contentType string
		marker      string
	}{
		{"/assets/app.css", "text/css", "--navy"},
		{"/assets/app.js", "text/javascript", "data-nav-toggle"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			response := serve(t, http.MethodGet, tt.path)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Content-Type"); !strings.Contains(got, tt.contentType) {
				t.Errorf("Content-Type = %q, want it to contain %q", got, tt.contentType)
			}
			if body := response.Body.String(); !strings.Contains(body, tt.marker) {
				t.Errorf("asset missing %q", tt.marker)
			}
		})
	}
}

func TestUnknownRouteAndUnsupportedMethod(t *testing.T) {
	t.Parallel()

	if got := serve(t, http.MethodGet, "/missing").Code; got != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want %d", got, http.StatusNotFound)
	}
	if got := serve(t, http.MethodDelete, "/dashboard").Code; got != http.StatusMethodNotAllowed {
		t.Errorf("DELETE status = %d, want %d", got, http.StatusMethodNotAllowed)
	}
}

func TestMethodNotAllowedReportsRouteMethods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodPost, "/dashboard", "GET, HEAD"},
		{http.MethodDelete, "/login", "GET, HEAD"},
		{http.MethodGet, "/filters/toggle", "POST"},
		{http.MethodPatch, "/assets/app.css", "GET, HEAD"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			response := serve(t, tt.method, tt.path)
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
			}
			if got := response.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
}

func TestNoticesFilterSampleRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		query   string
		present string
		absent  string
	}{
		{"q=" + url.QueryEscape("정보시스템"), "2026-sample-002", "2026-sample-001"},
		{"category=" + url.QueryEscape("물품"), "2026-sample-003", "2026-sample-002"},
		{"region=" + url.QueryEscape("서울"), "2026-sample-002", "2026-sample-003"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			t.Parallel()
			body := serve(t, http.MethodGet, "/notices?"+tt.query).Body.String()
			if !strings.Contains(body, tt.present) {
				t.Errorf("filtered notices missing %q", tt.present)
			}
			if strings.Contains(body, tt.absent) {
				t.Errorf("filtered notices unexpectedly contain %q", tt.absent)
			}
		})
	}
}

func TestNoticesDefaultToAllRowsAndApplySavedFilterWhenSelected(t *testing.T) {
	t.Parallel()

	all := serve(t, http.MethodGet, "/notices").Body.String()
	for _, id := range []string{"2026-sample-001", "2026-sample-002", "2026-sample-003"} {
		if !strings.Contains(all, id) {
			t.Errorf("unfiltered notices missing %q", id)
		}
	}
	if !strings.Contains(all, ">전체 공고 <strong>3건</strong>") {
		t.Error("unfiltered list is not labelled as all notices")
	}

	filtered := serve(t, http.MethodGet, "/notices?filter=1").Body.String()
	if !strings.Contains(filtered, "2026-sample-001") || strings.Contains(filtered, "2026-sample-002") || strings.Contains(filtered, "2026-sample-003") {
		t.Errorf("saved filter did not isolate its matched notice")
	}
	if !strings.Contains(filtered, `name="filter"`) || !strings.Contains(filtered, `value="1" selected`) {
		t.Error("saved filter selection is not preserved")
	}
}

func TestNoticeSearchAndSavedFilterCombine(t *testing.T) {
	t.Parallel()

	body := serve(t, http.MethodGet, "/notices?filter=2&q="+url.QueryEscape("샘플")).Body.String()
	if !strings.Contains(body, "2026-sample-002") || strings.Contains(body, "2026-sample-001") {
		t.Error("search did not narrow the selected saved filter")
	}
}

func TestNoticePaginationSupportsTenTwentyThirtyAndPreservesFilters(t *testing.T) {
	t.Parallel()

	notices := make([]noticeView, 45)
	for i := range notices {
		notices[i].ID = strconv.Itoa(i + 1)
	}
	query := url.Values{
		"q":        {"감사"},
		"filter":   {"filter-1"},
		"category": {"용역"},
		"region":   {"서울"},
		"per_page": {"20"},
		"page":     {"2"},
	}
	page, pagination := paginateNotices(notices, query)
	if len(page) != 20 || page[0].ID != "21" || page[19].ID != "40" {
		t.Fatalf("page rows=%+v", page)
	}
	if pagination.Page != 2 || pagination.Pages != 3 || pagination.PageSize != 20 || pagination.Total != 45 {
		t.Fatalf("pagination=%+v", pagination)
	}
	for _, raw := range []string{pagination.PreviousURL, pagination.NextURL} {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"q": "감사", "filter": "filter-1", "category": "용역", "region": "서울", "per_page": "20"} {
			if got := parsed.Query().Get(key); got != want {
				t.Fatalf("%s=%q in %q, want %q", key, got, raw, want)
			}
		}
	}

	_, fallback := paginateNotices(notices, url.Values{"per_page": {"99"}, "page": {"99"}})
	if fallback.PageSize != 10 || fallback.Page != 5 || fallback.Pages != 5 {
		t.Fatalf("fallback pagination=%+v", fallback)
	}
	_, empty := paginateNotices(nil, url.Values{"page": {"99"}})
	if empty.Page != 1 || empty.Pages != 0 || empty.PreviousURL != "" || empty.NextURL != "" {
		t.Fatalf("empty pagination=%+v", empty)
	}
}

func TestNoticePageRendersPageSizeChoices(t *testing.T) {
	t.Parallel()

	body := serve(t, http.MethodGet, "/notices?per_page=20").Body.String()
	for _, want := range []string{`name="per_page"`, `value="10"`, `value="20" selected`, `value="30"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page size control missing %q", want)
		}
	}
}

func TestNoticeTableShowsSearchHistoryColumns(t *testing.T) {
	body := serve(t, http.MethodGet, "/notices").Body.String()
	start := strings.Index(body, "<thead>")
	end := strings.Index(body, "</thead>")
	if start < 0 || end < start {
		t.Fatal("notice table missing")
	}
	header := body[start:end]
	labels := []string{"구분", "일자", "시간", "키워드", "업무구분", "업무여부", "공고명", "공고기관", "진행·게시일자", "추정가격", "마감"}
	last := -1
	for _, label := range labels {
		index := strings.Index(header, `<th scope="col">`+label+`</th>`)
		if index <= last {
			t.Fatalf("notice table missing or misordered %q: %s", label, header)
		}
		last = index
		if !strings.Contains(body, `data-label="`+label+`"`) {
			t.Fatalf("missing mobile label %q", label)
		}
	}
	if strings.Count(header, `<th scope="col">`) != 11 || strings.Contains(body, "레코드생성일시") {
		t.Fatal("unexpected screen columns")
	}
}

func TestNoticeSearchHistoryTitleKeepsMobileBlockAndTouchTarget(t *testing.T) {
	body := serve(t, http.MethodGet, "/notices").Body.String()
	if !strings.Contains(body, `<td data-label="공고명" class="notice-title-cell">`) {
		t.Error("notice title is missing its semantic mobile layout class")
	}
	stylesheet := serve(t, http.MethodGet, "/assets/app.css").Body.String()
	_, mobile, ok := strings.Cut(stylesheet, "@media (max-width: 560px)")
	if !ok {
		t.Fatal("phone styles missing")
	}
	for _, want := range []string{
		`td.notice-title-cell { display: block;`,
		`td.notice-title-cell::before { display: block;`,
		`td.notice-title-cell > a { display: inline-flex; align-items: center; min-width: 44px; min-height: 44px; }`,
	} {
		if !strings.Contains(mobile, want) {
			t.Errorf("notice title mobile contract missing %q", want)
		}
	}
}

func TestSearchHistorySelectionKeepsOnlySelectedKeywordsWithoutMutation(t *testing.T) {
	notices := []NoticeView{{ID: "n1", Title: "스마트폴", Keyword: "경관조명, 스마트폴", Reasons: []string{"A 사유", "B 사유"},
		FilterKeywords: map[string]string{"a": "경관조명", "b": "스마트폴", "metadata": ""},
		FilterReasons:  map[string][]string{"a": {"A 사유"}, "b": {"B 사유"}, "metadata": {"지역 사유"}},
	}, {ID: "unmatched"}}
	for _, tt := range []struct{ id, keyword, reason string }{{"a", "경관조명", "A 사유"}, {"b", "스마트폴", "B 사유"}, {"metadata", "", "지역 사유"}} {
		got := filterNotices(notices, "", tt.id, "", "")
		if len(got) != 1 || got[0].Keyword != tt.keyword || !reflect.DeepEqual(got[0].Reasons, []string{tt.reason}) {
			t.Fatalf("%s selection=%+v", tt.id, got)
		}
	}
	all := filterNotices(notices, "", "", "", "")
	if len(all) != 2 || all[0].Keyword != "경관조명, 스마트폴" || !reflect.DeepEqual(all[0].Reasons, []string{"A 사유", "B 사유"}) {
		t.Fatalf("selection mutated original data: %+v", notices)
	}
	if notices[0].FilterKeywords["a"] != "경관조명" || notices[0].FilterKeywords["b"] != "스마트폴" {
		t.Fatal("selection mutated per-filter keywords")
	}
}

func TestNoticeTableSearchHistoryValuesAndMissingCells(t *testing.T) {
	for _, tt := range []struct {
		name   string
		notice NoticeView
		cells  map[string]string
	}{
		{"values", NoticeView{ID: "n1", SourceKind: "입찰공고목록-입찰공고", CollectedDate: "20260107", CollectedClock: "2354", Keyword: "경관조명", Category: "물품", Trade: "내자", Title: "스마트폴 제작", Agency: "만원복지재단", PostedAt: "2026-01-02", Amount: "599,995,000원", Deadline: "2026.01.09 12:00"}, map[string]string{"구분": "입찰공고목록-입찰공고", "일자": "20260107", "시간": "2354", "키워드": "경관조명", "업무구분": "물품", "업무여부": "내자", "공고명": "스마트폴 제작", "공고기관": "만원복지재단", "진행·게시일자": "2026-01-02", "추정가격": "599,995,000원", "마감": "2026.01.09 12:00"}},
		{"missing", NoticeView{ID: "n1"}, map[string]string{"구분": "-", "일자": "-", "시간": "-", "키워드": "-", "업무구분": "-", "업무여부": "-", "공고명": "-", "공고기관": "-", "진행·게시일자": "-", "추정가격": "-", "마감": "-"}},
		{"zero amount", NoticeView{ID: "n1", Amount: "0원"}, map[string]string{"추정가격": "-"}},
		{"undetermined amount", NoticeView{ID: "n1", Amount: "미정"}, map[string]string{"추정가격": "-"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := NewHandlerWithOptions(Options{Actions: &recordingActions{}, Backend: &staticBackend{data: AppData{Notices: []NoticeView{tt.notice}}}, MapContext: func(*http.Request) (RequestContext, error) {
				return RequestContext{TenantID: "tenant-1", Role: "tenant_admin"}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			body := serveHandler(t, handler, http.MethodGet, "/notices", "").Body.String()
			for label, want := range tt.cells {
				_, rest, found := strings.Cut(body, `data-label="`+label+`"`)
				cell, _, closed := strings.Cut(rest, "</td>")
				if !found || !closed || !strings.Contains(cell+"</td>", ">"+want+"<") {
					t.Errorf("%s cell=%q, want %q", label, cell, want)
				}
			}
		})
	}
}

func TestSelectedFilterShowsOnlyItsMatchReasons(t *testing.T) {
	t.Parallel()

	notices := []noticeView{{
		ID:      "notice-1",
		Reasons: []string{"필터 A 사유", "필터 B 사유"},
		FilterReasons: map[string][]string{
			"filter-a": {"필터 A 사유"},
			"filter-b": {"필터 B 사유"},
		},
	}}
	got := filterNotices(notices, "", "filter-b", "", "")
	if len(got) != 1 || !reflect.DeepEqual(got[0].Reasons, []string{"필터 B 사유"}) {
		t.Fatalf("selected filter reasons=%+v", got)
	}
}

func TestNoticeDetailKeepsSelectedFilterAndItsReasons(t *testing.T) {
	t.Parallel()

	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{data: AppData{Notices: []NoticeView{{
			ID: "notice-1", Title: "공고", Reasons: []string{"필터 A 사유", "필터 B 사유"},
			FilterReasons: map[string][]string{"filter-a": {"필터 A 사유"}, "filter-b": {"필터 B 사유"}},
		}}}},
		Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{TenantID: "tenant-1", Role: "tenant_admin"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	list := serveHandler(t, handler, http.MethodGet, "/notices?filter=filter-b", "").Body.String()
	if !strings.Contains(list, `href="/notices/notice-1?filter=filter-b"`) {
		t.Error("filtered list does not preserve the selected filter in its detail link")
	}
	detail := serveHandler(t, handler, http.MethodGet, "/notices/notice-1?filter=filter-b", "").Body.String()
	if !strings.Contains(detail, "필터 B 사유") || strings.Contains(detail, "필터 A 사유") {
		t.Error("detail does not isolate the selected filter reason")
	}
}

func TestNoticeSearchValueIsEscaped(t *testing.T) {
	t.Parallel()

	body := serve(t, http.MethodGet, "/notices?q=%3Cscript%3Ealert(1)%3C%2Fscript%3E").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("search query rendered as executable markup")
	}
	if !strings.Contains(body, `value="&lt;script&gt;alert(1)&lt;/script&gt;"`) {
		t.Error("escaped search value is not preserved in the control")
	}
}

func TestInvalidNoticeIDsReturnNotFound(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/notices/unknown", "/notices/2026-sample-001/extra", "/notices//"} {
		if got := serve(t, http.MethodGet, path).Code; got != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, got, http.StatusNotFound)
		}
	}
	body := serve(t, http.MethodGet, "/notices/2026-sample-002").Body.String()
	if !strings.Contains(body, "샘플: 정보시스템 운영 지원") {
		t.Error("valid notice ID did not render the matching notice")
	}
}

func TestInjectedFilterToggleCallsAction(t *testing.T) {
	t.Parallel()

	actions := &recordingActions{}
	handler := productionHandler(t, actions)
	request := httptest.NewRequest(http.MethodPost, "/filters/toggle", strings.NewReader("_csrf=token-123&filter=3&enabled=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if got := response.Header().Get("Location"); got != "/filters?saved=1" {
		t.Fatalf("Location = %q", got)
	}
	if actions.toggleCalls != 1 || actions.lastToggle.FilterID != "3" || !actions.lastToggle.Enabled {
		t.Errorf("toggle action = %#v, calls = %d", actions.lastToggle, actions.toggleCalls)
	}
}

func TestFilterDeleteUsesConfirmationAndDelegatesValidatedPOST(t *testing.T) {
	t.Parallel()

	body := serveHandler(t, tenantAdminHandler(t, &recordingActions{}), http.MethodGet, "/filters", "").Body.String()
	if !strings.Contains(body, `action="/filters/delete"`) || !strings.Contains(body, `data-confirm="실제 필터 필터를 삭제할까요?`) {
		t.Error("filter delete control or confirmation is missing")
	}

	actions := &recordingActions{}
	handler := tenantAdminHandler(t, actions)
	response := serveHandler(t, handler, http.MethodPost, "/filters/delete", "_csrf=token-123&filter=real-filter")
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/filters?deleted=1" {
		t.Fatalf("delete response=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	if actions.deleteFilterCalls != 1 || actions.lastDeleteFilter.FilterID != "real-filter" {
		t.Fatalf("delete action=%#v calls=%d", actions.lastDeleteFilter, actions.deleteFilterCalls)
	}

	bad := serveHandler(t, handler, http.MethodPost, "/filters/delete", "_csrf=wrong&filter=real-filter")
	if bad.Code != http.StatusForbidden || actions.deleteFilterCalls != 1 {
		t.Fatalf("invalid CSRF status=%d calls=%d", bad.Code, actions.deleteFilterCalls)
	}
}

func TestFilterDeleteRejectsPlatformAdminWithForgedTenantContext(t *testing.T) {
	t.Parallel()

	actions := &recordingActions{}
	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{},
		Actions: actions,
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{TenantID: "tenant-1", Role: "platform_admin", CSRFToken: "token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serveHandler(t, handler, http.MethodPost, "/filters/delete", "_csrf=token&filter=filter-1")
	if response.Code != http.StatusForbidden || actions.deleteFilterCalls != 0 {
		t.Fatalf("platform delete status=%d calls=%d", response.Code, actions.deleteFilterCalls)
	}
}

func TestSettingsTableKeepsMobileLabels(t *testing.T) {
	t.Parallel()

	body := serve(t, http.MethodGet, "/settings").Body.String()
	for _, want := range []string{`<th scope="col">이름</th>`, `data-label="이름"`, `data-label="이메일"`, `data-label="역할"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings table missing %q", want)
		}
	}
}

func TestUnavailableControlsExplainWhyTheyAreDisabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		ids  []string
	}{
		{"/notices/2026-sample-001", []string{"detail-original-note"}},
		{"/reports", []string{"mail-disabled-note"}},
		{"/settings", []string{"session-note"}},
		{"/admin", []string{"admin-integration-note"}},
	}
	for _, tt := range tests {
		body := serve(t, http.MethodGet, tt.path).Body.String()
		for _, id := range tt.ids {
			if !strings.Contains(body, `aria-describedby="`+id+`"`) || !strings.Contains(body, `id="`+id+`"`) {
				t.Errorf("%s does not connect disabled control to %q", tt.path, id)
			}
		}
	}
}

func TestViewHelpersHandleEmptyAndUnicodeValues(t *testing.T) {
	t.Parallel()

	if got := firstReason(nil); got != "" {
		t.Errorf("firstReason(nil) = %q, want empty", got)
	}
	if got := initial("김담당"); got != "김" {
		t.Errorf("initial(김담당) = %q, want 김", got)
	}
	if got := initial(""); got != "" {
		t.Errorf("initial(empty) = %q, want empty", got)
	}
}

func serve(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()
	NewHandler().ServeHTTP(response, request)
	return response
}

func TestLoginRedirectsToDashboard(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=demo%40example.com&password=demo"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	NewHandler().ServeHTTP(response, request)

	result := response.Result()
	defer result.Body.Close()
	_, _ = io.Copy(io.Discard, result.Body)
	if result.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusNotImplemented)
	}
	if body := response.Body.String(); !strings.Contains(body, "읽기 전용 데모") {
		t.Errorf("body missing read-only explanation: %q", body)
	}
}

func TestDemoMutationDoesNotClaimPersistence(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/filters", strings.NewReader("name=새+필터"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	NewHandler().ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotImplemented)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("demo mutation redirected to fake success %q", location)
	}
	if body := serve(t, http.MethodGet, "/filters").Body.String(); !strings.Contains(body, `disabled aria-describedby="demo-readonly-note"`) {
		t.Error("demo save controls are not visibly disabled")
	}
}

func TestInjectedContextAndBackendMapToPages(t *testing.T) {
	t.Parallel()

	handler := productionHandler(t, &recordingActions{})
	tests := []struct {
		path string
		want []string
	}{
		{"/dashboard", []string{"실사용자", "실테넌트", "오늘 05:55", "9건"}},
		{"/notices", []string{"실제 연동 공고", "실제 기관"}},
		{"/filters", []string{"실제 필터"}},
		{"/reports", []string{"namo-20260902-081500.html", `value="08:15"`, "Asia/Seoul", "9건", `value="1" checked`, `value="5" checked`}},
		{"/settings", []string{"실제 구성원", `value="contact@real.example"`}},
		{"/admin", []string{"실제 테넌트", "오늘 05:40", "2,468건", "3건"}},
	}
	for _, tt := range tests {
		response := serveHandler(t, handler, http.MethodGet, tt.path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", tt.path, response.Code)
		}
		for _, want := range tt.want {
			if !strings.Contains(response.Body.String(), want) {
				t.Errorf("GET %s missing %q", tt.path, want)
			}
		}
		if strings.Contains(response.Body.String(), "샘플 화면") || strings.Contains(response.Body.String(), "샘플 데이터") {
			t.Errorf("GET %s labels production data as sample", tt.path)
		}
	}
}

func TestProductionNoticeUsesSafeSourceURL(t *testing.T) {
	t.Parallel()

	body := serveHandler(t, productionHandler(t, &recordingActions{}), http.MethodGet, "/notices/real-001", "").Body.String()
	for _, want := range []string{`href="https://www.g2b.go.kr/sample/real-001"`, `rel="noopener noreferrer"`, "나라장터 원문 보기"} {
		if !strings.Contains(body, want) {
			t.Errorf("notice detail missing %q", want)
		}
	}
}

func TestProductionMutationFormsContainMappedCSRF(t *testing.T) {
	t.Parallel()

	handler := tenantAdminHandler(t, &recordingActions{})
	for _, path := range []string{"/filters", "/reports", "/settings"} {
		body := serveHandler(t, handler, http.MethodGet, path, "").Body.String()
		if !strings.Contains(body, `type="hidden" name="_csrf" value="token-123"`) {
			t.Errorf("GET %s missing mapped CSRF field", path)
		}
	}
}

func TestSaveFilterValidatesCSRFAndCommandBeforeAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		form      string
		wantCode  int
		wantCalls int
	}{
		{"valid", "_csrf=token-123&name=감사+용역&include_keywords=감사&include_mode=all&deadline_days=3", http.StatusSeeOther, 1},
		{"busan and gyeongnam", "_csrf=token-123&name=부산+공사&deadline_days=3&region=부산%2C+경남", http.StatusSeeOther, 1},
		{"region too long", "_csrf=token-123&name=지역&deadline_days=3&region=" + strings.Repeat("가", 129), http.StatusBadRequest, 0},
		{"default all", "_csrf=token-123&name=감사+용역&include_keywords=감사&deadline_days=3&category=&region=&min_amount=", http.StatusSeeOther, 1},
		{"bad csrf", "_csrf=wrong&name=감사+용역&deadline_days=3", http.StatusForbidden, 0},
		{"missing name", "_csrf=token-123&deadline_days=3", http.StatusBadRequest, 0},
		{"invalid deadline", "_csrf=token-123&name=감사+용역&deadline_days=-1", http.StatusBadRequest, 0},
		{"invalid category", "_csrf=token-123&name=감사+용역&deadline_days=3&category=임의값", http.StatusBadRequest, 0},
		{"invalid include mode", "_csrf=token-123&name=감사+용역&deadline_days=3&include_mode=임의값", http.StatusBadRequest, 0},
		{"malformed amount", "_csrf=token-123&name=감사+용역&deadline_days=3&min_amount=만원", http.StatusBadRequest, 0},
		{"negative amount", "_csrf=token-123&name=감사+용역&deadline_days=3&min_amount=-1", http.StatusBadRequest, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := &recordingActions{}
			handler := productionHandler(t, actions)
			response := serveHandler(t, handler, http.MethodPost, "/filters", tt.form)
			if response.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantCode, response.Body.String())
			}
			if actions.saveFilterCalls != tt.wantCalls {
				t.Errorf("SaveFilter calls = %d, want %d", actions.saveFilterCalls, tt.wantCalls)
			}
			if tt.name == "default all" && (actions.lastFilter.Category != "" || actions.lastFilter.Region != "" || actions.lastFilter.MinimumAmount != nil) {
				t.Errorf("default all command = %#v", actions.lastFilter)
			}
			if tt.name == "valid" && actions.lastFilter.IncludeMode != "all" {
				t.Errorf("include mode = %q, want all", actions.lastFilter.IncludeMode)
			}
		})
	}
}

func TestSaveFilterRejectsZeroDeadlineDays(t *testing.T) {
	response := serveHandler(t, productionHandler(t, &recordingActions{}), http.MethodPost, "/filters", "_csrf=token-123&name=03&include_keywords=회계&deadline_days=0")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("zero deadline window accepted: %d", response.Code)
	}
}

func TestSaveFilterAcceptsEmptyDeadlineDays(t *testing.T) {
	response := serveHandler(t, productionHandler(t, &recordingActions{}), http.MethodPost, "/filters", "_csrf=token-123&name=데이터&include_keywords=데이터&deadline_days=")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("unlimited deadline rejected: %d", response.Code)
	}
}

func TestFilterAllOptionsHaveEmptyValues(t *testing.T) {
	t.Parallel()

	body := serve(t, http.MethodGet, "/filters").Body.String()
	if strings.Count(body, `<option value="">전체</option>`) != 1 || !strings.Contains(body, `name="region" maxlength="128"`) {
		t.Errorf("filter category default or free-text region input is missing")
	}
}

func TestDisabledFilterShowsPausedLabelInsteadOfZeroMatches(t *testing.T) {
	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{data: AppData{Filters: []FilterView{{ID: "f1", Name: "데이터", Summary: "ANY: 데이터", Enabled: false}}}},
		Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{Role: "tenant_admin", TenantID: "tenant-1", CSRFToken: "token-123"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := serveHandler(t, handler, http.MethodGet, "/filters", "").Body.String()
	if strings.Contains(body, "현재 공고 0건 일치") {
		t.Fatal("disabled filter renders a misleading zero count")
	}
	if !strings.Contains(body, "사용 안 함") {
		t.Fatalf("disabled filter is not labelled: %s", body)
	}
}

func TestNoticeRegionSearchAcceptsCommaSeparatedFreeText(t *testing.T) {
	notices := []noticeView{
		{ID: "busan", Region: "부산광역시"},
		{ID: "seoul", Region: "서울특별시"},
	}
	got := filterNotices(notices, "", "", "", "경남, 부산")
	if len(got) != 1 || got[0].ID != "busan" {
		t.Fatalf("filtered notices=%+v", got)
	}
}

func TestInjectedRoleRestrictsPlatformAdmin(t *testing.T) {
	t.Parallel()

	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{data: AppData{}},
		Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{UserName: "담당자", TenantName: "일반 테넌트", TenantID: "tenant-1", Role: "member", CSRFToken: "token-123"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := serveHandler(t, handler, http.MethodGet, "/dashboard", "").Body.String(); strings.Contains(body, `href="/admin"`) {
		t.Error("member dashboard exposes platform admin navigation")
	}
	if got := serveHandler(t, handler, http.MethodGet, "/admin", "").Code; got != http.StatusForbidden {
		t.Errorf("member GET /admin status = %d, want %d", got, http.StatusForbidden)
	}
}

func TestAuthenticatedShellIncludesLogoutForm(t *testing.T) {
	t.Parallel()

	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{data: AppData{}},
		Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{UserName: "담당자", TenantName: "테넌트", TenantID: "tenant-1", Role: "member", CSRFToken: "logout-token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := serveHandler(t, handler, http.MethodGet, "/dashboard", "").Body.String()
	for _, want := range []string{
		`<form class="logout-form" method="post" action="/logout">`,
		`<input type="hidden" name="_csrf" value="logout-token">`,
		`>로그아웃</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("authenticated shell missing %q", want)
		}
	}
}

func TestNonPlatformAdminRejectedBeforeBackendLoad(t *testing.T) {
	t.Parallel()

	backend := &staticBackend{data: AppData{}}
	handler, err := NewHandlerWithOptions(Options{
		Backend: backend,
		Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{Role: "tenant_admin", TenantID: "tenant-1", CSRFToken: "token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := serveHandler(t, handler, http.MethodGet, "/admin", "").Code; got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
	if backend.calls != 0 {
		t.Errorf("Backend.Load calls = %d, want 0", backend.calls)
	}
}

func TestTenantMutationCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		role      string
		tenantID  string
		wantCode  int
		wantCalls int
	}{
		{"member", "member", "tenant-1", http.StatusForbidden, 0},
		{"tenant admin", "tenant_admin", "tenant-1", http.StatusSeeOther, 1},
		{"platform without tenant", "platform_admin", "", http.StatusForbidden, 0},
		{"platform with tenant", "platform_admin", "tenant-1", http.StatusSeeOther, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := &recordingActions{}
			handler, err := NewHandlerWithOptions(Options{
				Backend: &staticBackend{data: AppData{}},
				Actions: actions,
				MapContext: func(*http.Request) (RequestContext, error) {
					return RequestContext{Role: tt.role, TenantID: tt.tenantID, CSRFToken: "token"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			response := serveHandler(t, handler, http.MethodPost, "/filters", "_csrf=token&name=필터&deadline_days=3")
			if response.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", response.Code, tt.wantCode)
			}
			if actions.saveFilterCalls != tt.wantCalls {
				t.Errorf("SaveFilter calls = %d, want %d", actions.saveFilterCalls, tt.wantCalls)
			}
		})
	}
}

func TestMemberPagesRenderReadOnlyControls(t *testing.T) {
	t.Parallel()

	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{data: AppData{Filters: []FilterView{{ID: "f", Name: "필터"}}}},
		Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{Role: "member", TenantID: "tenant-1", CSRFToken: "token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/filters", "/reports", "/settings"} {
		body := serveHandler(t, handler, http.MethodGet, path, "").Body.String()
		if !strings.Contains(body, "읽기 전용") || !strings.Contains(body, "disabled") {
			t.Errorf("GET %s does not render read-only controls", path)
		}
	}
}

func TestProductionLoginFormIsEnabledForOuterAuth(t *testing.T) {
	t.Parallel()

	body := serveHandler(t, productionHandler(t, &recordingActions{}), http.MethodGet, "/login", "").Body.String()
	if !strings.Contains(body, `<form method="post" action="/login"`) {
		t.Error("production login form does not target outer auth middleware")
	}
	if strings.Contains(body, `id="email" name="email" type="email" autocomplete="username" placeholder="name@company.com" disabled`) {
		t.Error("production login email remains disabled")
	}
	if !strings.Contains(body, `type="hidden" name="_csrf" value="token-123"`) {
		t.Error("production login form missing CSRF field")
	}
}

func TestLoginPageAcceptsEmailOrUsername(t *testing.T) {
	body := serveHandler(t, productionHandler(t, &recordingActions{}), http.MethodGet, "/login", "").Body.String()
	for _, want := range []string{`<label for="email">이메일 또는 아이디</label>`, `id="email" name="email" type="text"`, `placeholder="이메일 또는 아이디"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("login form missing %q", want)
		}
	}
}

func TestProductionLoginDoesNotLoadAuthenticatedBackend(t *testing.T) {
	t.Parallel()
	backend := &staticBackend{err: errors.New("authentication required")}
	handler, err := NewHandlerWithOptions(Options{
		Backend: backend,
		Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{CSRFToken: "login-token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serveHandler(t, handler, http.MethodGet, "/login", "")
	if response.Code != http.StatusOK || backend.calls != 0 {
		t.Fatalf("status=%d backend calls=%d body=%s", response.Code, backend.calls, response.Body.String())
	}
}

func TestNewHandlerWithOptionsRejectsMissingProductionDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewHandlerWithOptions(Options{}); err == nil {
		t.Fatal("NewHandlerWithOptions accepted empty production dependencies")
	}
}

func TestReportScheduleAndSettingsActionsValidateMapAndReportErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		form        string
		actionErr   error
		wantCode    int
		wantReport  int
		wantSetting int
	}{
		{"report valid", "/reports", "_csrf=token-123&delivery_time=08%3A15&timezone=Asia%2FSeoul&delivery_days=1&delivery_days=5", nil, http.StatusSeeOther, 1, 0},
		{"report invalid time", "/reports", "_csrf=token-123&delivery_time=25%3A99&timezone=Asia%2FSeoul&delivery_days=1", nil, http.StatusBadRequest, 0, 0},
		{"report action error", "/reports", "_csrf=token-123&delivery_time=08%3A15&timezone=Asia%2FSeoul&delivery_days=1", errors.New("save failed"), http.StatusInternalServerError, 1, 0},
		{"settings valid", "/settings", "_csrf=token-123&tenant_name=실제+테넌트&contact_email=contact%40real.example", nil, http.StatusSeeOther, 0, 1},
		{"settings invalid email", "/settings", "_csrf=token-123&tenant_name=실제+테넌트&contact_email=bad", nil, http.StatusBadRequest, 0, 0},
		{"settings action error", "/settings", "_csrf=token-123&tenant_name=실제+테넌트&contact_email=contact%40real.example", errors.New("save failed"), http.StatusInternalServerError, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := &recordingActions{err: tt.actionErr}
			handler := productionHandler(t, actions)
			if strings.HasPrefix(tt.path, "/reports") {
				handler = tenantAdminHandler(t, actions)
			}
			response := serveHandler(t, handler, http.MethodPost, tt.path, tt.form)
			if response.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantCode, response.Body.String())
			}
			if actions.reportScheduleCalls != tt.wantReport || actions.settingsCalls != tt.wantSetting {
				t.Errorf("report calls=%d settings calls=%d", actions.reportScheduleCalls, actions.settingsCalls)
			}
			if tt.wantReport == 1 && (actions.lastReportSchedule.DeliveryTime != "08:15" || actions.lastReportSchedule.Timezone != "Asia/Seoul" || len(actions.lastReportSchedule.DeliveryDays) == 0) {
				t.Errorf("report schedule command = %#v", actions.lastReportSchedule)
			}
			if tt.wantSetting == 1 && actions.lastSettings.ContactEmail != "contact@real.example" {
				t.Errorf("settings command = %#v", actions.lastSettings)
			}
		})
	}
}

func TestReportWeekdaysRequireOneValidDay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		days      string
		wantCode  int
		wantCalls int
	}{
		{"none", "", http.StatusBadRequest, 0},
		{"below range", "&delivery_days=-1", http.StatusBadRequest, 0},
		{"above range", "&delivery_days=7", http.StatusBadRequest, 0},
		{"valid", "&delivery_days=0&delivery_days=6", http.StatusSeeOther, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := &recordingActions{}
			form := "_csrf=token-123&delivery_time=08%3A15&timezone=Asia%2FSeoul" + tt.days
			response := serveHandler(t, tenantAdminHandler(t, actions), http.MethodPost, "/reports", form)
			if response.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantCode)
			}
			if actions.reportScheduleCalls != tt.wantCalls {
				t.Errorf("SaveReportSchedule calls = %d, want %d", actions.reportScheduleCalls, tt.wantCalls)
			}
			if tt.wantCalls == 1 && (len(actions.lastReportSchedule.DeliveryDays) != 2 || actions.lastReportSchedule.DeliveryDays[0] != 0 || actions.lastReportSchedule.DeliveryDays[1] != 6) {
				t.Errorf("DeliveryDays = %#v", actions.lastReportSchedule.DeliveryDays)
			}
		})
	}
}

func TestReportScheduleValidationUsesCreationWording(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, form, want string
	}{
		{"days", "_csrf=token-123&delivery_time=08%3A15&timezone=Asia%2FSeoul", "생성 요일"},
		{"time", "_csrf=token-123&delivery_time=25%3A99&timezone=Asia%2FSeoul&delivery_days=1", "생성 시각"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := serveHandler(t, tenantAdminHandler(t, &recordingActions{}), http.MethodPost, "/reports", tt.form)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			if body := response.Body.String(); !strings.Contains(body, tt.want) || strings.Contains(body, "발송") {
				t.Errorf("validation body = %q, want creation wording", body)
			}
		})
	}
}

func TestNotificationRoutesRedirectOrStayRemoved(t *testing.T) {
	t.Parallel()

	response := serve(t, http.MethodGet, "/notifications")
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/reports" {
		t.Fatalf("notification redirect = %d %q", response.Code, response.Header().Get("Location"))
	}
	actions := &recordingActions{}
	handler := productionHandler(t, actions)
	for _, path := range []string{"/notifications/recipients", "/admin/test-mail"} {
		if got := serveHandler(t, handler, http.MethodPost, path, "_csrf=token-123").Code; got != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want %d", path, got, http.StatusNotFound)
		}
	}
	if actions.recipientCalls != 0 || actions.testMailCalls != 0 {
		t.Error("removed mail execution route called an action")
	}
}

func TestPlatformActionsValidateCSRFPermissionsAndShowResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		csrf        string
		actionErr   error
		wantCode    int
		wantCollect int
		wantMail    int
		wantResult  string
	}{
		{"collect", "/admin/collect", "token-123", nil, http.StatusSeeOther, 1, 0, "수집 작업을 시작했습니다."},
		{"bad csrf", "/admin/collect", "wrong", nil, http.StatusForbidden, 0, 0, ""},
		{"action error", "/admin/collect", "token-123", errors.New("run failed"), http.StatusInternalServerError, 1, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := &recordingActions{err: tt.actionErr}
			handler := productionHandler(t, actions)
			response := serveHandler(t, handler, http.MethodPost, tt.path, "_csrf="+tt.csrf)
			if response.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantCode)
			}
			if actions.collectCalls != tt.wantCollect || actions.testMailCalls != tt.wantMail {
				t.Errorf("collect=%d test-mail=%d", actions.collectCalls, actions.testMailCalls)
			}
			if tt.wantResult != "" {
				location := response.Header().Get("Location")
				body := serveHandler(t, handler, http.MethodGet, location, "").Body.String()
				if !strings.Contains(body, tt.wantResult) {
					t.Errorf("result page missing %q", tt.wantResult)
				}
			}
		})
	}
}

func TestPlatformActionsDisabledForDemoAndForbiddenForMember(t *testing.T) {
	t.Parallel()

	if got := serveHandler(t, NewHandler(), http.MethodPost, "/admin/collect", "_csrf=x").Code; got != http.StatusNotImplemented {
		t.Errorf("demo status = %d, want %d", got, http.StatusNotImplemented)
	}
	actions := &recordingActions{}
	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{}, Actions: actions,
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{Role: "member", TenantID: "tenant-1", CSRFToken: "token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := serveHandler(t, handler, http.MethodPost, "/admin/collect", "_csrf=token").Code; got != http.StatusForbidden {
		t.Errorf("member status = %d, want %d", got, http.StatusForbidden)
	}
	if actions.collectCalls != 0 || actions.testMailCalls != 0 {
		t.Error("forbidden platform action was called")
	}
}

const testReportID = "123e4567-e89b-12d3-a456-426614174000"

func TestReportsRenderScheduleHistoryStatesAndDisabledMail(t *testing.T) {
	t.Parallel()

	body := serve(t, http.MethodGet, "/reports").Body.String()
	for _, want := range []string{
		"리포트 일정", "최근 생성 리포트", "namo-20260902-070000.html", "7건", "다운로드", "지금 생성",
		`data-label="파일명"`, `data-label="공고 수"`, `disabled aria-describedby="mail-disabled-note"`, "메일 발송", "준비 중",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reports page missing %q", want)
		}
	}
	if strings.Count(body, "/retry") != 1 {
		t.Errorf("retry action count = %d, want only the exhausted failure", strings.Count(body, "/retry"))
	}
	for _, state := range []struct{ query, marker string }{{"empty", "아직 생성된 리포트가 없습니다."}, {"error", "리포트를 불러오지 못했습니다."}, {"loading", `aria-busy="true"`}} {
		if got := serve(t, http.MethodGet, "/reports?state="+state.query).Body.String(); !strings.Contains(got, state.marker) {
			t.Errorf("%s state missing %q", state.query, state.marker)
		}
	}
}

func TestReportMutationsRequireTenantAdminAndCSRF(t *testing.T) {
	t.Parallel()

	actions := &recordingActions{}
	handler := tenantAdminHandler(t, actions)
	tests := []struct {
		path, form string
		wantCode   int
	}{
		{"/reports", "_csrf=token-123&delivery_time=08%3A15&timezone=Asia%2FSeoul&delivery_days=1", http.StatusSeeOther},
		{"/reports/generate", "_csrf=token-123", http.StatusSeeOther},
		{"/reports/" + testReportID + "/retry", "_csrf=token-123", http.StatusSeeOther},
		{"/reports/generate", "_csrf=wrong", http.StatusForbidden},
		{"/reports/not-a-uuid/retry", "_csrf=token-123", http.StatusNotFound},
	}
	for _, tt := range tests {
		response := serveHandler(t, handler, http.MethodPost, tt.path, tt.form)
		if response.Code != tt.wantCode {
			t.Errorf("POST %s status = %d, want %d; body=%s", tt.path, response.Code, tt.wantCode, response.Body.String())
		}
	}
	if actions.reportScheduleCalls != 1 || actions.generateReportCalls != 1 || actions.retryReportCalls != 1 || actions.lastReportID != testReportID {
		t.Fatalf("report actions schedule=%d generate=%d retry=%d id=%q", actions.reportScheduleCalls, actions.generateReportCalls, actions.retryReportCalls, actions.lastReportID)
	}

	memberActions := &recordingActions{downloadName: "namo-20260902-081500.html", downloadBody: "<html>ok</html>"}
	member, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{data: AppData{Reports: []ReportView{{ID: testReportID, FileName: memberActions.downloadName, Downloadable: true}}}},
		Actions: memberActions,
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{Role: "member", TenantID: "tenant-1", CSRFToken: "token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := serveHandler(t, member, http.MethodGet, "/reports", "").Code; got != http.StatusOK {
		t.Errorf("member list status = %d", got)
	}
	if got := serveHandler(t, member, http.MethodGet, "/reports/"+testReportID+"/download", "").Code; got != http.StatusOK {
		t.Errorf("member download status = %d", got)
	}
	for _, path := range []string{"/reports", "/reports/generate", "/reports/" + testReportID + "/retry"} {
		if got := serveHandler(t, member, http.MethodPost, path, "_csrf=token").Code; got != http.StatusForbidden {
			t.Errorf("member POST %s status = %d, want %d", path, got, http.StatusForbidden)
		}
	}
	if memberActions.reportScheduleCalls != 0 || memberActions.generateReportCalls != 0 || memberActions.retryReportCalls != 0 {
		t.Error("member mutation called a report action")
	}
	platformActions := &recordingActions{}
	if got := serveHandler(t, productionHandler(t, platformActions), http.MethodPost, "/reports/generate", "_csrf=token-123").Code; got != http.StatusForbidden {
		t.Errorf("platform admin report mutation status = %d, want %d", got, http.StatusForbidden)
	}
	if platformActions.generateReportCalls != 0 {
		t.Error("platform admin called tenant report mutation")
	}
}

func TestManualReportOutcomeRedirectsToAccurateResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		actionErr    error
		wantCode     int
		wantLocation string
	}{
		{"created", nil, http.StatusSeeOther, "/reports?result=generated"},
		{"no eligible matches", ErrNoReportMatches, http.StatusSeeOther, "/reports?result=empty"},
		{"failure", errors.New("private database failure"), http.StatusInternalServerError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := &recordingActions{err: tt.actionErr}
			response := serveHandler(t, tenantAdminHandler(t, actions), http.MethodPost, "/reports/generate", "_csrf=token-123")
			if response.Code != tt.wantCode || response.Header().Get("Location") != tt.wantLocation {
				t.Fatalf("status=%d location=%q, want %d %q; body=%s", response.Code, response.Header().Get("Location"), tt.wantCode, tt.wantLocation, response.Body.String())
			}
			if actions.generateReportCalls != 1 {
				t.Errorf("GenerateReport calls=%d, want 1", actions.generateReportCalls)
			}
			if strings.Contains(response.Body.String(), "private database failure") {
				t.Error("manual report failure exposed internal details")
			}
		})
	}
}

func TestManualReportEmptyResultExplainsNoFileWasCreated(t *testing.T) {
	t.Parallel()

	body := serveHandler(t, tenantAdminHandler(t, &recordingActions{}), http.MethodGet, "/reports?result=empty", "").Body.String()
	for _, want := range []string{"현재 일치한 공고가 없습니다.", "리포트 파일을 생성하지 않았습니다."} {
		if !strings.Contains(body, want) {
			t.Errorf("empty result missing %q", want)
		}
	}
	for _, unwanted := range []string{"리포트 생성을 요청했습니다.", `alert alert-success`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("empty result incorrectly contains success marker %q", unwanted)
		}
	}
}

func TestReportDownloadSetsAttachmentSecurityHeadersAndHonorsHEAD(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		actions := &recordingActions{
			downloadName: "namo-20260902-081500.html", downloadBody: "<html><body>report</body></html>",
			downloadModified: time.Date(2026, 9, 2, 8, 15, 0, 0, time.UTC),
		}
		response := serveHandler(t, productionHandler(t, actions), method, "/reports/"+testReportID+"/download", "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d; body=%s", method, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Disposition"); got != "attachment; filename=namo-20260902-081500.html" {
			t.Errorf("Content-Disposition = %q", got)
		}
		if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q", got)
		}
		if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		if method == http.MethodHead && response.Body.Len() != 0 {
			t.Errorf("HEAD returned %d body bytes", response.Body.Len())
		}
		if method == http.MethodGet && response.Body.String() != actions.downloadBody {
			t.Errorf("GET body = %q", response.Body.String())
		}
		if actions.openReportCalls != 1 || actions.lastReportID != testReportID || actions.lastDownloadBody == nil || !actions.lastDownloadBody.closed {
			t.Errorf("download action calls=%d id=%q closed=%v", actions.openReportCalls, actions.lastReportID, actions.lastDownloadBody != nil && actions.lastDownloadBody.closed)
		}
	}
}

func TestReportDownloadPreservesKoreanFilename(t *testing.T) {
	for _, name := range []string{"20260907_데이터보고서.html", "20260907_통합보고서.html", "20260907_" + strings.Repeat("😀", 55) + "보고서.html"} {
		actions := &recordingActions{downloadName: name, downloadBody: "<html>report</html>"}
		response := serveHandler(t, productionHandler(t, actions), http.MethodGet, "/reports/"+testReportID+"/download", "")
		kind, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
		if response.Code != http.StatusOK || err != nil || kind != "attachment" || params["filename"] != name {
			t.Fatalf("status=%d disposition=%q err=%v", response.Code, response.Header().Get("Content-Disposition"), err)
		}
	}
}

func TestReportAttachmentRejectsUnsafeOrMalformedFilenames(t *testing.T) {
	for _, name := range []string{
		"", "report.html", "20261307_데이터보고서.html", "20260907_보고서.html",
		"20260907_../데이터보고서.html", "20260907_..\\데이터보고서.html",
		"20260907_데이터\r\n보고서.html", "20260907_데이터\x00보고서.html",
		"20260907_데이터\x7f보고서.html", "20260907_데이터\u0085보고서.html",
		"20260907_\xff보고서.html", "20260907_데이터:보고서.html",
		"20260907_" + strings.Repeat("가", 100) + "보고서.html",
	} {
		if safeAttachmentName(name) {
			t.Errorf("accepted unsafe attachment %q", name)
		}
	}
}

func TestReportDownloadHidesMissingIDsAndRejectsUnsafeAttachmentNames(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{"missing", ErrReportNotFound, http.StatusNotFound},
		{"open error", errors.New("open failed"), http.StatusInternalServerError},
	} {
		actions := &recordingActions{downloadName: "report.html", downloadBody: "error body", reportErr: tt.err}
		if got := serveHandler(t, productionHandler(t, actions), http.MethodGet, "/reports/"+testReportID+"/download", "").Code; got != tt.wantCode {
			t.Errorf("%s report status = %d, want %d", tt.name, got, tt.wantCode)
		}
		if actions.lastDownloadBody == nil || !actions.lastDownloadBody.closed {
			t.Errorf("%s report body closed = %v, want true", tt.name, actions.lastDownloadBody != nil && actions.lastDownloadBody.closed)
		}
	}
	unsafe := &recordingActions{downloadName: "bad\r\nX-Evil: yes.html", downloadBody: "bad"}
	response := serveHandler(t, productionHandler(t, unsafe), http.MethodGet, "/reports/"+testReportID+"/download", "")
	if response.Code != http.StatusInternalServerError || response.Header().Get("X-Evil") != "" || response.Header().Get("Content-Disposition") != "" {
		t.Errorf("unsafe download status=%d headers=%v", response.Code, response.Header())
	}
	if unsafe.lastDownloadBody == nil || !unsafe.lastDownloadBody.closed {
		t.Errorf("unsafe report body closed = %v, want true", unsafe.lastDownloadBody != nil && unsafe.lastDownloadBody.closed)
	}
}

func TestReportDirectoryIsVisibleOnlyOnPlatformAdminPage(t *testing.T) {
	t.Parallel()

	const reportDir = `C:\\private\\namo\\reports`
	backend := &staticBackend{data: AppData{Admin: AdminView{ReportDir: reportDir}}}
	handler, err := NewHandlerWithOptions(Options{
		Backend: backend, Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{Role: "platform_admin", TenantID: "tenant-1", CSRFToken: "token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := serveHandler(t, handler, http.MethodGet, "/admin", "").Body.String(); !strings.Contains(body, reportDir) {
		t.Error("platform admin page does not show REPORT_DIR")
	}
	if body := serveHandler(t, handler, http.MethodGet, "/reports", "").Body.String(); strings.Contains(body, reportDir) {
		t.Error("tenant report page exposes full REPORT_DIR")
	}
}

type testReportBody struct {
	*strings.Reader
	closed bool
}

func (b *testReportBody) Close() error {
	b.closed = true
	return nil
}

type staticBackend struct {
	data     AppData
	err      error
	calls    int
	lastPage PageRequest
}

func (b *staticBackend) Load(_ context.Context, _ RequestContext, page PageRequest) (AppData, error) {
	b.calls++
	b.lastPage = page
	return b.data, b.err
}

type recordingActions struct {
	saveFilterCalls     int
	toggleCalls         int
	deleteFilterCalls   int
	lastFilter          FilterCommand
	lastToggle          ToggleFilterCommand
	lastDeleteFilter    DeleteFilterCommand
	notificationCalls   int
	settingsCalls       int
	lastNotification    NotificationCommand
	lastSettings        SettingsCommand
	recipientCalls      int
	lastRecipient       RecipientCommand
	collectCalls        int
	testMailCalls       int
	reportScheduleCalls int
	generateReportCalls int
	retryReportCalls    int
	openReportCalls     int
	lastReportSchedule  NotificationCommand
	lastReportID        string
	downloadName        string
	downloadBody        string
	downloadModified    time.Time
	lastDownloadBody    *testReportBody
	assignCalls         int
	lastAssign          AssignAccountCommand
	tenantCalls         int
	lastTenant          TenantCommand
	tenantErr           error
	removeCalls         int
	lastRemove          AccountCommand
	deleteCalls         int
	lastDelete          AccountCommand
	reportErr           error
	err                 error
}

func (a *recordingActions) AssignAccountTenant(_ context.Context, _ RequestContext, command AssignAccountCommand) error {
	a.assignCalls++
	a.lastAssign = command
	return a.err
}

func (a *recordingActions) RemoveMember(_ context.Context, _ RequestContext, command AccountCommand) error {
	a.removeCalls++
	a.lastRemove = command
	return a.err
}

func (a *recordingActions) DeleteAccount(_ context.Context, _ RequestContext, command AccountCommand) error {
	a.deleteCalls++
	a.lastDelete = command
	return a.err
}

func (a *recordingActions) CreateTenant(_ context.Context, _ RequestContext, command TenantCommand) error {
	a.tenantCalls++
	a.lastTenant = command
	if a.tenantErr != nil {
		return a.tenantErr
	}
	return a.err
}

func (a *recordingActions) SaveFilter(_ context.Context, _ RequestContext, command FilterCommand) error {
	a.saveFilterCalls++
	a.lastFilter = command
	return a.err
}

func (a *recordingActions) ToggleFilter(_ context.Context, _ RequestContext, command ToggleFilterCommand) error {
	a.toggleCalls++
	a.lastToggle = command
	return a.err
}

func (a *recordingActions) DeleteFilter(_ context.Context, _ RequestContext, command DeleteFilterCommand) error {
	a.deleteFilterCalls++
	a.lastDeleteFilter = command
	return a.err
}

func (a *recordingActions) SaveNotification(_ context.Context, _ RequestContext, command NotificationCommand) error {
	a.notificationCalls++
	a.lastNotification = command
	return a.err
}

func (a *recordingActions) SaveSettings(_ context.Context, _ RequestContext, command SettingsCommand) error {
	a.settingsCalls++
	a.lastSettings = command
	return a.err
}

func (a *recordingActions) AddRecipient(_ context.Context, _ RequestContext, command RecipientCommand) error {
	a.recipientCalls++
	a.lastRecipient = command
	return a.err
}

func (a *recordingActions) RunCollection(context.Context, RequestContext) error {
	a.collectCalls++
	return a.err
}

func (a *recordingActions) SendTestMail(context.Context, RequestContext) error {
	a.testMailCalls++
	return a.err
}

func (a *recordingActions) SaveReportSchedule(_ context.Context, _ RequestContext, command NotificationCommand) error {
	a.reportScheduleCalls++
	a.lastReportSchedule = command
	return a.err
}

func (a *recordingActions) GenerateReport(context.Context, RequestContext) error {
	a.generateReportCalls++
	return a.err
}

func (a *recordingActions) RetryReport(_ context.Context, _ RequestContext, reportID string) error {
	a.retryReportCalls++
	a.lastReportID = reportID
	return a.err
}

func (a *recordingActions) DeleteReport(_ context.Context, _ RequestContext, reportID string) error {
	a.lastReportID = reportID
	return a.err
}

func (a *recordingActions) OpenReport(_ context.Context, _ RequestContext, reportID string) (ReportDownload, error) {
	a.openReportCalls++
	a.lastReportID = reportID
	body := &testReportBody{Reader: strings.NewReader(a.downloadBody)}
	a.lastDownloadBody = body
	download := ReportDownload{Name: a.downloadName, Modified: a.downloadModified, Body: body}
	return download, a.reportErr
}

func productionHandler(t *testing.T, actions Actions) http.Handler {
	return productionHandlerForRole(t, actions, "platform_admin")
}

func tenantAdminHandler(t *testing.T, actions Actions) http.Handler {
	return productionHandlerForRole(t, actions, "tenant_admin")
}

func productionHandlerForRole(t *testing.T, actions Actions, role string) http.Handler {
	t.Helper()
	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{data: AppData{
			Dashboard:    DashboardView{LastCollected: "오늘 05:55", NewNotices: 18, Matches: 9, RunTime: "06:30", Healthy: true},
			Notices:      []NoticeView{{ID: "real-001", Title: "실제 연동 공고", Category: "용역", Agency: "실제 기관", Region: "서울", Amount: "1원", Deadline: "2026.09.10", SourceURL: "https://www.g2b.go.kr/sample/real-001", Reasons: []string{"실제 조건"}}},
			Filters:      []FilterView{{ID: "real-filter", Name: "실제 필터", Summary: "실제 조건", Matches: 1, Enabled: true}},
			Recipients:   []RecipientView{{Name: "실제 수신자", Email: "real@example.com", State: "수신"}},
			Members:      []MemberView{{Name: "실제 구성원", Email: "member@example.com", Role: "담당자"}},
			Tenants:      []TenantView{{Name: "실제 테넌트", Members: 1, LastDigest: "오늘", State: "정상"}},
			Reports:      []ReportView{{ID: testReportID, FileName: "namo-20260902-081500.html", Trigger: "수동", Status: "생성 완료", DueAt: "2026.09.02 08:15", GeneratedAt: "2026.09.02 08:15", NoticeCount: 9, Downloadable: true}},
			DeliveryTime: "08:15",
			DeliveryDays: []int{1, 2, 3, 4, 5},
			Timezone:     "Asia/Seoul",
			ContactEmail: "contact@real.example",
			Admin:        AdminView{Healthy: true, LastCollected: "오늘 05:40", CollectedCount: 2468, FailedJobs: 3, ReportDir: `C:\\private\\reports`},
			Demo:         false,
		}},
		Actions: actions,
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{UserName: "실사용자", TenantName: "실테넌트", TenantID: "tenant-real", Role: role, CSRFToken: "token-123"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func serveHandler(t *testing.T, handler http.Handler, method, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(form))
	if form != "" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
