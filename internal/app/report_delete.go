package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
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
	var relativePath string
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT relative_path,deleted_at IS NOT NULL
FROM public.reports
WHERE tenant_id=$1::uuid AND id=$2::uuid AND status='generated'
FOR UPDATE`, tenantID, reportID).Scan(&relativePath, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return appweb.ErrReportNotFound
	}
	if err != nil {
		return fmt.Errorf("lock report for deletion: %w", err)
	}
	if deleted {
		return nil
	}
	if !strings.HasPrefix(relativePath, tenantID+"/") {
		return errors.New("stored report path is outside its tenant")
	}
	// Keep the generated row and its scheduled-window identity: removing it
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
