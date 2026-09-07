# namo

나라장터 공고를 정기 수집하고 고객사 규칙으로 선별해 웹 화면과 HTML 리포트로 제공하는 Go 서비스다.

## 구성

- Go 1.27 단일 서비스
- PostgreSQL
- 서버 렌더링 HTML, CSS, 최소 JavaScript
- FreeBSD amd64, Nginx, `rc.d`

## 설치

```text
go test ./...
go install
```

FreeBSD `root` 계정에서 `GOBIN`을 따로 설정하지 않았다면 실행 파일은
`/root/go/bin/namo`에 설치된다.

## 명령

```text
namo migrate
namo create-admin --email admin@example.internal --name 관리자 --password-file /secure/admin.pass
namo collect-once
namo generate-report --tenant 11111111-1111-1111-1111-111111111111
namo serve
```

환경 변수 예시는 `.env.example`에 있다. 실제 비밀값 파일은 저장소에 포함하지 않는다.
`serve`의 `BASE_URL`은 경로·쿼리 없는 HTTPS origin, `LISTEN_ADDR`은 1~65535의
loopback 포트만 허용한다. `serve`와 `generate-report`는 `DELIVERY_MODE=report`와
기존의 안전한 절대 디렉터리인 `REPORT_DIR`이 필요하다. 파일시스템 루트와 심볼릭 링크는
리포트 디렉터리로 사용할 수 없다.
`DATABASE_URL`은 `NOBYPASSRLS` 서비스 계정, `MIGRATION_DATABASE_URL`은 PostgreSQL
superuser 계정으로 반드시 분리한다. `CREATEROLE`만 있는 계정은 사용할 수 없다. 마이그레이션이
`BYPASSRLS` 보조 역할을 고정하고 그 역할 소유 함수를 안전하게 교체하기 때문이다. 관리자 URL은
`migrate`와 `create-admin`을 마친 뒤 서비스 환경에서 제거한다.

FreeBSD에서 수동 리포트 생성은 `root`로 실행하지 않는다. 서비스 계정으로 실행한다.

```text
daemon -f -u namo namo generate-report --tenant 11111111-1111-1111-1111-111111111111
```

## 계정

플랫폼 관리자는 **환경 설정 → 나라장터 API 키**에서 Encoding 또는 Decoding 인증키를 등록한다.
Encoding 키는 한 번 디코딩한 뒤 저장한다. 원래 키의 `+`는 보존하며 API 요청 시 한 번만 인코딩한다.
잘못된 퍼센트 인코딩, 이중 인코딩, 키 내부의 공백은 거부한다.
키 없이도 앱과 로그인을 시작할 수 있다. 공고 수집만 대기하며, 저장한 키는 다음 수집부터
적용된다. 저장 후 **플랫폼 관리 → 수집 1회 실행**으로 실제 인증과 수집을 확인한다.
등록됨 표시는 저장 상태이며 API 인증 성공을 뜻하지 않는다.

키는 `G2B_API_KEY_FILE`에 저장한다. FreeBSD 기본 경로는
`/var/db/namo/secrets/g2b-api-key`이고, 디렉터리는 `0700`, 파일은 `0600`이다.
Windows 개발 환경의 기본 경로는 `%APPDATA%\namo\secrets\g2b-api-key`이다.
저장 파일이 없을 때만 기존 `G2B_API_KEY` 환경변수를 사용한다. 파일이 손상되거나
읽기 권한이 없으면 수집을 중단한다. 사용자 지정 경로는 서비스 계정 전용 디렉터리로
미리 준비한다. 키는 암호화된 DB가 아닌 권한 제한 파일에 저장되므로 서버 관리자도 읽을 수 있다.
기존 DB·리포트 백업에는 키가 포함되지 않는다. 복구 후 웹에서 다시 등록한다.

계정은 `/signup`에서 이메일과 비밀번호만으로 만든다. 가입하면 바로 로그인되지만
회사(테넌트)가 배정되지 않은 상태이므로 대기 화면만 보인다. 비밀번호는 UTF-8 기준
12~72바이트로 제한한다. 플랫폼 관리자 계정은 `create-admin` 명령으로 만든다.

회사와 배정은 플랫폼 관리자가 `/admin`에서 처리한다.

1. `회사 등록`에 회사명, 대표 이메일, 관리자 이름, 관리자 이메일을 입력한다. 회사와
   기본 07:00 리포트 일정이 생성된다. 초대 메일은 보내지 않는다.
2. `회원 계정 배정`에서 가입 계정에 회사와 권한을 지정하고 `적용`한다. `해제`는 회사
   배정을 없애고 권한을 `일반 사용자`로 되돌린다.

구성원 제외와 계정 삭제는 권한이 다르다. 회사 관리자는 `환경 설정`의 구성원 목록에서
`회사에서 제외`로 구성원을 내린다. 계정은 남고 배정 대기 상태로 돌아간다. 플랫폼
관리자는 `/admin`의 `계정 삭제`로 계정 자체를 지운다. 이때 로그인 세션도 함께 사라지며
되돌릴 수 없다. 두 경우 모두 자기 자신은 대상이 될 수 없고, 플랫폼 관리자 계정은 웹에서
삭제되지 않는다.

권한은 두 가지다. `회사 관리자`(`tenant_admin`)는 필터, 환경 설정, 리포트 생성을 바꿀 수
있고 `일반 사용자`(`member`)는 조회만 한다. 회사 없이 `회사 관리자`를 줄 수 없고, 플랫폼
관리자 계정은 이 화면의 대상이 아니다. 변경 결과는 해당 사용자의 다음 요청부터 적용된다.

## 로컬 확인

```text
go test ./...
go test -race ./...
go vet ./...
```

FreeBSD 교차 빌드:

```text
$env:CGO_ENABLED='0'
$env:GOOS='freebsd'
$env:GOARCH='amd64'
go build -trimpath -o build/namo-freebsd-amd64 .
```

실서버 절차는 `docs/operations-freebsd.md`를 따른다.
유료 서비스 없이 운영하는 범위와 전제는 `docs/zero-cost-stack.md`에 정리했다.
