package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"namo/internal/config"
)

func TestRunPrintsUsageWithoutCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false

	code := Run(context.Background(), nil, nil, func(context.Context, string, config.Config, []string) error {
		called = true
		return nil
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	if called {
		t.Fatal("executor called without a command")
	}
	if !strings.Contains(stderr.String(), "사용법: namo ") || !strings.Contains(stderr.String(), "serve") || !strings.Contains(stderr.String(), "collect-once") {
		t.Fatalf("usage missing commands: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "generate-report") || strings.Contains(stderr.String(), "send-test-mail") {
		t.Fatalf("usage exposes the wrong delivery commands: %q", stderr.String())
	}
}

func TestRunBlocksHiddenSendTestMailBeforeConfigurationLoad(t *testing.T) {
	var stdout, stderr bytes.Buffer
	lookupCalled, executeCalled := false, false
	code := Run(context.Background(), []string{"send-test-mail", "--to", "admin@example.com"}, func(string) (string, bool) {
		lookupCalled = true
		return "", false
	}, func(context.Context, string, config.Config, []string) error {
		executeCalled = true
		return nil
	}, &stdout, &stderr)
	if code != 1 || lookupCalled || executeCalled || !strings.Contains(stderr.String(), "메일 기능은 현재 비활성화되어 있습니다") {
		t.Fatalf("Run() code=%d lookup=%v execute=%v stderr=%q", code, lookupCalled, executeCalled, stderr.String())
	}
}

func TestRunExecutesGenerateReportWithTenantArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	reportDir := t.TempDir()
	lookup := lookupMap(map[string]string{
		"DATABASE_URL": "postgres://runtime@localhost/monitor", "SESSION_KEY": strings.Repeat("x", 32),
		"DELIVERY_MODE": "report", "REPORT_DIR": reportDir,
	})
	var gotArgs []string
	code := Run(context.Background(), []string{"generate-report", "--tenant", "11111111-1111-1111-1111-111111111111"}, lookup,
		func(_ context.Context, command string, _ config.Config, args []string) error {
			if command != "generate-report" {
				t.Fatalf("command = %q", command)
			}
			gotArgs = append([]string(nil), args...)
			return nil
		}, &stdout, &stderr)
	if code != 0 || strings.Join(gotArgs, " ") != "--tenant 11111111-1111-1111-1111-111111111111" {
		t.Fatalf("Run() code=%d args=%q stderr=%q", code, gotArgs, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("generate-report CLI added output: %q", stdout.String())
	}
}

func TestRunHidesGenerateReportExecutorError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	reportDir := t.TempDir()
	lookup := lookupMap(map[string]string{
		"DATABASE_URL": "postgres://runtime@localhost/monitor", "SESSION_KEY": strings.Repeat("x", 32),
		"DELIVERY_MODE": "report", "REPORT_DIR": reportDir,
	})
	code := Run(context.Background(), []string{"generate-report", "--tenant", "11111111-1111-1111-1111-111111111111"}, lookup,
		func(context.Context, string, config.Config, []string) error {
			return errors.New("injected file failure: " + reportDir)
		}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run() code=%d, want 1", code)
	}
	if strings.Contains(stderr.String(), reportDir) || strings.Contains(stderr.String(), "injected") || !strings.Contains(stderr.String(), "리포트를 생성하지 못했습니다") {
		t.Fatalf("Run() leaked generate-report error: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run() wrote success output on failure: %q", stdout.String())
	}
}

func TestRunExecutesValidatedCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var gotCommand string
	lookup := lookupMap(map[string]string{
		"DATABASE_URL":           "postgres://runtime@localhost/monitor",
		"MIGRATION_DATABASE_URL": "postgres://owner@localhost/monitor",
		"SESSION_KEY":            strings.Repeat("x", 32),
	})

	code := Run(context.Background(), []string{"migrate"}, lookup, func(_ context.Context, command string, _ config.Config, _ []string) error {
		gotCommand = command
		return nil
	}, &stdout, &stderr)

	if code != 0 || gotCommand != "migrate" {
		t.Fatalf("Run() code = %d command = %q stderr = %q", code, gotCommand, stderr.String())
	}
}

func TestRunResolvesCollectionKeyAtExecution(t *testing.T) {
	var stdout, stderr bytes.Buffer
	lookup := lookupMap(map[string]string{
		"DATABASE_URL":           "postgres://runtime@localhost/monitor",
		"MIGRATION_DATABASE_URL": "postgres://owner@localhost/monitor",
		"SESSION_KEY":            strings.Repeat("x", 32),
	})

	code := Run(context.Background(), []string{"collect-once"}, lookup, func(context.Context, string, config.Config, []string) error {
		return config.ErrAPIKeyNotConfigured
	}, &stdout, &stderr)

	if code != 1 || !strings.Contains(stderr.String(), config.ErrAPIKeyNotConfigured.Error()) {
		t.Fatalf("Run() code = %d stderr = %q", code, stderr.String())
	}
}

func TestRunReportsExecutorFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	lookup := lookupMap(map[string]string{
		"DATABASE_URL":           "postgres://runtime@localhost/monitor",
		"MIGRATION_DATABASE_URL": "postgres://owner@localhost/monitor",
		"SESSION_KEY":            strings.Repeat("x", 32),
	})

	code := Run(context.Background(), []string{"migrate"}, lookup, func(context.Context, string, config.Config, []string) error {
		return errors.New("database unavailable")
	}, &stdout, &stderr)

	if code != 1 || !strings.Contains(stderr.String(), "database unavailable") {
		t.Fatalf("Run() code = %d stderr = %q", code, stderr.String())
	}
}

func lookupMap(values map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
