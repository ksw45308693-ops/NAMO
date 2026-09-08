package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type recordingOnboarding struct {
	lookupCalls, acceptCalls int
	acceptCommand            AcceptInviteCommand
	invitation               InvitationView
	err                      error
}

func (o *recordingOnboarding) Invitation(context.Context, string) (InvitationView, error) {
	o.lookupCalls++
	return o.invitation, o.err
}

func (o *recordingOnboarding) AcceptInvitation(_ context.Context, command AcceptInviteCommand) error {
	o.acceptCalls++
	o.acceptCommand = command
	return o.err
}

func TestPublicInvitationPageAndAcceptance(t *testing.T) {
	token := strings.Repeat("a", 43)
	onboarding := &recordingOnboarding{invitation: InvitationView{
		TenantName: "초대 회사", Email: "member@example.com", DisplayName: "김담당", Role: "member",
		ExpiresAt: time.Date(2026, 9, 3, 7, 0, 0, 0, time.FixedZone("KST", 9*60*60)),
	}}
	handler := onboardingHandler(t, RequestContext{CSRFToken: "anonymous-token"}, onboarding)
	bootstrap := serveHandler(t, handler, http.MethodGet, "/accept-invite", "")
	if bootstrap.Code != http.StatusOK || !strings.Contains(bootstrap.Body.String(), `data-invite-token-form`) || !strings.Contains(bootstrap.Body.String(), `/assets/app.js?v=3`) || onboarding.lookupCalls != 0 {
		t.Fatalf("bootstrap status=%d lookup=%d body=%q", bootstrap.Code, onboarding.lookupCalls, bootstrap.Body.String())
	}
	page := serveHandler(t, handler, http.MethodPost, "/accept-invite", "_csrf=anonymous-token&mode=inspect&token="+token)
	if page.Header().Get("Referrer-Policy") != "no-referrer" || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("sensitive invitation headers = %v", page.Header())
	}
	for _, want := range []string{"초대 회사", "member@example.com", "김담당", `name="token" value="` + token + `"`, `name="_csrf" value="anonymous-token"`} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("page status=%d missing %q: %s", page.Code, want, page.Body.String())
		}
	}

	form := "_csrf=anonymous-token&mode=accept&token=" + token + "&display_name=새담당&password=correct-horse-123&password_confirm=correct-horse-123"
	accepted := serveHandler(t, handler, http.MethodPost, "/accept-invite", form)
	if accepted.Code != http.StatusSeeOther || accepted.Header().Get("Location") != "/login?accepted=1" || onboarding.acceptCalls != 1 {
		t.Fatalf("accepted status=%d location=%q calls=%d", accepted.Code, accepted.Header().Get("Location"), onboarding.acceptCalls)
	}
	if onboarding.acceptCommand.Token != token || onboarding.acceptCommand.Password != "correct-horse-123" {
		t.Fatalf("accept command=%#v", onboarding.acceptCommand)
	}
}

func TestInvitationExpiryRendersInSeoulWhenHostAndRecordAreUTC(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = originalLocal })

	token := strings.Repeat("a", 43)
	onboarding := &recordingOnboarding{invitation: InvitationView{
		TenantName: "초대 회사", Email: "member@example.com", DisplayName: "김담당", Role: "member",
		ExpiresAt: time.Date(2026, 9, 2, 22, 0, 0, 0, time.UTC),
	}}
	handler := onboardingHandler(t, RequestContext{CSRFToken: "anonymous-token"}, onboarding)
	page := serveHandler(t, handler, http.MethodPost, "/accept-invite", "_csrf=anonymous-token&mode=inspect&token="+token)

	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "2026.09.03 07:00") {
		t.Fatalf("UTC-host invitation expiry status=%d body=%q", page.Code, page.Body.String())
	}
}

func TestUnavailableAndInvalidInvitationAcceptanceFailClosed(t *testing.T) {
	token := strings.Repeat("a", 43)
	unavailable := &recordingOnboarding{err: ErrInvitationUnavailable}
	handler := onboardingHandler(t, RequestContext{CSRFToken: "token"}, unavailable)
	if got := serveHandler(t, handler, http.MethodPost, "/accept-invite", "_csrf=token&mode=inspect&token="+token); got.Code != http.StatusGone {
		t.Fatalf("unavailable inspection status=%d", got.Code)
	}
	temporary := &recordingOnboarding{err: errors.New("database unavailable")}
	handler = onboardingHandler(t, RequestContext{CSRFToken: "token"}, temporary)
	if got := serveHandler(t, handler, http.MethodPost, "/accept-invite", "_csrf=token&mode=inspect&token="+token); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("temporary inspection failure status=%d", got.Code)
	}

	valid := &recordingOnboarding{invitation: InvitationView{Email: "member@example.com"}}
	handler = onboardingHandler(t, RequestContext{CSRFToken: "token"}, valid)
	for _, form := range []string{
		"_csrf=wrong&token=" + token + "&display_name=담당자&password=correct-horse-123&password_confirm=correct-horse-123",
		"_csrf=token&token=" + token + "&display_name=담당자&password=correct-horse-123&password_confirm=different-pass",
		"_csrf=token&token=" + token + "&display_name=담당자&password=short&password_confirm=short",
	} {
		got := serveHandler(t, handler, http.MethodPost, "/accept-invite", form)
		if got.Code != http.StatusForbidden && got.Code != http.StatusBadRequest {
			t.Fatalf("invalid acceptance status=%d body=%q", got.Code, got.Body.String())
		}
	}
	if valid.acceptCalls != 0 {
		t.Fatalf("invalid forms called accept %d times", valid.acceptCalls)
	}
}

func onboardingHandler(t *testing.T, requestContext RequestContext, onboarding Onboarding) http.Handler {
	t.Helper()
	handler, err := NewHandlerWithOptions(Options{
		Backend: &staticBackend{}, Actions: &recordingActions{}, Onboarding: onboarding,
		MapContext: func(*http.Request) (RequestContext, error) { return requestContext, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestInvitationBootstrapMovesFragmentTokenIntoPostBody(t *testing.T) {
	body := serve(t, http.MethodGet, "/assets/app.js").Body.String()
	for _, want := range []string{"data-invite-token-form", "window.location.hash", "history.replaceState", "requestSubmit"} {
		if !strings.Contains(body, want) {
			t.Fatalf("invitation bootstrap script missing %q", want)
		}
	}
	// Destructive company and account removals confirm in the browser first.
	for _, want := range []string{"form[data-confirm]", "window.confirm", "event.preventDefault()"} {
		if !strings.Contains(body, want) {
			t.Fatalf("removal confirmation script missing %q", want)
		}
	}
}
