package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"namo/internal/store"
	"namo/migrations"
)

func TestPostgresReportSchemaMigrationBaseline(t *testing.T) {
	assets, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) < 8 {
		t.Fatalf("migration count=%d, want at least 8", len(assets))
	}
	if assets[6].Version != 7 || assets[7].Version != 8 {
		t.Fatalf("migration versions at positions 6 and 7 are %04d and %04d, want 0007 and 0008", assets[6].Version, assets[7].Version)
	}
}

func TestPostgresReportSchema(t *testing.T) {
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
	if len(assets) < 8 {
		t.Fatalf("migration count=%d, want at least 8", len(assets))
	}
	if assets[6].Version != 7 || assets[7].Version != 8 {
		t.Fatalf("migration versions at positions 6 and 7 are %04d and %04d, want 0007 and 0008", assets[6].Version, assets[7].Version)
	}
	migrator := store.PgxMigrationBeginner{DB: harness.ownerPool}
	if err := store.ApplyMigrations(ctx, migrator, assets[:7]); err != nil {
		t.Fatalf("apply migrations through 0007: %v", err)
	}
	upgradeTenant := insertTenant(t, ctx, harness.ownerPool, "기존 고객")
	var upgradeSchedule string
	if err := harness.ownerPool.QueryRow(ctx, `INSERT INTO public.schedules
(tenant_id,name,hour,minute,timezone) VALUES ($1::uuid,'기존 예약',7,0,'Asia/Seoul') RETURNING id::text`, upgradeTenant).Scan(&upgradeSchedule); err != nil {
		t.Fatal(err)
	}
	upgradeDue := time.Now().UTC().Truncate(time.Second)
	if _, err := harness.ownerPool.Exec(ctx, `INSERT INTO public.digest_windows
(tenant_id,schedule_id,due_at,window_end_at,status) VALUES ($1::uuid,$2::uuid,$3,$3,'pending')`, upgradeTenant, upgradeSchedule, upgradeDue); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.ownerPool.Exec(ctx, `INSERT INTO public.reports
(tenant_id,schedule_id,due_at,window_end_at,trigger,status) VALUES ($1::uuid,$2::uuid,$3,$3,'scheduled','failed')`, upgradeTenant, upgradeSchedule, upgradeDue); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyMigrations(ctx, migrator, assets); err != nil {
		t.Fatalf("apply migrations after 0007: %v", err)
	}
	var upgradedTenantName, upgradedScheduleName string
	if err := harness.ownerPool.QueryRow(ctx, `SELECT tenant_name,schedule_name FROM public.reports
WHERE tenant_id=$1::uuid AND schedule_id=$2::uuid`, upgradeTenant, upgradeSchedule).Scan(&upgradedTenantName, &upgradedScheduleName); err != nil {
		t.Fatal(err)
	}
	if upgradedTenantName != "기존 고객" || upgradedScheduleName != "기존 예약" {
		t.Fatalf("backfilled names tenant=%q schedule=%q", upgradedTenantName, upgradedScheduleName)
	}
	if err := store.ApplyMigrations(ctx, migrator, assets); err != nil {
		t.Fatalf("reapply migrations: %v", err)
	}
	assertMigrationLedger(t, ctx, harness.ownerPool, assets)

	runtimeConfig, err := pgx.ParseConfig(runtimeURL)
	if err != nil {
		t.Fatalf("parse TEST_POSTGRES_RUNTIME_URL: %v", err)
	}
	runtimeConfig.Database = harness.database
	harness.runtimePool, err = OpenRuntimePool(ctx, runtimeConfig.ConnString())
	if err != nil {
		t.Fatalf("open migrated database with runtime role: %v", err)
	}

	for _, table := range []string{"reports", "report_items"} {
		var enabled, forced bool
		if err := harness.ownerPool.QueryRow(ctx, `SELECT relrowsecurity,relforcerowsecurity
FROM pg_catalog.pg_class WHERE oid=($1::text)::regclass`, "public."+table).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s RLS enabled=%t forced=%t", table, enabled, forced)
		}
	}
	assertReportRuntimePrivileges(t, ctx, harness.runtimePool)

	tenantA := insertTenant(t, ctx, harness.ownerPool, "Report Schema A")
	tenantB := insertTenant(t, ctx, harness.ownerPool, "Report Schema B")
	var scheduleA string
	if err := harness.ownerPool.QueryRow(ctx, `INSERT INTO public.schedules
(tenant_id,name,hour,minute,timezone) VALUES ($1::uuid,'리포트',7,0,'Asia/Seoul') RETURNING id::text`, tenantA).Scan(&scheduleA); err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().UTC().Truncate(time.Second)
	windowEnd := dueAt.Add(time.Hour)
	if _, err := harness.ownerPool.Exec(ctx, `INSERT INTO public.digest_windows
(tenant_id,schedule_id,due_at,window_end_at,status) VALUES ($1::uuid,$2::uuid,$3,$4,'pending')`, tenantA, scheduleA, dueAt, windowEnd); err != nil {
		t.Fatal(err)
	}
	var reportA string
	if err := harness.ownerPool.QueryRow(ctx, `INSERT INTO public.reports
(tenant_id,schedule_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status)
VALUES ($1::uuid,$2::uuid,$3,$4,'Report Schema A','리포트','scheduled','generating') RETURNING id::text`, tenantA, scheduleA, dueAt, windowEnd).Scan(&reportA); err != nil {
		t.Fatal(err)
	}
	_, err = harness.ownerPool.Exec(ctx, `INSERT INTO public.reports
(tenant_id,schedule_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status)
VALUES ($1::uuid,$2::uuid,$3,$4,'Report Schema A','리포트','scheduled','generating')`, tenantA, scheduleA, dueAt, windowEnd)
	assertPostgresCode(t, err, "23505")

	_, err = harness.ownerPool.Exec(ctx, `INSERT INTO public.reports
(tenant_id,schedule_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status)
VALUES ($1::uuid,$2::uuid,$3,$4,'Report Schema A','리포트','scheduled','generating')`, tenantA, scheduleA, dueAt.Add(time.Hour), windowEnd.Add(time.Hour))
	assertPostgresCode(t, err, "23503")

	for range 2 {
		if _, err := harness.ownerPool.Exec(ctx, `INSERT INTO public.reports
(tenant_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status)
VALUES ($1::uuid,$2,$2,'Report Schema A','수동','manual','generating')`, tenantA, dueAt); err != nil {
			t.Fatal(err)
		}
	}
	var manualCount int
	if err := harness.ownerPool.QueryRow(ctx, `SELECT count(*) FROM public.reports
WHERE tenant_id=$1::uuid AND trigger='manual' AND due_at=$2`, tenantA, dueAt).Scan(&manualCount); err != nil {
		t.Fatal(err)
	}
	if manualCount != 2 {
		t.Fatalf("manual report count=%d, want 2", manualCount)
	}

	_, err = harness.ownerPool.Exec(ctx, `INSERT INTO public.reports
(tenant_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status,attempts)
VALUES ($1::uuid,$2,$2,'Report Schema A','수동','manual','failed',4)`, tenantA, dueAt)
	assertPostgresCode(t, err, "23514")
	_, err = harness.ownerPool.Exec(ctx, `INSERT INTO public.reports
(tenant_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status)
VALUES ($1::uuid,$2,$2,'','수동','manual','failed')`, tenantA, dueAt)
	assertPostgresCode(t, err, "23514")
	_, err = harness.ownerPool.Exec(ctx, `INSERT INTO public.reports
(tenant_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status)
VALUES ($1::uuid,$2,$2,'Report Schema A','   ','manual','failed')`, tenantA, dueAt)
	assertPostgresCode(t, err, "23514")
	var reportB string
	if err := harness.ownerPool.QueryRow(ctx, `INSERT INTO public.reports
(tenant_id,due_at,window_end_at,tenant_name,schedule_name,trigger,status)
VALUES ($1::uuid,$2,$2,'Report Schema B','수동','manual','generating') RETURNING id::text`, tenantB, dueAt).Scan(&reportB); err != nil {
		t.Fatal(err)
	}
	for _, report := range []struct{ tenantID, reportID, title string }{
		{tenantA, reportA, "A snapshot"},
		{tenantB, reportB, "B snapshot"},
	} {
		if _, err := harness.ownerPool.Exec(ctx, `INSERT INTO public.report_items
(tenant_id,report_id,ordinal,match_id,notice_id,title,category,amount,deadline_at,source_url,rule_name,reasons)
VALUES ($1::uuid,$2::uuid,1,gen_random_uuid(),gen_random_uuid(),$3,'goods',0,$4,'','','[]'::jsonb)`, report.tenantID, report.reportID, report.title, dueAt); err != nil {
			t.Fatalf("insert snapshot without live match/notice rows: %v", err)
		}
	}

	tx, err := harness.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('app.tenant_id',$1,true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	var visible, cross int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.reports`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.reports WHERE tenant_id=$1::uuid`, tenantB).Scan(&cross); err != nil {
		t.Fatal(err)
	}
	if visible != 3 || cross != 0 {
		t.Fatalf("tenant A sees total=%d cross-tenant=%d", visible, cross)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.report_items`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.report_items WHERE tenant_id=$1::uuid`, tenantB).Scan(&cross); err != nil {
		t.Fatal(err)
	}
	if visible != 1 || cross != 0 {
		t.Fatalf("tenant A sees report items total=%d cross-tenant=%d", visible, cross)
	}
	command, err := tx.Exec(ctx, `UPDATE public.reports SET status='failed' WHERE tenant_id=$1::uuid`, tenantB)
	if err != nil {
		t.Fatal(err)
	}
	if command.RowsAffected() != 0 {
		t.Fatalf("cross-tenant update affected %d rows", command.RowsAffected())
	}
}

func assertReportRuntimePrivileges(t *testing.T, ctx context.Context, runtime *pgxpool.Pool) {
	t.Helper()
	for _, check := range []struct {
		table, privilege string
		want             bool
	}{
		{"reports", "SELECT", true},
		{"reports", "INSERT", true},
		{"reports", "UPDATE", true},
		{"reports", "DELETE", false},
		{"report_items", "SELECT", true},
		{"report_items", "INSERT", true},
		{"report_items", "UPDATE", false},
		{"report_items", "DELETE", true},
	} {
		var allowed bool
		if err := runtime.QueryRow(ctx, `SELECT pg_catalog.has_table_privilege(current_user,$1,$2)`, "public."+check.table, check.privilege).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed != check.want {
			t.Fatalf("runtime %s on %s=%t, want %t", check.privilege, check.table, allowed, check.want)
		}
	}
}
