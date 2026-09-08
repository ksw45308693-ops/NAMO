# 필터 매칭 일관성 수정 계획 (dev001)

> **실행자용:** 이 문서를 그대로 실행 프롬프트로 사용한다. 각 Task는 순서대로,
> 실패하는 테스트 → 최소 구현 → 테스트 통과 → 커밋 순으로 진행한다.

**목표:** 필터 관리 화면의 "현재 공고 N건 일치", 공고 목록의 "전체 공고 N건",
HTML 리포트의 매칭 건수가 같은 판정 규칙과 같은 공고 집합에서 나오게 만든다.

**아키텍처:** 화면은 `internal/matcher`로 요청 시점에 실시간 판정하고, 리포트는
`public.matches` 스냅샷을 읽는다. 리포트 생성 직전에 `refreshFilterMatches`가 전체 활성
공고를 같은 matcher로 다시 판정하므로 두 경로의 규칙은 이미 동일하다. 남은 차이는
**대상 공고 집합**(화면은 최근 300건, 리포트는 전체 활성 공고)과 **규칙 가시성**이다.

**기술 스택:** Go 1.27, pgx v5, PostgreSQL, 서버 렌더링 `html/template`, 무료 의존성만 사용.

**대상 브랜치:** `dev001`

---

## 배경: 관찰된 증상

| 화면 | 표시 | 비고 |
| --- | --- | --- |
| 공고 목록, 검색어 `회계` | 전체 공고 2건 | 마감 2026.09.09 / 09.11 |
| 필터 관리, 필터 `03` (ANY: 회계) | 현재 공고 0건 일치 | 사용 ON |
| 필터 관리, 필터 `데이터` (ANY: 데이터) | 현재 공고 0건 일치 | 사용 ON |
| 수동 리포트 2026-09-03 07:47 UTC | 64건 (마감 여유일 9999) | 용역 41 / 물품 23 |
| 수동 리포트 2026-09-03 07:54 UTC | 15건 (마감 여유일 3) | 공사 8 / 용역 2 / 물품 5 |

리포트는 정상 동작한다. 화면 건수만 0으로 굳어 있다.

## 근본 원인

### RC1 (주원인) 화면은 최근 300건만 판정한다

- `internal/app/postgres_web.go:240` `tenantNoticesSQL`이 `LIMIT 300`으로 활성 공고를
  자른다. 정렬은 `published_at DESC NULLS LAST`이므로 **가장 최근 게시된 300건**만 남는다.
- `internal/app/postgres_web.go:279` `applyNoticeFilterCounts`는 이 300건에 대해서만
  필터 건수를 누적한다. 공고 목록의 `Pagination.Total`도 같은 300건에서 나온다.
- 리포트는 `internal/app/postgres_report.go:271`에서 `public.matches`를 읽고, 그
  스냅샷은 `loadActiveNotices`(`internal/app/postgres_collector.go:186`, 상한 없음)로
  전체 활성 공고를 판정해 만든다.
- 나라장터 수집은 페이지당 최대 999건(`internal/procurement/client.go:26`)이므로 활성 공고가
  수천 건이면 300건 창은 몇 시간치 신규 공고만 담는다. 리포트에 있던 `데이터` 공고 64건이
  화면에서 0건으로 보이는 현상과 정확히 일치한다.

### RC2 필터의 마감 조건이 화면에 보이지 않는다

- `internal/app/postgres_web.go:993` `filterRuleFromWebCommand`는 `DeadlineWithinDays`를
  **항상** 설정한다(폼 기본값 3, `web/templates/pages.html:223`).
- `internal/matcher/matcher.go:126` 은 마감이 N일을 넘으면 탈락시킨다. 화면 1의 회계 공고
  2건은 마감이 5~7일 뒤이므로 필터 `03`(3일) 기준으로는 실제로 0건이 맞다.
- 그런데 `internal/app/postgres_web.go:1103` `filterSummary`는 포함/제외 키워드, 지역,
  최소 금액만 보여주고 **마감 여유일·업종·수요기관을 생략**한다. 사용자는 "ANY: 회계"만
  보고 왜 0건인지 알 수 없다.

### RC3 마감일이 없는 공고는 어떤 필터에도 잡히지 않는다

- `tenantNoticesSQL`은 `deadline_at IS NULL` 공고를 목록에 포함시키지만,
  `internal/matcher/matcher.go:126-129`는 `DeadlineWithinDays`가 설정된 상태에서 마감이
  0값이면 `invalid_deadline`으로 탈락시킨다. `DeadlineWithinDays`는 항상 설정되므로
  마감 미상 공고는 영구히 0건 취급된다.

### RC4 마감 여유일에 "제한 없음"이 없다

- `internal/web/handler.go:812`는 `deadline_days`를 필수 정수로 받고 0 이상만 검증한다.
  0을 넣으면 사실상 전부 탈락하고, 제한을 없애려면 사용자가 `9999` 같은 매직값을 직접
  입력해야 한다(실제로 07:47 리포트가 그렇게 생성됐다).

### RC5 비활성 필터는 항상 0건으로 보인다

- `internal/app/postgres_web.go:338-341`은 `enabled`인 필터만 matcher에 넘기므로
  사용 OFF 필터의 건수는 계산되지 않고 0으로 렌더링된다. 실제 "0건 일치"와 구분되지 않는다.

---

## Global Constraints

- 무료 오픈소스 의존성만 사용한다. `go.mod`에 새 의존성을 추가하지 않는다.
- 테스트 우선. 각 Task는 실패하는 테스트부터 작성한다.
- 커밋 메시지는 기존 관례를 따른다: `fix(web): 한국어 요약`.
- 완료 게이트: `go test ./...`, `go test -race ./...`, `go vet ./...` 전부 통과.
- FreeBSD amd64 교차 빌드가 깨지지 않아야 한다.
- 화면 문구는 한국어, 서버 렌더링 HTML + 최소 JavaScript 원칙을 유지한다.

---

## Task 0: 현장 진단 (코드 수정 없음)

**목적:** RC1이 실제 원인인지 운영 DB에서 확인하고, 이후 Task의 기대값을 확정한다.

- [ ] **Step 1: 활성 공고 수를 센다**

```sql
SELECT count(*) AS active_notices
FROM public.notices
WHERE deadline_at IS NULL OR deadline_at >= now();
```

기대: 300보다 크면 RC1 확정. 300 이하이면 RC2/RC4가 주원인이므로 Task 1을 Task 2·3 뒤로 미룬다.

- [ ] **Step 2: 300건 창 안팎의 매칭 수를 비교한다**

```sql
WITH window_notices AS (
    SELECT id FROM public.notices
    WHERE deadline_at IS NULL OR deadline_at >= now()
    ORDER BY published_at DESC NULLS LAST, id
    LIMIT 300
)
SELECT f.name,
       count(*) FILTER (WHERE w.id IS NOT NULL) AS in_window,
       count(*)                                 AS total_matches
FROM public.matches m
JOIN public.filters f ON f.id = m.filter_id AND f.tenant_id = m.tenant_id
JOIN public.notices n ON n.id = m.notice_id
LEFT JOIN window_notices w ON w.id = n.id
WHERE m.tenant_id = '<tenant-uuid>'
  AND (n.deadline_at IS NULL OR n.deadline_at >= now())
GROUP BY f.name
ORDER BY f.name;
```

기대: `in_window`가 0에 가깝고 `total_matches`가 크면 RC1 확정.

- [ ] **Step 3: 필터가 실제로 무슨 규칙을 들고 있는지 본다**

```sql
SELECT name, enabled, rules FROM public.filters
WHERE tenant_id = '<tenant-uuid>' ORDER BY created_at;
```

기대: `"DeadlineWithinDays": 3` 같은 숨은 조건 확인. RC2 판단 근거로 남긴다.

- [ ] **Step 4: 결과를 이 문서 하단 "진단 기록"에 붙여넣고 커밋**

```bash
git add docs/implementation/filter-count-consistency-plan.md
git commit -m "docs(web): 필터 건수 불일치 진단 결과 기록"
```

---

## Task 1: 화면 판정 대상을 전체 활성 공고로 넓힌다

**Files:**
- Modify: `internal/app/postgres_web.go:240-248` (`tenantNoticesSQL`)
- Modify: `internal/app/postgres_web.go:76-98` (`Load`), `:124-155` (`loadTenantWebData`)
- Test: `internal/app/postgres_web_test.go`

**Interfaces:**
- Consumes: `appweb.PageRequest{Path string}` (`internal/web/handler.go:220`), 지금은 `Load`에서 무시된다.
- Produces: `loadTenantWebData(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData, withNotices bool) error`,
  `noticesNeeded(path string) bool`.

- [ ] **Step 1: 실패하는 테스트를 작성한다**

`internal/app/postgres_web_test.go`의 기존
`TestTenantNoticeQueryLoadsAllActiveNoticesWithoutStoredMatches`에서 `"LIMIT 300"` 기대를
지우고 아래 테스트를 추가한다.

```go
func TestTenantNoticeQueryHasNoRowCap(t *testing.T) {
	if strings.Contains(tenantNoticesSQL, "LIMIT") {
		t.Fatalf("web notice query still caps rows and undercounts filters: %s", tenantNoticesSQL)
	}
	for _, want := range []string{"FROM public.notices", "deadline_at IS NULL OR deadline_at >= now()"} {
		if !strings.Contains(tenantNoticesSQL, want) {
			t.Fatalf("tenant notice query missing %q: %s", want, tenantNoticesSQL)
		}
	}
}

func TestNoticesLoadOnlyForPagesThatShowThem(t *testing.T) {
	for _, path := range []string{"/notices", "/notices/abc", "/filters"} {
		if !noticesNeeded(path) {
			t.Fatalf("%s needs notices but load was skipped", path)
		}
	}
	for _, path := range []string{"/dashboard", "/settings", "/reports", "/admin"} {
		if noticesNeeded(path) {
			t.Fatalf("%s does not render notices but still loads them", path)
		}
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/app -run "TenantNoticeQueryHasNoRowCap|NoticesLoadOnlyForPages" -v`
Expected: FAIL — `undefined: noticesNeeded`, `LIMIT` 잔존.

- [ ] **Step 3: 최소 구현**

`internal/app/postgres_web.go`

```go
const tenantNoticesSQL = `SELECT id::text,payload
FROM public.notices
WHERE deadline_at IS NULL OR deadline_at >= now()
ORDER BY published_at DESC NULLS LAST,id`

func noticesNeeded(path string) bool {
	return path == "/notices" || strings.HasPrefix(path, "/notices/") || path == "/filters"
}
```

`Load`에서 무시하던 인자를 받아 넘긴다.

```go
func (s *WebService) Load(ctx context.Context, requestContext appweb.RequestContext, page appweb.PageRequest) (appweb.AppData, error) {
	...
	err = s.Repository.withTenant(ctx, requestContext.TenantID, func(tx pgx.Tx) error {
		return loadTenantWebData(ctx, tx, requestContext.TenantID, &data, noticesNeeded(page.Path))
	})
	return data, err
}
```

`loadTenantWebData`에서 조건부로 공고를 읽는다.

```go
func loadTenantWebData(ctx context.Context, tx pgx.Tx, tenantID string, data *appweb.AppData, withNotices bool) error {
	...
	filters, err := loadTenantFilters(ctx, tx, tenantID, data)
	if err != nil {
		return err
	}
	if withNotices {
		if err := loadTenantNotices(ctx, tx, data, filters); err != nil {
			return err
		}
	}
	...
}
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/app ./internal/web -v`
Expected: PASS. 실패하면 `loadTenantWebData` 호출부(테스트 포함)를 모두 새 시그니처로 고친다.

- [ ] **Step 5: 커밋**

```bash
git add internal/app/postgres_web.go internal/app/postgres_web_test.go
git commit -m "fix(web): 필터 건수를 전체 활성 공고 기준으로 계산"
```

---

## Task 2: 필터 요약에 숨은 조건을 드러낸다

**Files:**
- Modify: `internal/app/postgres_web.go:1103-1124` (`filterSummary`)
- Test: `internal/app/postgres_web_test.go`

- [ ] **Step 1: 실패하는 테스트를 작성한다**

```go
func TestFilterSummaryShowsHiddenNarrowingRules(t *testing.T) {
	days := 3
	summary := filterSummary(matcher.Rule{
		IncludeAny:         []string{"회계"},
		Categories:         []model.Category{model.CategoryConstruction},
		Agencies:           []string{"한국철도공사"},
		DeadlineWithinDays: &days,
	})
	for _, want := range []string{"ANY: 회계", "업종: 공사", "기관: 한국철도공사", "마감 3일 이내"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("filter summary %q missing %q", summary, want)
		}
	}
}

func TestFilterSummaryShowsUnlimitedDeadline(t *testing.T) {
	if summary := filterSummary(matcher.Rule{IncludeAny: []string{"데이터"}}); strings.Contains(summary, "마감") {
		t.Fatalf("unlimited deadline should not print a window: %q", summary)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/app -run FilterSummary -v`
Expected: FAIL — `업종: 공사` 등이 없음.

- [ ] **Step 3: 최소 구현**

`filterSummary`의 `parts` 용량을 `make([]string, 0, 8)`로 늘리고 아래를 추가한다.
`strconv`는 이미 import되어 있다.

```go
	if len(rule.Categories) > 0 {
		labels := make([]string, 0, len(rule.Categories))
		for _, category := range rule.Categories {
			labels = append(labels, categoryLabel(category))
		}
		parts = append(parts, "업종: "+strings.Join(labels, ", "))
	}
	if len(rule.Agencies) > 0 {
		parts = append(parts, "기관: "+strings.Join(rule.Agencies, ", "))
	}
	if rule.DeadlineWithinDays != nil {
		parts = append(parts, "마감 "+strconv.Itoa(*rule.DeadlineWithinDays)+"일 이내")
	}
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/app -run FilterSummary -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/app/postgres_web.go internal/app/postgres_web_test.go
git commit -m "fix(web): 필터 요약에 마감·업종·기관 조건 표시"
```

---

## Task 3: 마감 여유일에 "제한 없음"을 넣고 0일을 막는다

**Files:**
- Modify: `internal/web/handler.go:807-852` (`handleSaveFilter`), `:230-241` (`FilterCommand`)
- Modify: `internal/app/postgres_web.go:987-1005` (`filterRuleFromWebCommand`)
- Modify: `web/templates/pages.html:223`
- Test: `internal/web/handler_test.go`, `internal/app/postgres_web_test.go`

**Interfaces:**
- Produces: `appweb.FilterCommand.DeadlineDays *int` (nil = 제한 없음). 기존 `int` 필드를 교체하므로
  `internal/app` 호출부와 테스트를 함께 고친다.

- [ ] **Step 1: 실패하는 테스트를 작성한다**

`internal/app/postgres_web_test.go`

```go
func TestFilterRuleOmitsDeadlineWhenUnlimited(t *testing.T) {
	rule := filterRuleFromWebCommand(appweb.FilterCommand{Name: "데이터", IncludeKeywords: "데이터"})
	if rule.DeadlineWithinDays != nil {
		t.Fatalf("unlimited filter still carries a deadline window: %d", *rule.DeadlineWithinDays)
	}
}

func TestFilterRuleKeepsRequestedDeadlineWindow(t *testing.T) {
	days := 3
	rule := filterRuleFromWebCommand(appweb.FilterCommand{Name: "03", IncludeKeywords: "회계", DeadlineDays: &days})
	if rule.DeadlineWithinDays == nil || *rule.DeadlineWithinDays != 3 {
		t.Fatalf("deadline window lost: %+v", rule.DeadlineWithinDays)
	}
}
```

`internal/web/handler_test.go` — 기존 필터 저장 테스트의 요청 생성 방식(CSRF 토큰,
`tenant_admin` 컨텍스트)을 그대로 재사용한다.

```go
func TestSaveFilterRejectsZeroDeadlineDays(t *testing.T) {
	form := url.Values{"name": {"03"}, "include_keywords": {"회계"}, "deadline_days": {"0"}}
	if code := postFilterForm(t, form); code != http.StatusBadRequest {
		t.Fatalf("zero deadline window accepted: %d", code)
	}
}

func TestSaveFilterAcceptsEmptyDeadlineDays(t *testing.T) {
	form := url.Values{"name": {"데이터"}, "include_keywords": {"데이터"}, "deadline_days": {""}}
	if code := postFilterForm(t, form); code != http.StatusSeeOther {
		t.Fatalf("unlimited deadline rejected: %d", code)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/app ./internal/web -run "Deadline" -v`
Expected: FAIL — 컴파일 오류(`DeadlineDays` 타입 불일치) 또는 상태 코드 불일치.

- [ ] **Step 3: 최소 구현**

`internal/web/handler.go`

```go
type FilterCommand struct {
	...
	DeadlineDays *int
}
```

```go
	var deadlineDays *int
	if raw := strings.TrimSpace(r.FormValue("deadline_days")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 1 || days > 365 {
			http.Error(w, "마감 여유일은 1~365 사이로 입력하거나 비워 주세요.", http.StatusBadRequest)
			return
		}
		deadlineDays = &days
	}
```

`command.DeadlineDays < 0` 검증은 삭제한다(위에서 이미 검증한다).

`internal/app/postgres_web.go`

```go
	rule := matcher.Rule{
		Exclude:            splitTerms(command.ExcludeKeywords),
		Agencies:           splitTerms(command.Agency),
		Regions:            splitTerms(command.Region),
		MinAmount:          command.MinimumAmount,
		DeadlineWithinDays: command.DeadlineDays,
	}
```

`web/templates/pages.html:223`

```html
<label>마감 여유일<input name="deadline_days" type="number" min="1" max="365" placeholder="비우면 제한 없음" {{if not .Writable}}disabled{{end}}></label>
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/app ./internal/web -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/web/handler.go internal/web/handler_test.go internal/app/postgres_web.go internal/app/postgres_web_test.go web/templates/pages.html
git commit -m "fix(web): 마감 여유일에 제한 없음 선택 추가"
```

---

## Task 4: 마감일 미상 공고 정책을 고정한다

**Files:**
- Modify(필요 시): `internal/matcher/matcher.go:126-134`
- Test: `internal/matcher/matcher_test.go`

**결정:** 마감 여유일이 설정된 필터에서 마감일 없는 공고는 탈락을 유지한다. 마감 여유일이
없는(nil) 필터에서는 마감일 없는 공고도 매칭되어야 한다. Task 3 이후 이 경로가 실제로
생기므로 회귀 테스트로 고정한다.

- [ ] **Step 1: 회귀 테스트를 작성한다**

```go
func TestUnlimitedDeadlineRuleMatchesNoticeWithoutDeadline(t *testing.T) {
	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	result := MatchAt(now, model.Notice{Title: "데이터 구축 용역"}, Rule{IncludeAny: []string{"데이터"}})
	if !result.Matched {
		t.Fatalf("notice without deadline was dropped: %+v", result)
	}
}

func TestDeadlineWindowRuleDropsNoticeWithoutDeadline(t *testing.T) {
	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	days := 3
	result := MatchAt(now, model.Notice{Title: "데이터 구축 용역"}, Rule{IncludeAny: []string{"데이터"}, DeadlineWithinDays: &days})
	if result.Matched {
		t.Fatal("notice without deadline slipped through a deadline window rule")
	}
	if len(result.Reasons) != 1 || result.Reasons[0] != ReasonInvalidDeadline {
		t.Fatalf("unexpected reasons: %+v", result.Reasons)
	}
}
```

- [ ] **Step 2: 실행한다**

Run: `go test ./internal/matcher -run Deadline -v`
Expected: 두 테스트 모두 PASS면 구현 변경 없이 회귀 고정으로 끝낸다. 실패하면 Step 3.

- [ ] **Step 3: 실패한 경우에만 구현을 고친다**

`matcher.go`의 `DeadlineWithinDays` 분기를 위 기대에 맞게 정리한다. 다른 조건은 건드리지 않는다.

- [ ] **Step 4: 커밋**

```bash
git add internal/matcher/matcher_test.go internal/matcher/matcher.go
git commit -m "test(matcher): 마감일 미상 공고 처리 회귀 고정"
```

---

## Task 5: 비활성 필터 건수 표기를 구분한다

**Files:**
- Modify: `web/templates/pages.html` 저장된 필터 목록의 `<small>현재 공고 {{.Matches}}건 일치</small>`
- Test: `internal/web/handler_test.go`

- [ ] **Step 1: 실패하는 테스트를 작성한다**

기존 필터 화면 렌더링 테스트의 헬퍼를 재사용한다.

```go
func TestDisabledFilterShowsPausedLabelInsteadOfZeroMatches(t *testing.T) {
	body := renderFiltersPage(t, []FilterView{{ID: "f1", Name: "데이터", Summary: "ANY: 데이터", Enabled: false}})
	if strings.Contains(body, "현재 공고 0건 일치") {
		t.Fatal("disabled filter renders a misleading zero count")
	}
	if !strings.Contains(body, "사용 안 함") {
		t.Fatalf("disabled filter is not labelled: %s", body)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/web -run DisabledFilter -v`
Expected: FAIL

- [ ] **Step 3: 최소 구현**

```html
<small>{{if .Enabled}}현재 공고 {{.Matches}}건 일치{{else}}사용 안 함{{end}}</small>
```

- [ ] **Step 4: 통과 확인 후 커밋**

```bash
git add web/templates/pages.html internal/web/handler_test.go
git commit -m "fix(web): 비활성 필터 건수 표기 구분"
```

---

## Task 6: 전체 회귀 검증

- [ ] **Step 1: 전체 테스트**

```bash
go test ./...
go test -race ./...
go vet ./...
```

Expected: 전부 PASS.

- [ ] **Step 2: FreeBSD 교차 빌드**

```powershell
$env:CGO_ENABLED='0'; $env:GOOS='freebsd'; $env:GOARCH='amd64'
go build -trimpath -o build/namo-freebsd-amd64 .
```

Expected: 빌드 성공.

- [ ] **Step 3: 화면 대조 확인**

같은 테넌트에서 아래 세 값이 일치하는지 확인한다. 리포트는 생성 시점 스냅샷이므로
비교 직전에 수동 생성을 한 번 돌린 뒤 비교한다.

1. `/filters`의 필터별 "현재 공고 N건 일치"
2. `/notices?filter=<필터 ID>`의 "필터 적용 공고 N건"
3. 방금 생성한 수동 리포트의 "일치 공고 총 N건"

- [ ] **Step 4: 결과를 문서에 남기고 커밋**

```bash
git add docs/implementation/filter-count-consistency-plan.md
git commit -m "docs(web): 필터 건수 일관성 검증 결과 기록"
```

---

## 하지 말 것

- `public.matches`를 화면 건수 소스로 되돌리지 말 것. `TestTenantNoticeQueryLoadsAllActiveNoticesWithoutStoredMatches`가
  지키고 있는 "필터 즉시 반영" 요구사항을 되돌리는 회귀다.
- `LIMIT`을 더 큰 숫자로 바꾸는 임시 처방을 하지 말 것. 같은 버그가 조용히 재발한다.
- 한 커밋에 여러 Task를 섞지 말 것.

## 진단 기록

### 2026-09-04 운영 DB 확인

- 환경: FreeBSD Bastille Jail `namo`, PostgreSQL 데이터베이스 `namo`, 테넌트 `FM`
- 활성 공고: `6,869건` — 300건을 초과하므로 RC1 확정
- 최근 300건 창과 전체 활성 공고의 저장 매칭 비교:

| 필터 | 최근 300건 창 | 전체 활성 공고 |
| --- | ---: | ---: |
| `03` | 1건 | 10건 |
| `데이터` | 0건 | 5건 |

- 두 필터 모두 활성 상태이며 `DeadlineWithinDays: 3`을 포함함 — RC2 확인
- 결론: 계획 순서를 유지해 Task 1부터 진행한다.
