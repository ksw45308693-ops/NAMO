package app

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"namo/internal/report"
)

type ReportResult struct {
	Generated, Failed, Skipped int
	TenantRuns                 []ReportTenantResult
}

type ReportTenantResult struct {
	TenantID                   string
	Generated, Failed, Skipped int
	Err                        error
}

type ReportRunner struct {
	Repository ReportRepository
	Writer     ReportWriter
	Now        func() time.Time
}

func (r ReportRunner) Run(ctx context.Context) (ReportResult, error) {
	var result ReportResult
	if r.Repository == nil || r.Writer == nil {
		return result, errors.New("report dependencies are required")
	}
	work, err := r.Repository.ClaimDueReports(ctx, r.now())
	if err != nil {
		return result, fmt.Errorf("claim due reports: %w", err)
	}
	tenants := make(map[string]*ReportTenantResult)
	var order []string
	for _, item := range work {
		tenant := tenants[item.TenantID]
		if tenant == nil {
			tenant = &ReportTenantResult{TenantID: item.TenantID}
			tenants[item.TenantID] = tenant
			order = append(order, item.TenantID)
		}
		_, err := r.runClaimed(ctx, item)
		if err != nil {
			result.Failed++
			tenant.Failed++
			tenant.Err = errors.Join(tenant.Err, err)
			continue
		}
		result.Generated++
		tenant.Generated++
	}
	var runErr error
	for _, tenantID := range order {
		tenant := *tenants[tenantID]
		result.TenantRuns = append(result.TenantRuns, tenant)
		runErr = errors.Join(runErr, tenant.Err)
	}
	return result, runErr
}

func (r ReportRunner) RunManual(ctx context.Context, tenantID string) (ReportOutcome, error) {
	if r.Repository == nil || r.Writer == nil {
		return ReportOutcome{}, errors.New("report dependencies are required")
	}
	work, claimed, err := r.Repository.ClaimManualReport(ctx, tenantID, r.now())
	if err != nil {
		return ReportOutcome{}, fmt.Errorf("claim manual report: %w", err)
	}
	if !claimed {
		return ReportOutcome{}, nil
	}
	return r.runClaimed(ctx, work)
}

func (r ReportRunner) Retry(ctx context.Context, tenantID, reportID string) (ReportOutcome, error) {
	if r.Repository == nil || r.Writer == nil {
		return ReportOutcome{}, errors.New("report dependencies are required")
	}
	work, claimed, err := r.Repository.RetryReport(ctx, tenantID, reportID, r.now())
	if err != nil {
		return ReportOutcome{}, fmt.Errorf("claim report retry: %w", err)
	}
	if !claimed {
		return ReportOutcome{ID: reportID}, nil
	}
	return r.runClaimed(ctx, work)
}

func (r ReportRunner) runClaimed(ctx context.Context, work ReportWork) (ReportOutcome, error) {
	outcome := ReportOutcome{ID: work.ReportID, NoticeCount: len(work.Notices)}
	for {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		if work.Attempts < 1 || work.Attempts > 3 {
			return outcome, fmt.Errorf("report %s has invalid attempt %d", work.ReportID, work.Attempts)
		}
		relativePath, err := reportRelativePath(work)
		if err != nil {
			return outcome, err
		}
		work.RelativePath = relativePath
		outcome.RelativePath = relativePath
		body, operationErr := report.BuildHTML(report.Document{
			TenantName: work.TenantName, ScheduleName: work.ScheduleName, Trigger: work.Trigger,
			DueAt: work.DueAt, WindowStart: work.WindowStart, WindowEnd: work.WindowEnd, Notices: work.Notices,
		})
		var fileResult report.FileResult
		if operationErr == nil {
			fileResult, operationErr = r.Writer.Write(ctx, relativePath, body)
			if operationErr == nil && (filepath.Clean(fileResult.RelativePath) != filepath.Clean(relativePath) || strings.TrimSpace(fileResult.SHA256) == "") {
				operationErr = errors.New("report writer returned an invalid artifact")
			}
		}
		if operationErr == nil {
			artifact := ReportArtifact{RelativePath: relativePath, SHA256: fileResult.SHA256, NoticeCount: len(work.Notices)}
			operationErr = r.Repository.FinalizeReport(ctx, work, artifact, r.now())
			if operationErr == nil {
				outcome.Created = true
				return outcome, nil
			}
			operationErr = fmt.Errorf("finalize report: %w", operationErr)
		} else {
			operationErr = fmt.Errorf("create report: %w", operationErr)
		}
		if failureErr := r.Repository.FinalizeReportFailure(ctx, work, operationErr); failureErr != nil {
			operationErr = errors.Join(operationErr, failureErr)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return outcome, errors.Join(operationErr, ctxErr)
		}
		if work.Attempts == 3 {
			return outcome, operationErr
		}
		next, reclaimed, reclaimErr := r.Repository.ReclaimReport(ctx, work.TenantID, work.ReportID)
		if reclaimErr != nil {
			return outcome, errors.Join(operationErr, fmt.Errorf("reclaim report: %w", reclaimErr))
		}
		if !reclaimed {
			return outcome, operationErr
		}
		work = next
	}
}

func (r ReportRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func reportRelativePath(work ReportWork) (string, error) {
	if !safeReportPathID(work.TenantID) || !safeReportPathID(work.ReportID) || work.DueAt.IsZero() {
		return "", errors.New("report path requires safe tenant and report IDs and a due time")
	}
	dueAt := work.DueAt.In(time.FixedZone("KST", 9*60*60))
	name := "namo-" + dueAt.Format("20060102-150405")
	switch work.Trigger {
	case "scheduled":
	case "manual":
		name += "-" + work.ReportID
	default:
		return "", fmt.Errorf("unsupported report trigger %q", work.Trigger)
	}
	parent := path.Join(work.TenantID, dueAt.Format("2006"), dueAt.Format("01"))
	legacyPath := path.Join(parent, name+".html")
	if work.RelativePath == legacyPath {
		return legacyPath, nil
	}
	name = dueAt.Format("20060102") + "_" + reportFilterLabel(work.Notices) + "보고서.html"
	return path.Join(parent, work.ReportID, name), nil
}

func reportFilterLabel(notices []report.Notice) string {
	name := ""
	for _, notice := range notices {
		for _, match := range notice.Matches {
			candidate := strings.TrimSpace(match.RuleName)
			if candidate == "" {
				continue
			}
			if name != "" && name != candidate {
				return "통합"
			}
			name = candidate
		}
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	var label strings.Builder
	count := 0
	for _, r := range name {
		// Leave room for the date and suffix within a 255-byte filename.
		if count == 60 || label.Len()+utf8.RuneLen(r) > 220 {
			break
		}
		label.WriteRune(r)
		count++
	}
	if label.Len() == 0 {
		return "통합"
	}
	return label.String()
}

func safeReportPathID(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, `/\\`)
}
