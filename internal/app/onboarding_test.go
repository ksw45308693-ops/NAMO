package app

import (
	"context"
	"errors"
	"io"
	"mime/quotedprintable"
	"net/url"
	"strings"
	"testing"
	"time"

	"namo/internal/auth"
	appweb "namo/internal/web"
)

type invitationStoreStub struct {
	createdTenants []TenantInvitationInput
	createdMembers []MemberInvitationInput
	tenantDeadline time.Time
	memberDeadline time.Time
	invitation     InvitationRecord
	lookupCalls    int
	accepted       AcceptedInvitationInput
	tenantErr      error
	memberErr      error
}

func (s *invitationStoreStub) CreateTenantInvitation(ctx context.Context, input TenantInvitationInput) error {
	s.tenantDeadline, _ = ctx.Deadline()
	if s.tenantErr != nil {
		return s.tenantErr
	}
	s.createdTenants = append(s.createdTenants, input)
	return nil
}

func (s *invitationStoreStub) CreateMemberInvitation(ctx context.Context, input MemberInvitationInput) error {
	s.memberDeadline, _ = ctx.Deadline()
	if s.memberErr != nil {
		return s.memberErr
	}
	s.createdMembers = append(s.createdMembers, input)
	return nil
}

func (s *invitationStoreStub) InvitationByHash(_ context.Context, _ string) (InvitationRecord, error) {
	s.lookupCalls++
	if s.invitation.Email == "" {
		return InvitationRecord{}, appweb.ErrInvitationUnavailable
	}
	return s.invitation, nil
}

func (s *invitationStoreStub) AcceptInvitation(_ context.Context, input AcceptedInvitationInput) error {
	s.accepted = input
	return nil
}

type invitationMailerStub struct {
	failures int
	calls    int
	from     string
	to       string
	message  []byte
}

func (m *invitationMailerStub) Send(_ context.Context, from, to string, message []byte) error {
	m.calls++
	m.from, m.to, m.message = from, to, append([]byte(nil), message...)
	if m.calls <= m.failures {
		return errors.New("smtp unavailable")
	}
	return nil
}

func TestInvitationServiceCreatesTenantWithHashedFortyEightHourInvitation(t *testing.T) {
	now := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	store, mailer := &invitationStoreStub{}, &invitationMailerStub{}
	service := InvitationService{Store: store, Mailer: mailer, From: "monitor@example.com", BaseURL: "https://monitor.example", Now: func() time.Time { return now }}
	requestContext := appweb.RequestContext{UserID: "platform-user", Role: "platform_admin"}
	command := TenantInviteInput{TenantName: "샘플 <기업>", ContactEmail: "contact@example.com", AdminName: "김관리", AdminEmail: "Admin@Example.com"}

	result, err := service.InviteTenant(context.Background(), requestContext, command)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.createdTenants) != 1 {
		t.Fatalf("stored tenant invitations = %d", len(store.createdTenants))
	}
	stored := store.createdTenants[0]
	if stored.ActorUserID != "platform-user" || stored.AdminEmail != "admin@example.com" || stored.Role != auth.TenantAdmin {
		t.Fatalf("stored invitation = %#v", stored)
	}
	if len(stored.TokenHash) != 64 || !stored.ExpiresAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("hash/expiry = %q / %v", stored.TokenHash, stored.ExpiresAt)
	}
	linkToken := invitationTokenFromURL(t, result.URL)
	if !auth.ValidInvitationToken(linkToken) || auth.HashInvitationToken(linkToken) != stored.TokenHash || stored.TokenHash == linkToken {
		t.Fatal("returned link and persisted token hash contract do not match")
	}
	if result.ExpiresAt != stored.ExpiresAt || !strings.HasPrefix(result.URL, "https://monitor.example/accept-invite#token=") || strings.Contains(result.URL, "?token=") || mailer.calls != 0 {
		t.Fatalf("result=%#v mail calls=%d", result, mailer.calls)
	}
}

func TestInvitationServiceRejectsHeaderInjectionBeforePersistence(t *testing.T) {
	store, mailer := &invitationStoreStub{}, &invitationMailerStub{}
	service := InvitationService{Store: store, Mailer: mailer, From: "monitor@example.com", BaseURL: "https://monitor.example"}
	_, err := service.InviteMember(context.Background(), appweb.RequestContext{UserID: "admin", TenantID: "tenant-1", Role: "tenant_admin"}, appweb.MemberInviteCommand{
		Name: "담당자", Email: "victim@example.com\r\nBcc: attacker@example.com", Role: "member",
	})
	if err == nil || len(store.createdMembers) != 0 || mailer.calls != 0 {
		t.Fatalf("injected address reached persistence or mail: err=%v store=%d mail=%d", err, len(store.createdMembers), mailer.calls)
	}
}

func TestInvitationRequestBudgetStartsBeforePersistence(t *testing.T) {
	store := &invitationStoreStub{}
	service := InvitationService{Store: store, Mailer: &invitationMailerStub{}, From: "monitor@example.com", BaseURL: "https://monitor.example"}
	_, err := service.InviteMember(context.Background(), appweb.RequestContext{
		UserID: "admin", TenantID: "tenant-1", TenantName: "테넌트", Role: "tenant_admin",
	}, appweb.MemberInviteCommand{Name: "담당자", Email: "member@example.com", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if store.memberDeadline.IsZero() {
		t.Fatal("persistence did not receive the interactive request deadline")
	}
	remaining := time.Until(store.memberDeadline)
	if remaining <= 0 || remaining > interactiveMailRetryPolicy.TotalTimeout {
		t.Fatalf("persistence deadline remaining=%v, total=%v", remaining, interactiveMailRetryPolicy.TotalTimeout)
	}
}

func TestInvitationURLRequiresHTTPSOrigin(t *testing.T) {
	token, _, err := auth.NewInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, baseURL := range []string{"http://monitor.example", "https://monitor.example/app", "https://monitor.example/?source=test"} {
		if _, err := invitationURL(baseURL, token); err == nil {
			t.Errorf("invitationURL accepted %q", baseURL)
		}
	}
	if _, err := invitationURL("https://monitor.example/", token); err != nil {
		t.Fatalf("root HTTPS origin rejected: %v", err)
	}
}

func TestInvitationReplayPreservesFirstLinkWithoutSendingMail(t *testing.T) {
	store, mailer := &invitationStoreStub{}, &invitationMailerStub{failures: 6}
	service := InvitationService{Store: store, Mailer: mailer, From: "monitor@example.com", BaseURL: "https://monitor.example"}
	ctx := appweb.RequestContext{UserID: "admin", TenantID: "tenant-1", Role: "tenant_admin"}
	command := appweb.MemberInviteCommand{Name: "담당자", Email: "member@example.com", Role: "member"}

	first, err := service.InviteMember(context.Background(), ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := store.createdMembers[0].TokenHash
	store.memberErr = appweb.ErrInvitationPending
	replayed, err := service.InviteMember(context.Background(), ctx, command)
	if !errors.Is(err, appweb.ErrInvitationPending) || replayed.URL != "" || !replayed.ExpiresAt.IsZero() {
		t.Fatalf("replay result=%#v err=%v", replayed, err)
	}
	if len(store.createdMembers) != 1 || auth.HashInvitationToken(invitationTokenFromURL(t, first.URL)) != firstHash || store.createdMembers[0].TokenHash != firstHash || mailer.calls != 0 {
		t.Fatalf("replay changed first invitation: stored=%#v first=%#v mail calls=%d", store.createdMembers, first, mailer.calls)
	}
}

func TestInvitationServiceAcceptsOnlyValidPasswordAndActiveToken(t *testing.T) {
	token, hash, err := auth.NewInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	store := &invitationStoreStub{invitation: InvitationRecord{TenantName: "샘플", Email: "member@example.com", DisplayName: "담당자", Role: auth.Member}}
	service := InvitationService{Store: store}

	view, err := service.Invitation(context.Background(), token)
	if err != nil || view.Email != "member@example.com" || view.Role != "member" {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	if err := service.AcceptInvitation(context.Background(), appweb.AcceptInviteCommand{Token: token, DisplayName: "새 담당자", Password: "올바른-비밀번호-123"}); err != nil {
		t.Fatal(err)
	}
	if store.accepted.TokenHash != hash || store.accepted.DisplayName != "새 담당자" || !auth.CheckPassword(store.accepted.PasswordHash, "올바른-비밀번호-123") {
		t.Fatalf("accepted invitation = %#v", store.accepted)
	}

	for _, password := range []string{"short", strings.Repeat("가", 25)} {
		store.accepted = AcceptedInvitationInput{}
		err := service.AcceptInvitation(context.Background(), appweb.AcceptInviteCommand{Token: token, DisplayName: "담당자", Password: password})
		if err == nil || store.accepted.TokenHash != "" {
			t.Fatalf("invalid password bytes=%d accepted: err=%v", len([]byte(password)), err)
		}
	}
	store.lookupCalls = 0
	if _, err := service.Invitation(context.Background(), "not-a-token"); !errors.Is(err, appweb.ErrInvitationUnavailable) || store.lookupCalls != 0 {
		t.Fatalf("malformed token lookup calls=%d err=%v", store.lookupCalls, err)
	}
}

func TestMemberInvitationUsesAuthenticatedTenantAndRestrictedRole(t *testing.T) {
	store := &invitationStoreStub{}
	service := InvitationService{Store: store, Mailer: &invitationMailerStub{}, From: "monitor@example.com", BaseURL: "https://monitor.example"}
	ctx := appweb.RequestContext{UserID: "tenant-admin", TenantID: "tenant-real", Role: "tenant_admin"}
	if _, err := service.InviteMember(context.Background(), ctx, appweb.MemberInviteCommand{Name: "관리자", Email: "next@example.com", Role: "tenant_admin"}); err != nil {
		t.Fatal(err)
	}
	if len(store.createdMembers) != 1 || store.createdMembers[0].TenantID != "tenant-real" || store.createdMembers[0].Role != auth.TenantAdmin {
		t.Fatalf("member invitation = %#v", store.createdMembers)
	}
	if _, err := service.InviteMember(context.Background(), appweb.RequestContext{UserID: "member", TenantID: "tenant-other", Role: "member"}, appweb.MemberInviteCommand{Name: "X", Email: "x@example.com", Role: "member"}); err == nil {
		t.Fatal("member role created an invitation")
	}
}

func TestInvitationMessageBuilderRemainsAvailableForSMTPFlows(t *testing.T) {
	message, err := buildInvitationMessage("monitor@example.com", "admin@example.com", "샘플 <기업>", auth.TenantAdmin, "https://monitor.example/accept-invite#token=abc")
	if err != nil {
		t.Fatal(err)
	}
	body := decodeQuotedPrintableBody(t, message)
	if strings.Contains(body, "<기업>") || !strings.Contains(body, "샘플 &lt;기업&gt;") || !strings.Contains(body, "#token=abc") {
		t.Fatalf("SMTP invitation message is not escaped or complete: %s", body)
	}
}

func decodeQuotedPrintableBody(t *testing.T, message []byte) string {
	t.Helper()
	_, body, found := strings.Cut(string(message), "\r\n\r\n")
	if !found {
		t.Fatal("SMTP message has no header boundary")
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}

func invitationTokenFromURL(t *testing.T, invitationURL string) string {
	t.Helper()
	start := strings.Index(invitationURL, "#token=")
	if start < 0 {
		t.Fatal("invitation link has no token")
	}
	raw := invitationURL[start+len("#token="):]
	token, err := url.QueryUnescape(raw)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
