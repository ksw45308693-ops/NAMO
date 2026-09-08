package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"namo/internal/auth"
	appweb "namo/internal/web"
)

var ErrInvitationMail = appweb.ErrInvitationMailDelivery

// TenantInviteInput asks for a tenant plus the invitation of its first
// administrator. The web surface registers companies directly, so this path
// stays available only for operator tooling.
type TenantInviteInput struct {
	TenantName, ContactEmail, AdminName, AdminEmail string
}

type TenantInvitationInput struct {
	ActorUserID, TenantName, ContactEmail, AdminName, AdminEmail, TokenHash string
	Role                                                                    auth.Role
	ExpiresAt                                                               time.Time
}

type MemberInvitationInput struct {
	ActorUserID, TenantID, Name, Email, TokenHash string
	Role                                          auth.Role
	ExpiresAt                                     time.Time
}

type InvitationRecord struct {
	TenantName, Email, DisplayName string
	Role                           auth.Role
	ExpiresAt                      time.Time
}

type AcceptedInvitationInput struct{ TokenHash, DisplayName, PasswordHash string }

type InvitationStore interface {
	CreateTenantInvitation(context.Context, TenantInvitationInput) error
	CreateMemberInvitation(context.Context, MemberInvitationInput) error
	InvitationByHash(context.Context, string) (InvitationRecord, error)
	AcceptInvitation(context.Context, AcceptedInvitationInput) error
}

type InvitationService struct {
	Store         InvitationStore
	Mailer        Mailer
	From, BaseURL string
	Now           func() time.Time
}

func (s InvitationService) InviteTenant(ctx context.Context, requestContext appweb.RequestContext, command TenantInviteInput) (appweb.InvitationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, interactiveMailRetryPolicy.TotalTimeout)
	defer cancel()

	if requestContext.Role != string(auth.PlatformAdmin) || requestContext.UserID == "" {
		return appweb.InvitationResult{}, errors.New("platform administrator role is required")
	}
	tenantName, err := invitationName(command.TenantName)
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	adminName, err := invitationName(command.AdminName)
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	contactEmail, err := normalizeMailbox(command.ContactEmail)
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	adminEmail, err := normalizeMailbox(command.AdminEmail)
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	link, hash, expiresAt, err := s.prepareInvitation()
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	input := TenantInvitationInput{
		ActorUserID: requestContext.UserID, TenantName: tenantName, ContactEmail: contactEmail,
		AdminName: adminName, AdminEmail: adminEmail, Role: auth.TenantAdmin, TokenHash: hash, ExpiresAt: expiresAt,
	}
	if err := s.Store.CreateTenantInvitation(ctx, input); err != nil {
		return appweb.InvitationResult{}, fmt.Errorf("create tenant invitation: %w", err)
	}
	return appweb.InvitationResult{URL: link, ExpiresAt: expiresAt}, nil
}

func (s InvitationService) InviteMember(ctx context.Context, requestContext appweb.RequestContext, command appweb.MemberInviteCommand) (appweb.InvitationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, interactiveMailRetryPolicy.TotalTimeout)
	defer cancel()

	if requestContext.Role != string(auth.TenantAdmin) || requestContext.UserID == "" || requestContext.TenantID == "" {
		return appweb.InvitationResult{}, errors.New("tenant administrator role is required")
	}
	name, err := invitationName(command.Name)
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	email, err := normalizeMailbox(command.Email)
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	role := auth.Role(command.Role)
	if role != auth.Member && role != auth.TenantAdmin {
		return appweb.InvitationResult{}, errors.New("invitation role must be member or tenant_admin")
	}
	tenantName := requestContext.TenantName
	if strings.TrimSpace(tenantName) == "" {
		tenantName = "소속 회사"
	}
	link, hash, expiresAt, err := s.prepareInvitation()
	if err != nil {
		return appweb.InvitationResult{}, err
	}
	input := MemberInvitationInput{
		ActorUserID: requestContext.UserID, TenantID: requestContext.TenantID, Name: name,
		Email: email, Role: role, TokenHash: hash, ExpiresAt: expiresAt,
	}
	if err := s.Store.CreateMemberInvitation(ctx, input); err != nil {
		return appweb.InvitationResult{}, fmt.Errorf("create member invitation: %w", err)
	}
	return appweb.InvitationResult{URL: link, ExpiresAt: expiresAt}, nil
}

func (s InvitationService) Invitation(ctx context.Context, token string) (appweb.InvitationView, error) {
	if s.Store == nil || !auth.ValidInvitationToken(token) {
		return appweb.InvitationView{}, appweb.ErrInvitationUnavailable
	}
	record, err := s.Store.InvitationByHash(ctx, auth.HashInvitationToken(token))
	if err != nil {
		return appweb.InvitationView{}, err
	}
	return appweb.InvitationView{
		TenantName: record.TenantName, Email: record.Email, DisplayName: record.DisplayName,
		Role: string(record.Role), ExpiresAt: record.ExpiresAt,
	}, nil
}

func (s InvitationService) AcceptInvitation(ctx context.Context, command appweb.AcceptInviteCommand) error {
	if s.Store == nil || !auth.ValidInvitationToken(command.Token) {
		return appweb.ErrInvitationUnavailable
	}
	name, err := invitationName(command.DisplayName)
	if err != nil {
		return err
	}
	if !utf8.ValidString(command.Password) || len([]byte(command.Password)) < 12 || len([]byte(command.Password)) > 72 {
		return errors.New("password must contain 12 to 72 UTF-8 bytes")
	}
	if _, err := s.Store.InvitationByHash(ctx, auth.HashInvitationToken(command.Token)); err != nil {
		return err
	}
	passwordHash, err := auth.HashPassword(command.Password)
	if err != nil {
		return errors.New("could not secure invitation password")
	}
	if err := s.Store.AcceptInvitation(ctx, AcceptedInvitationInput{
		TokenHash: auth.HashInvitationToken(command.Token), DisplayName: name, PasswordHash: passwordHash,
	}); err != nil {
		return err
	}
	return nil
}

func (s InvitationService) prepareInvitation() (link, hash string, expiresAt time.Time, err error) {
	if s.Store == nil {
		return "", "", time.Time{}, errors.New("invitation store is not configured")
	}
	token, hash, err := auth.NewInvitationToken()
	if err != nil {
		return "", "", time.Time{}, errors.New("could not create invitation token")
	}
	link, err = invitationURL(s.BaseURL, token)
	if err != nil {
		return "", "", time.Time{}, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return link, hash, now().Add(48 * time.Hour), nil
}

func (s InvitationService) sendInvitation(ctx context.Context, to string, message []byte) error {
	return s.sendInvitationWithPolicy(ctx, to, message, interactiveMailRetryPolicy)
}

func (s InvitationService) sendInvitationWithPolicy(ctx context.Context, to string, message []byte, policy mailRetryPolicy) error {
	if s.Mailer == nil {
		return ErrInvitationMail
	}
	if err := sendMailWithRetry(ctx, s.Mailer, s.From, to, message, policy); err != nil {
		return fmt.Errorf("%w: %w", ErrInvitationMail, err)
	}
	return nil
}

func invitationName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || len([]byte(value)) > 512 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", errors.New("a valid name is required")
	}
	return value, nil
}

func invitationURL(baseURL, token string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Host == "" || base.User != nil || base.Scheme != "https" || base.Fragment != "" || base.RawQuery != "" || base.ForceQuery || !auth.ValidInvitationToken(token) {
		return "", errors.New("valid BASE_URL is required for invitations")
	}
	if path := base.EscapedPath(); path != "" && path != "/" {
		return "", errors.New("valid BASE_URL is required for invitations")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/accept-invite"
	base.RawPath, base.RawQuery = "", ""
	base.Fragment = "token=" + url.QueryEscape(token)
	return base.String(), nil
}

func buildInvitationMessage(from, to, tenantName string, role auth.Role, link string) ([]byte, error) {
	from, err := normalizeMailbox(from)
	if err != nil {
		return nil, errors.New("valid invitation sender is required")
	}
	to, err = normalizeMailbox(to)
	if err != nil {
		return nil, errors.New("valid invitation recipient is required")
	}
	roleLabel := map[auth.Role]string{auth.TenantAdmin: "테넌트 관리자", auth.Member: "일반 사용자"}[role]
	if roleLabel == "" {
		return nil, errors.New("valid invitation role is required")
	}
	body := "<!doctype html><html lang=\"ko\"><body>" +
		"<h1>나라장터 입찰공고 모니터링 초대</h1><p><strong>" + html.EscapeString(tenantName) +
		"</strong>의 " + roleLabel + "로 초대되었습니다.</p><p><a href=\"" + html.EscapeString(link) +
		"\">48시간 안에 계정 만들기</a></p><p>요청하지 않은 초대라면 이 메일을 무시하세요.</p></body></html>"
	var encoded bytes.Buffer
	writer := quotedprintable.NewWriter(&encoded)
	if _, err := writer.Write([]byte(body)); err != nil {
		return nil, errors.New("could not encode invitation mail")
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("could not finish invitation mail")
	}
	subject := mime.QEncoding.Encode("UTF-8", "나라장터 모니터링 계정 초대")
	return []byte("From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" + encoded.String()), nil
}
