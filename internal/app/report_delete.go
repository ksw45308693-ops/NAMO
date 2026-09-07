package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"namo/internal/report"
	appweb "namo/internal/web"
)

func (s *WebService) DeleteReport(ctx context.Context, rc appweb.RequestContext, reportID string) error {
	if err := requireReportAdmin(rc); err != nil {
		return err
	}
	if s == nil || s.Repository == nil || s.ReportStore == nil {
		return errors.New("report deletion is unavailable")
	}
	return s.Repository.withTenant(ctx, rc.TenantID, func(tx pgx.Tx) error {
		return deleteReport(ctx, tx, rc.TenantID, reportID, s.ReportStore.Remove)
	})
}

func deleteReport(ctx context.Context, tx reportStore, tenantID, reportID string, remove func(context.Context, string) error) error {
	work := ReportWork{TenantID: tenantID, ReportID: reportID}
	var status string
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT relative_path,deleted_at IS NOT NULL,status,due_at,trigger
FROM public.reports
WHERE tenant_id=$1::uuid AND id=$2::uuid AND status IN ('generated','failed')
FOR UPDATE`, tenantID, reportID).Scan(&work.RelativePath, &deleted, &status, &work.DueAt, &work.Trigger)
	if errors.Is(err, pgx.ErrNoRows) {
		return appweb.ErrReportNotFound
	}
	if err != nil {
		return fmt.Errorf("lock report for deletion: %w", err)
	}
	if deleted {
		return nil
	}
	relativePath := work.RelativePath
	if status == "failed" && relativePath == "reports/"+tenantID+"/"+reportID+".html" {
		// A failed finalization leaves the initial placeholder in the database.
		// Reconstruct this report's actual filename from its saved filter names,
		// without requiring the rest of a possibly malformed snapshot to render.
		rows, err := tx.Query(ctx, `SELECT DISTINCT rule_name FROM public.report_items
WHERE tenant_id=$1::uuid AND report_id=$2::uuid`, tenantID, reportID)
		if err != nil {
			return fmt.Errorf("load failed report filter names: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			work.Notices = append(work.Notices, report.Notice{Matches: []report.Match{{RuleName: name}}})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		relativePath, err = reportRelativePath(work)
		if err != nil {
			return err
		}
	}
	if !strings.HasPrefix(relativePath, tenantID+"/") {
		return errors.New("stored report path is outside its tenant")
	}
	// Keep the terminal row and its scheduled-window identity: removing it
	// would cause the scheduler to recreate the same report. Roll back these
	// changes if file removal fails; a missing file permits retry after a crash.
	if _, err := tx.Exec(ctx, `UPDATE public.reports
SET deleted_at=pg_catalog.clock_timestamp(),relative_path='',sha256='',last_error=NULL
WHERE tenant_id=$1::uuid AND id=$2::uuid`, tenantID, reportID); err != nil {
		return fmt.Errorf("mark report deleted: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.report_items
WHERE tenant_id=$1::uuid AND report_id=$2::uuid`, tenantID, reportID); err != nil {
		return fmt.Errorf("delete report snapshot: %w", err)
	}
	return remove(ctx, relativePath)
}
