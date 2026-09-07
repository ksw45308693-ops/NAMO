package app

import (
	"context"
	"time"

	"namo/internal/report"
)

type ReportWork struct {
	ReportID, TenantID, TenantName, ScheduleID, ScheduleName string
	Trigger, RelativePath, ClaimToken                        string
	DueAt, WindowEnd                                         time.Time
	WindowStart                                              *time.Time
	Attempts                                                 int
	Notices                                                  []report.Notice
}

type ReportRepository interface {
	ClaimDueReports(context.Context, time.Time) ([]ReportWork, error)
	ReclaimReport(context.Context, string, string) (ReportWork, bool, error)
	ClaimManualReport(context.Context, string, time.Time) (ReportWork, bool, error)
	RetryReport(context.Context, string, string, time.Time) (ReportWork, bool, error)
	WithReportClaim(context.Context, ReportWork, func() error) error
	FinalizeReport(context.Context, ReportWork, ReportArtifact, time.Time) error
	FinalizeReportFailure(context.Context, ReportWork, error) error
}

type ReportArtifact struct {
	RelativePath string
	SHA256       string
	NoticeCount  int
}

type ReportWriter interface {
	Write(context.Context, string, []byte) (report.FileResult, error)
}

type ReportOutcome struct {
	ID, RelativePath string
	Created          bool
	NoticeCount      int
}
