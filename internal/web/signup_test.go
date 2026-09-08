package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func signupPageHandler(t *testing.T, actions Actions, requestContext RequestContext) (http.Handler, *staticBackend) {
	t.Helper()
	backend := &staticBackend{data: AppData{
		Tenants: []TenantView{{Name: "실제 테넌트", Members: 1, LastDigest: "오늘", State: "정상"}},
		Accounts: []AccountView{
			{UserID: "user-pending", Email: "newcomer@example.com", DisplayName: "newcomer", Created: "2026.09.03 09:12", Role: "member", RoleLabel: "일반 사용자"},
			{UserID: "user-assigned", Email: "member@example.com", DisplayName: "member", TenantID: "tenant-real", TenantName: "실제 테넌트", Created: "2026.08.28 14:03", Role: "tenant_admin", RoleLabel: "회사 관리자", Assigned: true},
		},
		TenantOptions: []TenantOption{{ID: "tenant-real", Name: "실제 테넌트"}},
		Admin:         AdminView{Healthy: true, LastCollected: "오늘 05:40"},
	}}
	handler, err := NewHandlerWithOptions(Options{
		Backend: backend,
		Actions: actions,
		MapContext: func(*http.Request) (RequestContext, error) {
			return requestContext, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, backend
}

func TestSignupPageOffersEmailAndPasswordOnly(t *testing.T) {
	handler, backend := signupPageHandler(t, &recordingActions{}, RequestContext{CSRFToken: "token-123"})

	response := serveHandler(t, handler, http.MethodGet, "/signup", "")

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if backend.calls != 0 {
		t.Fatalf("anonymous signup page loaded tenant data %d times", backend.calls)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := response.Body.String()
	for _, want := range []string{
		`method="post" action="/signup"`,
		`name="_csrf" value="token-123"`,
		`name="email"`,
		`name="password"`,
		`name="password_confirm"`,
		`href="/login"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("signup page missing %q", want)
		}
	}
	for _, forbidden := range []string{`name="display_name"`, `name="tenant_name"`, `name="role"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("signup page collects more than email and password: %q", forbidden)
		}
	}
}

func TestLoginPageLinksToSignup(t *testing.T) {
	handler, _ := signupPageHandler(t, &recordingActions{}, RequestContext{CSRFToken: "token-123"})

	response := serveHandler(t, handler, http.MethodGet, "/login", "")

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `href="/signup"`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestAccountWithoutTenantSeesWaitingScreen(t *testing.T) {
	member := RequestContext{UserID: "user-pending", UserName: "newcomer@example.com", Email: "newcomer@example.com", Role: "member", CSRFToken: "token-123"}
	handler, backend := signupPageHandler(t, &recordingActions{}, member)

	dashboard := serveHandler(t, handler, http.MethodGet, "/dashboard", "")

	if dashboard.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", dashboard.Code, dashboard.Body.String())
	}
	if backend.calls != 0 {
		t.Fatalf("waiting account loaded tenant data %d times", backend.calls)
	}
	body := dashboard.Body.String()
	if !strings.Contains(body, "회사 배정 대기") || !strings.Contains(body, `action="/logout"`) {
		t.Fatalf("waiting screen body=%q", body)
	}
	if strings.Contains(body, `href="/notices"`) {
		t.Fatal("waiting screen exposes tenant navigation")
	}
}

func TestAccountWithoutTenantCannotSubmitTenantCommands(t *testing.T) {
	member := RequestContext{UserID: "user-pending", Email: "newcomer@example.com", Role: "member", CSRFToken: "token-123"}
	actions := &recordingActions{}
	handler, _ := signupPageHandler(t, actions, member)

	response := serveHandler(t, handler, http.MethodPost, "/filters", url.Values{"_csrf": {"token-123"}, "name": {"필터"}}.Encode())

	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", response.Code)
	}
	if actions.saveFilterCalls != 0 {
		t.Fatalf("filter command ran %d times for an unassigned account", actions.saveFilterCalls)
	}
}

func TestAdminPageAssignsAndRevokesMemberTenant(t *testing.T) {
	admin := RequestContext{UserID: "user-admin", UserName: "관리자", Role: "platform_admin", CSRFToken: "token-123"}
	actions := &recordingActions{}
	handler, _ := signupPageHandler(t, actions, admin)

	page := serveHandler(t, handler, http.MethodGet, "/admin", "")
	if page.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", page.Code, page.Body.String())
	}
	for _, want := range []string{
		`action="/admin/accounts"`,
		`name="user_id" value="user-pending"`,
		`value="tenant-real"`,
		"실제 테넌트",
		"미배정",
		`name="role"`,
		`<option value="tenant_admin" selected>회사 관리자</option>`,
		"회사 관리자",
		`name="mode" value="revoke"`,
	} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("admin page missing %q", want)
		}
	}

	assign := serveHandler(t, handler, http.MethodPost, "/admin/accounts",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-pending"}, "tenant_id": {"tenant-real"}, "role": {"tenant_admin"}}.Encode())
	if assign.Code != http.StatusSeeOther || assign.Header().Get("Location") != "/admin?result=account-assigned" {
		t.Fatalf("assign status=%d location=%q", assign.Code, assign.Header().Get("Location"))
	}
	if actions.lastAssign != (AssignAccountCommand{UserID: "user-pending", TenantID: "tenant-real", Role: "tenant_admin"}) {
		t.Fatalf("assign command = %+v", actions.lastAssign)
	}

	revoke := serveHandler(t, handler, http.MethodPost, "/admin/accounts",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-assigned"}, "tenant_id": {"tenant-real"}, "role": {"tenant_admin"}, "mode": {"revoke"}}.Encode())
	if revoke.Code != http.StatusSeeOther || revoke.Header().Get("Location") != "/admin?result=account-revoked" {
		t.Fatalf("revoke status=%d location=%q", revoke.Code, revoke.Header().Get("Location"))
	}
	if actions.lastAssign != (AssignAccountCommand{UserID: "user-assigned", Role: "member"}) {
		t.Fatalf("revoke command = %+v", actions.lastAssign)
	}
	if actions.assignCalls != 2 {
		t.Fatalf("assignCalls=%d", actions.assignCalls)
	}
}

func TestAccountAssignmentRejectsNonPlatformAdminAndBadRequest(t *testing.T) {
	actions := &recordingActions{}
	tenantAdmin, _ := signupPageHandler(t, actions, RequestContext{UserID: "user-admin", TenantID: "tenant-real", Role: "tenant_admin", CSRFToken: "token-123"})

	forbidden := serveHandler(t, tenantAdmin, http.MethodPost, "/admin/accounts",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-pending"}, "tenant_id": {"tenant-real"}}.Encode())
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("tenant admin status=%d, want 403", forbidden.Code)
	}

	admin, _ := signupPageHandler(t, actions, RequestContext{UserID: "user-admin", Role: "platform_admin", CSRFToken: "token-123"})
	missingCSRF := serveHandler(t, admin, http.MethodPost, "/admin/accounts",
		url.Values{"user_id": {"user-pending"}, "tenant_id": {"tenant-real"}}.Encode())
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d, want 403", missingCSRF.Code)
	}
	missingUser := serveHandler(t, admin, http.MethodPost, "/admin/accounts",
		url.Values{"_csrf": {"token-123"}, "tenant_id": {"tenant-real"}}.Encode())
	if missingUser.Code != http.StatusBadRequest {
		t.Fatalf("missing account status=%d, want 400", missingUser.Code)
	}
	unknownRole := serveHandler(t, admin, http.MethodPost, "/admin/accounts",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-pending"}, "tenant_id": {"tenant-real"}, "role": {"platform_admin"}}.Encode())
	if unknownRole.Code != http.StatusBadRequest {
		t.Fatalf("unknown role status=%d, want 400", unknownRole.Code)
	}
	// A company administrator without a company would gain a role it cannot use.
	adminWithoutCompany := serveHandler(t, admin, http.MethodPost, "/admin/accounts",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-pending"}, "role": {"tenant_admin"}}.Encode())
	if adminWithoutCompany.Code != http.StatusBadRequest {
		t.Fatalf("company-less administrator status=%d, want 400", adminWithoutCompany.Code)
	}
	if actions.assignCalls != 0 {
		t.Fatalf("assignCalls=%d for rejected requests", actions.assignCalls)
	}
}

func TestAdminRegistersCompanyWithoutInvitation(t *testing.T) {
	admin := RequestContext{UserID: "user-admin", UserName: "관리자", Role: "platform_admin", CSRFToken: "token-123"}
	actions := &recordingActions{}
	handler, _ := signupPageHandler(t, actions, admin)

	page := serveHandler(t, handler, http.MethodGet, "/admin", "")
	for _, want := range []string{"회사 등록", `action="/admin/tenants"`, `name="tenant_name"`, `name="contact_email"`, `name="admin_name"`, `name="admin_email"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("admin page missing %q", want)
		}
	}
	if strings.Contains(page.Body.String(), "초대") {
		t.Fatal("company registration must not mention invitations")
	}

	form := url.Values{
		"_csrf": {"token-123"}, "tenant_name": {" FM "}, "contact_email": {"sample@example.com"},
		"admin_name": {"FM"}, "admin_email": {"sample@example.com"},
	}
	created := serveHandler(t, handler, http.MethodPost, "/admin/tenants", form.Encode())

	if created.Code != http.StatusSeeOther || created.Header().Get("Location") != "/admin?result=tenant-created" {
		t.Fatalf("status=%d location=%q body=%q", created.Code, created.Header().Get("Location"), created.Body.String())
	}
	want := TenantCommand{Name: "FM", ContactEmail: "sample@example.com", AdminName: "FM", AdminEmail: "sample@example.com"}
	if actions.lastTenant != want || actions.tenantCalls != 1 {
		t.Fatalf("tenant command = %+v calls=%d", actions.lastTenant, actions.tenantCalls)
	}
}

func TestCompanyRegistrationRejectsBadInputAndDuplicates(t *testing.T) {
	admin := RequestContext{UserID: "user-admin", Role: "platform_admin", CSRFToken: "token-123"}
	valid := url.Values{
		"_csrf": {"token-123"}, "tenant_name": {"FM"}, "contact_email": {"sample@example.com"},
		"admin_name": {"FM"}, "admin_email": {"sample@example.com"},
	}
	for name, mutate := range map[string]func(url.Values){
		"no name":          func(form url.Values) { form.Set("tenant_name", "  ") },
		"no admin name":    func(form url.Values) { form.Set("admin_name", "") },
		"bad contact":      func(form url.Values) { form.Set("contact_email", "sample@@example.com") },
		"named admin mail": func(form url.Values) { form.Set("admin_email", "FM <sample@example.com>") },
	} {
		actions := &recordingActions{}
		handler, _ := signupPageHandler(t, actions, admin)
		form := url.Values{}
		for key, values := range valid {
			form[key] = append([]string(nil), values...)
		}
		mutate(form)

		response := serveHandler(t, handler, http.MethodPost, "/admin/tenants", form.Encode())

		if response.Code != http.StatusBadRequest || actions.tenantCalls != 0 {
			t.Fatalf("%s: status=%d calls=%d", name, response.Code, actions.tenantCalls)
		}
	}

	duplicate := &recordingActions{tenantErr: ErrTenantExists}
	handler, _ := signupPageHandler(t, duplicate, admin)
	response := serveHandler(t, handler, http.MethodPost, "/admin/tenants", valid.Encode())
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d, want 409", response.Code)
	}

	member, _ := signupPageHandler(t, &recordingActions{}, RequestContext{UserID: "user-member", TenantID: "tenant-real", Role: "tenant_admin", CSRFToken: "token-123"})
	forbidden := serveHandler(t, member, http.MethodPost, "/admin/tenants", valid.Encode())
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("tenant admin status=%d, want 403", forbidden.Code)
	}
}

func TestSettingsListsMembersWithoutInvitation(t *testing.T) {
	tenantAdmin := RequestContext{UserID: "user-self", UserName: "관리자", TenantID: "tenant-real", TenantName: "실제 테넌트", Role: "tenant_admin", CSRFToken: "token-123"}
	actions := &recordingActions{}
	handler, backend := signupPageHandler(t, actions, tenantAdmin)
	backend.data.Members = []MemberView{
		{UserID: "user-self", Name: "관리자", Email: "admin@example.com", Role: "회사 관리자"},
		{UserID: "user-other", Name: "담당자", Email: "member@example.com", Role: "일반 사용자"},
	}

	page := serveHandler(t, handler, http.MethodGet, "/settings", "")

	if page.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", page.Code, page.Body.String())
	}
	body := page.Body.String()
	if strings.Contains(body, "/settings/invitations") || strings.Contains(body, "구성원 초대") {
		t.Fatal("settings still offers the removed invitation form")
	}
	for _, want := range []string{
		`action="/settings/members"`,
		`name="user_id" value="user-other"`,
		"회사에서 제외",
		"본인 계정",
		"data-confirm=",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page missing %q", want)
		}
	}
	// The caller must not be offered a control that removes itself.
	if strings.Contains(body, `name="user_id" value="user-self"`) {
		t.Fatal("settings offers self-removal")
	}
}

func TestRemoveMemberRequiresCompanyAdminAndOtherTarget(t *testing.T) {
	tenantAdmin := RequestContext{UserID: "user-self", TenantID: "tenant-real", Role: "tenant_admin", CSRFToken: "token-123"}
	actions := &recordingActions{}
	handler, _ := signupPageHandler(t, actions, tenantAdmin)

	removed := serveHandler(t, handler, http.MethodPost, "/settings/members",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-other"}}.Encode())
	if removed.Code != http.StatusSeeOther || removed.Header().Get("Location") != "/settings?result=member-removed" {
		t.Fatalf("status=%d location=%q body=%q", removed.Code, removed.Header().Get("Location"), removed.Body.String())
	}
	if actions.removeCalls != 1 || actions.lastRemove != (AccountCommand{UserID: "user-other"}) {
		t.Fatalf("remove command = %+v calls=%d", actions.lastRemove, actions.removeCalls)
	}

	self := serveHandler(t, handler, http.MethodPost, "/settings/members",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-self"}}.Encode())
	if self.Code != http.StatusBadRequest {
		t.Fatalf("self removal status=%d, want 400", self.Code)
	}
	noTarget := serveHandler(t, handler, http.MethodPost, "/settings/members",
		url.Values{"_csrf": {"token-123"}}.Encode())
	if noTarget.Code != http.StatusBadRequest {
		t.Fatalf("missing target status=%d, want 400", noTarget.Code)
	}
	badCSRF := serveHandler(t, handler, http.MethodPost, "/settings/members",
		url.Values{"user_id": {"user-other"}}.Encode())
	if badCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d, want 403", badCSRF.Code)
	}

	member, _ := signupPageHandler(t, actions, RequestContext{UserID: "user-plain", TenantID: "tenant-real", Role: "member", CSRFToken: "token-123"})
	forbidden := serveHandler(t, member, http.MethodPost, "/settings/members",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-other"}}.Encode())
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("member status=%d, want 403", forbidden.Code)
	}
	if actions.removeCalls != 1 {
		t.Fatalf("removeCalls=%d, want only the permitted removal", actions.removeCalls)
	}
}

func TestDeleteAccountIsPlatformAdminOnlyAndNeverSelf(t *testing.T) {
	admin := RequestContext{UserID: "user-admin", Role: "platform_admin", CSRFToken: "token-123"}
	actions := &recordingActions{}
	handler, _ := signupPageHandler(t, actions, admin)

	page := serveHandler(t, handler, http.MethodGet, "/admin", "")
	for _, want := range []string{`action="/admin/accounts/delete"`, "계정 삭제", "data-confirm="} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("admin page missing %q", want)
		}
	}

	deleted := serveHandler(t, handler, http.MethodPost, "/admin/accounts/delete",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-pending"}}.Encode())
	if deleted.Code != http.StatusSeeOther || deleted.Header().Get("Location") != "/admin?result=account-deleted" {
		t.Fatalf("status=%d location=%q body=%q", deleted.Code, deleted.Header().Get("Location"), deleted.Body.String())
	}
	if actions.deleteCalls != 1 || actions.lastDelete != (AccountCommand{UserID: "user-pending"}) {
		t.Fatalf("delete command = %+v calls=%d", actions.lastDelete, actions.deleteCalls)
	}

	self := serveHandler(t, handler, http.MethodPost, "/admin/accounts/delete",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-admin"}}.Encode())
	if self.Code != http.StatusBadRequest {
		t.Fatalf("self deletion status=%d, want 400", self.Code)
	}

	tenantAdmin, _ := signupPageHandler(t, actions, RequestContext{UserID: "user-company", TenantID: "tenant-real", Role: "tenant_admin", CSRFToken: "token-123"})
	forbidden := serveHandler(t, tenantAdmin, http.MethodPost, "/admin/accounts/delete",
		url.Values{"_csrf": {"token-123"}, "user_id": {"user-pending"}}.Encode())
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("company admin status=%d, want 403", forbidden.Code)
	}
	if actions.deleteCalls != 1 {
		t.Fatalf("deleteCalls=%d, want only the permitted deletion", actions.deleteCalls)
	}
}
