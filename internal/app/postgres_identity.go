package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"namo/internal/auth"
)

type PostgresRepository struct {
	Pool *pgxpool.Pool
}

func (r *PostgresRepository) Ping(ctx context.Context) error {
	if r == nil || r.Pool == nil {
		return errors.New("database pool is not configured")
	}
	return r.Pool.Ping(ctx)
}

func (r *PostgresRepository) AccountByEmail(ctx context.Context, email string) (LoginAccount, error) {
	if r == nil || r.Pool == nil {
		return LoginAccount{}, ErrUnauthenticated
	}
	var account LoginAccount
	var role string
	var tenantID *string
	err := r.Pool.QueryRow(ctx,
		`SELECT user_id, tenant_id::text, email, password_hash, role FROM public.auth_account_lookup($1)`,
		strings.TrimSpace(strings.ToLower(email)),
	).Scan(&account.UserID, &tenantID, &account.Email, &account.PasswordHash, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginAccount{}, ErrUnauthenticated
	}
	if err != nil {
		return LoginAccount{}, fmt.Errorf("lookup login account: %w", err)
	}
	if tenantID != nil {
		account.TenantID = *tenantID
	}
	account.Role = auth.Role(role)
	if !account.Role.Valid() {
		return LoginAccount{}, ErrUnauthenticated
	}
	return account, nil
}

func (r *PostgresRepository) SaveSession(ctx context.Context, session SessionRecord) error {
	if r == nil || r.Pool == nil || session.UserID == "" || session.TokenHash == "" || session.ExpiresAt.IsZero() {
		return errors.New("valid session record is required")
	}
	if _, err := r.Pool.Exec(ctx,
		`SELECT public.auth_session_create($1::uuid, $2, $3)`,
		session.UserID, session.TokenHash, session.ExpiresAt,
	); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *PostgresRepository) SessionByHash(ctx context.Context, hash string) (SessionRecord, error) {
	if r == nil || r.Pool == nil || hash == "" {
		return SessionRecord{}, ErrUnauthenticated
	}
	var record SessionRecord
	var role string
	var tenantID *string
	err := r.Pool.QueryRow(ctx,
		`SELECT user_id, tenant_id::text, email, role, expires_at FROM public.auth_session_lookup($1)`, hash,
	).Scan(&record.UserID, &tenantID, &record.Email, &role, &record.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionRecord{}, ErrUnauthenticated
	}
	if err != nil {
		return SessionRecord{}, fmt.Errorf("lookup session: %w", err)
	}
	if tenantID != nil {
		record.TenantID = *tenantID
	}
	record.TokenHash = hash
	record.Role = auth.Role(role)
	if !record.Role.Valid() {
		return SessionRecord{}, ErrUnauthenticated
	}
	return record, nil
}

func (r *PostgresRepository) DeleteSession(ctx context.Context, hash string) error {
	if r == nil || r.Pool == nil || hash == "" {
		return nil
	}
	var deleted bool
	if err := r.Pool.QueryRow(ctx, `SELECT public.auth_session_delete($1)`, hash).Scan(&deleted); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (r *PostgresRepository) CreateAccount(ctx context.Context, input SignupInput) (LoginAccount, error) {
	if r == nil || r.Pool == nil {
		return LoginAccount{}, errors.New("database pool is not configured")
	}
	email := strings.TrimSpace(strings.ToLower(input.Email))
	if email == "" || strings.TrimSpace(input.PasswordHash) == "" {
		return LoginAccount{}, errors.New("valid signup account is required")
	}
	var account LoginAccount
	var role string
	var tenantID *string
	err := r.Pool.QueryRow(ctx,
		`SELECT user_id::text, tenant_id::text, email, role FROM public.signup_create_account($1, $2)`,
		email, input.PasswordHash,
	).Scan(&account.UserID, &tenantID, &account.Email, &role)
	if err != nil {
		return LoginAccount{}, signupError(err)
	}
	if tenantID != nil {
		account.TenantID = *tenantID
	}
	account.Role = auth.Role(role)
	if !account.Role.Valid() {
		return LoginAccount{}, errors.New("signup produced an invalid role")
	}
	account.PasswordHash = input.PasswordHash
	return account, nil
}

func (r *PostgresRepository) MemberAccounts(ctx context.Context, actorUserID string) ([]MemberAccount, error) {
	if r == nil || r.Pool == nil {
		return nil, errors.New("database pool is not configured")
	}
	if strings.TrimSpace(actorUserID) == "" {
		return nil, ErrSignupPrivileges
	}
	rows, err := r.Pool.Query(ctx,
		`SELECT user_id::text, email, display_name, role, coalesce(tenant_id::text, ''), tenant_name, created_at
FROM public.admin_account_registry($1::uuid)`, actorUserID)
	if err != nil {
		return nil, signupError(err)
	}
	defer rows.Close()
	var accounts []MemberAccount
	for rows.Next() {
		var account MemberAccount
		var role string
		if err := rows.Scan(&account.UserID, &account.Email, &account.DisplayName, &role,
			&account.TenantID, &account.TenantName, &account.Created); err != nil {
			return nil, fmt.Errorf("scan member account: %w", err)
		}
		account.Role = auth.Role(role)
		if !account.Role.Valid() {
			return nil, fmt.Errorf("account %q carries an unknown role", account.UserID)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, signupError(err)
	}
	return accounts, nil
}

// SetAccountAccess assigns a company and the role inside it. An empty tenantID
// revokes company access, which is only valid together with the member role.
func (r *PostgresRepository) SetAccountAccess(ctx context.Context, actorUserID, userID, tenantID string, role auth.Role) error {
	if r == nil || r.Pool == nil {
		return errors.New("database pool is not configured")
	}
	if strings.TrimSpace(actorUserID) == "" {
		return ErrSignupPrivileges
	}
	if strings.TrimSpace(userID) == "" {
		return ErrAccountUnknown
	}
	if role != auth.Member && role != auth.TenantAdmin {
		return ErrAccountRole
	}
	var target *string
	if trimmed := strings.TrimSpace(tenantID); trimmed != "" {
		target = &trimmed
	}
	if target == nil && role != auth.Member {
		return ErrAccountRole
	}
	var assignedUserID, email, assignedRole string
	var assignedTenant *string
	err := r.Pool.QueryRow(ctx,
		`SELECT user_id::text, tenant_id::text, email, role
FROM public.admin_set_account_access($1::uuid, $2::uuid, $3::uuid, $4)`,
		actorUserID, userID, target, string(role),
	).Scan(&assignedUserID, &assignedTenant, &email, &assignedRole)
	if err != nil {
		return signupError(err)
	}
	return nil
}

func signupError(err error) error {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountUnknown
		}
		return fmt.Errorf("signup account operation: %w", err)
	}
	switch {
	case postgresError.Code == "23505":
		return ErrEmailRegistered
	case postgresError.Code == "P0001" && postgresError.Message == "invitation already pending":
		return ErrInvitationWaits
	case postgresError.Code == "42501":
		return ErrSignupPrivileges
	case postgresError.Code == "22023":
		return ErrAccountRole
	case postgresError.Code == "P0002" && postgresError.Message == "tenant is unavailable":
		return ErrTenantUnknown
	case postgresError.Code == "P0002":
		return ErrAccountUnknown
	}
	return fmt.Errorf("signup account operation: %w", err)
}
