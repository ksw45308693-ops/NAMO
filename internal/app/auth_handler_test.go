package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"namo/internal/auth"
)

type identityRepoStub struct {
	account          LoginAccount
	accountErr       error
	lookupIdentifier string
	sessions         map[string]SessionRecord
	sessionErr       error
	deleteErr        error
	created          SignupInput
	createCalls      int
	createErr        error
}

func (r *identityRepoStub) AccountByEmail(_ context.Context, identifier string) (LoginAccount, error) {
	r.lookupIdentifier = identifier
	return r.account, r.accountErr
}
func (r *identityRepoStub) CreateAccount(_ context.Context, input SignupInput) (LoginAccount, error) {
	r.createCalls++
	r.created = input
	if r.createErr != nil {
		return LoginAccount{}, r.createErr
	}
	return LoginAccount{UserID: "11111111-2222-3333-4444-555555555555", Email: input.Email, PasswordHash: input.PasswordHash, Role: auth.Member}, nil
}
func (r *identityRepoStub) SaveSession(_ context.Context, session SessionRecord) error {
	if r.sessions == nil {
		r.sessions = make(map[string]SessionRecord)
	}
	r.sessions[session.TokenHash] = session
	return nil
}
func (r *identityRepoStub) SessionByHash(_ context.Context, hash string) (SessionRecord, error) {
	if r.sessionErr != nil {
		return SessionRecord{}, r.sessionErr
	}
	session, ok := r.sessions[hash]
	if !ok {
		return SessionRecord{}, ErrUnauthenticated
	}
	return session, nil
}

func TestLogoutKeepsCookieWhenSessionLookupFails(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	cookieValue, _ := signedSessionValue(key, "lookup-failure-token")
	repo := &identityRepoStub{sessionErr: errors.New("database unavailable")}
	handler, err := NewAuthHandler(http.NotFoundHandler(), repo, key, true, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/logout", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookieValue})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", recorder.Code)
	}
	if recorder.Header().Get("Location") != "" {
		t.Fatalf("lookup failure redirected to %q", recorder.Header().Get("Location"))
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookieName && cookie.MaxAge < 0 {
			t.Fatal("session cookie was cleared after a transient lookup failure")
		}
	}
}
func (r *identityRepoStub) DeleteSession(_ context.Context, hash string) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	delete(r.sessions, hash)
	return nil
}

func TestLogoutKeepsCookieWhenSessionRevocationFails(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	key := []byte("01234567890123456789012345678901")
	cookieValue, tokenHash := signedSessionValue(key, "active-token")
	repo := &identityRepoStub{deleteErr: errors.New("database unavailable"), sessions: map[string]SessionRecord{
		tokenHash: {TokenHash: tokenHash, ExpiresAt: now.Add(time.Hour)},
	}}
	handler, err := NewAuthHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("failed logout reached UI")
	}), repo, key, true, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := csrfToken(key, "active-token")
	request := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(url.Values{"_csrf": {token}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookieValue})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("logout Cache-Control = %q", got)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == SessionCookieName && cookie.MaxAge < 0 {
			t.Fatal("session cookie was cleared although server-side revocation failed")
		}
	}
}

func TestLoginUsesDummyBcryptForUnknownAccount(t *testing.T) {
	if cost, err := bcrypt.Cost([]byte(dummyLoginPasswordHash)); err != nil || cost != bcrypt.DefaultCost {
		t.Fatalf("dummy bcrypt hash cost=%d err=%v", cost, err)
	}
	repo := &identityRepoStub{accountErr: ErrUnauthenticated}
	handler, err := NewAuthHandler(http.NotFoundHandler(), repo, []byte("01234567890123456789012345678901"), true, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	authenticated := handler.(*authHandler)
	checks := 0
	authenticated.checkPassword = func(hash, password string) bool {
		checks++
		if hash != dummyLoginPasswordHash || password != "guess" {
			t.Fatalf("hash=%q password=%q", hash, password)
		}
		return false
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", nil))
	var cookie *http.Cookie
	for _, candidate := range page.Result().Cookies() {
		if candidate.Name == LoginCSRFCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("login CSRF cookie missing")
	}
	rawToken, ok := authenticated.loginCSRFToken(requestWithCookie(cookie))
	if !ok {
		t.Fatal("login CSRF token could not be decoded")
	}
	form := url.Values{"email": {"missing@example.com"}, "password": {"guess"}, "_csrf": {rawToken}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || checks != 1 {
		t.Fatalf("status=%d checks=%d body=%q", response.Code, checks, response.Body.String())
	}
}

func TestLoginRejectsInvalidIdentifierBeforeAuthentication(t *testing.T) {
	passwordHash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	repo := &identityRepoStub{account: LoginAccount{
		UserID: "admin-1", Email: "admin", PasswordHash: passwordHash, Role: auth.PlatformAdmin,
	}}
	handler, err := NewAuthHandler(http.NotFoundHandler(), repo, []byte("01234567890123456789012345678901"), true, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", nil))
	var cookie *http.Cookie
	for _, candidate := range page.Result().Cookies() {
		if candidate.Name == LoginCSRFCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("login CSRF cookie missing")
	}
	token, ok := handler.(*authHandler).loginCSRFToken(requestWithCookie(cookie))
	if !ok {
		t.Fatal("login CSRF token could not be decoded")
	}
	form := url.Values{"email": {"admin name"}, "password": {"correct horse battery staple"}, "_csrf": {token}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", response.Code)
	}
}

func TestLoginAcceptsNormalizedPlatformUsername(t *testing.T) {
	passwordHash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	repo := &identityRepoStub{account: LoginAccount{
		UserID: "admin-1", Email: "admin", PasswordHash: passwordHash, Role: auth.PlatformAdmin,
	}}
	handler, err := NewAuthHandler(http.NotFoundHandler(), repo, []byte("01234567890123456789012345678901"), true, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", nil))
	var cookie *http.Cookie
	for _, candidate := range page.Result().Cookies() {
		if candidate.Name == LoginCSRFCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("login CSRF cookie missing")
	}
	token, ok := handler.(*authHandler).loginCSRFToken(requestWithCookie(cookie))
	if !ok {
		t.Fatal("login CSRF token could not be decoded")
	}
	form := url.Values{"email": {" Admin "}, "password": {"correct horse battery staple"}, "_csrf": {token}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || repo.lookupIdentifier != "admin" || len(repo.sessions) != 1 {
		t.Fatalf("status=%d identifier=%q sessions=%d", response.Code, repo.lookupIdentifier, len(repo.sessions))
	}
}

func TestNormalizeLoginIdentifierAcceptsEmailAndPlatformUsername(t *testing.T) {
	tests := []struct {
		raw, want string
		valid     bool
	}{
		{raw: " Admin ", want: "admin", valid: true},
		{raw: " User@Example.COM ", want: "user@example.com", valid: true},
		{raw: "ab", valid: false},
		{raw: "admin name", valid: false},
		{raw: "admin@example", want: "admin@example", valid: true},
		{raw: "admin@ example", valid: false},
	}
	for _, tt := range tests {
		got, err := normalizeLoginIdentifier(tt.raw)
		if tt.valid && (err != nil || got != tt.want) {
			t.Fatalf("normalizeLoginIdentifier(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
		}
		if !tt.valid && err == nil {
			t.Fatalf("normalizeLoginIdentifier(%q) = %q; want error", tt.raw, got)
		}
	}
}

func requestWithCookie(cookie *http.Cookie) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/login", nil)
	request.AddCookie(cookie)
	return request
}

func TestAuthHandlerLoginProtectionRoleAndCSRF(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	passwordHash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	repo := &identityRepoStub{account: LoginAccount{
		UserID: "user-1", TenantID: "tenant-1", Email: "member@example.com",
		PasswordHash: passwordHash, Role: auth.Member,
	}}
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ui:" + r.URL.Path + ":" + CSRFTokenFromContext(r.Context())))
	})
	handler, err := NewAuthHandler(ui, repo, []byte("01234567890123456789012345678901"), true, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	protected := httptest.NewRecorder()
	handler.ServeHTTP(protected, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if protected.Code != http.StatusSeeOther || protected.Header().Get("Location") != "/login" {
		t.Fatalf("protected status=%d location=%q", protected.Code, protected.Header().Get("Location"))
	}

	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "/login", nil))
	loginToken := strings.TrimPrefix(loginPage.Body.String(), "ui:/login:")
	var loginCSRFCookie *http.Cookie
	for _, cookie := range loginPage.Result().Cookies() {
		if cookie.Name == LoginCSRFCookieName {
			loginCSRFCookie = cookie
		}
	}
	if loginToken == "" || loginCSRFCookie == nil || !loginCSRFCookie.HttpOnly {
		t.Fatalf("login csrf token=%q cookies=%+v", loginToken, loginPage.Result().Cookies())
	}

	rejectedLogin := httptest.NewRecorder()
	rejectedRequest := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=member%40example.com&password=correct"))
	rejectedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rejectedRequest.AddCookie(loginCSRFCookie)
	handler.ServeHTTP(rejectedLogin, rejectedRequest)
	if rejectedLogin.Code != http.StatusForbidden {
		t.Fatalf("login without csrf status=%d, want 403", rejectedLogin.Code)
	}

	form := url.Values{"email": {"member@example.com"}, "password": {"correct horse battery staple"}, "_csrf": {loginToken}}
	login := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(loginCSRFCookie)
	handler.ServeHTTP(login, request)
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != "/dashboard" {
		t.Fatalf("login status=%d location=%q body=%q", login.Code, login.Header().Get("Location"), login.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == SessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookies = %+v", login.Result().Cookies())
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dashboardRequest.AddCookie(sessionCookie)
	dashboard := httptest.NewRecorder()
	handler.ServeHTTP(dashboard, dashboardRequest)
	if dashboard.Code != http.StatusOK || !strings.HasPrefix(dashboard.Body.String(), "ui:/dashboard:") {
		t.Fatalf("dashboard status=%d body=%q", dashboard.Code, dashboard.Body.String())
	}
	if got := dashboard.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("dashboard Cache-Control = %q", got)
	}
	csrf := strings.TrimPrefix(dashboard.Body.String(), "ui:/dashboard:")
	if csrf == "" {
		t.Fatal("authenticated request did not receive a CSRF token")
	}

	adminRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminRequest.AddCookie(sessionCookie)
	admin := httptest.NewRecorder()
	handler.ServeHTTP(admin, adminRequest)
	if admin.Code != http.StatusForbidden {
		t.Fatalf("member admin status=%d, want 403", admin.Code)
	}

	unsafe := httptest.NewRequest(http.MethodPost, "/filters", strings.NewReader("_csrf=wrong"))
	unsafe.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	unsafe.AddCookie(sessionCookie)
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, unsafe)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("invalid CSRF status=%d, want 403", rejected.Code)
	}

	valid := httptest.NewRequest(http.MethodPost, "/filters", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
	valid.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	valid.AddCookie(sessionCookie)
	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, valid)
	if accepted.Code != http.StatusOK || !strings.HasPrefix(accepted.Body.String(), "ui:/filters:") {
		t.Fatalf("valid CSRF status=%d body=%q", accepted.Code, accepted.Body.String())
	}
}

func TestAuthHandlerRejectsExpiredSession(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	repo := &identityRepoStub{sessions: make(map[string]SessionRecord)}
	handler, err := NewAuthHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("expired session reached UI")
	}), repo, []byte("01234567890123456789012345678901"), true, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	cookieValue, tokenHash := signedSessionValue([]byte("01234567890123456789012345678901"), "expired-token")
	repo.sessions[tokenHash] = SessionRecord{TokenHash: tokenHash, ExpiresAt: now}
	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookieValue})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("expired status=%d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestAuthHandlerProtectsPublicInvitationAcceptanceWithAnonymousCSRF(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	handler, err := NewAuthHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ui:" + r.URL.Path + ":" + CSRFTokenFromContext(r.Context())))
	}), &identityRepoStub{}, []byte("01234567890123456789012345678901"), true, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/accept-invite?token=opaque", nil))
	csrf := strings.TrimPrefix(page.Body.String(), "ui:/accept-invite:")
	var csrfCookie *http.Cookie
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == InviteCSRFCookieName {
			csrfCookie = cookie
		}
	}
	if page.Code != http.StatusOK || csrf == "" || csrfCookie == nil || !csrfCookie.HttpOnly || !csrfCookie.Secure || csrfCookie.Path != "/accept-invite" {
		t.Fatalf("page=%d csrf=%q cookies=%+v", page.Code, csrf, page.Result().Cookies())
	}

	rejected := httptest.NewRecorder()
	rejectedRequest := httptest.NewRequest(http.MethodPost, "/accept-invite", strings.NewReader("_csrf=wrong"))
	rejectedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rejectedRequest.AddCookie(csrfCookie)
	handler.ServeHTTP(rejected, rejectedRequest)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("invalid invitation CSRF status=%d", rejected.Code)
	}

	accepted := httptest.NewRecorder()
	acceptedRequest := httptest.NewRequest(http.MethodPost, "/accept-invite", strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()))
	acceptedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acceptedRequest.AddCookie(csrfCookie)
	handler.ServeHTTP(accepted, acceptedRequest)
	if accepted.Code != http.StatusOK || accepted.Body.String() != "ui:/accept-invite:"+csrf {
		t.Fatalf("valid invitation CSRF status=%d body=%q", accepted.Code, accepted.Body.String())
	}
}
