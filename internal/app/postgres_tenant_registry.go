package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Platform administrators register a company directly. No invitation is
// created, so an account reaches company data only through tenant assignment.
var (
	ErrTenantRegistered = errors.New("tenant is already registered")
	ErrTenantInvalid    = errors.New("company and administrator details are required")
)

// TenantRegistration is one validated company registration request.
type TenantRegistration struct {
	Name, ContactEmail, AdminName, AdminEmail string
}

// TenantRegistryEntry is one registered company as shown to platform
// administrators.
type TenantRegistryEntry struct {
	ID, Name, ContactEmail, AdminName, AdminEmail string
	Members                                       int
	Created                                       time.Time
}

func (r *PostgresRepository) RegisterTenant(ctx context.Context, actorUserID string, input TenantRegistration) (string, error) {
	if r == nil || r.Pool == nil {
		return "", errors.New("database pool is not configured")
	}
	if strings.TrimSpace(actorUserID) == "" {
		return "", ErrSignupPrivileges
	}
	var tenantID string
	err := r.Pool.QueryRow(ctx,
		`SELECT public.admin_register_tenant($1::uuid, $2, $3, $4, $5)::text`,
		actorUserID, strings.TrimSpace(input.Name), strings.TrimSpace(strings.ToLower(input.ContactEmail)),
		strings.TrimSpace(input.AdminName), strings.TrimSpace(strings.ToLower(input.AdminEmail)),
	).Scan(&tenantID)
	if err != nil {
		return "", tenantRegistryError(err)
	}
	return tenantID, nil
}

func (r *PostgresRepository) TenantRegistry(ctx context.Context, actorUserID string) ([]TenantRegistryEntry, error) {
	if r == nil || r.Pool == nil {
		return nil, errors.New("database pool is not configured")
	}
	if strings.TrimSpace(actorUserID) == "" {
		return nil, ErrSignupPrivileges
	}
	rows, err := r.Pool.Query(ctx,
		`SELECT tenant_id::text, name, contact_email, admin_name, admin_email, member_count, created_at
FROM public.admin_tenant_registry($1::uuid)`, actorUserID)
	if err != nil {
		return nil, tenantRegistryError(err)
	}
	defer rows.Close()
	var entries []TenantRegistryEntry
	for rows.Next() {
		var entry TenantRegistryEntry
		var members int64
		if err := rows.Scan(&entry.ID, &entry.Name, &entry.ContactEmail,
			&entry.AdminName, &entry.AdminEmail, &members, &entry.Created); err != nil {
			return nil, fmt.Errorf("scan tenant registry: %w", err)
		}
		entry.Members = int(members)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, tenantRegistryError(err)
	}
	return entries, nil
}

func tenantRegistryError(err error) error {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTenantUnknown
		}
		return fmt.Errorf("tenant registry operation: %w", err)
	}
	switch postgresError.Code {
	case "23505":
		return ErrTenantRegistered
	case "42501":
		return ErrSignupPrivileges
	case "22023":
		return ErrTenantInvalid
	}
	return fmt.Errorf("tenant registry operation: %w", err)
}

// RemoveTenantMember drops one account from a company. The account itself
// survives and returns to the unassigned state.
func (r *PostgresRepository) RemoveTenantMember(ctx context.Context, actorUserID, tenantID, userID string) error {
	if r == nil || r.Pool == nil {
		return errors.New("database pool is not configured")
	}
	if strings.TrimSpace(actorUserID) == "" || strings.TrimSpace(tenantID) == "" {
		return ErrSignupPrivileges
	}
	if strings.TrimSpace(userID) == "" {
		return ErrAccountUnknown
	}
	var email string
	err := r.Pool.QueryRow(ctx,
		`SELECT public.tenant_remove_member($1::uuid, $2::uuid, $3::uuid)`,
		actorUserID, tenantID, userID,
	).Scan(&email)
	if err != nil {
		return accountRemovalError(err)
	}
	return nil
}

// DeleteAccount removes the account itself. Its sessions cascade, so the
// deleted account can no longer sign in.
func (r *PostgresRepository) DeleteAccount(ctx context.Context, actorUserID, userID string) error {
	if r == nil || r.Pool == nil {
		return errors.New("database pool is not configured")
	}
	if strings.TrimSpace(actorUserID) == "" {
		return ErrSignupPrivileges
	}
	if strings.TrimSpace(userID) == "" {
		return ErrAccountUnknown
	}
	var email string
	err := r.Pool.QueryRow(ctx,
		`SELECT public.admin_delete_account($1::uuid, $2::uuid)`, actorUserID, userID,
	).Scan(&email)
	if err != nil {
		return accountRemovalError(err)
	}
	return nil
}

func accountRemovalError(err error) error {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountUnknown
		}
		return fmt.Errorf("account removal: %w", err)
	}
	switch postgresError.Code {
	case "42501":
		return ErrSignupPrivileges
	case "22023":
		return ErrAccountRole
	case "P0002":
		return ErrAccountUnknown
	}
	return fmt.Errorf("account removal: %w", err)
}
