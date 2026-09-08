package app

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDigestSnapshotAdvisoryLockWaitsForCollectorConnection(t *testing.T) {
	databaseURL := os.Getenv("NAMO_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("NAMO_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	collector, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Release()
	if _, err := collector.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, collectionAdvisoryLock); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = collector.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock($1)`, collectionAdvisoryLock)
		}
	}()

	type snapshotResult struct {
		cutoff time.Time
		err    error
	}
	attempted := make(chan struct{})
	result := make(chan snapshotResult, 1)
	go func() {
		close(attempted)
		var cutoff time.Time
		repository := &PostgresRepository{Pool: pool}
		err := repository.withTenant(ctx, "00000000-0000-0000-0000-000000000001", func(tx pgx.Tx) error {
			var err error
			cutoff, err = lockDigestSnapshot(ctx, tx)
			return err
		})
		result <- snapshotResult{cutoff: cutoff, err: err}
	}()

	<-attempted
	select {
	case got := <-result:
		t.Fatalf("digest lock did not wait for collector: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}

	var beforeUnlock time.Time
	if err := collector.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&beforeUnlock); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Exec(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, collectionAdvisoryLock); err != nil {
		t.Fatal(err)
	}
	locked = false

	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.cutoff.Before(beforeUnlock) {
		t.Fatalf("snapshot cutoff %v predates collector release %v", got.cutoff, beforeUnlock)
	}
}
