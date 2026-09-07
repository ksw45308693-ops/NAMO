package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"namo/internal/config"
)

func TestAPIKeyFormPersistsSecretWithAdminAndCSRFGuards(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := config.APIKeyStore{Path: filepath.Join(dir, "key")}
	identity := RequestContext{UserID: "admin", TenantID: "tenant", Role: "platform_admin", CSRFToken: "token-123"}
	backend := &staticBackend{data: AppData{}}
	handler, err := NewHandlerWithOptions(Options{
		Backend: backend, Actions: &recordingActions{},
		MapContext: func(*http.Request) (RequestContext, error) { return identity, nil },
		SaveAPIKey: func(_ context.Context, _ RequestContext, key string) error { return store.Save(key) },
	})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"_csrf": {"token-123"}, "api_key": {"new+/=secret"}}.Encode()
	for _, tt := range []struct {
		role, user, method, body string
		code                     int
	}{
		{"tenant_admin", "tenant", "POST", form, 403},
		{"member", "member", "POST", form, 403},
		{"platform_admin", "", "POST", form, 403},
		{"platform_admin", "admin", "GET", "", 405},
		{"platform_admin", "admin", "POST", "api_key=new-secret", 403},
		{"platform_admin", "admin", "POST", "_csrf=token-123&api_key=bad%25GGsecret", 400},
		{"platform_admin", "admin", "POST", "_csrf=token-123&api_key=" + strings.Repeat("a", 20000), 413},
		{"platform_admin", "admin", "POST", form, 303},
	} {
		identity.Role, identity.UserID = tt.role, tt.user
		response := serveHandler(t, handler, tt.method, "/settings/g2b-api-key", tt.body)
		if response.Code != tt.code {
			t.Fatalf("%s/%s: got %d want %d: %s", tt.role, tt.method, response.Code, tt.code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret") {
			t.Fatal("secret reflected in response")
		}
	}
	if key, err := store.Read(); err != nil || key != "new+/=secret" {
		t.Fatal("form did not persist decoded key")
	}
	for _, input := range []string{"new%2B%2F%3Dsecret", "new+%2F%3Dsecret"} {
		body := url.Values{"_csrf": {"token-123"}, "api_key": {input}}.Encode()
		response := serveHandler(t, handler, "POST", "/settings/g2b-api-key", body)
		if response.Code != 303 {
			t.Fatalf("encoded portal key rejected: %d", response.Code)
		}
		if key, err := store.Read(); err != nil || key != "new+/=secret" {
			t.Fatal("encoded key was changed or decoded twice")
		}
	}
	for _, invalid := range []string{"bad%252Bsecret", "bad%20secret", "bad%secret"} {
		body := url.Values{"_csrf": {"token-123"}, "api_key": {invalid}}.Encode()
		response := serveHandler(t, handler, "POST", "/settings/g2b-api-key", body)
		if response.Code != 400 || strings.Contains(response.Body.String(), "secret") {
			t.Fatal("invalid key accepted or reflected")
		}
		if key, _ := store.Read(); key != "new+/=secret" {
			t.Fatal("invalid replacement lost previous key")
		}
	}
	backend.data.Admin.APIKeyConfigured = true
	page := serveHandler(t, handler, "GET", "/settings?result=g2b-key-saved", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `action="/settings/g2b-api-key"`) || !strings.Contains(page.Body.String(), "등록됨") {
		t.Fatal("missing administrator key settings")
	}
	if strings.Contains(page.Body.String(), "new+/=secret") || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("settings leaked or cached secret")
	}
	identity.Role, identity.TenantID = "tenant_admin", "tenant"
	if strings.Contains(serveHandler(t, handler, "GET", "/settings", "").Body.String(), `name="api_key"`) {
		t.Fatal("key form exposed to tenant admin")
	}
	if response := serveHandler(t, NewHandler(), "POST", "/settings/g2b-api-key", form); response.Code != 501 {
		t.Fatal("demo accepted key")
	}
	identity.Role = "platform_admin"
	request := httptest.NewRequest("POST", "/settings/g2b-api-key", strings.NewReader("_csrf=token-123&api_key="+strings.Repeat("a", 20000)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	} // Auth middleware parses first.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 413 {
		t.Fatalf("preparsed oversized form: got %d", response.Code)
	}
}
