# FreeBSD 내부 검증 운영 절차

이름 전환용 기존 데이터베이스 마이그레이션은 제공하지 않는다. 반드시 새 `namo`
데이터베이스에서 시작하며, 기존 스키마가 있다면 별도의 이전 계획을 먼저 수립한다.

## 준비

1. PostgreSQL, Nginx, Go 빌드 결과를 준비한다.
2. OS 전용 사용자 `namo`와 데이터베이스를 만든다.
3. 실행 파일을 `/usr/local/sbin/namo`에 설치한다.
4. 서비스 환경은 `/usr/local/etc/namo.env`, 초기화 전용 환경은 `/usr/local/etc/namo-owner.env`로 분리한다. 두 파일은 `root:wheel`, `0600`으로 둔다. 서비스 파일에는 `MIGRATION_DATABASE_URL`을 넣지 않는다. 서비스 환경에는 `DELIVERY_MODE=report`와 `REPORT_DIR=/var/db/namo/reports`를 설정한다.
5. `deploy/freebsd/namo.in`을 `/usr/local/etc/rc.d/namo`에 설치하고 실행 권한을 준다.
6. `deploy/freebsd/newsyslog.conf`를 `/etc/newsyslog.conf.d/namo.conf`에 설치한다. `daemon -H`와 supervisor PID 파일을 사용하므로 회전 뒤 HUP을 받은 `daemon`이 로그 파일을 다시 연다.

`serve`는 `BASE_URL`의 HTTPS scheme과 `LISTEN_ADDR`의 loopback 주소를 강제한다.
TLS는 Nginx가 종료하고 Go 서비스 포트는 외부 인터페이스에 직접 노출하지 않는다.
`rc.d`는 시작 전에 `REPORT_DIR`을 검사하고 `/var/db/namo/reports`를 `namo:namo`,
`0750`으로 준비한다. Nginx에는 이 경로의 `alias`나 정적 `location`을 추가하지 않는다.
인증과 고객사 권한을 검사하는 Go 다운로드 경로만 사용한다.

서비스 환경에는 다음 런타임 값을 둔다. `SMTP_*`는 1차 리포트 운영에 필요하지 않다.

```sh
export DATABASE_URL='postgres://namo_app:change-runtime-password@127.0.0.1/namo?sslmode=disable'
export G2B_API_KEY_FILE='/var/db/namo/secrets/g2b-api-key'
export DELIVERY_MODE='report'
export REPORT_DIR='/var/db/namo/reports'
export BASE_URL='https://namo.example.internal'
export SESSION_KEY='replace-with-at-least-32-random-characters'
export LISTEN_ADDR='127.0.0.1:8080'
export TIME_ZONE='Asia/Seoul'
```

## 초기화와 실행

API 키는 배포 중 입력하지 않는다. 앱 시작 후 플랫폼 관리자로 로그인하여
**환경 설정 → 나라장터 API 키**에 Decoding 인증키를 저장한다.
키가 없으면 공고 수집만 대기한다. 저장 후 재시작은 필요하지 않으며,
**플랫폼 관리 → 수집 1회 실행**으로 실제 API 인증을 확인한다.

업데이트할 때는 실행 파일과 `deploy/freebsd/namo.in`을 함께 교체한다.
새 서비스 스크립트가 기본 `/var/db/namo/secrets`를 서비스 계정 소유, `0700`으로 준비한다.
사용자 지정 `G2B_API_KEY_FILE` 경로의 부모는 운영자가 미리 준비한다.
파일이 없을 때만 기존 `G2B_API_KEY`가 사용되며, 웹에서 저장한 키가 우선한다.
키 파일은 `0600`이고 저장된 키는 화면에 다시 표시하지 않는다.
DB·리포트 백업은 키 파일을 포함하지 않으므로 복구 뒤 웹에서 다시 등록한다.

PostgreSQL 관리자 셸에서 DB를 먼저 만든다. 현재 마이그레이션은 `BYPASSRLS` 보조 역할을
생성·고정하고 해당 `NOLOGIN` 역할 소유 함수를 교체한다. 따라서 `MIGRATION_DATABASE_URL`에는
PostgreSQL superuser 자격증명이 필요하며 `CREATEROLE`만 있는 계정은 사용할 수 없다. 명령은
연결 직후 `rolsuper`를 검사하고 조건을 만족하지 않으면 SQL 실행 전에 중단한다.
이 자격증명은 서비스가 읽지 않으며 초기화가 끝나면 파일을 제거한다.

```sql
CREATE DATABASE namo;
```

초기화 전용 환경을 현재 셸에 읽힌 뒤 마이그레이션을 실행한다. 이 환경은
`MIGRATION_DATABASE_URL`에는 PostgreSQL 관리자 URL, `DATABASE_URL`에는 별도의 서비스 URL을 둔다.

```sh
set -a
. /usr/local/etc/namo-owner.env
set +a
/usr/local/sbin/namo migrate
install -m 0600 /dev/null /root/namo-admin.pass
# /root/namo-admin.pass에 12~72바이트의 초기 비밀번호 한 줄을 입력한다.
/usr/local/sbin/namo create-admin --email admin@example.internal --name 관리자 --password-file /root/namo-admin.pass
rm -f /root/namo-admin.pass
unset MIGRATION_DATABASE_URL
rm -f /usr/local/etc/namo-owner.env
```

마이그레이션이 만든 `namo_runtime` 권한 그룹에 로그인 역할을 연결한다.

```sql
CREATE ROLE namo_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD 'change-runtime-password';
GRANT namo_runtime TO namo_app;
```

서비스 환경의 `DATABASE_URL`은 `namo_app`만 사용한다. 역할 속성을 확인한 뒤 시작한다.

```sql
SELECT rolname, rolsuper, rolcreaterole, rolcreatedb, rolreplication, rolbypassrls
FROM pg_roles WHERE rolname IN ('namo_app', 'namo_runtime', 'namo_auth_definer');
```

```sh
sysrc namo_enable=YES
service namo start
service namo status
newsyslog -nvv -f /etc/newsyslog.conf.d/namo.conf
```

`daemon -r -R 5`는 Go child가 비정상 종료되면 5초 뒤 다시 시작한다. 정상적인
`service namo stop`은 supervisor를 종료하므로 재시작하지 않는다. 다음 검사는
운영과 분리된 검증용 Jail에서만 실행한다.

```sh
namo_supervisor_pid="$(cat /var/run/namo.pid)"
namo_child_pid="$(pgrep -P "$namo_supervisor_pid" | head -n 1)"
test -n "$namo_child_pid"
kill -KILL "$namo_child_pid"
sleep 6
namo_new_child_pid="$(pgrep -P "$namo_supervisor_pid" | head -n 1)"
test -n "$namo_new_child_pid" && test "$namo_new_child_pid" != "$namo_child_pid"
service namo stop
sleep 1
! kill -0 "$namo_supervisor_pid" 2>/dev/null
service namo start
```

서비스 환경을 관리자 셸에 읽어 수집을 점검한다. 수동 리포트는 웹의 **지금 생성**
버튼을 사용하거나 `namo` 사용자로 실행한다. `root`로 `generate-report`를 실행하지 않는다.

```sh
set -a
. /usr/local/etc/namo.env
set +a
/usr/local/sbin/namo collect-once
daemon -f -u namo /usr/local/sbin/namo generate-report --tenant 11111111-1111-1111-1111-111111111111
```

예약 리포트는 설정한 요일과 시각에 신규 매칭 공고가 있을 때 생성된다. 예약 구간이
비어 있으면 DB 리포트 행과 파일을 만들지 않는다. 다음 순서로 예약, 재시작, 다운로드를
확인한다.

1. 고객사 리포트 화면에서 예약 요일과 시각을 저장한다.
2. 예약 실행 뒤 `/reports`에서 생성 시각, 공고 수, 파일명을 확인한다.
3. 다음 예약 시각 전에 `service namo restart`를 실행한다.
4. 같은 예약 구간의 행과 파일이 추가되지 않았는지 확인한다.
5. **다운로드**를 선택해 인증된 브라우저에서 자체 포함 HTML을 연다.

```sh
stat -f '%Su:%Sg %Lp %N' /var/db/namo/reports
find /var/db/namo/reports \( ! -user namo -o ! -group namo \) -print
find /var/db/namo/reports -type d ! -perm 0750 -print
find /var/db/namo/reports -type f ! -perm 0640 -print
```

세 `find` 명령은 출력이 없어야 한다. 생성 파일은 `namo:namo`, `0640`이어야 한다.

Nginx 설정을 설치한 뒤 적용 전에 검사한다. `deploy/nginx/namo-http.conf`는
반드시 Nginx `http {}` 안에서 서버 설정보다 먼저 읽히게 하고,
`deploy/nginx/namo.conf`는 같은 `http {}` 안에 설치한다. 전자는
`POST /login`만 클라이언트 주소별로 제한한다. 초과 요청은 HTTP 429를 반환하며
로그인 화면을 여는 GET 요청은 제한 키가 비어 있으므로 계속 허용된다.

```sh
nginx -t
service nginx reload
sockstat -4 -6 -l | grep -E ':(443|8080)'
fetch -qo- https://namo.example.internal/healthz
```

## 백업과 복구

백업은 PostgreSQL 서버에서 `root`로 실행한다. 백업 중에는 짧은 서비스 중단이 발생한다.
스크립트는 실행 중인 `namo`만 중지하고 종료 trap에서 다시 시작한다. `namo`가 원래 중지
상태였다면 백업 후 임의로 시작하지 않는다.

제공 스크립트는 `root:wheel`, `0600`인 `/usr/local/etc/namo.env`를 읽는다. 환경 파일의
`REPORT_DIR`을 정규화된 절대 경로로 검증하고, 기존 경로 구성 요소가 심볼릭 링크가 아닌지
확인한다. 검증은 서비스를 중지하기 전에 끝난다.

스크립트는 `umask 077`로 `pg_dumpall --globals-only`와 custom-format `pg_dump`를
실행한 뒤 설정된 `REPORT_DIR`을 tar로 보관한다. 고정된 전용 디렉터리 안에서 `mktemp`로
`0600` 임시 파일을 만들고, dump가 비어 있지 않은지와 `pg_restore --list` 결과를 확인한
뒤 `mv -f`로 같은 파일시스템에서 원자적으로 이름을 바꾼다. 각 결과에는 UTC 시각과 PID가
붙는다. 스크립트는 이전 정상 세대를 덮어쓰거나 자동 삭제하지 않는다. globals, database,
reports 파일을 모두 게시한 뒤에만 같은 세대의 `complete-<세대>.manifest`를 게시한다.
manifest는 세 파일의 절대 경로와 SHA-256을 기록한다. 완료 마커가 없는 세대는 중단된
백업으로 간주하며 복구에 사용하지 않는다.
스크립트는 `su -l`과 `env -i`를 사용하고 `PGHOST=/tmp`,
`PGPORT=5432`, `PGUSER=postgres`를 명시하므로 실행자에게 남은 `PGHOST`, `PGPORT`,
`PGSERVICE`, `PGUSER` 등으로 다른 PostgreSQL 클러스터를 선택하지 않는다. 실제
운영 클러스터의 로컬 socket이나 port가 다르면 적용 전에 스크립트의 고정값을 수정한다.

```sh
install -o root -g wheel -m 0700 deploy/freebsd/backup-namo.sh /usr/local/sbin/namo-backup
/usr/local/sbin/namo-backup
stat -f '%Su:%Sg %Lp %N' /var/backups/namo/complete-*.manifest /var/backups/namo/globals-*.sql /var/backups/namo/namo-*.dump /var/backups/namo/reports-*.tar
```

실행 상태 보존을 두 경우로 확인한다.

```sh
service namo start
/usr/local/sbin/namo-backup
service namo onestatus
service namo stop
/usr/local/sbin/namo-backup
! service namo onestatus
```

`globals-<세대>.sql`에는 역할과 비밀번호 해시가 들어갈 수 있으므로 디렉터리는
`0700`, 파일은 `0600`을 유지한다. 복구할 한 세대의 globals, database, reports 파일을
확정하고 아래 네 변수에 같은 세대의 절대 경로를 지정한다. manifest의 세 SHA-256이
모두 일치하는 경우에만 복구한다. 세 요소를 모두 복구하고 한 세트로 검증한다. 기존
클러스터에서는 같은 역할이 이미 있어 충돌할 수 있으므로 다음 시험은 운영과 분리된
일회용 Jail의 깨끗한 PostgreSQL 클러스터에서 수행한다.
임시 복구 뒤에는 각 HTML 파일을 `public.reports.sha256`과 대조한다.

다음 검증은 운영 서비스에 파일이나 DB를 게시하지 않는다. 격리 PostgreSQL과 staging
`REPORT_DIR`을 하나의 복구 세트로 유지한다. `generate-report`만 실행하므로 실제 나라장터
수집은 시작하지 않는다. 검증 프로세스는 복구된 `namo_app` 역할을 사용하고 `env -i`로
필요한 변수만 받는다. 명령 출력은 버려서 상대 경로와 해시를 노출하지 않는다.

```sh
set -eu
restore_root='/var/tmp/namo-restore-cluster'
restore_stage=''
archive_list=''
report_hashes=''
restore_cluster_started=0

cleanup_restore_test()
{
	restore_status=$?
	trap - 0 1 2 15
	if [ "$restore_cluster_started" -eq 1 ]; then
		su -l postgres -c 'env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin pg_ctl -D /var/tmp/namo-restore-cluster/data -m fast -w stop' || restore_status=1
	fi
	[ -z "$archive_list" ] || rm -f "$archive_list" || restore_status=1
	[ -z "$report_hashes" ] || rm -f "$report_hashes" || restore_status=1
	if [ -n "$restore_stage" ]; then
		case "$restore_stage" in
		/var/tmp/namo-report-stage.*) rm -rf "$restore_stage" || restore_status=1 ;;
		*) restore_status=1 ;;
		esac
	fi
	if [ "$restore_root" = '/var/tmp/namo-restore-cluster' ]; then
		rm -rf "$restore_root" || restore_status=1
	else
		restore_status=1
	fi
	exit "$restore_status"
}
trap cleanup_restore_test 0
trap 'exit 1' 1 2 15

test ! -e "$restore_root"
install -d -o postgres -g namo -m 0750 "$restore_root"
restore_globals='/var/backups/namo/globals-YYYYMMDDTHHMMSSZ-PID.sql'
restore_database='/var/backups/namo/namo-YYYYMMDDTHHMMSSZ-PID.dump'
restore_reports='/var/backups/namo/reports-YYYYMMDDTHHMMSSZ-PID.tar'
restore_complete='/var/backups/namo/complete-YYYYMMDDTHHMMSSZ-PID.manifest'
test -s "$restore_complete" && test -s "$restore_globals" && test -s "$restore_database" && test -s "$restore_reports"
test "$(wc -l < "$restore_complete")" -eq 3
verify_backup_hash()
{
	restore_target=$1
	expected_sha256="$(awk -v target="$restore_target" '$2 == target { count++; hash=$1 } END { if (count == 1) print hash }' "$restore_complete")"
	test "${#expected_sha256}" -eq 64
	printf '%s\n' "$expected_sha256" | grep -Eq '^[0-9a-f]{64}$'
	test "$(sha256 -q "$restore_target")" = "$expected_sha256"
}
verify_backup_hash "$restore_globals"
verify_backup_hash "$restore_database"
verify_backup_hash "$restore_reports"
su -l postgres -c "env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin pg_restore --list '$restore_database' >/dev/null"

restore_stage="$(mktemp -d /var/tmp/namo-report-stage.XXXXXX)"
archive_list="$(mktemp /var/tmp/namo-report-archive.XXXXXX)"
tar -tf "$restore_reports" > "$archive_list"
awk '$0 ~ /^\// || $0 ~ /(^|\/)\.\.?($|\/)/ || $0 ~ /\/\// || $0 !~ /^reports(\/|$)/ { exit 1 }' "$archive_list"
tar -xpf "$restore_reports" -C "$restore_stage"
rm -f "$archive_list"
archive_list=''
test -d "$restore_stage/reports"
test -z "$(find "$restore_stage/reports" -type l -print -quit)"
test -z "$(find "$restore_stage/reports" ! -type d ! -type f -print -quit)"
chown namo:namo "$restore_stage"
chmod 0750 "$restore_stage"
chown -R namo:namo "$restore_stage/reports"
find "$restore_stage/reports" -type d -exec chmod 0750 {} +
find "$restore_stage/reports" -type f -exec chmod 0640 {} +

su -l postgres -c 'env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin initdb -D /var/tmp/namo-restore-cluster/data -U restore_admin --auth-local=trust --auth-host=scram-sha-256 --no-locale -E UTF8'
su -l postgres -c 'env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin pg_ctl -D /var/tmp/namo-restore-cluster/data -o "-p 55432 -k /var/tmp/namo-restore-cluster" -w start'
restore_cluster_started=1
su -l postgres -c "env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin psql -U restore_admin -h /var/tmp/namo-restore-cluster -p 55432 -v ON_ERROR_STOP=1 -d postgres -f '$restore_globals'"
su -l postgres -c 'env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin createdb -U restore_admin -h /var/tmp/namo-restore-cluster -p 55432 namo_restore_test'
su -l postgres -c "env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin pg_restore --exit-on-error -U restore_admin -h /var/tmp/namo-restore-cluster -p 55432 -d namo_restore_test '$restore_database'"

verify_tenant="$(printf '%s\n' 'SELECT id FROM public.tenants ORDER BY created_at, id LIMIT 1;' | su -l postgres -c 'env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin psql -U restore_admin -h /var/tmp/namo-restore-cluster -p 55432 -v ON_ERROR_STOP=1 -At -d namo_restore_test')"
test -n "$verify_tenant"
daemon -f -u namo /usr/bin/env -i \
	PATH='/bin:/usr/bin:/usr/local/bin:/usr/local/sbin' \
	DATABASE_URL='postgres://namo_app@/namo_restore_test?host=/var/tmp/namo-restore-cluster&port=55432&sslmode=disable' \
	DELIVERY_MODE='report' \
	REPORT_DIR="$restore_stage/reports" \
	SESSION_KEY='restore-verification-session-key-0001' \
	/usr/local/sbin/namo generate-report --tenant "$verify_tenant" >/dev/null

duplicate_relative_paths="$(printf '%s\n' "SELECT count(*) FROM (SELECT relative_path FROM public.reports WHERE status = 'generated' GROUP BY relative_path HAVING count(*) > 1) AS duplicates;" | su -l postgres -c 'env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin psql -U restore_admin -h /var/tmp/namo-restore-cluster -p 55432 -v ON_ERROR_STOP=1 -At -d namo_restore_test')"
test "$duplicate_relative_paths" -eq 0
report_hashes="$(mktemp /var/tmp/namo-report-hashes.XXXXXX)"
chmod 0600 "$report_hashes"
printf '%s\n' "SELECT relative_path, sha256 FROM public.reports WHERE status = 'generated' ORDER BY relative_path;" | su -l postgres -c 'env -i PATH=/bin:/usr/bin:/usr/local/bin:/usr/local/sbin psql -U restore_admin -h /var/tmp/namo-restore-cluster -p 55432 -v ON_ERROR_STOP=1 -At -F "|" -d namo_restore_test' > "$report_hashes"
while IFS='|' read -r relative_path expected_sha256; do
	case "$relative_path" in
	""|/*|*//*|*/./*|*/.|*/../*|*/..|*/)
		echo "restored report path verification failed" >&2
		exit 1
		;;
	esac
	printf '%s\n' "$expected_sha256" | grep -Eq '^[0-9a-f]{64}$'
	restored_report="$restore_stage/reports/$relative_path"
	test -f "$restored_report" && test ! -L "$restored_report"
	test "$(sha256 -q "$restored_report")" = "$expected_sha256"
done < "$report_hashes"
test "$(wc -l < "$report_hashes")" -eq "$(find "$restore_stage/reports" -type f | wc -l)"
test -z "$(find "$restore_stage/reports" \( ! -user namo -o ! -group namo \) -print -quit)"
test -z "$(find "$restore_stage/reports" -type d ! -perm 0750 -print -quit)"
test -z "$(find "$restore_stage/reports" -type f ! -perm 0640 -print -quit)"
```

정상 종료와 실패 종료는 같은 cleanup trap을 사용한다. trap은 임시 PostgreSQL을 먼저
중지한 뒤 해시 목록, staging 리포트, 임시 DB를 제거한다. 운영 `DATABASE_URL`,
운영 `REPORT_DIR`, 운영 `namo` 서비스는 변경하지 않는다.

## 향후 메일 기능

메일 코드와 테스트는 보존하지만 1차 런타임에는 연결하지 않는다. `serve`와 수동 생성은
SMTP 설정을 요구하지 않는다. 메일 발송과 테스트 메일은 후속 기능으로 검증한다.

## 검증 경계

- 교차 빌드 성공은 FreeBSD 런타임 성공을 증명하지 않는다.
- API fixture 테스트는 실제 서비스 키·호출량·현재 응답을 검증하지 않는다.
- `daemon -H`/newsyslog 설정 검사는 실제 회전 뒤 새 로그에 계속 기록되는지를 검증하지 않는다.
- 로컬 shell 테스트의 mock 결과는 실제 FreeBSD 서비스 상태, tar 내용, 소유권을 검증하지 않는다.
- 실제 서버에서는 마이그레이션, 네 업무구분 수집, 필터, 예약·수동 리포트, 재시작 뒤 중복 방지, 브라우저 다운로드, 백업 복구를 각각 확인한다.
