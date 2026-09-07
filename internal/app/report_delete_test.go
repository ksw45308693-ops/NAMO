package app

import (
	"context"
	"errors"
	"io/fs"
	"namo/internal/report"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	appweb "namo/internal/web"
)

func TestDeleteReportRequiresTenantAdmin(t *testing.T) {
	for _, rc := range []appweb.RequestContext{{Role: "member", TenantID: "tenant-a"}, {Role: "platform_admin", TenantID: "tenant-a"}, {Role: "tenant_admin"}} {
		if err := (&WebService{}).DeleteReport(context.Background(), rc, testWebReportID); err == nil || !strings.Contains(err.Error(), "tenant administrator") {
			t.Fatalf("role check: %v", err)
		}
	}
}

func TestDeleteReportLocksTenantRowAndKeepsTombstone(t *testing.T) {
	stub := &reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error {
		*(dest[0].(*string)) = "tenant-a/2026/09/report/report.html"
		*(dest[1].(*bool)) = false
		return nil
	}}}
	removed := ""
	err := deleteReport(context.Background(), stub, "tenant-a", testWebReportID, func(_ context.Context, name string) error {
		if len(stub.execs) != 2 {
			t.Fatal("file removed before database statements succeeded")
		}
		removed = name
		return nil
	})
	if err != nil || removed != "tenant-a/2026/09/report/report.html" {
		t.Fatalf("removed=%q err=%v", removed, err)
	}
	for _, want := range []string{"tenant_id=$1::uuid", "id=$2::uuid", "status IN ('generated','failed')", "FOR UPDATE"} {
		if !strings.Contains(stub.rowQueries[0], want) {
			t.Errorf("lock query missing %s", want)
		}
	}
	for i, args := range stub.execArgs {
		if !reflect.DeepEqual(args, []any{"tenant-a", testWebReportID}) {
			t.Errorf("statement %d scope=%v", i, args)
		}
	}
	if !strings.Contains(stub.execs[0], "UPDATE public.reports") || !strings.Contains(stub.execs[0], "deleted_at") || !strings.Contains(stub.execs[1], "DELETE FROM public.report_items") {
		t.Fatalf("statements=%v", stub.execs)
	}
	if !strings.Contains(tenantReportsSQL, "deleted_at IS NULL") {
		t.Fatal("deleted reports still listed")
	}
}

func TestDeleteFailedReportWithPlaceholderAndMissingFile(t *testing.T) {
	files, err := report.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	for _, hasFile := range []bool{false, true} {
		stub := &reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error {
			*(dest[0].(*string)) = "reports/tenant-a/" + testWebReportID + ".html"
			*(dest[1].(*bool)) = false
			if len(dest) > 2 {
				*(dest[2].(*string)) = "failed"
				*(dest[3].(*time.Time)) = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
				*(dest[4].(*string)) = "scheduled"
			}
			return nil
		}}}
		name := "tenant-a/2026/09/" + testWebReportID + "/20260907_통합보고서.html"
		if hasFile {
			if _, err := files.Write(context.Background(), name, []byte("orphan")); err != nil {
				t.Fatal(err)
			}
		}
		if err := deleteReport(context.Background(), stub, "tenant-a", testWebReportID, files.Remove); err != nil {
			t.Fatal(err)
		}
		if file, _, err := files.Open(name); !errors.Is(err, fs.ErrNotExist) {
			if file != nil {
				file.Close()
			}
			t.Fatalf("failed report artifact remains: %v", err)
		}
		if len(stub.execs) != 2 || strings.Contains(strings.Join(stub.queries, "\n"), "public.schedules") {
			t.Fatal("deletion must not lock schedules or alter active delivery windows")
		}
	}
}

func TestScheduledDeletedReportClosesWindowWithoutReclaim(t *testing.T) {
	stub := &reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error {
		*(dest[0].(*bool)) = true
		if len(dest) > 1 {
			*(dest[1].(*bool)) = true
		}
		return nil
	}}}
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	_, claimed, err := claimScheduledReport(context.Background(), stub, "tenant-a", "tenant", reportSchedule{ID: "schedule-a"}, now, now, now)
	if err != nil || claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	if len(stub.rowQueries) != 1 || len(stub.execs) != 1 {
		t.Fatal("deleted report was reclaimed or its pending window was not handled")
	}
	for _, want := range []string{"active_lease", "NOT EXISTS (SELECT 1 FROM active_lease)", "UPDATE public.digest_windows", "UPDATE public.schedules"} {
		if !strings.Contains(stub.execs[0], want) {
			t.Fatalf("terminal window handling missing %s", want)
		}
	}
}

func TestDeletedReportCannotBeRetriedOrReclaimed(t *testing.T) {
	for _, retry := range []bool{false, true} {
		stub := &reportStoreStub{}
		var claimed bool
		var err error
		if retry {
			_, claimed, err = retryReport(context.Background(), stub, "tenant-a", testWebReportID, time.Now())
		} else {
			_, claimed, err = reclaimReport(context.Background(), stub, "tenant-a", testWebReportID)
		}
		if err != nil || claimed {
			t.Fatalf("claimed=%v err=%v", claimed, err)
		}
		if !strings.Contains(stub.rowQueries[0], "deleted_at IS NULL") {
			t.Fatal("deleted report can be claimed again")
		}
	}
}

func TestDeleteReportDoesNotRemoveOnMissingDeletedOrForeignPath(t *testing.T) {
	for _, tt := range []struct {
		name, path string
		deleted    bool
		rowErr     error
		wantErr    bool
	}{
		{"missing or wrong tenant", "", false, pgx.ErrNoRows, true},
		{"already deleted", "", true, nil, false},
		{"foreign stored path", "tenant-b/report.html", false, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error {
				if tt.rowErr != nil {
					return tt.rowErr
				}
				*(dest[0].(*string)) = tt.path
				*(dest[1].(*bool)) = tt.deleted
				return nil
			}}}
			err := deleteReport(context.Background(), stub, "tenant-a", testWebReportID, func(context.Context, string) error { t.Fatal("unexpected file removal"); return nil })
			if (err != nil) != tt.wantErr || len(stub.execs) != 0 {
				t.Fatalf("err=%v writes=%v", err, stub.execs)
			}
			if tt.rowErr == pgx.ErrNoRows && !errors.Is(err, appweb.ErrReportNotFound) {
				t.Fatal(err)
			}
		})
	}
}

func TestDeleteReportReturnsFileFailureForTransactionRollback(t *testing.T) {
	stub := &reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error {
		*(dest[0].(*string)) = "tenant-a/report.html"
		*(dest[1].(*bool)) = false
		return nil
	}}}
	failure := errors.New("permission denied")
	err := deleteReport(context.Background(), stub, "tenant-a", testWebReportID, func(context.Context, string) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("file failure swallowed: %v", err)
	}
}

type reportDeleteFailingStore struct {
	reportStoreStub
	failAt int
	calls  int
}

func (s *reportDeleteFailingStore) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.calls++
	if s.calls == s.failAt {
		return pgconn.CommandTag{}, errors.New("database write failed")
	}
	return s.reportStoreStub.Exec(ctx, sql, args...)
}

func TestDeleteReportDoesNotRemoveFileWhenDatabaseWriteFails(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		stub := &reportDeleteFailingStore{failAt: failAt, reportStoreStub: reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error {
			*(dest[0].(*string)) = "tenant-a/report.html"
			*(dest[1].(*bool)) = false
			return nil
		}}}}
		err := deleteReport(context.Background(), stub, "tenant-a", testWebReportID, func(context.Context, string) error { t.Fatal("file removed after database failure"); return nil })
		if err == nil {
			t.Fatal("database error swallowed")
		}
	}
}

func TestReportPublicationChecksClaimBeforeWriting(t *testing.T) {
	for _, rowErr := range []error{nil, pgx.ErrNoRows} {
		stub := &reportStoreStub{rowResults: []reportRowFunc{func(dest ...any) error {
			*(dest[0].(*string)) = "report-1"
			return rowErr
		}}}
		published := false
		work := scheduledReportWork(1)
		err := withReportClaim(context.Background(), stub, work, func() error { published = true; return nil })
		if published != (rowErr == nil) || !errors.Is(err, rowErr) {
			t.Fatalf("published=%v err=%v", published, err)
		}
		if !reflect.DeepEqual(stub.rowArgs[0], []any{work.TenantID, work.ReportID, work.ClaimToken}) {
			t.Fatal(stub.rowArgs)
		}
		for _, want := range []string{"tenant_id=$1::uuid", "id=$2::uuid", "claim_token=$3::uuid", "status='generating'", "deleted_at IS NULL", "FOR UPDATE"} {
			if !strings.Contains(stub.rowQueries[0], want) {
				t.Fatalf("publication query missing %q", want)
			}
		}
	}
}
