package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

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
	for _, want := range []string{"tenant_id=$1::uuid", "id=$2::uuid", "status='generated'", "FOR UPDATE"} {
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
