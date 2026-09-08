#!/bin/sh

set -eu

umask 077

env_file="/usr/local/etc/namo.env"
backup_dir="/var/backups/namo"
if [ "${NAMO_BACKUP_TEST_MODE:-0}" = "1" ]; then
	: "${NAMO_BACKUP_TEST_ENV_FILE:?NAMO_BACKUP_TEST_ENV_FILE is required in test mode}"
	: "${NAMO_BACKUP_TEST_DIR:?NAMO_BACKUP_TEST_DIR is required in test mode}"
	env_file=$NAMO_BACKUP_TEST_ENV_FILE
	backup_dir=$NAMO_BACKUP_TEST_DIR
fi

if [ ! -f "$env_file" ] || [ -L "$env_file" ]; then
	echo "namo environment must be a regular file, not a symbolic link" >&2
	exit 1
fi
if [ "${NAMO_BACKUP_TEST_MODE:-0}" != "1" ]; then
	env_contract="$(stat -f '%Su:%Sg:%Lp' "$env_file")"
	if [ "$env_contract" != "root:wheel:600" ]; then
		echo "namo environment must be root:wheel with mode 0600" >&2
		exit 1
	fi
fi
set -a
. "$env_file"
set +a
unset MIGRATION_DATABASE_URL

namo_validate_report_dir()
{
	case "${REPORT_DIR:-}" in
	"")
		echo "REPORT_DIR must not be empty" >&2
		return 1
		;;
	/*)
		;;
	*)
		echo "REPORT_DIR must be an absolute path" >&2
		return 1
		;;
	esac
	case "$REPORT_DIR" in
	"/"|*//*|*/./*|*/.|*/../*|*/..|*/)
		echo "REPORT_DIR must be a normalized non-root path" >&2
		return 1
		;;
	esac

	report_path_remaining=${REPORT_DIR#/}
	report_path_component=""
	while [ -n "$report_path_remaining" ]; do
		case "$report_path_remaining" in
		*/*)
			report_path_part=${report_path_remaining%%/*}
			report_path_remaining=${report_path_remaining#*/}
			;;
		*)
			report_path_part=$report_path_remaining
			report_path_remaining=""
			;;
		esac
		report_path_component="${report_path_component}/${report_path_part}"
		if [ -L "$report_path_component" ]; then
			echo "REPORT_DIR must not traverse a symbolic link" >&2
			return 1
		fi
	done
}

namo_validate_report_dir
report_dir="$REPORT_DIR"
test -d "$report_dir"

backup_stamp="$(date -u +%Y%m%dT%H%M%SZ)-$$"
report_parent="$(dirname "$report_dir")"
report_name="${report_dir##*/}"
pg_host="/tmp"
pg_port="5432"
pg_user="postgres"
clean_path="/bin:/usr/bin:/usr/local/bin:/usr/local/sbin"
globals_tmp=""
database_tmp=""
reports_tmp=""
completion_tmp=""
restart_namo=0

cleanup()
{
	exit_status=$?
	trap - 0 1 2 15
	if [ -n "$globals_tmp" ] && [ -f "$globals_tmp" ]; then
		rm -f "$globals_tmp" || exit_status=1
	fi
	if [ -n "$database_tmp" ] && [ -f "$database_tmp" ]; then
		rm -f "$database_tmp" || exit_status=1
	fi
	if [ -n "$reports_tmp" ] && [ -f "$reports_tmp" ]; then
		rm -f "$reports_tmp" || exit_status=1
	fi
	if [ -n "$completion_tmp" ] && [ -f "$completion_tmp" ]; then
		rm -f "$completion_tmp" || exit_status=1
	fi
	if [ "$restart_namo" -eq 1 ]; then
		if ! service namo start; then
			echo "failed to restore the initial namo service state" >&2
			exit_status=1
		fi
	fi
	exit "$exit_status"
}

trap cleanup 0
trap 'exit 1' 1 2 15

if service namo onestatus >/dev/null 2>&1; then
	restart_namo=1
	service namo stop
fi

install -d -o postgres -g postgres -m 0700 "$backup_dir"
globals_tmp="$(mktemp "${backup_dir}/.globals-${backup_stamp}.XXXXXX")"
database_tmp="$(mktemp "${backup_dir}/.database-${backup_stamp}.XXXXXX")"
chown postgres:postgres "$globals_tmp" "$database_tmp"
chmod 0600 "$globals_tmp" "$database_tmp"

su -l postgres -c "umask 077; env -i PATH='$clean_path' PGHOST='$pg_host' PGPORT='$pg_port' PGUSER='$pg_user' pg_dumpall --globals-only > '$globals_tmp'"
test -s "$globals_tmp"

su -l postgres -c "umask 077; env -i PATH='$clean_path' PGHOST='$pg_host' PGPORT='$pg_port' PGUSER='$pg_user' pg_dump -Fc -f '$database_tmp' namo"
test -s "$database_tmp"
su -l postgres -c "env -i PATH='$clean_path' pg_restore --list '$database_tmp' >/dev/null"

reports_tmp="$(mktemp "${backup_dir}/.reports-${backup_stamp}.XXXXXX")"
tar -cpf "$reports_tmp" -C "$report_parent" "$report_name"
test -s "$reports_tmp"
chown postgres:postgres "$reports_tmp"
chmod 0600 "$reports_tmp"

globals_final="${backup_dir}/globals-${backup_stamp}.sql"
database_final="${backup_dir}/namo-${backup_stamp}.dump"
reports_final="${backup_dir}/reports-${backup_stamp}.tar"
completion_final="${backup_dir}/complete-${backup_stamp}.manifest"
mv -f "$globals_tmp" "$globals_final"
globals_tmp=""
mv -f "$database_tmp" "$database_final"
database_tmp=""
mv -f "$reports_tmp" "$reports_final"
reports_tmp=""
completion_tmp="$(mktemp "${backup_dir}/.completion-${backup_stamp}.XXXXXX")"
globals_sha256="$(sha256 -q "$globals_final")"
database_sha256="$(sha256 -q "$database_final")"
reports_sha256="$(sha256 -q "$reports_final")"
for backup_sha256 in "$globals_sha256" "$database_sha256" "$reports_sha256"; do
	if [ "${#backup_sha256}" -ne 64 ]; then
		echo "backup SHA-256 has an invalid length" >&2
		exit 1
	fi
	case "$backup_sha256" in
	*[!0-9a-f]*)
		echo "backup SHA-256 has invalid characters" >&2
		exit 1
		;;
	esac
done
printf '%s  %s\n%s  %s\n%s  %s\n' \
	"$globals_sha256" "$globals_final" \
	"$database_sha256" "$database_final" \
	"$reports_sha256" "$reports_final" > "$completion_tmp"
chown postgres:postgres "$completion_tmp"
chmod 0600 "$completion_tmp"
mv -f "$completion_tmp" "$completion_final"
completion_tmp=""

printf '%s\n%s\n%s\n%s\n' "$globals_final" "$database_final" "$reports_final" "$completion_final"
