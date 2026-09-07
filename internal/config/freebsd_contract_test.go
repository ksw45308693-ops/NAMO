package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestFreeBSDLogRotationAndBackupContracts(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	read := func(path string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}

	rc := read("deploy/freebsd/namo.in")
	for _, reserved := range []string{`namo_program`, `namo_user`, `namo_group`} {
		if strings.Contains(rc, reserved) {
			t.Errorf("rc.d must not use reserved rc.subr variable %q for the daemon child", reserved)
		}
	}
	if !strings.Contains(rc, `install -o "$namo_run_user" -g "$namo_run_group" -m 0600 /dev/null "$pidfile"`) {
		t.Error("rc.d must create the supervisor PID file before daemon drops privileges")
	}
	for _, want := range []string{
		"umask 027",
		"namo_validate_report_dir",
		`[ "$#" -eq 0 ]`,
		`*//*|*/./*|*/.|*/../*|*/..|*/)`,
		`[ -L "$report_path_component" ]`,
		`install -d -o root -g "$namo_run_group" -m 0750 "$report_parent"`,
		`install -d -o "$namo_run_user" -g "$namo_run_group" -m 0750 "$REPORT_DIR"`,
	} {
		if !strings.Contains(rc, want) {
			t.Errorf("rc.d report directory contract missing %q", want)
		}
	}
	for _, want := range []string{`procname="/usr/sbin/daemon"`, `-r -R 5`, `-H -P ${pidfile}`, `-o ${namo_log_file}`} {
		if !strings.Contains(rc, want) {
			t.Errorf("rc.d script missing %q", want)
		}
	}
	envSource := strings.Index(rc, `. "$namo_env_file"`)
	reportValidation := strings.Index(rc, "\tnamo_validate_report_dir || return 1")
	parentInstall := strings.Index(rc, `install -d -o root -g "$namo_run_group" -m 0750 "$report_parent"`)
	reportInstall := strings.Index(rc, `install -d -o "$namo_run_user" -g "$namo_run_group" -m 0750 "$REPORT_DIR"`)
	pidInstall := strings.Index(rc, `install -o "$namo_run_user" -g "$namo_run_group" -m 0600 /dev/null "$pidfile"`)
	if envSource < 0 || reportValidation < envSource || parentInstall < reportValidation || reportInstall < parentInstall || pidInstall < reportInstall {
		t.Error("rc.d must load the environment, validate REPORT_DIR, then prepare parent, report, PID, and log paths")
	}
	unsetOwnerURL := strings.LastIndex(rc, "unset MIGRATION_DATABASE_URL")
	if envSource < 0 || unsetOwnerURL < envSource {
		t.Error("rc.d must unset MIGRATION_DATABASE_URL after loading the runtime environment")
	}
	rotation := read("deploy/freebsd/newsyslog.conf")
	for _, want := range []string{"/var/run/namo.pid", "HUP"} {
		if !strings.Contains(rotation, want) {
			t.Errorf("newsyslog configuration missing %q", want)
		}
	}
	backup := read("deploy/freebsd/backup-namo.sh")
	for _, want := range []string{
		"mktemp", "umask 077", "su -l postgres", "env -i", "PGHOST='$pg_host'", "PGPORT='$pg_port'", "PGUSER='$pg_user'",
		"pg_dumpall --globals-only", "pg_restore --list", "test -s", "mv -f", "complete-${backup_stamp}.manifest", "completion_tmp",
		`env_file="/usr/local/etc/namo.env"`, `. "$env_file"`, `report_dir="$REPORT_DIR"`, "namo_validate_report_dir", "reports_tmp", `reports-${backup_stamp}.tar`, `service namo onestatus`,
		`service namo stop`, `if [ "$restart_namo" -eq 1 ]; then`, `service namo start`,
		`tar -cpf "$reports_tmp" -C "$report_parent" "$report_name"`,
		`sha256 -q "$globals_final"`, `sha256 -q "$database_final"`, `sha256 -q "$reports_final"`,
	} {
		if !strings.Contains(backup, want) {
			t.Errorf("backup script missing %q", want)
		}
	}
	serviceStop := strings.Index(backup, "service namo stop")
	backupValidation := strings.Index(backup, "namo_validate_report_dir\nreport_dir=\"$REPORT_DIR\"")
	backupDirInstall := strings.Index(backup, `install -d -o postgres -g postgres -m 0700 "$backup_dir"`)
	databaseDump := strings.Index(backup, "pg_dump -Fc")
	reportArchive := strings.Index(backup, `tar -cpf "$reports_tmp"`)
	reportPublish := strings.Index(backup, `mv -f "$reports_tmp" "$reports_final"`)
	completionPublish := strings.Index(backup, `mv -f "$completion_tmp" "$completion_final"`)
	if serviceStop < 0 || databaseDump < serviceStop || reportArchive < databaseDump || reportPublish < reportArchive || completionPublish < reportPublish {
		t.Error("backup must stop a running service, dump PostgreSQL, archive reports, then publish the completion marker")
	}
	if backupValidation < 0 || serviceStop < backupValidation || backupDirInstall < serviceStop {
		t.Error("backup must validate REPORT_DIR before stopping the service or creating the backup directory")
	}
	if strings.Contains(backup, "su -m postgres") {
		t.Error("backup script must not preserve the caller PostgreSQL environment")
	}
	operations := read("docs/operations-freebsd.md")
	for _, want := range []string{
		"backup-namo.sh", "restore_globals", "restore_database", "restore_reports", "restore_complete", "pg_restore",
		"REPORT_DIR", "DELIVERY_MODE", "generate-report", "daemon -f -u namo", "짧은 서비스 중단", "원래 중지",
		"/var/db/namo/reports", "namo:namo", "0750", "0640", "mktemp", "pg_restore --list", "mv -f", "sha256 -q",
		"reports.sha256", "relative_path", "cleanup_restore_test", "duplicate_relative_paths",
		`DATABASE_URL='postgres://namo_app@/namo_restore_test`, `REPORT_DIR="$restore_stage/reports"`,
		`daemon -f -u namo /usr/bin/env -i`, `PATH='/bin:/usr/bin:/usr/local/bin:/usr/local/sbin'`,
		`install -d -o postgres -g namo -m 0750 "$restore_root"`,
	} {
		if !strings.Contains(operations, want) {
			t.Errorf("operations guide missing %q", want)
		}
	}
	if strings.Contains(operations, "/usr/local/sbin/namo send-test-mail") {
		t.Error("operations guide must not instruct operators to run disabled test mail")
	}
	for _, forbidden := range []string{
		`report_live='/var/db/namo/reports'`, "reports.rollback",
		`DATABASE_URL='postgres://restore_admin@/namo_restore_test`, "verify_env", ". \"$verify_env\"",
	} {
		if strings.Contains(operations, forbidden) {
			t.Errorf("isolated restore guide must not publish staged reports to the live service: found %q", forbidden)
		}
	}

	zeroCost := read("docs/zero-cost-stack.md")
	if !strings.Contains(zeroCost, "REPORT_DIR") || !strings.Contains(zeroCost, "후속 기능") || strings.Contains(zeroCost, "| 메일 |") {
		t.Error("zero-cost stack must describe report delivery without an SMTP runtime dependency")
	}
	product := read("PRODUCT.md")
	if strings.Contains(product, "Send scheduled HTML email") || !strings.Contains(product, "HTML report") {
		t.Error("product contract must describe HTML report delivery instead of scheduled email")
	}
}

func TestNginxLimitsLoginPostsWithoutThrottlingLoginPage(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	read := func(path string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}

	httpConfig := read("deploy/nginx/namo-http.conf")
	for _, want := range []string{"map $request_method $namo_login_limit_key", "POST $binary_remote_addr", `default ""`, "limit_req_zone $namo_login_limit_key"} {
		if !strings.Contains(httpConfig, want) {
			t.Errorf("Nginx HTTP login limit config missing %q", want)
		}
	}
	serverConfig := read("deploy/nginx/namo.conf")
	for _, want := range []string{"location = /login", "limit_req zone=namo_login", "limit_req_status 429"} {
		if !strings.Contains(serverConfig, want) {
			t.Errorf("Nginx server login limit config missing %q", want)
		}
	}
	// Both public password-hashing endpoints must select a limited location,
	// rather than falling through to the unrestricted application proxy.
	for _, route := range []string{"/login", "/signup"} {
		location := regexp.MustCompile(`(?s)location\s+=\s+` + regexp.QuoteMeta(route) + `\s*\{([^}]+)\}`).FindStringSubmatch(serverConfig)
		if len(location) != 2 || !strings.Contains(location[1], "limit_req zone=namo_login") || !strings.Contains(location[1], "limit_req_status 429") || !strings.Contains(location[1], "proxy_pass") {
			t.Errorf("POST %s is not routed through the authentication request limit", route)
		}
	}
	operations := read("docs/operations-freebsd.md")
	for _, want := range []string{"namo-http.conf", "HTTP 429", "POST /login"} {
		if !strings.Contains(operations, want) {
			t.Errorf("operations guide missing login rate-limit contract %q", want)
		}
	}
}

func TestNginxDoesNotPublishReportDirectory(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	staticDirective := regexp.MustCompile(`(?mi)^[\t ]*(?:alias|root)[\t ]+`)
	for _, path := range []string{"deploy/nginx/namo-http.conf", "deploy/nginx/namo.conf"} {
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		config := strings.ToLower(string(contents))
		for _, forbidden := range []string{"report_dir", "/var/db/namo/reports"} {
			if strings.Contains(config, forbidden) {
				t.Errorf("%s must not expose REPORT_DIR through Nginx: found %q", path, forbidden)
			}
		}
		if staticDirective.MatchString(config) {
			t.Errorf("%s must not contain an alias or root directive", path)
		}
	}
	for _, unsafe := range []string{"\talias\t/var/db/namo/reports;", "   root    /var/db/namo/reports;"} {
		if !staticDirective.MatchString(unsafe) {
			t.Errorf("static directive guard missed %q", unsafe)
		}
	}
}

func TestFreeBSDScriptsExecuteSecurityAndBackupStateContracts(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("wsl.exe"); err != nil {
			t.Skip("WSL is unavailable")
		}
		command = exec.Command("wsl.exe", "--cd", root, "sh", "deploy/freebsd/freebsd_scripts_test.sh")
	} else {
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("POSIX sh is unavailable")
		}
		command = exec.Command("sh", filepath.Join(root, "deploy/freebsd/freebsd_scripts_test.sh"))
		command.Dir = root
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("FreeBSD shell contract test: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "freebsd scripts: PASS") {
		t.Fatalf("FreeBSD shell contract output = %q", output)
	}
}
