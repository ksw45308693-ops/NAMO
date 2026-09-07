#!/bin/sh

set -eu

script_dir="$(CDPATH= cd "$(dirname "$0")" && pwd)"
workspace="$(CDPATH= cd "$script_dir/../.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/namo-freebsd-test.XXXXXX")"

cleanup()
{
	rm -rf "$test_root"
}
trap cleanup 0 1 2 15

expect_rc_path()
{
	expected=$1
	report_dir=$2
	if REPORT_DIR="$report_dir" NAMO_RC_TEST_VALIDATE_ONLY=1 sh "$workspace/deploy/freebsd/namo.in" >/dev/null 2>&1; then
		actual=pass
	else
		actual=fail
	fi
	if [ "$actual" != "$expected" ]; then
		echo "rc path validation: $report_dir: got $actual, want $expected" >&2
		exit 1
	fi
}

mkdir -p "$test_root/path/real/reports"
ln -s "$test_root/path/real" "$test_root/path/link"
expect_rc_path pass "$test_root/path/real/reports"
for invalid in / /./ // "$test_root/path//reports" "$test_root/path/./reports" "$test_root/path/real/reports/" "$test_root/path/../reports" "$test_root/path/link/reports"
do
	expect_rc_path fail "$invalid"
done
if [ ! -r /etc/rc.subr ]; then
	if REPORT_DIR="$test_root/path/real/reports" NAMO_RC_TEST_VALIDATE_ONLY=1 sh "$workspace/deploy/freebsd/namo.in" start >/dev/null 2>&1; then
		echo "rc test validation mode intercepted a real start argument" >&2
		exit 1
	fi
fi

mock_bin="$test_root/bin"
mkdir -p "$mock_bin"

# Execute the real prestart function without loading FreeBSD rc.subr or touching
# system paths. The install boundary records the requested owner and group.
rc_case="$test_root/rc-owner"
mkdir -p "$rc_case"
printf "REPORT_DIR='%s'\n" "$rc_case/reports" > "$rc_case/namo.env"
sed '/^\. \/etc\/rc.subr$/d; /^run_rc_command /d' "$workspace/deploy/freebsd/namo.in" > "$rc_case/namo.rc"
(
	load_rc_config() { :; }
	install() { printf '%s\n' "$*" >> "$rc_case/install.log"; }
	namo_run_user=custom_namo
	namo_run_group=custom_group
	namo_env_file="$rc_case/namo.env"
	namo_log_file="$rc_case/namo.log"
	. "$rc_case/namo.rc"
	namo_prestart
)
if ! grep -Fqx -- "-d -o custom_namo -g custom_group -m 0750 $rc_case/reports" "$rc_case/install.log"; then
	echo "rc prestart ignored the configured report owner/group" >&2
	exit 1
fi
if ! grep -Fqx -- "-d -o custom_namo -g custom_group -m 0700 /var/db/namo/secrets" "$rc_case/install.log"; then
	echo "rc prestart did not provision an owner-only API key directory" >&2
	exit 1
fi

cat > "$mock_bin/service" <<'EOF'
#!/bin/sh
case "$2" in
onestatus)
	[ "$MOCK_SERVICE_INITIAL" = running ]
	;;
stop|start)
	printf '%s\n' "$2" >> "$MOCK_SERVICE_LOG"
	;;
*)
	exit 2
	;;
esac
EOF

cat > "$mock_bin/install" <<'EOF'
#!/bin/sh
for argument do target=$argument; done
[ -z "${MOCK_INSTALL_LOG:-}" ] || printf '%s\n' "$target" >> "$MOCK_INSTALL_LOG"
mkdir -p "$target"
EOF

cat > "$mock_bin/chown" <<'EOF'
#!/bin/sh
exit 0
EOF

cat > "$mock_bin/su" <<'EOF'
#!/bin/sh
for argument do command_text=$argument; done
case "$command_text" in
*pg_dumpall*)
	[ "${MOCK_FAIL_STAGE:-}" != dump ] || exit 1
	output="$(printf '%s\n' "$command_text" | sed -n "s/.*> '\([^']*\)'.*/\1/p")"
	printf '%s\n' globals > "$output"
	;;
*"pg_dump -Fc"*)
	output="$(printf '%s\n' "$command_text" | sed -n "s/.*-f '\([^']*\)'.*/\1/p")"
	printf '%s\n' database > "$output"
	;;
*"pg_restore --list"*)
	;;
*)
	exit 2
	;;
esac
EOF

cat > "$mock_bin/tar" <<'EOF'
#!/bin/sh
[ "${MOCK_FAIL_STAGE:-}" != archive ] || exit 1
printf '%s\n' "$*" >> "$MOCK_TAR_LOG"
printf '%s\n' reports > "$2"
EOF

cat > "$mock_bin/sha256" <<'EOF'
#!/bin/sh
printf '%s\n' "$2" >> "$MOCK_SHA_LOG"
printf '%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
EOF

chmod 0755 "$mock_bin/service" "$mock_bin/install" "$mock_bin/chown" "$mock_bin/su" "$mock_bin/tar" "$mock_bin/sha256"

run_backup_case()
{
	initial=$1
	result=$2
	path_case=${3:-nested}
	case_dir="$test_root/case-$initial-$result-$path_case"
	report_dir="$case_dir/custom/reports"
	expected_tar_base="-C $case_dir/custom reports"
	if [ "$path_case" = root-child ]; then
		# /tmp already exists; tar is mocked, so none of its contents are read.
		report_dir=/tmp
		expected_tar_base="-C / tmp"
	fi
	backup_dir="$case_dir/backups"
	env_file="$case_dir/namo.env"
	service_log="$case_dir/service.log"
	tar_log="$case_dir/tar.log"
	sha_log="$case_dir/sha.log"
	mkdir -p "$report_dir" "$backup_dir"
	printf "export REPORT_DIR='%s'\n" "$report_dir" > "$env_file"
	: > "$service_log"
	: > "$tar_log"
	: > "$sha_log"
	fail_stage=
	[ "$result" = success ] || fail_stage=archive
	if PATH="$mock_bin:/usr/bin:/bin" \
		NAMO_BACKUP_TEST_MODE=1 \
		NAMO_BACKUP_TEST_ENV_FILE="$env_file" \
		NAMO_BACKUP_TEST_DIR="$backup_dir" \
		MOCK_SERVICE_INITIAL="$initial" \
		MOCK_SERVICE_LOG="$service_log" \
		MOCK_TAR_LOG="$tar_log" \
		MOCK_SHA_LOG="$sha_log" \
		MOCK_FAIL_STAGE="$fail_stage" \
		sh "$workspace/deploy/freebsd/backup-namo.sh" >/dev/null 2>&1
	then
		actual=success
	else
		actual=failure
	fi
	[ "$actual" = "$result" ] || { echo "$initial $result: got $actual" >&2; exit 1; }
	if [ "$initial" = running ]; then
		[ "$(sed -n '1p' "$service_log")" = stop ]
		[ "$(sed -n '2p' "$service_log")" = start ]
		[ "$(wc -l < "$service_log")" -eq 2 ]
	else
		[ ! -s "$service_log" ]
	fi
	if [ "$result" = success ]; then
		manifest="$(find "$backup_dir" -name 'complete-*.manifest' -type f -print -quit)"
		[ -n "$manifest" ]
		[ "$(wc -l < "$manifest")" -eq 3 ]
		awk 'NF != 2 || length($1) != 64 { exit 1 }' "$manifest"
		if ! grep -F -- "$expected_tar_base" "$tar_log" >/dev/null; then
			echo "backup used the wrong archive base for $report_dir" >&2
			exit 1
		fi
		globals_final="$(find "$backup_dir" -name 'globals-*.sql' -type f -print -quit)"
		database_final="$(find "$backup_dir" -name 'namo-*.dump' -type f -print -quit)"
		reports_final="$(find "$backup_dir" -name 'reports-*.tar' -type f -print -quit)"
		[ "$(wc -l < "$sha_log")" -eq 3 ]
		grep -Fqx "$globals_final" "$sha_log"
		grep -Fqx "$database_final" "$sha_log"
		grep -Fqx "$reports_final" "$sha_log"
	else
		[ -z "$(find "$backup_dir" -name 'complete-*.manifest' -type f -print -quit)" ]
	fi
}

for initial in running stopped
do
	run_backup_case "$initial" success
	run_backup_case "$initial" failure
done

run_backup_case stopped success root-child

symlink_case="$test_root/symlink-case"
mkdir -p "$symlink_case/real/reports" "$symlink_case/backups"
ln -s "$symlink_case/real" "$symlink_case/link"
printf "export REPORT_DIR='%s'\n" "$symlink_case/link/reports" > "$symlink_case/namo.env"
: > "$symlink_case/service.log"
: > "$symlink_case/install.log"
if PATH="$mock_bin:/usr/bin:/bin" \
	NAMO_BACKUP_TEST_MODE=1 \
	NAMO_BACKUP_TEST_ENV_FILE="$symlink_case/namo.env" \
	NAMO_BACKUP_TEST_DIR="$symlink_case/backups" \
	MOCK_SERVICE_INITIAL=running \
	MOCK_SERVICE_LOG="$symlink_case/service.log" \
	MOCK_INSTALL_LOG="$symlink_case/install.log" \
	MOCK_TAR_LOG="$symlink_case/tar.log" \
	sh "$workspace/deploy/freebsd/backup-namo.sh" >/dev/null 2>&1
then
	echo "backup accepted a symlink path component" >&2
	exit 1
fi
[ ! -s "$symlink_case/service.log" ]
[ ! -s "$symlink_case/install.log" ]

echo "freebsd scripts: PASS"
