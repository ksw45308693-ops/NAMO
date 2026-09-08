package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadUsesPortableDefaults(t *testing.T) {
	values := map[string]string{
		"DATABASE_URL": "postgres://monitor:test@localhost/monitor",
		"SESSION_KEY":  strings.Repeat("s", 32),
	}

	cfg, err := Load(mapLookup(values))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("ListenAddr = %q, want loopback default", cfg.ListenAddr)
	}
	if cfg.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.SMTPPort != 587 {
		t.Fatalf("SMTPPort = %d, want 587", cfg.SMTPPort)
	}
	if cfg.TimeZone != "Asia/Seoul" {
		t.Fatalf("TimeZone = %q", cfg.TimeZone)
	}
	if cfg.DeliveryMode != "report" {
		t.Fatalf("DeliveryMode = %q, want report", cfg.DeliveryMode)
	}
}

func TestLoadRejectsMissingDatabaseURL(t *testing.T) {
	_, err := Load(mapLookup(map[string]string{
		"SESSION_KEY": strings.Repeat("s", 32),
	}))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("Load() error = %v, want DATABASE_URL error", err)
	}
}

func TestLoadRejectsShortSessionKey(t *testing.T) {
	_, err := Load(mapLookup(map[string]string{
		"DATABASE_URL": "postgres://localhost/monitor",
		"SESSION_KEY":  "short",
	}))
	if err == nil || !strings.Contains(err.Error(), "SESSION_KEY") {
		t.Fatalf("Load() error = %v, want SESSION_KEY error", err)
	}
}

func TestLoadRejectsUnsafeInvitationBaseURL(t *testing.T) {
	for _, baseURL := range []string{
		"ftp://monitor.example", "https://user:secret@monitor.example", "https://monitor.example/#fragment",
		"https://monitor.example/app", "https://monitor.example/?source=test",
	} {
		_, err := Load(mapLookup(map[string]string{
			"DATABASE_URL": "postgres://localhost/monitor",
			"SESSION_KEY":  strings.Repeat("s", 32),
			"BASE_URL":     baseURL,
		}))
		if err == nil || !strings.Contains(err.Error(), "BASE_URL") {
			t.Fatalf("BASE_URL %q error = %v", baseURL, err)
		}
	}
}

func TestValidateCommandRequiresOnlyItsExternalService(t *testing.T) {
	cfg := Config{
		DatabaseURL:          "postgres://runtime@localhost/monitor",
		MigrationDatabaseURL: "postgres://owner@localhost/monitor",
		SessionKey:           strings.Repeat("s", 32),
	}
	if err := cfg.ValidateCommand("migrate"); err != nil {
		t.Fatalf("migrate validation = %v", err)
	}
	if err := cfg.ValidateCommand("collect-once"); err != nil {
		t.Fatalf("collect-once must resolve its key at execution: %v", err)
	}
	if err := cfg.ValidateCommand("send-test-mail"); err == nil || !strings.Contains(err.Error(), "SMTP_HOST") {
		t.Fatalf("send-test-mail validation = %v, want SMTP_HOST error", err)
	}
}

func TestValidateServeRequiresHTTPSAndLoopbackListener(t *testing.T) {
	base := Config{
		DatabaseURL:  "postgres://runtime@localhost/monitor",
		G2BAPIKey:    "",
		SessionKey:   strings.Repeat("s", 32),
		BaseURL:      "https://monitor.example.internal",
		ListenAddr:   "127.0.0.1:8080",
		DeliveryMode: "report",
		ReportDir:    t.TempDir(),
	}
	if err := base.ValidateCommand("serve"); err != nil {
		t.Fatalf("secure loopback serve validation = %v", err)
	}

	for _, baseURL := range []string{
		"", "http://127.0.0.1:8080", "http://monitor.example.internal",
		"https://monitor.example.internal/app", "https://monitor.example.internal/?source=test",
	} {
		cfg := base
		cfg.BaseURL = baseURL
		if err := cfg.ValidateCommand("serve"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
			t.Errorf("BASE_URL %q validation = %v, want HTTPS error", baseURL, err)
		}
	}
	for _, listenAddr := range []string{
		"0.0.0.0:8080", ":8080", "192.0.2.10:8080", "localhost", "bad-address",
		"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:notaport",
	} {
		cfg := base
		cfg.ListenAddr = listenAddr
		if err := cfg.ValidateCommand("serve"); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("LISTEN_ADDR %q validation = %v, want loopback error", listenAddr, err)
		}
	}
	for _, listenAddr := range []string{"localhost:8080", "[::1]:8080"} {
		cfg := base
		cfg.ListenAddr = listenAddr
		if err := cfg.ValidateCommand("serve"); err != nil {
			t.Errorf("LISTEN_ADDR %q validation = %v", listenAddr, err)
		}
	}
}

func TestValidateMailCommandsRequirePlainSMTPFromMailbox(t *testing.T) {
	base := Config{
		DatabaseURL: "postgres://runtime@localhost/monitor", G2BAPIKey: "test-key",
		SMTPHost: "mail.example.internal", SMTPFrom: "monitor@example.internal",
		SessionKey: strings.Repeat("s", 32), BaseURL: "https://monitor.example.internal",
		ListenAddr: "127.0.0.1:8080",
	}
	for _, command := range []string{"send-test-mail"} {
		if err := base.ValidateCommand(command); err != nil {
			t.Fatalf("%s valid SMTP_FROM error = %v", command, err)
		}
		for _, from := range []string{"", "x", "Monitor <monitor@example.internal>", "monitor@example.internal\r\nBcc: attacker@example.com"} {
			cfg := base
			cfg.SMTPFrom = from
			if err := cfg.ValidateCommand(command); err == nil || !strings.Contains(err.Error(), "SMTP_FROM") {
				t.Errorf("%s SMTP_FROM %q error = %v", command, from, err)
			}
		}
	}
}

func TestValidateReportCommandsRequireReportModeAndSafeAbsoluteDirectory(t *testing.T) {
	base := Config{
		DatabaseURL: "postgres://runtime@localhost/monitor", G2BAPIKey: "test-key",
		SessionKey: strings.Repeat("s", 32), BaseURL: "https://monitor.example.internal",
		ListenAddr: "127.0.0.1:8080", DeliveryMode: "report", ReportDir: t.TempDir(),
	}
	for _, command := range []string{"serve", "generate-report"} {
		if err := base.ValidateCommand(command); err != nil {
			t.Fatalf("%s valid report configuration = %v", command, err)
		}
		for _, mode := range []string{"", "mail", "REPORT"} {
			cfg := base
			cfg.DeliveryMode = mode
			if err := cfg.ValidateCommand(command); err == nil || !strings.Contains(err.Error(), "DELIVERY_MODE") {
				t.Errorf("%s DELIVERY_MODE %q error = %v", command, mode, err)
			}
		}
		unsafeParent := t.TempDir() + string(filepath.Separator) + ".." + string(filepath.Separator) + "reports"
		for _, directory := range []string{"", ".", unsafeParent, filepath.VolumeName(t.TempDir()) + string(filepath.Separator)} {
			cfg := base
			cfg.ReportDir = directory
			if err := cfg.ValidateCommand(command); err == nil || !strings.Contains(err.Error(), "REPORT_DIR") {
				t.Errorf("%s REPORT_DIR %q error = %v", command, directory, err)
			}
		}
	}
}

func TestValidateCommandSeparatesMigrationAndRuntimeRoles(t *testing.T) {
	cfg := Config{DatabaseURL: "postgres://runtime@localhost/monitor", SessionKey: strings.Repeat("s", 32)}
	if err := cfg.ValidateCommand("migrate"); err == nil || !strings.Contains(err.Error(), "MIGRATION_DATABASE_URL") {
		t.Fatalf("migrate validation = %v, want owner URL requirement", err)
	}
	cfg.MigrationDatabaseURL = cfg.DatabaseURL
	if err := cfg.ValidateCommand("migrate"); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("migrate validation = %v, want role separation", err)
	}
	if err := cfg.ValidateCommand("create-admin"); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("create-admin validation = %v, want role separation", err)
	}
	cfg.MigrationDatabaseURL = "postgres://runtime@localhost/monitor?application_name=migrate"
	if err := cfg.ValidateCommand("migrate"); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("query-only URL change bypassed separation: %v", err)
	}
	cfg.MigrationDatabaseURL = "postgres://owner@localhost/monitor"
	if err := cfg.ValidateCommand("migrate"); err != nil {
		t.Fatalf("distinct role rejected: %v", err)
	}
}

func mapLookup(values map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
