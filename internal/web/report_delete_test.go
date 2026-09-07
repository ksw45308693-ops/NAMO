package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type reportDeleteActions struct {
	recordingActions
	deletes int
}

func (a *reportDeleteActions) DeleteReport(_ context.Context, _ RequestContext, id string) error {
	a.deletes++
	a.lastReportID = id
	return a.err
}

func TestDeleteReportRouteRequiresPOSTAdminCSRF(t *testing.T) {
	for _, tt := range []struct {
		role, method, csrf, id string
		code, calls            int
	}{
		{"tenant_admin", "POST", "token-123", testReportID, 303, 1},
		{"tenant_admin", "GET", "token-123", testReportID, 405, 0},
		{"tenant_admin", "POST", "wrong", testReportID, 403, 0},
		{"member", "POST", "token-123", testReportID, 403, 0},
		{"platform_admin", "POST", "token-123", testReportID, 403, 0},
		{"tenant_admin", "POST", "token-123", "invalid", 404, 0},
	} {
		actions := &reportDeleteActions{}
		response := serveHandler(t, productionHandlerForRole(t, actions, tt.role), tt.method, "/reports/"+tt.id+"/delete", "_csrf="+tt.csrf)
		if response.Code != tt.code || actions.deletes != tt.calls {
			t.Fatalf("%+v status=%d calls=%d", tt, response.Code, actions.deletes)
		}
		if tt.calls == 1 && (actions.lastReportID != testReportID || response.Header().Get("Location") != "/reports?result=deleted") {
			t.Fatal("wrong delete target or redirect")
		}
	}
}

func TestDeleteReportErrorsDoNotClaimSuccess(t *testing.T) {
	for _, tt := range []struct {
		err  error
		code int
	}{{ErrReportNotFound, 404}, {errors.New("private filesystem failure"), 500}} {
		actions := &reportDeleteActions{recordingActions: recordingActions{err: tt.err}}
		r := serveHandler(t, tenantAdminHandler(t, actions), http.MethodPost, "/reports/"+testReportID+"/delete", "_csrf=token-123")
		if r.Code != tt.code || r.Header().Get("Location") != "" || strings.Contains(r.Body.String(), "private filesystem") {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
	}
}

func TestReportDeleteButtonOnlyForTerminalAndWritable(t *testing.T) {
	for _, role := range []string{"tenant_admin", "member", "platform_admin"} {
		h, err := NewHandlerWithOptions(Options{Backend: &staticBackend{data: AppData{Reports: []ReportView{
			{ID: testReportID, FileName: "20260907_데이터보고서.html", Status: "생성 완료", Downloadable: true},
			{ID: "failed", Status: "생성 실패"}, {ID: "pending", Status: "생성 중"},
		}}}, Actions: &reportDeleteActions{}, MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{Role: role, TenantID: "tenant-a", CSRFToken: "token-123"}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		body := serveHandler(t, h, http.MethodGet, "/reports", "").Body.String()
		want := role == "tenant_admin"
		if strings.Contains(body, "/reports/"+testReportID+"/delete") != want {
			t.Fatalf("delete button role=%s", role)
		}
		if want && (!strings.Contains(body, "data-confirm=") || !strings.Contains(body, "복구할 수 없습니다")) {
			t.Fatal("missing delete confirmation")
		}
		if strings.Contains(body, "/reports/failed/delete") != want {
			t.Fatalf("failed report delete button role=%s", role)
		}
		if strings.Contains(body, "/reports/pending/delete") {
			t.Fatal("unfinished report deletion offered")
		}
	}
}
