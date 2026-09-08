package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"namo/internal/model"
	"namo/internal/report"
	"namo/internal/store"
	appweb "namo/internal/web"
	"namo/migrations"
)

func TestPostgresReportRepositoryClaimsFencesSnapshotsAndIsolatesTenants(t *testing.T) {
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

	now := time.Now().In(time.FixedZone("KST", 9*60*60)).Truncate(time.Second)
	tenantA := insertTenant(t, ctx, harness.ownerPool, "../고객 이름")
	tenantB := insertTenant(t, ctx, harness.ownerPool, "빈 고객")
	tenantC := insertTenant(t, ctx, harness.ownerPool, "경쟁 고객")
	t.Run("collection waiters release pool capacity", func(t *testing.T) {
		testCollectionWaiterReleasesPool(t, ctx, harness.runtimePool, tenantA)
	})
	scheduleA := insertReportSchedule(t, ctx, harness, tenantA, "예약 A")
	scheduleB := insertReportSchedule(t, ctx, harness, tenantB, "예약 B")
	_ = insertReportSchedule(t, ctx, harness, tenantC, "예약 C")
	insertReportMatch(t, ctx, harness, tenantA, now)
	insertReportMatch(t, ctx, harness, tenantC, now)

	digestWorks, err := repository.DueDigests(ctx, now)
	if err != nil {
		t.Fatalf("claim digest before report: %v", err)
	}
	var completedBeforeReport bool
	for _, work := range digestWorks {
		if work.TenantID == tenantC {
			continue
		}
		if err := repository.CompleteNoop(ctx, work.TenantID, work.ScheduleID, work.DueAt, work.WindowEnd); err != nil {
			t.Fatalf("complete digest before report: %v", err)
		}
		completedBeforeReport = completedBeforeReport || work.TenantID == tenantA
	}
	if !completedBeforeReport {
		t.Fatal("tenant A digest did not complete before report claim")
	}

	start := make(chan struct{})
	results := make(chan []ReportWork, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			works, err := repository.ClaimDueReports(ctx, now)
			results <- works
			errors <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent scheduled claim: %v", err)
		}
	}
	var scheduled []ReportWork
	var digestWinsRace ReportWork
	for works := range results {
		for _, work := range works {
			if work.TenantID == tenantA {
				scheduled = append(scheduled, work)
			}
			if work.TenantID == tenantC {
				digestWinsRace = work
			}
		}
	}
	if len(scheduled) != 1 || len(scheduled[0].Notices) != 1 {
		t.Fatalf("scheduled claims=%+v", scheduled)
	}
	if digestWinsRace.ReportID == "" {
		t.Fatal("tenant C report was not claimed before digest completion")
	}
	if err := repository.CompleteNoop(ctx, digestWinsRace.TenantID, digestWinsRace.ScheduleID, digestWinsRace.DueAt, digestWinsRace.WindowEnd); err != nil {
		t.Fatalf("digest completion after report claim: %v", err)
	}
	raceArtifact := ReportArtifact{RelativePath: digestWinsRace.RelativePath, SHA256: strings.Repeat("c", 64), NoticeCount: len(digestWinsRace.Notices)}
	if err := repository.FinalizeReport(ctx, digestWinsRace, raceArtifact, now.Add(time.Minute)); err != nil {
		t.Fatalf("finalize report after digest completed same window: %v", err)
	}
	first := scheduled[0]
	if strings.Contains(first.RelativePath, "고객") || strings.Contains(first.RelativePath, "..") {
		t.Fatalf("tenant-entered name leaked into path %q", first.RelativePath)
	}
	var recipientCount int
	if err := harness.ownerPool.QueryRow(ctx, `SELECT count(*) FROM public.recipients WHERE tenant_id=$1::uuid`, tenantA).Scan(&recipientCount); err != nil || recipientCount != 0 {
		t.Fatalf("recipient precondition count=%d err=%v", recipientCount, err)
	}

	if _, err := harness.ownerPool.Exec(ctx, `UPDATE public.tenants SET name='변경 고객' WHERE id=$1::uuid`, tenantA); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.ownerPool.Exec(ctx, `UPDATE public.schedules SET name='변경 예약' WHERE tenant_id=$1::uuid AND id=$2::uuid`, tenantA, first.ScheduleID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.ownerPool.Exec(ctx, `UPDATE public.reports SET claimed_at=pg_catalog.clock_timestamp()-interval '16 minutes'
WHERE tenant_id=$1::uuid AND id=$2::uuid`, tenantA, first.ReportID); err != nil {
		t.Fatal(err)
	}
	second, ok, err := repository.ReclaimReport(ctx, tenantA, first.ReportID)
	if err != nil || !ok || second.Attempts != 2 || second.ClaimToken == first.ClaimToken || second.TenantName != first.TenantName || second.ScheduleName != first.ScheduleName {
		t.Fatalf("stale reclaim=%+v ok=%t err=%v", second, ok, err)
	}
	if err := repository.FinalizeReportFailure(ctx, first, context.Canceled); err == nil {
		t.Fatal("stale worker finalized failure")
	}
	if err := repository.FinalizeReportFailure(ctx, second, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	third, ok, err := repository.ReclaimReport(ctx, tenantA, first.ReportID)
	if err != nil || !ok || third.Attempts != 3 {
		t.Fatalf("third claim=%+v ok=%t err=%v", third, ok, err)
	}
	if err := repository.FinalizeReportFailure(ctx, third, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repository.ReclaimReport(ctx, tenantA, first.ReportID); err != nil || ok {
		t.Fatalf("fourth automatic claim ok=%t err=%v", ok, err)
	}
	retried, ok, err := repository.RetryReport(ctx, tenantA, first.ReportID, now.Add(time.Minute))
	if err != nil || !ok || retried.Attempts != 1 || retried.ReportID != first.ReportID || retried.RelativePath != first.RelativePath || len(retried.Notices) != len(first.Notices) {
		t.Fatalf("operator retry=%+v ok=%t err=%v", retried, ok, err)
	}
	artifact := ReportArtifact{RelativePath: retried.RelativePath, SHA256: strings.Repeat("a", 64), NoticeCount: len(retried.Notices)}
	if err := repository.FinalizeReport(ctx, retried, artifact, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var reportStatus, windowStatus string
	var lastSuccess time.Time
	if err := harness.ownerPool.QueryRow(ctx, `SELECT r.status,w.status,s.last_success_at
FROM public.reports r
JOIN public.digest_windows w ON w.tenant_id=r.tenant_id AND w.schedule_id=r.schedule_id AND w.due_at=r.due_at AND w.window_end_at=r.window_end_at
JOIN public.schedules s ON s.tenant_id=r.tenant_id AND s.id=r.schedule_id
WHERE r.tenant_id=$1::uuid AND r.id=$2::uuid`, tenantA, first.ReportID).Scan(&reportStatus, &windowStatus, &lastSuccess); err != nil {
		t.Fatal(err)
	}
	if reportStatus != "generated" || windowStatus != "completed" || !lastSuccess.Equal(first.WindowEnd) {
		t.Fatalf("scheduled final status report=%s window=%s last=%v", reportStatus, windowStatus, lastSuccess)
	}

	var tenantBReports, tenantBCompleted int
	if err := harness.ownerPool.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM public.reports WHERE tenant_id=$1::uuid),
  (SELECT count(*) FROM public.digest_windows WHERE tenant_id=$1::uuid AND schedule_id=$2::uuid AND status='completed')`,
		tenantB, scheduleB).Scan(&tenantBReports, &tenantBCompleted); err != nil {
		t.Fatal(err)
	}
	if tenantBReports != 0 || tenantBCompleted != 1 {
		t.Fatalf("empty scheduled tenant reports=%d completed=%d", tenantBReports, tenantBCompleted)
	}

	manual, ok, err := repository.ClaimManualReport(ctx, tenantA, now.Add(3*time.Minute))
	// The empty rule matches both globally shared notices, even though only one
	// stored match existed for A before the fresh manual evaluation.
	if err != nil || !ok || manual.Trigger != "manual" || len(manual.Notices) != 2 {
		t.Fatalf("manual claim=%+v ok=%t err=%v", manual, ok, err)
	}
	if _, ok, err := repository.ReclaimReport(ctx, tenantB, manual.ReportID); err != nil || ok {
		t.Fatalf("cross-tenant reclaim ok=%t err=%v", ok, err)
	}
	if _, err := harness.ownerPool.Exec(ctx, `DELETE FROM public.matches WHERE tenant_id=$1::uuid`, tenantA); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinalizeReportFailure(ctx, manual, context.Canceled); err != nil {
		t.Fatal(err)
	}
	manualRetry, ok, err := repository.RetryReport(ctx, tenantA, manual.ReportID, now.Add(4*time.Minute))
	if err != nil || !ok || len(manualRetry.Notices) != 2 || len(manualRetry.Notices[0].Matches) != 1 {
		t.Fatalf("manual snapshot retry=%+v ok=%t err=%v", manualRetry, ok, err)
	}
	manualArtifact := ReportArtifact{RelativePath: manualRetry.RelativePath, SHA256: strings.Repeat("b", 64), NoticeCount: 2}
	if err := repository.FinalizeReport(ctx, manualRetry, manualArtifact, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var afterManual time.Time
	if err := harness.ownerPool.QueryRow(ctx, `SELECT last_success_at FROM public.schedules WHERE tenant_id=$1::uuid AND id=$2::uuid`, tenantA, scheduleA).Scan(&afterManual); err != nil {
		t.Fatal(err)
	}
	if !afterManual.Equal(lastSuccess) {
		t.Fatalf("manual report advanced schedule from %v to %v", lastSuccess, afterManual)
	}
	t.Run("delete generated reports", func(t *testing.T) {
		testDeleteGeneratedReports(t, ctx, harness, repository, tenantA, tenantB, first, manualRetry, now)
	})
	t.Run("delete failed reports", func(t *testing.T) {
		testDeleteFailedReports(t, ctx, harness, repository, tenantB, now)
	})
}

func testDeleteFailedReports(t *testing.T, ctx context.Context, harness *releasePostgresHarness, repository *PostgresRepository, otherTenant string, now time.Time) {
	t.Helper()
	files, err := report.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	service := &WebService{Repository: repository, ReportStore: files}
	for _, trigger := range []string{"manual", "scheduled"} {
		t.Run(trigger, func(t *testing.T) {
			tenantID := insertTenant(t, ctx, harness.ownerPool, "삭제 테스트")
			insertReportMatch(t, ctx, harness, tenantID, now)
			scheduleID := insertReportSchedule(t, ctx, harness, tenantID, "삭제 예약")
			var work ReportWork
			if trigger == "manual" {
				var claimed bool
				work, claimed, err = repository.ClaimManualReport(ctx, tenantID, now)
				if err != nil || !claimed {
					t.Fatalf("manual claim=%v err=%v", claimed, err)
				}
			} else {
				works, err := repository.ClaimDueReports(ctx, now)
				if err != nil {
					t.Fatal(err)
				}
				for _, candidate := range works {
					if candidate.TenantID == tenantID {
						work = candidate
					}
				}
				if work.ReportID == "" {
					t.Fatal("scheduled report not claimed")
				}
			}
			if err := repository.FinalizeReportFailure(ctx, work, context.Canceled); err != nil {
				t.Fatal(err)
			}
			// Attempts=1 tests resurrection guards independently of the retry cap.
			name, err := reportRelativePath(work)
			if err != nil {
				t.Fatal(err)
			}
			if trigger == "manual" {
				if _, err := files.Write(ctx, name, []byte("published before finalization failed")); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.DeleteReport(ctx, appweb.RequestContext{Role: "tenant_admin", TenantID: otherTenant}, work.ReportID); !errors.Is(err, appweb.ErrReportNotFound) {
				t.Fatalf("cross tenant delete=%v", err)
			}
			rc := appweb.RequestContext{Role: "tenant_admin", TenantID: tenantID}
			for range 2 {
				if err := service.DeleteReport(ctx, rc, work.ReportID); err != nil {
					t.Fatal(err)
				}
			}
			if file, _, err := files.Open(name); !errors.Is(err, fs.ErrNotExist) {
				if file != nil {
					file.Close()
				}
				t.Fatalf("artifact still exists: %v", err)
			}
			var deleted bool
			var items int
			if err := harness.ownerPool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,(SELECT count(*) FROM public.report_items WHERE report_id=r.id) FROM public.reports r WHERE id=$1::uuid`, work.ReportID).Scan(&deleted, &items); err != nil {
				t.Fatal(err)
			}
			if !deleted || items != 0 {
				t.Fatalf("deleted=%v items=%d", deleted, items)
			}
			if _, claimed, err := repository.RetryReport(ctx, tenantID, work.ReportID, now); err != nil || claimed {
				t.Fatalf("retry deleted report: claimed=%v err=%v", claimed, err)
			}
			if _, claimed, err := repository.ReclaimReport(ctx, tenantID, work.ReportID); err != nil || claimed {
				t.Fatalf("reclaim deleted report: claimed=%v err=%v", claimed, err)
			}
			if trigger == "scheduled" {
				if _, err := repository.ClaimDueReports(ctx, now); err != nil {
					t.Fatal(err)
				}
				var status string
				var lastSuccess time.Time
				if err := harness.ownerPool.QueryRow(ctx, `SELECT w.status,s.last_success_at FROM public.digest_windows w JOIN public.schedules s ON s.tenant_id=w.tenant_id AND s.id=w.schedule_id WHERE w.tenant_id=$1::uuid AND w.schedule_id=$2::uuid AND w.due_at=$3`, tenantID, scheduleID, work.DueAt).Scan(&status, &lastSuccess); err != nil {
					t.Fatal(err)
				}
				if status != "completed" || !lastSuccess.Equal(work.WindowEnd) {
					t.Fatalf("deleted window blocks schedule: %s %v", status, lastSuccess)
				}
				if err := repository.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
					_, claimed, err := claimScheduledReport(ctx, tx, tenantID, "삭제 테스트", reportSchedule{ID: scheduleID}, work.DueAt, work.WindowEnd, now)
					if claimed {
						t.Fatal("scheduler resurrected tombstone")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := repository.ClaimDueReports(ctx, now.Add(24*time.Hour)); err != nil {
					t.Fatal(err)
				}
				var windows int
				if err := harness.ownerPool.QueryRow(ctx, `SELECT count(*) FROM public.digest_windows WHERE tenant_id=$1::uuid AND schedule_id=$2::uuid`, tenantID, scheduleID).Scan(&windows); err != nil {
					t.Fatal(err)
				}
				if windows < 2 {
					t.Fatal("next scheduled window blocked by deleted failed report")
				}
			}
		})
	}
}

func testDeleteGeneratedReports(t *testing.T, ctx context.Context, harness *releasePostgresHarness, repository *PostgresRepository, tenantA, tenantB string, scheduled, manual ReportWork, now time.Time) {
	t.Helper()
	files, err := report.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	service := &WebService{Repository: repository, ReportStore: files}
	rc := appweb.RequestContext{Role: "tenant_admin", TenantID: tenantA}
	for _, work := range []ReportWork{scheduled, manual} {
		name, err := reportRelativePath(work)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := files.Write(ctx, name, []byte("report")); err != nil {
			t.Fatal(err)
		}
		if _, err := harness.ownerPool.Exec(ctx, `UPDATE public.reports SET relative_path=$1 WHERE id=$2::uuid`, name, work.ReportID); err != nil {
			t.Fatal(err)
		}
		if err := service.DeleteReport(ctx, appweb.RequestContext{Role: "tenant_admin", TenantID: tenantB}, work.ReportID); !errors.Is(err, appweb.ErrReportNotFound) {
			t.Fatalf("cross tenant delete=%v", err)
		}
		failure := errors.New("file deletion unavailable")
		err = repository.withTenant(ctx, tenantA, func(tx pgx.Tx) error {
			return deleteReport(ctx, tx, tenantA, work.ReportID, func(context.Context, string) error { return failure })
		})
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		var deleted bool
		var items int
		if err := harness.ownerPool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,(SELECT count(*) FROM public.report_items WHERE report_id=r.id) FROM public.reports r WHERE id=$1::uuid`, work.ReportID).Scan(&deleted, &items); err != nil {
			t.Fatal(err)
		}
		if deleted || items == 0 {
			t.Fatal("file failure did not roll back database deletion")
		}
		for range 2 {
			if err := service.DeleteReport(ctx, rc, work.ReportID); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := files.Open(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("file remains: %v", err)
		}
		if err := harness.ownerPool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,(SELECT count(*) FROM public.report_items WHERE report_id=r.id) FROM public.reports r WHERE id=$1::uuid`, work.ReportID).Scan(&deleted, &items); err != nil {
			t.Fatal(err)
		}
		if !deleted || items != 0 {
			t.Fatalf("deleted=%v items=%d", deleted, items)
		}
		if _, err := service.OpenReport(ctx, rc, work.ReportID); !errors.Is(err, appweb.ErrReportNotFound) {
			t.Fatalf("deleted download=%v", err)
		}
		if _, claimed, err := repository.RetryReport(ctx, tenantA, work.ReportID, now); err != nil || claimed {
			t.Fatalf("deleted report retry=%v err=%v", claimed, err)
		}
		// A worker holding an obsolete claim must not publish after deletion.
		if _, err := (ReportRunner{Repository: repository, Writer: files}).runClaimed(ctx, work); err == nil {
			t.Fatal("stale worker published a deleted report")
		}
		if file, _, err := files.Open(name); !errors.Is(err, fs.ErrNotExist) {
			if file != nil {
				file.Close()
			}
			t.Fatalf("deleted file resurrected: %v", err)
		}
	}
	works, err := repository.ClaimDueReports(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, work := range works {
		if work.TenantID == tenantA && work.DueAt.Equal(scheduled.DueAt) {
			t.Fatal("deleted scheduled report recreated")
		}
	}
	err = repository.withTenant(ctx, tenantA, func(tx pgx.Tx) error {
		data := appweb.AppData{}
		if err := loadTenantReports(ctx, tx, tenantA, &data); err != nil {
			return err
		}
		for _, item := range data.Reports {
			if item.ID == scheduled.ReportID || item.ID == manual.ReportID {
				t.Fatal("deleted report listed")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testCollectionWaiterReleasesPool(t *testing.T, parent context.Context, runtimePool *pgxpool.Pool, tenantID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	config := runtimePool.Config()
	config.MaxConns, config.MinConns = 2, 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	collector, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = collector.Rollback(context.Background()) }()
	if err := tryCollectionLock(ctx, collector); err != nil {
		t.Fatal(err)
	}
	before := pool.Stat().AcquireCount()
	result := make(chan error, 1)
	repository := &PostgresRepository{Pool: pool}
	go func() {
		result <- repository.withTenant(ctx, tenantID, func(tx pgx.Tx) error { return tryCollectionLock(ctx, tx) })
	}()
	// Wait until the contender has actually acquired the only remaining slot.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for pool.Stat().AcquireCount() == before {
		select {
		case err := <-result:
			t.Fatalf("contender finished while collection lock was held: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	readCtx, stopRead := context.WithTimeout(ctx, time.Second)
	defer stopRead()
	var one int
	if err := pool.QueryRow(readCtx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		cancel()
		<-result
		t.Fatalf("collector could not use the pool while a request waited: value=%d err=%v", one, err)
	}
	if err := collector.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("contender did not resume after collection: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func insertReportSchedule(t *testing.T, ctx context.Context, harness *releasePostgresHarness, tenantID, name string) string {
	t.Helper()
	var id string
	if err := harness.ownerPool.QueryRow(ctx, `INSERT INTO public.schedules
    (tenant_id,name,hour,minute,timezone,weekdays)
VALUES ($1::uuid,$2,7,0,'Asia/Seoul',ARRAY[0,1,2,3,4,5,6]::smallint[])
RETURNING id::text`, tenantID, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertReportMatch(t *testing.T, ctx context.Context, harness *releasePostgresHarness, tenantID string, now time.Time) {
	t.Helper()
	var filterID, noticeID string
	if err := harness.ownerPool.QueryRow(ctx, `INSERT INTO public.filters (tenant_id,name,rules)
VALUES ($1::uuid,'서울 필터','{}'::jsonb) RETURNING id::text`, tenantID).Scan(&filterID); err != nil {
		t.Fatal(err)
	}
	notice := model.Notice{
		Category: model.CategoryGoods, BidNumber: "R-1", BidSequence: "00", Title: "예약 공고",
		Agency: "기관", Region: "서울", SourceURL: "https://example.test/report/1", Amount: 100,
		PostedAt: now.Add(-2 * time.Hour), Deadline: now.Add(24 * time.Hour),
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.ownerPool.QueryRow(ctx, `INSERT INTO public.notices
    (identity_hash,revision_hash,source_id,title,published_at,deadline_at,payload,collected_at)
VALUES ($1,$2,'R-1','예약 공고',$3,$4,$5,$6) RETURNING id::text`,
		[]byte("report-identity-"+tenantID), []byte("report-revision-"+tenantID), now.Add(-2*time.Hour), now.Add(24*time.Hour), payload, now.Add(-time.Hour)).Scan(&noticeID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.ownerPool.Exec(ctx, `INSERT INTO public.matches
    (tenant_id,filter_id,notice_id,reasons,created_at)
VALUES ($1::uuid,$2::uuid,$3::uuid,$4,$5)`, tenantID, filterID, noticeID,
		[]byte(`{"reasons":["region"],"details":[{"Code":"region","RuleValue":"서울"}]}`), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}
