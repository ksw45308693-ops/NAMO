package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type tenantCatalogEntry struct {
	ID, Name, ContactEmail string
}

func (r *PostgresRepository) tenantCatalog(ctx context.Context) ([]tenantCatalogEntry, error) {
	if r == nil || r.Pool == nil {
		return nil, errors.New("database pool is not configured")
	}
	rows, err := r.Pool.Query(ctx, `SELECT tenant_id::text, name, contact_email FROM public.runtime_tenant_catalog()`)
	if err != nil {
		return nil, fmt.Errorf("load tenant catalog: %w", err)
	}
	defer rows.Close()
	var entries []tenantCatalogEntry
	for rows.Next() {
		var entry tenantCatalogEntry
		if err := rows.Scan(&entry.ID, &entry.Name, &entry.ContactEmail); err != nil {
			return nil, fmt.Errorf("scan tenant catalog: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tenant catalog: %w", err)
	}
	return entries, nil
}

func (r *PostgresRepository) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if r == nil || r.Pool == nil || tenantID == "" || fn == nil {
		return errors.New("tenant transaction requires a pool, tenant, and operation")
	}
	return retryCollectionBusy(ctx, func() error {
		return r.tenantTransaction(ctx, tenantID, fn)
	})
}

// A busy collection lock must release its transaction/connection before waiting:
// the collector itself needs this pool to finish and release the lock.
func retryCollectionBusy(ctx context.Context, operation func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := operation(); !errors.Is(err, ErrCollectionRunning) {
			return err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func tryCollectionLock(ctx context.Context, tx digestRowQuerier) error {
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_xact_lock($1)`, collectionAdvisoryLock).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrCollectionRunning
	}
	return nil
}

func (r *PostgresRepository) tenantTransaction(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tenant transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tenant transaction: %w", err)
	}
	return nil
}
