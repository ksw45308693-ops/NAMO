package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"namo/internal/auth"
)

const (
	SessionCookieName    = "namo_session"
	LoginCSRFCookieName  = "namo_login_csrf"
	SignupCSRFCookieName = "namo_signup_csrf"
	InviteCSRFCookieName = "namo_invite_csrf"
	// A fixed default-cost hash keeps unknown-account and invalid-role login
	// attempts on the same bcrypt path as a normal failed password.
	dummyLoginPasswordHash = "$2a$10$C6UzMDM.H6dfI/f/IKcEe.ko4ZJzV3BLH5Kf.Z8LFehYKmX3G/6y"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type LoginAccount struct {
	UserID, TenantID, Email, PasswordHash string
	Role                                  auth.Role
}

type SessionRecord struct {
	UserID, TenantID, Email, TokenHash string
	Role                               auth.Role
	ExpiresAt                          time.Time
}

type IdentityRepository interface {
	AccountByEmail(context.Context, string) (LoginAccount, error)
	CreateAccount(context.Context, SignupInput) (LoginAccount, error)
	SaveSession(context.Context, SessionRecord) error
	SessionByHash(context.Context, string) (SessionRecord, error)
	DeleteSession(context.Context, string) error
}

type authHandler struct {
	next          http.Handler
	repository    IdentityRepository
	key           []byte
	secure        bool
	now           func() time.Time
	checkPassword func(string, string) bool
}

type authContextKey int

const (
	principalContextKey authContextKey = iota
	csrfContextKey
)

func NewAuthHandler(next http.Handler, repository IdentityRepository, sessionKey []byte, secure bool, now func() time.Time) (http.Handler, error) {
	if next == nil || repository == nil {
		return nil, errors.New("auth handler dependencies are required")
	}
	if len(sessionKey) < 32 {
		return nil, errors.New("session key must contain at least 32 bytes")
	}
	if now == nil {
		now = time.Now
	}
	return &authHandler{
		next: next, repository: repository, key: append([]byte(nil), sessionKey...), secure: secure, now: now,
		checkPassword: auth.CheckPassword,
	}, nil
}

func (h *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		h.next.ServeHTTP(w, r)
		return
	}
	// Dynamic responses can contain tenant data or one-time form tokens. Keep
	// them out of browser back/forward caches and intermediary caches.
	w.Header().Set("Cache-Control", "private, no-store")
	if r.URL.Path == "/login" {
		if r.Method == http.MethodPost {
			h.login(w, r)
			return
		}
		h.loginPage(w, r)
		return
	}
	if r.URL.Path == "/signup" {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.anonymousPage(w, r, SignupCSRFCookieName, "signup-csrf", "/signup")
		case http.MethodPost:
			h.signup(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD, POST")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		}
		return
	}
	if r.URL.Path == "/accept-invite" {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.anonymousPage(w, r, InviteCSRFCookieName, "invite-csrf", "/accept-invite")
		case http.MethodPost:
			h.invitationPost(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD, POST")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		}
		return
	}
	token, record, ok, authErr := h.authenticate(r)
	if authErr != nil {
		http.Error(w, "세션을 확인하지 못했습니다. 잠시 후 다시 시도해 주세요.", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		h.clearCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.URL.Path == "/admin" && record.Role != auth.PlatformAdmin {
		http.Error(w, "접근 권한이 없습니다.", http.StatusForbidden)
		return
	}
	csrf := csrfToken(h.key, token)
	if unsafeMethod(r.Method) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil || !constantEqual(r.Form.Get("_csrf"), csrf) {
			http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
			return
		}
	}
	if r.URL.Path == "/logout" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		if err := h.repository.DeleteSession(r.Context(), record.TokenHash); err != nil {
			http.Error(w, "로그아웃을 완료하지 못했습니다. 잠시 후 다시 시도해 주세요.", http.StatusServiceUnavailable)
			return
		}
		h.clearCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := context.WithValue(r.Context(), principalContextKey, record)
	ctx = context.WithValue(ctx, csrfContextKey, csrf)
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

func (h *authHandler) loginPage(w http.ResponseWriter, r *http.Request) {
	h.anonymousPage(w, r, LoginCSRFCookieName, "login-csrf", "/login")
}

func (h *authHandler) anonymousPage(w http.ResponseWriter, r *http.Request, cookieName, scope, path string) {
	token, ok := h.anonymousCSRFToken(r, cookieName, scope)
	if !ok {
		var err error
		token, err = auth.NewCSRFToken()
		if err != nil {
			http.Error(w, "로그인 화면을 준비하지 못했습니다.", http.StatusInternalServerError)
			return
		}
		value := token + "." + sign(h.key, scope+"\x00"+token)
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: value, Path: path, Expires: h.now().Add(time.Hour),
			MaxAge: int(time.Hour.Seconds()), HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
		})
	}
	ctx := context.WithValue(r.Context(), csrfContextKey, token)
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

func (h *authHandler) invitationPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "초대 정보를 확인해 주세요.", http.StatusBadRequest)
		return
	}
	token, ok := h.anonymousCSRFToken(r, InviteCSRFCookieName, "invite-csrf")
	if !ok || !constantEqual(r.Form.Get("_csrf"), token) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	ctx := context.WithValue(r.Context(), csrfContextKey, token)
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "로그인 정보를 확인해 주세요.", http.StatusBadRequest)
		return
	}
	token, ok := h.loginCSRFToken(r)
	if !ok || !constantEqual(r.Form.Get("_csrf"), token) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	identifier, identifierErr := normalizeLoginIdentifier(r.Form.Get("email"))
	if identifierErr != nil {
		identifier = ""
	}
	account, err := h.repository.AccountByEmail(r.Context(), identifier)
	accountValid := identifierErr == nil && err == nil && account.Role.Valid()
	passwordHash := dummyLoginPasswordHash
	if accountValid {
		passwordHash = account.PasswordHash
	}
	passwordValid := h.checkPassword(passwordHash, r.Form.Get("password"))
	if !accountValid || !passwordValid {
		http.Error(w, "이메일 또는 아이디와 비밀번호가 올바르지 않습니다.", http.StatusUnauthorized)
		return
	}
	h.startSession(w, r, account, LoginCSRFCookieName, "/login")
}

// signup creates a self-service account and starts its session, so a new user
// reaches the service without a second credential prompt.
func (h *authHandler) signup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "가입 정보를 확인해 주세요.", http.StatusBadRequest)
		return
	}
	token, ok := h.anonymousCSRFToken(r, SignupCSRFCookieName, "signup-csrf")
	if !ok || !constantEqual(r.Form.Get("_csrf"), token) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	email, err := normalizeMailbox(r.Form.Get("email"))
	if err != nil || len([]byte(email)) > 320 {
		http.Error(w, "이메일 주소를 확인해 주세요.", http.StatusBadRequest)
		return
	}
	password := r.Form.Get("password")
	if !validSignupPassword(password) {
		http.Error(w, "비밀번호는 UTF-8 기준 12~72바이트로 입력해 주세요.", http.StatusBadRequest)
		return
	}
	if password != r.Form.Get("password_confirm") {
		http.Error(w, "비밀번호 확인이 일치하지 않습니다.", http.StatusBadRequest)
		return
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		http.Error(w, "가입을 완료하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	account, err := h.repository.CreateAccount(r.Context(), SignupInput{Email: email, PasswordHash: passwordHash})
	if err != nil {
		switch {
		case errors.Is(err, ErrEmailRegistered):
			http.Error(w, "이미 등록된 이메일입니다. 로그인 화면을 이용해 주세요.", http.StatusConflict)
		case errors.Is(err, ErrInvitationWaits):
			http.Error(w, "이 이메일에는 초대가 진행 중입니다. 초대 메일의 링크로 계정을 만들어 주세요.", http.StatusConflict)
		default:
			http.Error(w, "가입을 완료하지 못했습니다.", http.StatusInternalServerError)
		}
		return
	}
	h.startSession(w, r, account, SignupCSRFCookieName, "/signup")
}

func (h *authHandler) startSession(w http.ResponseWriter, r *http.Request, account LoginAccount, csrfCookieName, csrfCookiePath string) {
	session, err := auth.NewSession(account.UserID, h.now(), 12*time.Hour)
	if err != nil {
		http.Error(w, "로그인을 시작하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	record := SessionRecord{
		UserID: account.UserID, TenantID: account.TenantID, Email: account.Email,
		Role: account.Role, TokenHash: session.TokenHash, ExpiresAt: session.ExpiresAt,
	}
	if err := h.repository.SaveSession(r.Context(), record); err != nil {
		http.Error(w, "로그인을 시작하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	value, _ := signedSessionValue(h.key, session.Token)
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: value, Path: "/", Expires: session.ExpiresAt,
		MaxAge: int((12 * time.Hour).Seconds()), HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: "", Path: csrfCookiePath, MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *authHandler) loginCSRFToken(r *http.Request) (string, bool) {
	return h.anonymousCSRFToken(r, LoginCSRFCookieName, "login-csrf")
}

func (h *authHandler) anonymousCSRFToken(r *http.Request, cookieName, scope string) (string, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	separator := strings.LastIndexByte(cookie.Value, '.')
	if separator <= 0 || separator == len(cookie.Value)-1 {
		return "", false
	}
	token, signature := cookie.Value[:separator], cookie.Value[separator+1:]
	return token, constantEqual(signature, sign(h.key, scope+"\x00"+token))
}

func (h *authHandler) authenticate(r *http.Request) (string, SessionRecord, bool, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return "", SessionRecord{}, false, nil
	}
	token, ok := verifySignedSession(h.key, cookie.Value)
	if !ok {
		return "", SessionRecord{}, false, nil
	}
	hash := auth.HashSessionToken(token)
	record, err := h.repository.SessionByHash(r.Context(), hash)
	if errors.Is(err, ErrUnauthenticated) {
		return "", SessionRecord{}, false, nil
	}
	if err != nil {
		return "", SessionRecord{}, false, err
	}
	if record.TokenHash != hash || !h.now().Before(record.ExpiresAt) {
		return "", SessionRecord{}, false, nil
	}
	return token, record, true, nil
}

func (h *authHandler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
}

func PrincipalFromContext(ctx context.Context) (SessionRecord, bool) {
	principal, ok := ctx.Value(principalContextKey).(SessionRecord)
	return principal, ok
}

func CSRFTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(csrfContextKey).(string)
	return token
}

func signedSessionValue(key []byte, token string) (string, string) {
	return token + "." + sign(key, "session\x00"+token), auth.HashSessionToken(token)
}

func verifySignedSession(key []byte, value string) (string, bool) {
	separator := strings.LastIndexByte(value, '.')
	if separator <= 0 || separator == len(value)-1 {
		return "", false
	}
	token, signature := value[:separator], value[separator+1:]
	return token, constantEqual(signature, sign(key, "session\x00"+token))
}

func csrfToken(key []byte, token string) string { return sign(key, "csrf\x00"+token) }

func sign(key []byte, value string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func constantEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func unsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}
