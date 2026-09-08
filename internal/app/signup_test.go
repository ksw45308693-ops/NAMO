package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"namo/internal/auth"
)

func signupHandler(t *testing.T, repo *identityRepoStub) (http.Handler, *authHandler) {
	t.Helper()
	handler, err := NewAuthHandler(http.NotFoundHandler(), repo, []byte("01234567890123456789012345678901"), true, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return handler, handler.(*authHandler)
}

// signupCSRF walks the anonymous page path so the test uses the same cookie the
// browser receives.
func signupCSRF(t *testing.T, handler http.Handler, authenticated *authHandler) (*http.Cookie, string) {
	t.Helper()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/signup", nil))
	if page.Code != http.StatusNotFound {
		t.Fatalf("signup page did not reach the UI handler: status=%d", page.Code)
	}
	var cookie *http.Cookie
	for _, candidate := range page.Result().Cookies() {
		if candidate.Name == SignupCSRFCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("signup CSRF cookie missing")
	}
	if cookie.Path != "/signup" || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("signup CSRF cookie = %+v", cookie)
	}
	request := httptest.NewRequest(http.MethodGet, "/signup", nil)
	request.AddCookie(cookie)
	token, ok := authenticated.anonymousCSRFToken(request, SignupCSRFCookieName, "signup-csrf")
	if !ok {
		t.Fatal("signup CSRF token could not be decoded")
	}
	return cookie, token
}

func postSignup(t *testing.T, handler http.Handler, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSignupCreatesAccountAndStartsSession(t *testing.T) {
	repo := &identityRepoStub{}
	handler, authenticated := signupHandler(t, repo)
	cookie, token := signupCSRF(t, handler, authenticated)

	response := postSignup(t, handler, cookie, url.Values{
		"email":            {" Member@Example.com "},
		"password":         {"correct horse battery"},
		"password_confirm": {"correct horse battery"},
		"_csrf":            {token},
	})

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/dashboard" {
		t.Fatalf("status=%d location=%q body=%q", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if repo.createCalls != 1 || repo.created.Email != "member@example.com" {
		t.Fatalf("createCalls=%d email=%q", repo.createCalls, repo.created.Email)
	}
	if !auth.CheckPassword(repo.created.PasswordHash, "correct horse battery") {
		t.Fatal("stored password hash does not verify the submitted password")
	}
	var session, csrf *http.Cookie
	for _, candidate := range response.Result().Cookies() {
		switch candidate.Name {
		case SessionCookieName:
			session = candidate
		case SignupCSRFCookieName:
			csrf = candidate
		}
	}
	if session == nil || session.Value == "" || !session.HttpOnly || !session.Secure {
		t.Fatalf("session cookie = %+v", session)
	}
	if len(repo.sessions) != 1 {
		t.Fatalf("stored sessions = %d, want the signup session", len(repo.sessions))
	}
	for _, record := range repo.sessions {
		if record.TenantID != "" || record.Role != auth.Member {
			t.Fatalf("session record = %+v, want a member without a tenant", record)
		}
	}
	if csrf == nil || csrf.MaxAge >= 0 {
		t.Fatalf("signup CSRF cookie was not cleared: %+v", csrf)
	}
}

func TestSignupRejectsWeakAndMismatchedPasswords(t *testing.T) {
	for name, form := range map[string]url.Values{
		"short":     {"email": {"member@example.com"}, "password": {"short"}, "password_confirm": {"short"}},
		"long":      {"email": {"member@example.com"}, "password": {strings.Repeat("a", 73)}, "password_confirm": {strings.Repeat("a", 73)}},
		"mismatch":  {"email": {"member@example.com"}, "password": {"correct horse battery"}, "password_confirm": {"correct horse batteryy"}},
		"bad email": {"email": {"member@@example.com"}, "password": {"correct horse battery"}, "password_confirm": {"correct horse battery"}},
	} {
		repo := &identityRepoStub{}
		handler, authenticated := signupHandler(t, repo)
		cookie, token := signupCSRF(t, handler, authenticated)
		form.Set("_csrf", token)

		response := postSignup(t, handler, cookie, form)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", name, response.Code)
		}
		if repo.createCalls != 0 {
			t.Fatalf("%s: repository was called %d times", name, repo.createCalls)
		}
	}
}

func TestSignupRejectsMissingCSRF(t *testing.T) {
	repo := &identityRepoStub{}
	handler, authenticated := signupHandler(t, repo)
	cookie, token := signupCSRF(t, handler, authenticated)
	form := url.Values{
		"email": {"member@example.com"}, "password": {"correct horse battery"},
		"password_confirm": {"correct horse battery"},
	}

	withoutCookie := postSignup(t, handler, nil, cloneValues(form, token))
	withoutToken := postSignup(t, handler, cookie, form)

	if withoutCookie.Code != http.StatusForbidden || withoutToken.Code != http.StatusForbidden {
		t.Fatalf("cookie-less=%d token-less=%d, want 403", withoutCookie.Code, withoutToken.Code)
	}
	if repo.createCalls != 0 {
		t.Fatalf("repository was called %d times", repo.createCalls)
	}
}

func TestSignupReportsTakenEmailAndPendingInvitation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{"registered", ErrEmailRegistered},
		{"invited", ErrInvitationWaits},
	} {
		repo := &identityRepoStub{createErr: testCase.err}
		handler, authenticated := signupHandler(t, repo)
		cookie, token := signupCSRF(t, handler, authenticated)

		response := postSignup(t, handler, cookie, url.Values{
			"email": {"member@example.com"}, "password": {"correct horse battery"},
			"password_confirm": {"correct horse battery"}, "_csrf": {token},
		})

		if response.Code != http.StatusConflict {
			t.Fatalf("%s: status=%d, want 409", testCase.name, response.Code)
		}
		if len(repo.sessions) != 0 {
			t.Fatalf("%s: a session was created for a rejected signup", testCase.name)
		}
	}
}

func TestSignupRejectsUnsupportedMethod(t *testing.T) {
	handler, _ := signupHandler(t, &identityRepoStub{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/signup", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Fatalf("status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}

func cloneValues(form url.Values, token string) url.Values {
	clone := url.Values{}
	for key, values := range form {
		clone[key] = append([]string(nil), values...)
	}
	clone.Set("_csrf", token)
	return clone
}
