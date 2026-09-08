package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"namo/internal/auth"
	"namo/internal/store"
	"namo/migrations"
)

// TestPostgresSignupFunctionsAgainstLiveDatabase executes the signup path on a
// real PostgreSQL. The SQL text contracts cannot see run-time failures such as
// an ambiguous column reference between a RETURNS TABLE output name and a
// users column, so the functions are called here as the runtime role.
func TestPostgresSignupFunctionsAgainstLiveDatabase(t *testing.T) {
	ownerURL := strings.TrimSpace(os.Getenv("TEST_POSTGRES_OWNER_URL"))
	runtimeURL := strings.TrimSpace(os.Getenv("TEST_POSTGRES_RUNTIME_URL"))
	if ownerURL == "" || runtimeURL == "" {
		t.Skip("TEST_POSTGRES_OWNER_URL and TEST_POSTGRES_RUNTIME_URL are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	harness := newReleasePostgresHarness(t, ctx, ownerURL, runtimeURL)
	defer harness.close(t)
	assets, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyMigrations(ctx, store.PgxMigrationBeginner{DB: harness.ownerPool}, assets); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	runtimeConfig, err := pgx.ParseConfig(runtimeURL)
	if err != nil {
		t.Fatalf("parse TEST_POSTGRES_RUNTIME_URL: %v", err)
	}
	runtimeConfig.Database = harness.database
	harness.runtimePool, err = OpenRuntimePool(ctx, runtimeConfig.ConnString())
	if err != nil {
		t.Fatalf("open runtime database: %v", err)
	}
	repository := &PostgresRepository{Pool: harness.runtimePool}

	adminID := insertSignupTestUser(t, ctx, harness.ownerPool, "platform@example.com", auth.PlatformAdmin, "")

	// A platform administrator registers the company directly; no invitation
	// is created, so the company is immediately assignable.
	tenantID, err := repository.RegisterTenant(ctx, adminID, TenantRegistration{
		Name: " 가입 확인 고객 ", ContactEmail: "Contact@Example.com", AdminName: "김관리", AdminEmail: "Admin@Example.com",
	})
	if err != nil {
		t.Fatalf("register tenant: %v", err)
	}
	if _, err := repository.RegisterTenant(ctx, adminID, TenantRegistration{
		Name: "가입 확인 고객", ContactEmail: "contact@example.com", AdminName: "김관리", AdminEmail: "admin@example.com",
	}); !errors.Is(err, ErrTenantRegistered) {
		t.Fatalf("duplicate registration error = %v, want ErrTenantRegistered", err)
	}
	registry, err := repository.TenantRegistry(ctx, adminID)
	if err != nil {
		t.Fatalf("load tenant registry: %v", err)
	}
	if len(registry) != 1 || registry[0].ID != tenantID || registry[0].Name != "가입 확인 고객" ||
		registry[0].ContactEmail != "contact@example.com" || registry[0].AdminName != "김관리" ||
		registry[0].AdminEmail != "admin@example.com" || registry[0].Members != 0 {
		t.Fatalf("tenant registry = %+v", registry)
	}
	if _, err := repository.TenantRegistry(ctx, ""); !errors.Is(err, ErrSignupPrivileges) {
		t.Fatalf("anonymous registry error = %v, want ErrSignupPrivileges", err)
	}
	var scheduleName string
	if err := harness.ownerPool.QueryRow(ctx,
		`SELECT name FROM public.schedules WHERE tenant_id = $1::uuid`, tenantID).Scan(&scheduleName); err != nil {
		t.Fatalf("read default schedule: %v", err)
	}
	if scheduleName != "기본 알림" {
		t.Fatalf("default schedule = %q", scheduleName)
	}
	memberID := insertSignupTestUser(t, ctx, harness.ownerPool, "seated@example.com", auth.Member, tenantID)

	account, err := repository.CreateAccount(ctx, SignupInput{Email: "newcomer@example.com", PasswordHash: signupTestHash(t)})
	if err != nil {
		t.Fatalf("create signup account: %v", err)
	}
	if account.UserID == "" || account.TenantID != "" || account.Role != auth.Member || account.Email != "newcomer@example.com" {
		t.Fatalf("signup account = %+v, want a member without a tenant", account)
	}

	if _, err := repository.CreateAccount(ctx, SignupInput{Email: "NEWCOMER@example.com", PasswordHash: signupTestHash(t)}); !errors.Is(err, ErrEmailRegistered) {
		t.Fatalf("duplicate signup error = %v, want ErrEmailRegistered", err)
	}

	insertPendingInvitation(t, ctx, harness.ownerPool, tenantID, "invited@example.com")
	if _, err := repository.CreateAccount(ctx, SignupInput{Email: "invited@example.com", PasswordHash: signupTestHash(t)}); !errors.Is(err, ErrInvitationWaits) {
		t.Fatalf("invited signup error = %v, want ErrInvitationWaits", err)
	}

	if _, err := repository.MemberAccounts(ctx, memberID); !errors.Is(err, ErrSignupPrivileges) {
		t.Fatalf("member listing error = %v, want ErrSignupPrivileges", err)
	}
	accounts, err := repository.MemberAccounts(ctx, adminID)
	if err != nil {
		t.Fatalf("list member accounts: %v", err)
	}
	if len(accounts) != 2 || accounts[0].UserID != account.UserID || accounts[0].TenantID != "" {
		t.Fatalf("member accounts = %+v, want the unassigned account first", accounts)
	}
	if accounts[0].DisplayName != "newcomer" || accounts[0].TenantName != "" || accounts[0].Role != auth.Member {
		t.Fatalf("unassigned account = %+v", accounts[0])
	}
	if accounts[1].UserID != memberID || accounts[1].TenantName != "가입 확인 고객" || accounts[1].Role != auth.Member {
		t.Fatalf("assigned account = %+v", accounts[1])
	}

	if err := repository.SetAccountAccess(ctx, memberID, account.UserID, tenantID, auth.Member); !errors.Is(err, ErrSignupPrivileges) {
		t.Fatalf("member assignment error = %v, want ErrSignupPrivileges", err)
	}
	if err := repository.SetAccountAccess(ctx, adminID, adminID, tenantID, auth.Member); !errors.Is(err, ErrAccountUnknown) {
		t.Fatalf("platform admin target error = %v, want ErrAccountUnknown", err)
	}
	if err := repository.SetAccountAccess(ctx, adminID, account.UserID, "11111111-2222-3333-4444-555555555555", auth.Member); !errors.Is(err, ErrTenantUnknown) {
		t.Fatalf("unknown tenant error = %v, want ErrTenantUnknown", err)
	}
	if err := repository.SetAccountAccess(ctx, adminID, account.UserID, "", auth.TenantAdmin); !errors.Is(err, ErrAccountRole) {
		t.Fatalf("company-less administrator error = %v, want ErrAccountRole", err)
	}
	if err := repository.SetAccountAccess(ctx, adminID, account.UserID, tenantID, auth.PlatformAdmin); !errors.Is(err, ErrAccountRole) {
		t.Fatalf("platform admin grant error = %v, want ErrAccountRole", err)
	}

	// A company administrator can change filters, settings and reports, so the
	// role must reach the login lookup together with the tenant.
	if err := repository.SetAccountAccess(ctx, adminID, account.UserID, tenantID, auth.TenantAdmin); err != nil {
		t.Fatalf("assign company administrator: %v", err)
	}
	if got := signupTestTenantOf(t, ctx, harness.ownerPool, account.UserID); got != tenantID {
		t.Fatalf("tenant after assignment = %q, want %q", got, tenantID)
	}
	assigned, err := repository.AccountByEmail(ctx, "newcomer@example.com")
	if err != nil || assigned.TenantID != tenantID || assigned.Role != auth.TenantAdmin {
		t.Fatalf("account lookup = %+v err=%v", assigned, err)
	}
	promoted, err := repository.MemberAccounts(ctx, adminID)
	if err != nil {
		t.Fatalf("list accounts after promotion: %v", err)
	}
	var promotedRole auth.Role
	for _, entry := range promoted {
		if entry.UserID == account.UserID {
			promotedRole = entry.Role
		}
	}
	if promotedRole != auth.TenantAdmin {
		t.Fatalf("promoted account role = %q, want tenant_admin", promotedRole)
	}

	// Revoking company access must drop the administrator role with it.
	if err := repository.SetAccountAccess(ctx, adminID, account.UserID, "", auth.Member); err != nil {
		t.Fatalf("revoke tenant: %v", err)
	}
	if got := signupTestTenantOf(t, ctx, harness.ownerPool, account.UserID); got != "" {
		t.Fatalf("tenant after revocation = %q, want empty", got)
	}
	revoked, err := repository.AccountByEmail(ctx, "newcomer@example.com")
	if err != nil || revoked.TenantID != "" || revoked.Role != auth.Member {
		t.Fatalf("account after revocation = %+v err=%v", revoked, err)
	}

	// A company administrator drops a member; the account survives unassigned.
	if err := repository.SetAccountAccess(ctx, adminID, account.UserID, tenantID, auth.TenantAdmin); err != nil {
		t.Fatalf("reassign company administrator: %v", err)
	}
	if err := repository.RemoveTenantMember(ctx, account.UserID, tenantID, account.UserID); !errors.Is(err, ErrAccountRole) {
		t.Fatalf("self removal error = %v, want ErrAccountRole", err)
	}
	if err := repository.RemoveTenantMember(ctx, memberID, tenantID, account.UserID); !errors.Is(err, ErrSignupPrivileges) {
		t.Fatalf("member removal error = %v, want ErrSignupPrivileges", err)
	}
	if err := repository.RemoveTenantMember(ctx, account.UserID, tenantID, memberID); err != nil {
		t.Fatalf("remove company member: %v", err)
	}
	dropped, err := repository.AccountByEmail(ctx, "seated@example.com")
	if err != nil || dropped.TenantID != "" || dropped.Role != auth.Member {
		t.Fatalf("dropped member = %+v err=%v", dropped, err)
	}

	// Account deletion belongs to the platform administrator and takes the
	// sessions with it.
	session, err := auth.NewSession(memberID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveSession(ctx, SessionRecord{
		UserID: memberID, TokenHash: session.TokenHash, ExpiresAt: session.ExpiresAt,
	}); err != nil {
		t.Fatalf("save session for deletion test: %v", err)
	}
	if err := repository.DeleteAccount(ctx, memberID, account.UserID); !errors.Is(err, ErrSignupPrivileges) {
		t.Fatalf("member deletion error = %v, want ErrSignupPrivileges", err)
	}
	if err := repository.DeleteAccount(ctx, adminID, adminID); !errors.Is(err, ErrAccountRole) {
		t.Fatalf("self deletion error = %v, want ErrAccountRole", err)
	}
	if err := repository.DeleteAccount(ctx, adminID, memberID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if _, err := repository.AccountByEmail(ctx, "seated@example.com"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("deleted account lookup error = %v, want ErrUnauthenticated", err)
	}
	if _, err := repository.SessionByHash(ctx, session.TokenHash); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("session after deletion error = %v, want ErrUnauthenticated", err)
	}
	if err := repository.DeleteAccount(ctx, adminID, memberID); !errors.Is(err, ErrAccountUnknown) {
		t.Fatalf("repeated deletion error = %v, want ErrAccountUnknown", err)
	}
	t.Run("concurrent administrators cannot remove each other", func(t *testing.T) {
		testConcurrentAdminRemoval(t, ctx, harness)
	})
}

func testConcurrentAdminRemoval(t *testing.T, parent context.Context, harness *releasePostgresHarness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	tenantID := insertTenant(t, ctx, harness.ownerPool, "동시 제외")
	adminA := insertSignupTestUser(t, ctx, harness.ownerPool, "race-a@example.com", auth.TenantAdmin, tenantID)
	adminB := insertSignupTestUser(t, ctx, harness.ownerPool, "race-b@example.com", auth.TenantAdmin, tenantID)
	first, err := harness.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(context.Background()) }()
	var email string
	if err := first.QueryRow(ctx, `SELECT public.tenant_remove_member($1::uuid,$2::uuid,$3::uuid)`, adminA, tenantID, adminB).Scan(&email); err != nil {
		t.Fatal(err)
	}
	second, err := harness.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondPID := second.Conn().PgConn().PID()
	result := make(chan error, 1)
	go func() {
		defer func() { _ = second.Rollback(context.Background()) }()
		var removed string
		result <- second.QueryRow(ctx, `SELECT public.tenant_remove_member($1::uuid,$2::uuid,$3::uuid)`, adminB, tenantID, adminA).Scan(&removed)
	}()
	// Observe the real row-lock wait; no timing assumption about goroutine start.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			t.Fatalf("second administrator did not wait for revocation commit: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
		var waiting bool
		if err := harness.ownerPool.QueryRow(ctx, `SELECT cardinality(pg_catalog.pg_blocking_pids($1)) > 0`, secondPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("revoked administrator error=%v, want insufficient privilege", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var admins int
	if err := harness.ownerPool.QueryRow(ctx, `SELECT count(*) FROM public.users WHERE tenant_id=$1::uuid AND role='tenant_admin'`, tenantID).Scan(&admins); err != nil || admins != 1 {
		t.Fatalf("remaining administrators=%d err=%v", admins, err)
	}
}

func insertSignupTestUser(t *testing.T, ctx context.Context, owner *pgxpool.Pool, email string, role auth.Role, tenantID string) string {
	t.Helper()
	var tenant *string
	if strings.TrimSpace(tenantID) != "" {
		tenant = &tenantID
	}
	var userID string
	err := owner.QueryRow(ctx, `INSERT INTO public.users (tenant_id, email, display_name, password_hash, role)
VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text`,
		tenant, email, strings.Split(email, "@")[0], signupTestHash(t), string(role)).Scan(&userID)
	if err != nil {
		t.Fatalf("insert %s user %q: %v", role, email, err)
	}
	return userID
}

func insertPendingInvitation(t *testing.T, ctx context.Context, owner *pgxpool.Pool, tenantID, email string) {
	t.Helper()
	_, err := owner.Exec(ctx, `INSERT INTO public.invitations (tenant_id, email, display_name, role, token_hash, expires_at)
VALUES ($1::uuid, $2, $3, 'member', $4, clock_timestamp() + interval '24 hours')`,
		tenantID, email, strings.Split(email, "@")[0], integrationHash("signup-invitation:"+email))
	if err != nil {
		t.Fatalf("insert pending invitation %q: %v", email, err)
	}
}

func signupTestTenantOf(t *testing.T, ctx context.Context, owner *pgxpool.Pool, userID string) string {
	t.Helper()
	var tenantID *string
	if err := owner.QueryRow(ctx, `SELECT tenant_id::text FROM public.users WHERE id = $1::uuid`, userID).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant of %q: %v", userID, err)
	}
	if tenantID == nil {
		return ""
	}
	return *tenantID
}

func signupTestHash(t *testing.T) string {
	t.Helper()
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
