package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"namo/internal/report"
)

type reportRepositoryFake struct {
	due             []ReportWork
	dueErr          error
	manual          ReportWork
	manualClaimed   bool
	retry           ReportWork
	retryClaimed    bool
	reclaims        []ReportWork
	reclaimCalls    int
	manualCalls     int
	retryCalls      int
	finalizeCalls   []ReportWork
	finalizeErrors  []error
	failureAttempts []int
	publishErr      error
}

func (f *reportRepositoryFake) WithReportClaim(_ context.Context, _ ReportWork, publish func() error) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	return publish()
}

func TestStaleReportWorkerCannotRecreateDeletedFile(t *testing.T) {
	store, err := report.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	work := scheduledReportWork(1)
	name, err := reportRelativePath(work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(context.Background(), name, []byte("completed by replacement worker")); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	repo := &reportRepositoryFake{due: []ReportWork{work}, publishErr: errors.New("claim no longer owned")}
	_, err = (ReportRunner{Repository: repo, Writer: store}).Run(context.Background())
	if err == nil {
		t.Fatal("stale worker succeeded")
	}
	if file, _, err := store.Open(name); !errors.Is(err, fs.ErrNotExist) {
		if file != nil {
			file.Close()
		}
		t.Fatalf("stale worker recreated deleted report: %v", err)
	}
}

func (f *reportRepositoryFake) ClaimDueReports(context.Context, time.Time) ([]ReportWork, error) {
	return append([]ReportWork(nil), f.due...), f.dueErr
}

func (f *reportRepositoryFake) ReclaimReport(_ context.Context, tenantID, reportID string) (ReportWork, bool, error) {
	f.reclaimCalls++
	if len(f.reclaims) == 0 {
		return ReportWork{}, false, nil
	}
	work := f.reclaims[0]
	f.reclaims = f.reclaims[1:]
	if work.TenantID != tenantID || work.ReportID != reportID {
		return ReportWork{}, false, errors.New("reclaim identity changed")
	}
	return work, true, nil
}

func (f *reportRepositoryFake) ClaimManualReport(context.Context, string, time.Time) (ReportWork, bool, error) {
	f.manualCalls++
	return f.manual, f.manualClaimed, nil
}

func (f *reportRepositoryFake) RetryReport(context.Context, string, string, time.Time) (ReportWork, bool, error) {
	f.retryCalls++
	return f.retry, f.retryClaimed, nil
}

func (f *reportRepositoryFake) FinalizeReport(_ context.Context, work ReportWork, _ ReportArtifact, _ time.Time) error {
	f.finalizeCalls = append(f.finalizeCalls, work)
	if len(f.finalizeErrors) == 0 {
		return nil
	}
	err := f.finalizeErrors[0]
	f.finalizeErrors = f.finalizeErrors[1:]
	return err
}

func (f *reportRepositoryFake) FinalizeReportFailure(_ context.Context, work ReportWork, _ error) error {
	f.failureAttempts = append(f.failureAttempts, work.Attempts)
	return nil
}

type reportWriterFake struct {
	failures int
	calls    int
	paths    []string
	bodies   [][]byte
	cancel   context.CancelFunc
}

type recordingReportWriter struct {
	inner   ReportWriter
	paths   []string
	results []report.FileResult
}

func (w *recordingReportWriter) Write(ctx context.Context, relativePath string, body []byte) (report.FileResult, error) {
	w.paths = append(w.paths, relativePath)
	result, err := w.inner.Write(ctx, relativePath, body)
	if err == nil {
		w.results = append(w.results, result)
	}
	return result, err
}

func (w *reportWriterFake) Write(ctx context.Context, relativePath string, body []byte) (report.FileResult, error) {
	w.calls++
	w.paths = append(w.paths, relativePath)
	w.bodies = append(w.bodies, append([]byte(nil), body...))
	if w.cancel != nil {
		w.cancel()
		return report.FileResult{}, ctx.Err()
	}
	if w.calls <= w.failures {
		return report.FileResult{}, errors.New("disk unavailable")
	}
	hash := sha256.Sum256(body)
	return report.FileResult{RelativePath: relativePath, SHA256: hex.EncodeToString(hash[:])}, nil
}

func scheduledReportWork(attempt int) ReportWork {
	dueAt := time.Date(2026, 9, 2, 15, 4, 5, 0, time.UTC)
	return ReportWork{
		ReportID: "report-1", TenantID: "tenant-1", TenantName: "../고객 이름",
		ScheduleID: "schedule-1", ScheduleName: "매일", Trigger: "scheduled",
		RelativePath: "old/path.html", ClaimToken: "token", DueAt: dueAt,
		WindowEnd: dueAt, Attempts: attempt,
		Notices: []report.Notice{{ID: "notice-1", Title: "회계감사 용역", Category: "service"}},
	}
}

func TestReportRunnerWritesDeterministicScheduledPathAndFinalizes(t *testing.T) {
	now := time.Date(2026, 9, 3, 0, 5, 0, 0, time.FixedZone("KST", 9*60*60))
	repository := &reportRepositoryFake{due: []ReportWork{scheduledReportWork(1)}}
	writer := &reportWriterFake{}
	runner := ReportRunner{Repository: repository, Writer: writer, Now: func() time.Time { return now }}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Generated != 1 || result.Failed != 0 || len(result.TenantRuns) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantPath := "tenant-1/2026/09/report-1/20260903_통합보고서.html"
	if writer.calls != 1 || writer.paths[0] != wantPath {
		t.Fatalf("writes=%d paths=%q", writer.calls, writer.paths)
	}
	if strings.Contains(writer.paths[0], "고객") || !strings.Contains(string(writer.bodies[0]), "회계감사 용역") {
		t.Fatalf("path=%q body=%s", writer.paths[0], writer.bodies[0])
	}
	if len(repository.finalizeCalls) != 1 || repository.finalizeCalls[0].RelativePath != wantPath {
		t.Fatalf("finalized=%+v", repository.finalizeCalls)
	}
}

func TestReportRunnerRetriesWriterFailureAtMostThreeTimesInOneRun(t *testing.T) {
	repository := &reportRepositoryFake{
		due:      []ReportWork{scheduledReportWork(1)},
		reclaims: []ReportWork{scheduledReportWork(2), scheduledReportWork(3)},
	}
	writer := &reportWriterFake{failures: 2}
	result, err := (ReportRunner{Repository: repository, Writer: writer}).Run(context.Background())
	if err != nil || result.Generated != 1 || writer.calls != 3 || repository.reclaimCalls != 2 {
		t.Fatalf("result=%+v writes=%d reclaims=%d err=%v", result, writer.calls, repository.reclaimCalls, err)
	}
	if len(repository.failureAttempts) != 2 || repository.failureAttempts[0] != 1 || repository.failureAttempts[1] != 2 {
		t.Fatalf("failure attempts=%v", repository.failureAttempts)
	}
}

func TestReportRunnerLeavesFailureAfterThirdAttempt(t *testing.T) {
	repository := &reportRepositoryFake{
		due:      []ReportWork{scheduledReportWork(1)},
		reclaims: []ReportWork{scheduledReportWork(2), scheduledReportWork(3)},
	}
	writer := &reportWriterFake{failures: 3}
	result, err := (ReportRunner{Repository: repository, Writer: writer}).Run(context.Background())
	if err == nil || result.Failed != 1 || writer.calls != 3 || repository.reclaimCalls != 2 {
		t.Fatalf("result=%+v writes=%d reclaims=%d err=%v", result, writer.calls, repository.reclaimCalls, err)
	}
	if len(repository.failureAttempts) != 3 || repository.failureAttempts[2] != 3 {
		t.Fatalf("failure attempts=%v", repository.failureAttempts)
	}
}

func TestReportRunnerRecoversFinalizeFailureWithSamePathAndSHA(t *testing.T) {
	repository := &reportRepositoryFake{
		due: []ReportWork{scheduledReportWork(1)}, reclaims: []ReportWork{scheduledReportWork(2)},
		finalizeErrors: []error{errors.New("database unavailable"), nil},
	}
	store, err := report.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	writer := &recordingReportWriter{inner: store}
	result, err := (ReportRunner{Repository: repository, Writer: writer}).Run(context.Background())
	if err != nil || result.Generated != 1 || len(writer.results) != 2 || repository.reclaimCalls != 1 {
		t.Fatalf("result=%+v writes=%d reclaims=%d err=%v", result, len(writer.results), repository.reclaimCalls, err)
	}
	if writer.paths[0] != writer.paths[1] || writer.results[0].SHA256 != writer.results[1].SHA256 {
		t.Fatalf("recovery changed artifact path=%q hashes=%+v", writer.paths, writer.results)
	}
}

func TestReportRunnerStopsRetriesWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repository := &reportRepositoryFake{due: []ReportWork{scheduledReportWork(1)}, reclaims: []ReportWork{scheduledReportWork(2)}}
	writer := &reportWriterFake{cancel: cancel}
	result, err := (ReportRunner{Repository: repository, Writer: writer}).Run(ctx)
	if !errors.Is(err, context.Canceled) || result.Failed != 1 || writer.calls != 1 || repository.reclaimCalls != 0 {
		t.Fatalf("result=%+v writes=%d reclaims=%d err=%v", result, writer.calls, repository.reclaimCalls, err)
	}
}

func TestReportRunnerSkipsDuplicateScheduledAndEmptyManualClaims(t *testing.T) {
	repository := &reportRepositoryFake{}
	writer := &reportWriterFake{}
	runner := ReportRunner{Repository: repository, Writer: writer}
	result, err := runner.Run(context.Background())
	if err != nil || result.Generated != 0 || result.Failed != 0 || writer.calls != 0 {
		t.Fatalf("scheduled result=%+v writes=%d err=%v", result, writer.calls, err)
	}
	outcome, err := runner.RunManual(context.Background(), "tenant-1")
	if err != nil || outcome.Created || writer.calls != 0 || repository.manualCalls != 1 {
		t.Fatalf("manual outcome=%+v writes=%d calls=%d err=%v", outcome, writer.calls, repository.manualCalls, err)
	}
}

func TestReportRunnerManualAndAdministratorRetryReuseFixedReport(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 4, 5, 0, time.UTC)
	manual := scheduledReportWork(1)
	manual.ReportID, manual.Trigger, manual.ScheduleID, manual.ScheduleName, manual.DueAt = "report-2", "manual", "", "수동", now
	retry := manual
	repository := &reportRepositoryFake{manual: manual, manualClaimed: true, retry: retry, retryClaimed: true}
	writer := &reportWriterFake{}
	runner := ReportRunner{Repository: repository, Writer: writer, Now: func() time.Time { return now }}

	outcome, err := runner.RunManual(context.Background(), "tenant-1")
	if err != nil || !outcome.Created || outcome.ID != "report-2" {
		t.Fatalf("manual outcome=%+v err=%v", outcome, err)
	}
	wantPath := "tenant-1/2026/09/report-2/20260903_통합보고서.html"
	if outcome.RelativePath != wantPath {
		t.Fatalf("manual path=%q", outcome.RelativePath)
	}

	repository.reclaims = []ReportWork{func() ReportWork { value := retry; value.Attempts = 2; return value }(), func() ReportWork { value := retry; value.Attempts = 3; return value }()}
	writer.failures = writer.calls + 3
	retried, err := runner.Retry(context.Background(), "tenant-1", "report-2")
	if err == nil || retried.ID != "report-2" || repository.retryCalls != 1 || repository.manualCalls != 1 || repository.reclaimCalls != 2 || writer.calls != 4 {
		t.Fatalf("retry outcome=%+v manual=%d retry=%d reclaims=%d writes=%d err=%v", retried, repository.manualCalls, repository.retryCalls, repository.reclaimCalls, writer.calls, err)
	}
	for _, body := range writer.bodies[1:] {
		if !strings.Contains(string(body), "회계감사 용역") {
			t.Fatalf("administrator retry did not reuse fixed items: %s", body)
		}
	}
}

func TestReportFileNamesUseSnapshotFiltersAndKoreanDate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		filters []string
		want    string
	}{
		{"single", []string{"데이터"}, "20260907_데이터보고서.html"},
		{"repeated", []string{"데이터", "데이터"}, "20260907_데이터보고서.html"},
		{"multiple", []string{"데이터", "회계"}, "20260907_통합보고서.html"},
		{"missing", nil, "20260907_통합보고서.html"},
		{"blank", []string{"  ", "..."}, "20260907_통합보고서.html"},
		{"trimmed", []string{" 데이터 ", "데이터"}, "20260907_데이터보고서.html"},
		{"unsafe", []string{"../데이터\\분석:*?\"<>|\n"}, "20260907__데이터_분석_______보고서.html"},
		{"long", []string{strings.Repeat("가", 100)}, "20260907_" + strings.Repeat("가", 60) + "보고서.html"},
		{"long unicode", []string{strings.Repeat("😀", 100)}, "20260907_" + strings.Repeat("😀", 55) + "보고서.html"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			work := scheduledReportWork(1)
			work.DueAt = time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
			work.Notices = nil
			for _, name := range tt.filters {
				work.Notices = append(work.Notices, report.Notice{Matches: []report.Match{{RuleName: name}}})
			}
			for _, trigger := range []string{"manual", "scheduled"} {
				work.Trigger = trigger
				got, err := reportRelativePath(work)
				if err != nil || path.Base(got) != tt.want || path.Dir(got) != "tenant-1/2026/09/report-1" {
					t.Fatalf("%s path=%q err=%v want filename=%q", trigger, got, err, tt.want)
				}
			}
		})
	}
}

func TestSameDateReportsKeepSeparateFilesAndRetryPath(t *testing.T) {
	store, err := report.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := scheduledReportWork(1)
	first.Notices[0].Matches = []report.Match{{RuleName: "데이터"}}
	second := first
	second.ReportID = "report-2"
	second.Notices = append([]report.Notice(nil), first.Notices...)
	second.Notices[0].Title = "다른 공고"
	repository := &reportRepositoryFake{due: []ReportWork{first, second}}
	runner := ReportRunner{Repository: repository, Writer: store}
	result, err := runner.Run(context.Background())
	if err != nil || result.Generated != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, work := range repository.finalizeCalls {
		file, _, err := store.Open(work.RelativePath)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
		if filepath.Base(work.RelativePath) != "20260903_데이터보고서.html" {
			t.Fatal(work.RelativePath)
		}
	}
	repository.retry, repository.retryClaimed = repository.finalizeCalls[0], true
	outcome, err := runner.Retry(context.Background(), first.TenantID, first.ReportID)
	if err != nil || !outcome.Created || outcome.RelativePath != repository.retry.RelativePath {
		t.Fatalf("retry=%+v err=%v", outcome, err)
	}
}

func TestRetryKeepsAlreadyStoredLegacyReportPath(t *testing.T) {
	for _, trigger := range []string{"scheduled", "manual"} {
		work := scheduledReportWork(1)
		work.Trigger = trigger
		work.RelativePath = "tenant-1/2026/09/namo-20260903-000405"
		if trigger == "manual" {
			work.RelativePath += "-report-1"
		}
		work.RelativePath += ".html"
		got, err := reportRelativePath(work)
		if err != nil || got != work.RelativePath {
			t.Fatalf("path=%q err=%v", got, err)
		}
	}
}
