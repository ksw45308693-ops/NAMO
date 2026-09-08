package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"namo/internal/auth"
	"namo/internal/config"
	"namo/internal/report"
)

func TestRuntimeExecuteDispatchesOneKnownCommand(t *testing.T) {
	var called string
	runtime := &Runtime{Operations: RuntimeOperations{
		CollectOnce: func(_ context.Context, _ config.Config, args []string) error {
			called = strings.Join(args, ",")
			return nil
		},
	}}

	err := runtime.Execute(context.Background(), "collect-once", config.Config{}, []string{"unexpected"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if called != "unexpected" {
		t.Fatalf("handler args = %q", called)
	}
}

func TestRuntimeExecuteRejectsUnavailableHandler(t *testing.T) {
	err := (&Runtime{}).Execute(context.Background(), "serve", config.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("Execute() error = %v, want unavailable serve handler", err)
	}
}

func TestRuntimeExecuteDispatchesGenerateReportAndBlocksSendTestMail(t *testing.T) {
	var generated, mailed bool
	runtime := &Runtime{Operations: RuntimeOperations{
		GenerateReport: func(context.Context, config.Config, []string) error { generated = true; return nil },
		SendTestMail:   func(context.Context, config.Config, []string) error { mailed = true; return nil },
	}}
	if err := runtime.Execute(context.Background(), "generate-report", config.Config{}, nil); err != nil || !generated {
		t.Fatalf("generate-report dispatch error=%v called=%v", err, generated)
	}
	err := runtime.Execute(context.Background(), "send-test-mail", config.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "메일 기능은 현재 비활성화되어 있습니다") || mailed {
		t.Fatalf("send-test-mail error=%v called=%v", err, mailed)
	}
}

func TestParseGenerateReportRequiresCanonicalPGXUUIDAndNoPositionals(t *testing.T) {
	want := "11111111-1111-1111-1111-111111111111"
	got, err := parseGenerateReportOptions([]string{"--tenant", strings.ToUpper(want)})
	if err != nil || got != want {
		t.Fatalf("parseGenerateReportOptions()=%q,%v", got, err)
	}
	for _, args := range [][]string{{}, {"--tenant", "not-a-uuid"}, {"--tenant", want, "extra"}} {
		if _, err := parseGenerateReportOptions(args); err == nil {
			t.Fatalf("parseGenerateReportOptions(%q) unexpectedly succeeded", args)
		}
	}
}

func TestGenerateReportRejectsRootBeforeOpeningDatabaseOrCreatingFiles(t *testing.T) {
	reportDir := t.TempDir()
	runtime := NewRuntime(nil, io.Discard, io.Discard)
	runtime.CurrentUser = func() (*user.User, error) { return &user.User{Uid: "0"}, nil }
	err := runtime.generateReport(context.Background(), config.Config{
		DatabaseURL: "postgres://must-not-be-opened.invalid/monitor", DeliveryMode: "report", ReportDir: reportDir,
	}, []string{"--tenant", "11111111-1111-1111-1111-111111111111"})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("generateReport() error=%v, want root rejection", err)
	}
	entries, readErr := filepath.Glob(filepath.Join(reportDir, "*"))
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("files created before root rejection: %q, %v", entries, readErr)
	}
}

func TestGenerateReportValidatesReportDeliveryBeforeOpeningDatabase(t *testing.T) {
	runtime := NewRuntime(nil, io.Discard, io.Discard)
	runtime.CurrentUser = func() (*user.User, error) { return &user.User{Uid: "1001"}, nil }
	err := runtime.generateReport(context.Background(), config.Config{
		DatabaseURL: "postgres://must-not-be-opened.invalid/monitor", DeliveryMode: "mail", ReportDir: t.TempDir(),
	}, []string{"--tenant", "11111111-1111-1111-1111-111111111111"})
	if err == nil || !strings.Contains(err.Error(), "DELIVERY_MODE") {
		t.Fatalf("generateReport() error=%v, want delivery configuration rejection", err)
	}
}

func TestRuntimeBuildServeComponentsSharesFileStoreAndUsesReportJobOnly(t *testing.T) {
	store, err := report.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repository := &PostgresRepository{}
	var uiStore, jobStore *report.FileStore
	var digestCalled bool
	runtime := &Runtime{
		BuildUI: func(_ context.Context, gotRepository *PostgresRepository, gotStore *report.FileStore, _ config.Config) (http.Handler, error) {
			if gotRepository != repository {
				t.Fatal("UI repository changed")
			}
			uiStore = gotStore
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil
		},
		BuildReportJob: func(gotRepository *PostgresRepository, gotStore *report.FileStore, _ config.Config) (ScheduledJob, error) {
			if gotRepository != repository {
				t.Fatal("report repository changed")
			}
			jobStore = gotStore
			return func(context.Context, time.Time) error { return nil }, nil
		},
		BuildDigestJob: func(*PostgresRepository, config.Config) (ScheduledJob, error) {
			digestCalled = true
			return nil, errors.New("digest must stay disconnected")
		},
		BuildCollector: func(*PostgresRepository, config.Config) (CollectionRunner, error) {
			return func(context.Context) (CollectionResult, error) { return CollectionResult{}, nil }, nil
		},
	}
	if _, _, _, err := runtime.buildServeComponents(context.Background(), repository, store, config.Config{}); err != nil {
		t.Fatalf("buildServeComponents() error=%v", err)
	}
	if uiStore != store || jobStore != store || uiStore != jobStore || digestCalled {
		t.Fatalf("stores ui=%p job=%p want=%p digestCalled=%v", uiStore, jobStore, store, digestCalled)
	}
}

func TestRuntimeReportSchedulerRunsCollectionAndReport(t *testing.T) {
	collectionCalls, reportCalls := 0, 0
	scheduler := newServeScheduler(
		func(context.Context) (CollectionResult, error) { collectionCalls++; return CollectionResult{}, nil },
		func(context.Context, time.Time) error { reportCalls++; return nil },
	)
	if err := scheduler.Tick(context.Background(), time.Now()); err != nil {
		t.Fatalf("Tick() error=%v", err)
	}
	if collectionCalls != 1 || reportCalls != 1 {
		t.Fatalf("collection calls=%d report calls=%d", collectionCalls, reportCalls)
	}
}

func TestGenerateReportWithRepositoryClosesOneStoreOnceAndNeverPrintsRoot(t *testing.T) {
	reportDir := t.TempDir()
	var output strings.Builder
	closeCalls := 0
	runtime := &Runtime{
		Output: &output,
		BuildManualReport: func(_ *PostgresRepository, _ *report.FileStore, _ config.Config) (ManualReportRunner, error) {
			return func(context.Context, string) (ReportOutcome, error) {
				return ReportOutcome{Created: true, NoticeCount: 7, RelativePath: filepath.Join("tenant", "2026", "09", "report.html")}, nil
			}, nil
		},
		CloseReportStore: func(store *report.FileStore) error { closeCalls++; return store.Close() },
	}
	err := runtime.generateReportWithRepository(context.Background(), config.Config{ReportDir: reportDir}, "11111111-1111-1111-1111-111111111111", &PostgresRepository{})
	if err != nil || closeCalls != 1 {
		t.Fatalf("generateReportWithRepository() error=%v closeCalls=%d", err, closeCalls)
	}
	if strings.Contains(output.String(), reportDir) || !strings.Contains(output.String(), filepath.Join("tenant", "2026", "09", "report.html")) || !strings.Contains(output.String(), "7") {
		t.Fatalf("manual report output leaks root or omits result: %q", output.String())
	}
}

func TestGenerateReportHidesReportRootFromOpenCloseAndExecutionErrors(t *testing.T) {
	tenantID := "11111111-1111-1111-1111-111111111111"
	for _, test := range []struct {
		name      string
		configure func(*Runtime, string)
	}{
		{
			name: "open",
			configure: func(runtime *Runtime, root string) {
				runtime.OpenReportStore = func(string) (*report.FileStore, error) {
					return nil, errors.New("injected open failure: " + root)
				}
			},
		},
		{
			name: "close",
			configure: func(runtime *Runtime, root string) {
				runtime.BuildManualReport = func(*PostgresRepository, *report.FileStore, config.Config) (ManualReportRunner, error) {
					return func(context.Context, string) (ReportOutcome, error) { return ReportOutcome{}, nil }, nil
				}
				runtime.CloseReportStore = func(store *report.FileStore) error {
					_ = store.Close()
					return errors.New("injected close failure: " + root)
				}
			},
		},
		{
			name: "execution",
			configure: func(runtime *Runtime, root string) {
				runtime.BuildManualReport = func(*PostgresRepository, *report.FileStore, config.Config) (ManualReportRunner, error) {
					return func(context.Context, string) (ReportOutcome, error) {
						return ReportOutcome{}, errors.New("injected execution failure: " + root)
					}, nil
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reportDir := t.TempDir()
			runtime := NewRuntime(nil, io.Discard, io.Discard)
			test.configure(runtime, reportDir)
			err := runtime.generateReportWithRepository(context.Background(), config.Config{ReportDir: reportDir}, tenantID, &PostgresRepository{})
			if err == nil {
				t.Fatal("generateReportWithRepository() unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), reportDir) || strings.Contains(err.Error(), "injected") {
				t.Fatalf("generateReportWithRepository() leaked internal error: %v", err)
			}
		})
	}
}

func TestParseCreateAdminReadsPasswordOnlyFromExplicitStdin(t *testing.T) {
	options, password, err := parseCreateAdminOptions(
		[]string{"--email", " Admin@Example.COM ", "--name", " 운영자 ", "--password-stdin"},
		strings.NewReader("correct horse battery staple\r\n"),
		func(string) (io.ReadCloser, error) { return nil, errors.New("must not open a file") },
	)
	if err != nil {
		t.Fatalf("parseCreateAdminOptions() error = %v", err)
	}
	if options.Email != "admin@example.com" || options.DisplayName != "운영자" {
		t.Fatalf("options = %#v", options)
	}
	if password != "correct horse battery staple" {
		t.Fatalf("password = %q", password)
	}
}

func TestParseCreateAdminAcceptsPlatformUsername(t *testing.T) {
	options, _, err := parseCreateAdminOptions(
		[]string{"--email", " Admin ", "--name", "운영자", "--password-stdin"},
		strings.NewReader("correct horse battery staple\n"),
		func(string) (io.ReadCloser, error) { return nil, errors.New("must not open a file") },
	)
	if err != nil {
		t.Fatalf("platform username rejected: %v", err)
	}
	if options.Email != "admin" {
		t.Fatalf("platform login = %q, want admin", options.Email)
	}
}

func TestParseCreateAdminRequiresExactlyOneSecretSource(t *testing.T) {
	for _, args := range [][]string{
		{"--email", "admin@example.com"},
		{"--email", "admin@example.com", "--password-stdin", "--password-file", "secret.txt"},
		{"--email", "admin@example.com", "--password", "visible"},
	} {
		_, _, err := parseCreateAdminOptions(args, strings.NewReader("long-enough-password"), nil)
		if err == nil {
			t.Fatalf("parseCreateAdminOptions(%q) unexpectedly succeeded", args)
		}
	}
}

func TestParseCreateAdminRejectsWeakOrOversizedPasswords(t *testing.T) {
	for _, password := range []string{"too-short", strings.Repeat("x", 73)} {
		_, _, err := parseCreateAdminOptions(
			[]string{"--email", "admin@example.com", "--password-stdin"},
			strings.NewReader(password), nil,
		)
		if err == nil || !strings.Contains(err.Error(), "password") {
			t.Fatalf("password length %d error = %v", len(password), err)
		}
	}
}

type capturingAdminExecer struct {
	args []any
	err  error
}

func (e *capturingAdminExecer) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	e.args = append([]any(nil), args...)
	return pgconn.CommandTag{}, e.err
}

func TestInsertPlatformAdminStoresBcryptHash(t *testing.T) {
	store := &capturingAdminExecer{}
	err := insertPlatformAdmin(context.Background(), store, CreateAdminOptions{
		Email: "admin@example.com", DisplayName: "운영자",
	}, "correct horse battery staple")
	if err != nil {
		t.Fatalf("insertPlatformAdmin() error = %v", err)
	}
	if len(store.args) != 3 {
		t.Fatalf("Exec args = %#v", store.args)
	}
	hash, ok := store.args[2].(string)
	if !ok || hash == "correct horse battery staple" || !auth.CheckPassword(hash, "correct horse battery staple") {
		t.Fatalf("stored password is not a valid bcrypt hash: %#v", store.args[2])
	}
}

type scriptedMailer struct {
	failures int
	calls    int
	from     string
	to       string
	message  []byte
}

func (m *scriptedMailer) Send(_ context.Context, from, to string, message []byte) error {
	m.calls++
	m.from, m.to, m.message = from, to, append([]byte(nil), message...)
	if m.calls <= m.failures {
		return errors.New("temporary SMTP failure")
	}
	return nil
}

func TestSendTestMailRetriesAndBuildsSafeMessage(t *testing.T) {
	mailer := &scriptedMailer{failures: 2}
	err := sendTestMail(context.Background(), mailer, "monitor@example.com", "recipient@example.com")
	if err != nil {
		t.Fatalf("sendTestMail() error = %v", err)
	}
	if mailer.calls != 3 {
		t.Fatalf("Send() calls = %d, want 3", mailer.calls)
	}
	if mailer.from != "monitor@example.com" || mailer.to != "recipient@example.com" {
		t.Fatalf("from/to = %q/%q", mailer.from, mailer.to)
	}
	if !bytes.Contains(mailer.message, []byte("MIME-Version: 1.0")) {
		t.Fatalf("message lacks MIME headers: %q", mailer.message)
	}
}

func TestParseSendTestMailRequiresPlainMailbox(t *testing.T) {
	to, err := parseTestMailOptions([]string{"--to", "recipient@example.com"})
	if err != nil || to != "recipient@example.com" {
		t.Fatalf("parseTestMailOptions() = %q, %v", to, err)
	}
	for _, args := range [][]string{
		{},
		{"--to", "Recipient <recipient@example.com>"},
		{"--to", "recipient@example.com", "extra"},
	} {
		if _, err := parseTestMailOptions(args); err == nil {
			t.Fatalf("parseTestMailOptions(%q) unexpectedly succeeded", args)
		}
	}
}

func TestRejectCommandArguments(t *testing.T) {
	if err := rejectCommandArguments("migrate", nil); err != nil {
		t.Fatalf("rejectCommandArguments(nil) error = %v", err)
	}
	if err := rejectCommandArguments("migrate", []string{"extra"}); err == nil {
		t.Fatal("rejectCommandArguments(extra) unexpectedly succeeded")
	}
}

type cancelingScheduler struct {
	cancel context.CancelFunc
	calls  int
}

func (s *cancelingScheduler) Tick(context.Context, time.Time) error {
	s.calls++
	s.cancel()
	return errors.New("expected test failure")
}

func TestRunSchedulerTicksImmediatelyAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := &cancelingScheduler{cancel: cancel}
	done := make(chan struct{})
	go func() {
		runScheduler(ctx, scheduler, io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runScheduler did not stop after cancellation")
	}
	if scheduler.calls != 1 {
		t.Fatalf("Tick() calls = %d, want immediate call", scheduler.calls)
	}
}
