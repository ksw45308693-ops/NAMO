# 검색내역 조회 리포트 추가 계획 (dev001)

> **실행자용:** 이 문서를 그대로 실행 프롬프트로 사용한다. 각 Task는 순서대로,
> 실패하는 테스트 → 최소 구현 → 테스트 통과 → 커밋 순으로 진행한다.

**목표:** 리포트를 첨부된 레거시 "검색내역 조회" 화면과 같은 표 형식으로 만든다. 열은
`# / 구분 / 일자 / 시간 / 키워드 / 업무구분 / 업무여부 / 사업명·공고명 / 공고기관 /
진행·게시일자 / 추정가격 / 레코드생성일시` 12열이고, 표의 내용은 테넌트가 저장해 둔 활성
필터의 판정 결과로 채운다. 표 위에는 조회 조건(구분·일자·키워드·적용 필터), 총 행 수,
추정가격 합계를 둔다.

**아키텍처:** 리포트는 `public.report_items` 스냅샷을 읽어 `internal/report`가 결정적
HTML을 만든다(현행 유지). 이 계획은 세 곳만 바꾼다.

1. 스냅샷(`report_items`)에 표에 필요한 열을 추가한다.
2. 렌더러를 카드 단독 → **표 + 매칭 근거 카드**로 바꾼다.
3. 조회 조건 머리글과 추정가격 합계를 추가한다.

판정 로직(`internal/matcher`)과 건수 산정 방식은 건드리지 않는다. 직전 작업
(`docs/implementation/filter-count-consistency-plan.md`)이 맞춰 놓은 화면·리포트 건수
일치를 깨지 않는 것이 이 계획의 상위 제약이다.

**기술 스택:** Go 1.27, pgx v5, PostgreSQL, 서버 렌더링 `html/template`, 무료 의존성만 사용.

**대상 브랜치:** `dev001`

---

## 배경: 요청 원본

레거시 조회 화면 이미지 한 장이 요구사항이다. 읽어낸 구조는 다음과 같다.

- 상단 조회 조건 줄: `구분 = 전체`, `일자 = 2026-01-07`, `키워드 = 경관조명`
- 결과 요약: `24 rows`, 예산금액 합계 `3,886,767,820`
- 12열 표. `구분` 값은 `입찰공고목록-입찰공고`와 `발주목록-사전규격공개` 두 종류가 섞여 있다.
- 같은 사업명이 `시간` 2351과 2354에 중복 등장한다 → 원본은 **수집 실행마다 적중 결과를
  적재하는 로그**다.

**스타일 결정:** 이미지의 검은 배경·형광 색·모노스페이스 표는 채택하지 않는다. `DESIGN.md`가
navy/cyan/light-gray 토큰과 "장식 효과 없음"을 규정하고 있고 리포트는 인쇄·보관 산출물이다.
**표 구성(열, 정렬, 집계, 조회 조건 머리글)만 가져오고 색과 타이포는 기존 리포트 토큰을
유지한다.**

**행 정의 결정:** 이미지처럼 수집 실행별 로그를 쌓지 않는다. `report_items`는 이미
`UNIQUE (tenant_id, report_id, match_id)`로 리포트 1건 = 스냅샷 1회이고, 렌더러는
`loadReportNotices`(`internal/app/postgres_report.go:364`)에서 공고 단위로 병합한다.
**한 행 = 한 공고**를 유지하고, 여러 필터가 같은 공고를 잡으면 `키워드` 열에 합집합을
쉼표로 이어 붙인다. 이 규칙을 지켜야 표의 `N rows`가 기존 "일치 공고 총 N건"과 같은 수로
남는다.

---

## 열 매핑 (현행 데이터 기준)

| # | 열 | 이미지 예시 | 현행 소스 | 상태 |
| --- | --- | --- | --- | --- |
| 1 | `#` | 1~24 | `report_items.ordinal` 순서를 유지해 공고 단위 병합 후 1부터 부여 | 있음 |
| 2 | `구분` | 입찰공고목록-입찰공고 | 없음 | 수집원은 입찰공고 목록 4종뿐(`internal/procurement/client.go:492`). `입찰공고목록-입찰공고` 고정값으로 채운다. |
| 3 | `일자` | 20260107 | `notices.collected_at` (KST) | 열 추가 |
| 4 | `시간` | 2354 | 같은 값의 `HHmm` | 열 추가 |
| 5 | `키워드` | 경관조명 | `report_items.reasons` → `details[]` 중 `Code`가 `include_any`·`include_all`인 항목의 `RuleValue` | 있음(파싱만 필요) |
| 6 | `업무구분` | 물품 | `report_items.category` | 있음 |
| 7 | `업무여부` | 내자 | 없음 | `category='foreign'` → `외자`, 그 외 → `내자` 파생. Task 0에서 원본 필드 확인. |
| 8 | `사업명/공고명` | … 스마트폴 제작 | `report_items.title` | 있음 |
| 9 | `공고기관` | 사회복지법인 만원복지재단 | `report_items.agency`의 **공고기관** `ntceInsttNm` | Task 0에서 확정 |
| 10 | `진행/게시일자` | 2026-01-02 | `notices.published_at` (`bidNtceDt`) | 열 추가 |
| 11 | `추정가격` | 599,995,000 | `report_items.amount`의 **추정가격** `presmptPrce` | Task 0에서 확정 |
| 12 | `레코드생성일시` | 2026-01-07 23:55 | 예약: `digest_window_items.matched_at`, 수동: `matches.created_at` | 열 추가 |

**정직성 규칙(필수):** 원본에 없는 값을 만들지 않는다.

- Task 0 결과에 따라 9번 열 제목은 **`공고기관`**으로 쓴다.
- Task 0 결과에 따라 11번 열 제목은 **`추정가격`**으로 쓴다.
- 값이 비면 `-`로 렌더한다. 0을 추정값으로 채우지 않는다.
- 과거 스냅샷의 새 열은 NULL로 유지한다. 이를 새 렌더러로 읽을 때 해당 셀은 `-`다.
  이미 저장된 과거 HTML은 다운로드 시 재렌더링하지 않으므로 기존 형식을 그대로 유지한다.
  스냅샷과 HTML 모두 소급 백필하거나 덮어쓰지 않는다.

---

## Global Constraints

- 무료 오픈소스 의존성만 사용한다. `go.mod`에 새 의존성을 추가하지 않는다.
- 테스트 우선. 각 Task는 실패하는 테스트부터 작성한다.
- 커밋 메시지는 기존 관례를 따른다: `feat(report):`, `feat(db):`, `fix(web):`, `docs(report):`.
- 완료 게이트: `go test ./...`, `go test -race ./...`, `go vet ./...` 전부 통과.
- FreeBSD amd64 교차 빌드가 깨지지 않아야 한다.
- 리포트는 자기완결 단일 HTML을 유지한다. 외부 CSS·JS·폰트·이미지를 참조하지 않는다.
- 시간 변환은 `time.FixedZone("KST", 9*60*60)`을 쓴다. `time.LoadLocation`은 tzdata 의존성
  때문에 리포트 결정성을 깨뜨릴 수 있으므로 `internal/report`에서 쓰지 않는다
  (`internal/app/report_runner.go:170`이 이미 같은 방식이다).
- `internal/report/render_test.go`의 기존 단정은 **그대로 통과해야 한다.** 특히
  `총 N건`, `공사 1`/`용역 1`/`물품 1`/`외자 1`, `일치 규칙 없음`,
  `새로 일치한 공고가 없습니다.`, `마감 미정`, `미정`, 규칙·이유 출력 순서,
  `javascript:` URL 비링크화. 매칭 근거 카드 섹션을 **삭제하지 말 것**
  (`PRODUCT.md`의 "Explain why each notice matched"가 제품 원칙이다).
- 마이그레이션 파일은 새로 추가만 한다. 적용된 파일을 수정하면 체크섬 검증이 깨진다.

---

## Task 0: 원본 필드 진단 (코드 수정 없음)

**목적:** 9번·11번·7번 열의 열 제목과 소스를 확정한다. 수집된 원본 항목은
`notices.payload->'RawJSON'`에 스크럽된 상태로 보존된다(`internal/model/notice.go:38`).

- [x] **Step 1: RawJSON에 어떤 키가 실제로 있는지 센다**

```sql
SELECT key, count(*) AS rows
FROM public.notices n,
     LATERAL jsonb_object_keys(n.payload->'RawJSON') AS key
WHERE n.payload->'RawJSON' ? 'bidNtceNo'
GROUP BY key
ORDER BY rows DESC, key;
```

확인할 키: `dminsttNm`(수요기관), `asignBdgtAmt`(배정예산금액), `bsnsDivNm`(업무구분명),
`intrbidYn`(국제입찰여부), `ntceKindNm`(공고종류), `rgstDt`(등록일시).

- [x] **Step 2: 잘린 RawJSON 비율을 센다**

```sql
SELECT count(*) FILTER (WHERE payload->'RawJSON' ? 'truncated') AS truncated,
       count(*) FILTER (WHERE payload->'RawJSON' IS NULL)       AS missing,
       count(*)                                                 AS total
FROM public.notices;
```

`truncated`가 크면 RawJSON 파생 열은 신뢰할 수 없다 → 해당 열은 현행 필드(공고기관, 추정가격)로
확정하고 열 제목을 그에 맞춘다.

- [x] **Step 3: 표본을 눈으로 확인한다**

```sql
SELECT payload->'RawJSON'->>'dminsttNm'    AS demand_agency,
       payload->>'Agency'                  AS notice_agency,
       payload->'RawJSON'->>'asignBdgtAmt' AS budget,
       payload->>'Amount'                  AS estimate,
       payload->'RawJSON'->>'intrbidYn'    AS international
FROM public.notices
ORDER BY collected_at DESC
LIMIT 20;
```

- [x] **Step 4: 결정을 이 문서 하단 "진단 기록"에 적고 커밋**

결정 항목 3개를 명시한다.
(가) 9번 열 = `수요기관`(RawJSON) / `공고기관`(현행) 중 무엇인가.
(나) 11번 열 = `예산금액`(RawJSON) / `추정가격`(현행) 중 무엇인가.
(다) 7번 열 = `intrbidYn` 사용 / `category` 파생 중 무엇인가.

```bash
git add docs/implementation/search-history-report-plan.md
git commit -m "docs(report): 검색내역 열 원본 필드 진단 결과 기록"
```

---

## Task 1: 표 렌더러를 추가한다 (DB 변경 없이 먼저)

새 필드는 이 시점에 항상 빈 값이므로 `-`로 렌더된다. Task 3에서 값이 채워진다. 이렇게
쪼개면 렌더러 테스트가 PostgreSQL 없이 돈다.

**Files:**
- Modify: `internal/report/render.go:19` (`Notice`), `:26` (`Document`), `:34` (`reportView`),
  `:43` (`noticeView`), `:56` (`reportTemplate`), `:66` (`BuildHTML`), `:148` (`formatAmount`)
- Test: `internal/report/render_test.go`

**Interfaces:**
- Produces:

```go
type Notice struct {
	ID, Title, Category, Agency, Region, SourceURL string
	SourceKind                                     string
	Keywords                                       []string
	Amount                                         int64
	Deadline, PostedAt, CollectedAt, RecordedAt    time.Time
	Matches                                        []Match
}
```

  기존 필드 이름은 바꾸지 않는다. 기관 열은 기존 `Agency`를 사용한다.

- [x] **Step 1: 실패하는 테스트를 작성한다**

```go
func TestBuildHTMLRendersSearchHistoryTable(t *testing.T) {
	collected := time.Date(2026, 1, 7, 14, 54, 0, 0, time.UTC)  // KST 23:54
	posted := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)       // KST 10:00
	recorded := time.Date(2026, 1, 7, 14, 55, 12, 0, time.UTC)  // KST 23:55:12
	doc := Document{
		TenantName: "테넌트", ScheduleName: "일정", Trigger: "수동",
		DueAt:     time.Date(2026, 1, 7, 14, 55, 0, 0, time.UTC),
		WindowEnd: time.Date(2026, 1, 7, 14, 55, 0, 0, time.UTC),
		Notices: []Notice{
			{
				Title: "스마트폴 <제작>", Category: "goods", SourceKind: "입찰공고목록-입찰공고",
				Agency: "만원복지재단 & 부설", Keywords: []string{"경관조명", "스마트폴"},
				Amount: 599995000, PostedAt: posted, CollectedAt: collected, RecordedAt: recorded,
				Matches: []Match{{RuleName: "경관조명", Reasons: []string{"포함 키워드 일치"}}},
			},
			{Title: "열 없는 공고", Category: "service", Amount: 0},
		},
	}

	html, err := BuildHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := string(html)
	for _, want := range []string{
		"<table", "검색내역 조회",
		">#<", ">구분<", ">일자<", ">시간<", ">키워드<", ">업무구분<", ">업무여부<",
		">사업명/공고명<", ">진행/게시일자<", ">레코드생성일시<",
		"입찰공고목록-입찰공고", "20260107", "2354", "경관조명, 스마트폴",
		"내자", "스마트폴 &lt;제작&gt;", "만원복지재단 &amp; 부설",
		"2026-01-02", "599,995,000원", "2026-01-07 23:55:12", "2 rows",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("table missing %q:\n%s", want, got)
		}
	}
	// 값이 없는 셀은 추정하지 않고 비워 둔다.
	if !strings.Contains(got, ">-<") {
		t.Fatalf("missing values are not rendered as a dash:\n%s", got)
	}
	// 표의 행 순서가 곧 # 순서다.
	assertInOrder(t, got, "스마트폴 &lt;제작&gt;", "열 없는 공고")
}

func TestBuildHTMLShowsQueryCriteriaAndAmountTotal(t *testing.T) {
	doc := Document{
		TenantName: "테넌트", ScheduleName: "일정",
		DueAt:     time.Date(2026, 1, 7, 14, 55, 0, 0, time.UTC),
		WindowEnd: time.Date(2026, 1, 7, 14, 55, 0, 0, time.UTC),
		Notices: []Notice{
			{Title: "가", Category: "goods", SourceKind: "입찰공고목록-입찰공고", Keywords: []string{"경관조명"}, Amount: 100, Matches: []Match{{RuleName: "조명"}}},
			{Title: "나", Category: "goods", SourceKind: "입찰공고목록-입찰공고", Keywords: []string{"스마트폴"}, Amount: 250, Matches: []Match{{RuleName: "폴"}}},
		},
	}
	got, err := BuildHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	html := string(got)
	for _, want := range []string{
		"2026-01-07",            // 조회 일자(KST)
		"경관조명, 스마트폴",      // 조회 키워드 = 표에 나온 키워드 합집합
		"입찰공고목록-입찰공고",   // 조회 구분
		"조명, 폴",               // 적용 필터
		"350원",                 // 추정가격 합계
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("query criteria missing %q:\n%s", want, html)
		}
	}
}

func TestBuildHTMLKeepsForeignProcurementLabel(t *testing.T) {
	got, err := BuildHTML(Document{
		TenantName: "테넌트", ScheduleName: "일정",
		Notices: []Notice{{Title: "외자 건", Category: "foreign"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "외자") {
		t.Fatalf("foreign notice is not labelled 외자:\n%s", got)
	}
}
```

- [x] **Step 2: 실패를 확인한다**

Run: `go test ./internal/report -run "SearchHistory|QueryCriteria|ForeignProcurement" -v`
Expected: FAIL — `unknown field SourceKind`, `<table` 없음.

- [x] **Step 3: 최소 구현**

`internal/report/render.go`

```go
var kst = time.FixedZone("KST", 9*60*60)

type rowView struct {
	Ordinal                                 int
	Kind, Date, Clock, Keyword              string
	Category, Trade                         string
	Title, Agency, PostedAt, Amount, Record string
	Link                                    template.URL
	HasLink                                 bool
}
```

`reportView`에 다음을 더한다.

```go
	QueryDate, QueryKinds, QueryKeywords, QueryFilters string
	AmountTotal                                        string
	Rows                                               []rowView
```

`BuildHTML`은 기존 `noticeView` 조립을 유지하고 그 옆에서 `rowView`를 만든다. 조회 조건은
표에서 파생한다(호출부 인자를 늘리지 않는다).

```go
	var amountTotal int64
	var kinds, keywords, filters []string
	for index, notice := range doc.Notices {
		// ... 기존 countCategory / noticeView(entry) 조립 유지 ...
		var err error
		amountTotal, err = addAmountTotal(amountTotal, notice.Amount)
		if err != nil {
			return nil, err
		}
		row := rowView{
			Ordinal:  index + 1,
			Kind:     dash(notice.SourceKind),
			Date:     dashTime(notice.CollectedAt, "20060102"),
			Clock:    dashTime(notice.CollectedAt, "1504"),
			Keyword:  dash(strings.Join(notice.Keywords, ", ")),
			Category: categoryLabel(notice.Category),
			Trade:    tradeLabel(notice.Category),
			Title:    notice.Title,
			Agency:   dash(notice.Agency),
			PostedAt: dashTime(notice.PostedAt, "2006-01-02"),
			Amount:   formatTableAmount(notice.Amount),
			Record:   dashTime(notice.RecordedAt, "2006-01-02 15:04:05"),
		}
		if entry.HasLink {
			row.HasLink, row.Link = true, entry.Link
		}
		view.Rows = append(view.Rows, row)
		kinds = appendUnique(kinds, notice.SourceKind)
		for _, keyword := range notice.Keywords {
			keywords = appendUnique(keywords, keyword)
		}
		for _, match := range notice.Matches {
			filters = appendUnique(filters, match.RuleName)
		}
	}
	view.QueryDate = dashTime(doc.DueAt, "2006-01-02")
	view.QueryKinds = dashJoin(kinds)
	view.QueryKeywords = dashJoin(keywords)
	view.QueryFilters = dashJoin(filters)
	view.AmountTotal = formatTableAmount(amountTotal)
```

헬퍼는 새로 만든다. 카드용 `formatAmount`(0 → `미정`)는 **바꾸지 않는다.**
표의 0은 원본 누락과 구별할 수 없으므로 `-`로 표시한다. 합계 범위 검사는 표준 라이브러리
`math`의 경계값을 사용한다.

```go
func tradeLabel(category string) string {
	if category == "foreign" {
		return "외자"
	}
	return "내자"
}

func formatTableAmount(value int64) string {
	if value == 0 {
		return "-"
	}
	return groupDigits(value) + "원"
}

func addAmountTotal(total, amount int64) (int64, error) {
	if amount > 0 && total > math.MaxInt64-amount {
		return 0, fmt.Errorf("estimated amount total overflow: %d + %d", total, amount)
	}
	if amount < 0 && total < math.MinInt64-amount {
		return 0, fmt.Errorf("estimated amount total underflow: %d + %d", total, amount)
	}
	return total + amount, nil
}

func dashTime(value time.Time, layout string) string {
	if value.IsZero() {
		return "-"
	}
	return value.In(kst).Format(layout)
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func dashJoin(values []string) string { return dash(strings.Join(values, ", ")) }
```

`formatAmount`의 천단위 구분 로직을 `groupDigits(value int64) string`으로 뽑아 두 함수가
함께 쓰게 한다. `appendUnique`는 `internal/report`의 지역 헬퍼로 새로 만든다.
`internal/app`의 동명 함수를 참조하지 않는다 — `internal/report`는 `internal/app`에
의존하면 안 된다(의존 방향은 app → report 단방향이다).

템플릿에는 표 섹션을 `summary` 뒤, 카드 앞에 넣는다. 카드 섹션은 `<details>`로 감싸 접어 둔다.

```html
<section class="query"><strong>검색내역 조회</strong><dl>
<div><dt>구분</dt><dd>{{.QueryKinds}}</dd></div><div><dt>일자</dt><dd>{{.QueryDate}}</dd></div>
<div><dt>키워드</dt><dd>{{.QueryKeywords}}</dd></div><div><dt>적용 필터</dt><dd>{{.QueryFilters}}</dd></div>
</dl></section>
{{if .Rows}}<section class="grid"><div class="grid-head"><span>{{.Total}} rows</span><span>추정가격 합계 {{.AmountTotal}}</span></div>
<div class="grid-scroll"><table><thead><tr>
<th scope="col">#</th><th scope="col">구분</th><th scope="col">일자</th><th scope="col">시간</th>
<th scope="col">키워드</th><th scope="col">업무구분</th><th scope="col">업무여부</th>
<th scope="col">사업명/공고명</th><th scope="col">공고기관</th><th scope="col">진행/게시일자</th>
<th scope="col">추정가격</th><th scope="col">레코드생성일시</th>
</tr></thead><tbody>{{range .Rows}}<tr>
<td class="number">{{.Ordinal}}</td><td>{{.Kind}}</td><td class="number">{{.Date}}</td><td class="number">{{.Clock}}</td>
<td>{{.Keyword}}</td><td>{{.Category}}</td><td>{{.Trade}}</td>
<td class="name">{{if .HasLink}}<a href="{{.Link}}">{{.Title}}</a>{{else}}{{.Title}}{{end}}</td>
<td>{{.Agency}}</td><td class="number">{{.PostedAt}}</td>
<td class="number">{{.Amount}}</td><td class="number">{{.Record}}</td>
</tr>{{end}}</tbody></table></div></section>{{end}}
```

기관 열 제목은 Task 0에서 확정한 `공고기관`을 사용한다.

- [x] **Step 4: 통과를 확인한다**

Run: `go test ./internal/report -v`
Expected: PASS. 기존 3개 테스트도 함께 통과해야 한다. `assertInOrder`가 깨지면 표가 카드보다
**앞**에 있는지 확인한다(제목이 표와 카드에 두 번 나오지만 `strings.Index`는 첫 등장을 찾으므로
표 순서가 카드 순서와 같으면 통과한다).

- [x] **Step 5: 커밋**

```bash
git add internal/report/render.go internal/report/render_test.go
git commit -m "feat(report): 검색내역 조회 표 렌더러 추가"
```

---

## Task 2: 스냅샷 열을 추가한다 (마이그레이션 0016)

**Files:**
- Add: `migrations/0016_report_item_search_columns.sql`
- Test: `migrations/embed_test.go` 또는 `internal/store/migrations_test.go`

- [x] **Step 1: 실패하는 테스트를 작성한다**

기존 마이그레이션 목록 테스트에 최신 버전 계약을 더한다.

```go
func TestReportItemSearchColumnsMigrationExists(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for _, migration := range all {
		if migration.Version == 16 {
			body = migration.SQL
		}
	}
	if body == "" {
		t.Fatal("migration 16 is missing")
	}
	for _, want := range []string{"source_kind", "posted_at", "collected_at", "recorded_at", "namo_runtime"} {
		if !strings.Contains(body, want) {
			t.Fatalf("migration 16 missing %q", want)
		}
	}
}
```

- [x] **Step 2: 실패를 확인한다**

Run: `go test ./migrations ./internal/store -v`
Expected: FAIL — migration 16 is missing.

- [x] **Step 3: 최소 구현**

`migrations/0016_report_item_search_columns.sql`

```sql
ALTER TABLE public.report_items ADD COLUMN source_kind text;
ALTER TABLE public.report_items ADD COLUMN posted_at timestamptz;
ALTER TABLE public.report_items ADD COLUMN collected_at timestamptz;
ALTER TABLE public.report_items ADD COLUMN recorded_at timestamptz;

GRANT SELECT, INSERT ON TABLE public.report_items TO namo_runtime;
```

세 timestamptz 열은 **nullable로 둔다.** 과거 리포트 행에 넣을 진짜 값이 없고 스냅샷은
불변이므로 추정값 백필을 하지 않는다. `source_kind`도 같은 이유로 nullable로 둔다.
권한은 테이블 단위이므로 새 열에 추가 GRANT가 꼭
필요하지는 않지만 `0008_report_name_snapshots.sql`의 관례를 따라 GRANT 줄을 다시 적는다.
`migrations/line_endings_test.go`가 개행을 검사하므로 LF로 저장한다.

- [x] **Step 4: 통과를 확인한다**

Run: `go test ./migrations ./internal/store -v`
Expected: PASS

- [x] **Step 5: 커밋**

```bash
git add migrations/0016_report_item_search_columns.sql migrations internal/store
git commit -m "feat(db): 리포트 항목에 검색내역 열 추가"
```

---

## Task 3: 스냅샷과 로더를 새 열에 연결한다

**Files:**
- Modify: `internal/app/postgres_report.go:197` (`scheduledReportItemsSQL`),
  `:264` (수동 스냅샷 INSERT), `:364` (`loadReportNotices`)
- Reuse: `internal/app/postgres_digest.go:318` (`storedMatchReasons`, 변경 없음)
- Test: `internal/app/postgres_report_test.go`

**Interfaces:**
- Produces: `matchedKeywords(payload storedMatchReasons) []string` — `internal/app`.
  `include_any`·`include_all` 항목의 `RuleValue`만 중복 없이 모은다.

- [x] **Step 1: 실패하는 테스트를 작성한다**

```go
func TestScheduledSnapshotRecordsSearchColumns(t *testing.T) {
	for _, want := range []string{
		"source_kind", "posted_at", "collected_at", "recorded_at",
		"n.published_at", "n.collected_at", "i.matched_at",
	} {
		if !strings.Contains(scheduledReportItemsSQL, want) {
			t.Fatalf("scheduled snapshot missing %q: %s", want, scheduledReportItemsSQL)
		}
	}
}

func TestMatchedKeywordsKeepsOnlyIncludeTerms(t *testing.T) {
	var payload storedMatchReasons
	payload.Details = append(payload.Details,
		reasonDetail("include_any", "경관조명"),
		reasonDetail("include_all", "스마트폴"),
		reasonDetail("include_any", "경관조명"),
		reasonDetail("category", "goods"),
		reasonDetail("deadline_within_days", "3"),
	)
	got := matchedKeywords(payload)
	if len(got) != 2 || got[0] != "경관조명" || got[1] != "스마트폴" {
		t.Fatalf("unexpected keywords: %#v", got)
	}
}
```

`storedMatchReasons.Details`는 익명 구조체 슬라이스이므로 테스트 헬퍼 `reasonDetail`을
같은 익명 타입으로 만들거나, 구현 단계에서 `storedMatchReasons`의 익명 구조체를 명명
타입(`storedMatchDetail`)으로 승격한다. 승격을 택하면 `postgres_digest.go:318-323`과
`readableMatchReasons`(`:370`) 호출부를 함께 고치고 기존 digest 테스트가 통과하는지 확인한다.

`loadReportNotices`는 `postgres_report_test.go:283`의 stub 행을 재사용한다. stub이 돌려주는
열 개수를 새 SELECT에 맞춰 늘리고, 두 매칭이 같은 공고를 잡을 때 키워드가 합집합으로
병합되는지 단정을 추가한다.

```go
func TestLoadReportNoticesMergesKeywordsPerNotice(t *testing.T) {
	// stub 두 행: 같은 notice_id, 서로 다른 include_any RuleValue
	// 기대: len(work.Notices) == 1, Keywords == []string{"경관조명", "스마트폴"}
	// 기대: SourceKind/PostedAt/CollectedAt/RecordedAt가 첫 행 값으로 채워진다
}
```

- [x] **Step 2: 실패를 확인한다**

Run: `go test ./internal/app -run "SearchColumns|MatchedKeywords|MergesKeywords" -v`
Expected: FAIL — `undefined: matchedKeywords`, SQL에 열 없음.

- [x] **Step 3: 최소 구현**

`scheduledReportItemsSQL`

```sql
INSERT INTO public.report_items
    (tenant_id,report_id,ordinal,match_id,notice_id,title,category,agency,region,amount,deadline_at,source_url,rule_name,reasons,
     source_kind,posted_at,collected_at,recorded_at)
SELECT $1::uuid,$2::uuid,
       row_number() OVER (ORDER BY n.published_at DESC NULLS LAST,i.title,i.notice_id,i.matched_at,i.match_id),
       i.match_id,i.notice_id,i.title,
       n.payload->>'Category',COALESCE(n.payload->>'Agency',''),COALESCE(n.payload->>'Region',''),
       COALESCE(NULLIF(n.payload->>'Amount',''),'0')::bigint,
       COALESCE(n.deadline_at,TIMESTAMPTZ '0001-01-01 00:00:00+00'),i.source_url,
       COALESCE(f.name,''),i.reasons,
       '입찰공고목록-입찰공고',n.published_at,n.collected_at,i.matched_at
FROM public.digest_window_items i
JOIN public.notices n ON n.id=i.notice_id
LEFT JOIN public.matches m ON m.tenant_id=i.tenant_id AND m.id=i.match_id
LEFT JOIN public.filters f ON f.tenant_id=m.tenant_id AND f.id=m.filter_id
WHERE i.tenant_id=$1::uuid AND i.schedule_id=$3::uuid AND i.due_at=$4 AND i.window_end_at=$5
ORDER BY n.published_at DESC NULLS LAST,i.title,i.notice_id,i.matched_at,i.match_id
ON CONFLICT (tenant_id,report_id,match_id) DO NOTHING
```

수동 스냅샷(`:264`)도 같은 열을 넣고 `recorded_at`은 `m.created_at`, 정렬은
`n.published_at DESC NULLS LAST,n.title,n.id,m.created_at,m.id`로 맞춘다.

**정렬을 바꾸는 이유:** 검색내역 조회는 최신 게시 순이 자연 정렬이다. `ordinal`이 곧 표의
`#`이므로 `row_number()`와 `ORDER BY`를 함께 바꿔야 한다. 현재 `ordinal` 순서를 단정하는
테스트는 없다(`grep -rn "ordinal" internal/app/*_test.go`로 확인했다).

Task 0 결정이 RawJSON 사용이면 아래 식을 추가한다. **캐스팅을 정규식으로 보호한다.**
`payload->'RawJSON'`은 상한을 넘겨 잘렸을 때 `{"truncated":true,...}`가 되므로 키가 없을 수 있고,
원본 문자열이 숫자가 아니면 `::bigint`가 INSERT 전체를 실패시킨다.

```sql
       COALESCE(NULLIF(pg_catalog.btrim(n.payload->'RawJSON'->>'dminsttNm'),''),
                COALESCE(n.payload->>'Agency','')),
       CASE WHEN n.payload->'RawJSON'->>'asignBdgtAmt' ~ '^[0-9]+$'
            THEN (n.payload->'RawJSON'->>'asignBdgtAmt')::bigint END
```

`loadReportNotices`(`:364`)

```go
	rows, err := tx.Query(ctx, `SELECT notice_id::text,title,category,agency,region,amount,deadline_at,source_url,rule_name,reasons,
source_kind,posted_at,collected_at,recorded_at
FROM public.report_items
WHERE tenant_id=$1::uuid AND report_id=$2::uuid
ORDER BY ordinal`, work.TenantID, work.ReportID)
```

스캔에 `sourceKind string`과 `postedAt, collectedAt, recordedAt *time.Time`을 더한다.
첫 등장에서 `report.Notice`를 만들 때 값을 채우고, 이후 같은 공고의 행에서는 키워드만
합집합으로 누적한다.

```go
		if !exists {
			notice := report.Notice{
				ID: noticeID, Title: title, Category: category, Agency: agency, Region: region,
				Amount: amount, Deadline: deadline, SourceURL: sourceURL, SourceKind: sourceKind,
			}
			notice.PostedAt = timeOrZero(postedAt)
			notice.CollectedAt = timeOrZero(collectedAt)
			notice.RecordedAt = timeOrZero(recordedAt)
			work.Notices = append(work.Notices, notice)
		}
		for _, keyword := range matchedKeywords(reasons) {
			work.Notices[index].Keywords = appendUnique(work.Notices[index].Keywords, keyword)
		}
```

`matchedKeywords`는 `postgres_report.go`에 둔다.

```go
func matchedKeywords(payload storedMatchReasons) []string {
	var keywords []string
	for _, detail := range payload.Details {
		switch matcher.Reason(detail.Code) {
		case matcher.ReasonIncludeAny, matcher.ReasonIncludeAll:
			keywords = appendUnique(keywords, detail.RuleValue)
		}
	}
	return keywords
}
```

- [x] **Step 4: 통과를 확인한다**

Run: `go test ./internal/app -v`
Expected: PASS. 실패하면 `postgres_report_test.go`의 stub 행 열 개수를 새 SELECT와 맞춘다.

- [x] **Step 5: 커밋**

```bash
git add internal/app/postgres_report.go internal/app/postgres_report_test.go internal/app/postgres_digest.go
git commit -m "feat(report): 스냅샷에 구분·수집시각·게시일자 기록"
```

---

## Task 4: 넓은 표의 인쇄와 반응형을 처리한다

12열은 현재 리포트 폭(`max-width:880px`)에 들어가지 않는다.

**Files:**
- Modify: `internal/report/render.go:56` (템플릿 `<style>`)
- Test: `internal/report/render_test.go`

- [x] **Step 1: 실패하는 테스트를 작성한다**

```go
func TestReportStyleSupportsWideTable(t *testing.T) {
	html, err := BuildHTML(Document{TenantName: "테넌트", ScheduleName: "일정",
		Notices: []Notice{{Title: "가", Category: "goods"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"overflow-x:auto", "size:A4 landscape", "border-collapse"} {
		if !strings.Contains(string(html), want) {
			t.Fatalf("style missing %q", want)
		}
	}
}
```

- [x] **Step 2 → 4: 구현하고 통과를 확인한다**

`<style>`에 아래를 더한다. 색은 `DESIGN.md` 토큰만 쓴다.

```css
.report{max-width:1360px}
.query dl{display:flex;flex-wrap:wrap;gap:8px 20px;margin:8px 0 0}
.query div{display:flex;gap:6px}.query dt{color:#527080}
.grid{background:#fff;border:1px solid #d9e2e8;border-radius:8px;margin-top:16px;padding:18px}
.grid-head{display:flex;justify-content:space-between;gap:12px;font-weight:600;margin-bottom:10px}
.grid-scroll{overflow-x:auto}
.grid table{border-collapse:collapse;width:100%;font-size:12px;white-space:nowrap}
.grid th,.grid td{border:1px solid #d9e2e8;padding:5px 7px;text-align:left}
.grid thead th{background:#F0F1F3;color:#17243A}
.grid td.number{text-align:right;font-variant-numeric:tabular-nums}
.grid td.name{white-space:normal;min-width:260px}
@media print{@page{size:A4 landscape;margin:10mm}.report{max-width:none;padding:0}.grid{break-inside:auto}.grid tr{break-inside:avoid}}
```

기존 `@media print`의 `.header,.meta,.summary,.notice{break-inside:avoid}`는 유지한다.

Run: `go test ./internal/report -v` → PASS

- [x] **Step 5: 커밋**

```bash
git add internal/report/render.go internal/report/render_test.go
git commit -m "fix(report): 넓은 표의 인쇄와 가로 스크롤 처리"
```

---

## Task 5: 공고 목록 화면을 같은 열 구성으로 맞춘다

리포트만 바뀌면 화면과 산출물의 열이 어긋난다. 화면은 요청 시점 실시간 판정이므로
**`레코드생성일시`는 넣지 않는다** — 매칭 기록 시각이 화면 경로에 존재하지 않는다. 이 차이를
주석으로 변명하지 않고 열 구성으로 드러낸다.

**Files:**
- Modify: `internal/web/handler.go:110` (`NoticeView`), `:1489` (`filterNotices`)
- Modify: `internal/app/postgres_web.go:246` (`tenantNoticesSQL`), `:286` (`noticeViewFromModel`)
- Modify: `web/templates/pages.html` `{{define "notices"}}` 표 머리·본문
- Test: `internal/web/handler_test.go`, `internal/app/postgres_web_test.go`

- [x] **Step 1: 실패하는 테스트를 작성한다**

```go
func TestNoticeTableShowsSearchHistoryColumns(t *testing.T) {
	body := renderNoticesPage(t, []NoticeView{{
		ID: "n1", Title: "스마트폴 제작", Category: "물품", Trade: "내자",
		SourceKind: "입찰공고목록-입찰공고", Keyword: "경관조명",
		CollectedDate: "20260107", CollectedClock: "2354", PostedAt: "2026-01-02",
		Agency: "만원복지재단", Amount: "599,995,000원",
	}})
	for _, want := range []string{"구분", "일자", "시간", "키워드", "업무구분", "업무여부",
		"진행/게시일자", "입찰공고목록-입찰공고", "경관조명", "20260107", "2354", "내자"} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice table missing %q", want)
		}
	}
	if strings.Contains(body, "레코드생성일시") {
		t.Fatal("screen must not claim a record timestamp it does not have")
	}
}
```

`internal/app` 쪽은 `tenantNoticesSQL`이 `collected_at`을 읽고 `noticeViewFromModel`이
matcher `Details`에서 키워드를 뽑는지 단정한다.

```go
func TestNoticeViewCarriesSearchHistoryFields(t *testing.T) {
	if !strings.Contains(tenantNoticesSQL, "collected_at") {
		t.Fatalf("web notice query does not read collected_at: %s", tenantNoticesSQL)
	}
	// noticeViewFromModel: include_any 상세에서 Keyword="경관조명", Trade="내자" 확인
}
```

- [x] **Step 2: 실패를 확인한다**

Run: `go test ./internal/app ./internal/web -run "SearchHistory" -v`
Expected: FAIL

- [x] **Step 3: 최소 구현**

`NoticeView`에 `SourceKind, Keyword, Trade, CollectedDate, CollectedClock, PostedAt` 문자열
필드를 더한다. `tenantNoticesSQL`은 `SELECT id::text,payload,collected_at`으로 늘리고
`WHERE`·`ORDER BY`는 그대로 둔다 — 직전 계획이 없앤 `LIMIT`을 되살리지 말 것.
`noticeViewFromModel`은 KST 변환과 키워드 추출을 담당한다. `formatKoreanTime`은 그대로 두고
`formatKoreanDate(value time.Time, layout string) string` 같은 얇은 헬퍼를 새로 쓴다.

템플릿 표는 `#` 없이 열 순서를 리포트와 같게 맞춘다(`구분 / 일자 / 시간 / 키워드 /
업무구분 / 업무여부 / 공고명 / 공고기관 / 진행·게시일자 / 추정가격 / 마감`). `.table-scroll`이
이미 가로 스크롤을 처리하므로 CSS 변경은 최소로 한다. 모바일 라벨 규칙을 지키려면 새 열에도
`data-label`을 붙인다(`web/static/app.css`의 테이블 → 라벨 행 변환이 이 속성을 쓴다).

- [x] **Step 4: 통과를 확인한다**

Run: `go test ./internal/app ./internal/web -v`
Expected: PASS

- [x] **Step 5: 커밋**

```bash
git add internal/web/handler.go internal/web/handler_test.go internal/app/postgres_web.go internal/app/postgres_web_test.go web/templates/pages.html
git commit -m "feat(web): 공고 목록을 검색내역 열 구성으로 정리"
```

---

## Task 6: 전체 회귀 검증

- [x] **Step 1: 전체 테스트**

```bash
go test ./...
go test -race ./...
go vet ./...
```

- [x] **Step 2: FreeBSD 교차 빌드**

```powershell
$env:CGO_ENABLED='0'; $env:GOOS='freebsd'; $env:GOARCH='amd64'
go build -trimpath -o build/namo-freebsd-amd64 .
```

- [ ] **Step 3: 실서버 확인**

1. `namo migrate` — 0016이 적용되는지 확인한다.
2. `/reports`에서 수동 생성 1회.
3. 내려받은 HTML에서 확인한다.
   - 표의 `N rows`가 리포트 상단 "일치 공고 총 N건"과 같다.
   - 수동 리포트는 활성 필터 전체의 공고 합집합이다. 화면의 단일 필터 결과와 비교할 때는
     활성 필터가 그 하나뿐인 등 필터 범위와 기준 시각이 같은 조건에서 비교한다.
     필터 미선택 화면은 미매칭 공고도 포함하므로 수동 리포트와 무조건 같은 수가 아니다.
     예약 리포트는 신규 매칭 조회 기간까지 같아야 비교할 수 있다.
   - `일자`·`시간`·`진행/게시일자`·`레코드생성일시`가 KST로 나온다.
   - `키워드` 열이 실제 필터의 포함 키워드다.
   - 추정가격 합계가 열 값의 합과 같다.
   - 브라우저 인쇄 미리보기가 가로 A4로 잘리지 않는다.
4. 과거 리포트 다운로드가 기존 HTML을 변경 없이 반환하는지 확인한다.
   구버전 NULL 스냅샷을 새 렌더러로 읽는 경우에만 추가 셀의 `-`를 확인한다.

- [x] **Step 4: 결과를 문서에 남기고 커밋**

```bash
git add docs/implementation/search-history-report-plan.md
git commit -m "docs(report): 검색내역 리포트 검증 결과 기록"
```

---

## 하지 말 것

- 매칭 근거 섹션을 지우지 말 것. `키워드` 열은 근거의 요약일 뿐이고, 규칙별 상세 근거는
  `PRODUCT.md`의 제품 원칙이다.
- `tenantNoticesSQL`에 `LIMIT`을 되살리지 말 것. 직전 계획이 없앤 회귀다.
- 이미지의 검은 배경·형광 색을 그대로 옮기지 말 것. `DESIGN.md` 위반이다.
- `report_items`의 새 열을 과거 리포트에 추정값으로 백필하지 말 것. 스냅샷은 불변이다.
- 원본에 없는 `수요기관`·`예산금액`을 있는 것처럼 열 제목만 붙이지 말 것. Task 0의 결정을
  열 제목에 반영한다.
- `internal/report`가 `internal/app`을 import하게 만들지 말 것.
- 적용된 마이그레이션 파일(0001~0015)을 수정하지 말 것. 체크섬 검증이 깨진다.
- 한 커밋에 여러 Task를 섞지 말 것.

## 범위 밖 (후속 과제)

- **`구분`의 두 번째 값 `발주목록-사전규격공개`.** 사전규격은 `BidPublicInfoService`가 아닌
  별도 OpenAPI 서비스이고, 수집기·정규화·중복 판정·일일 쿼터
  (`internal/procurement/client.go`, `internal/app/collector.go`)를 모두 건드린다. 이 계획은
  `source_kind` 열과 고정값까지만 만들어 새 수집원이 붙을 자리를 남긴다.
- **수집 실행별 조회 로그.** 이미지 원본은 같은 공고가 수집 실행마다 다시 적재된다. 현재
  구조는 리포트 1건 = 스냅샷 1회이므로 실행 로그가 필요하면 `report_items`가 아니라 별도
  append-only 테이블로 설계한다.
- **CSV·엑셀 내보내기.** 표 형태가 되면 자연스러운 다음 요청이지만 이 계획은 HTML만 다룬다.
- **메일 발송.** `{{define "reports"}}`의 "준비 중" 상태를 유지한다.

## 진단 기록

### Task 0 결과

- 실행 일자: 2026-09-04
- 활성 공고 수: 6,906건
- 전체 공고 수: 8,921건
- RawJSON 키 존재 건수: `dminsttNm` = 3,863 / `asignBdgtAmt` = 3,661 / `intrbidYn` = 3,863 / `bsnsDivNm` = 0
- `truncated` 비율: 5,058 / 8,921건(56.7%)
- 결정 (가) 9번 열: `공고기관`(`report_items.agency`, 원본 `ntceInsttNm`)
- 결정 (나) 11번 열: `추정가격`(`report_items.amount`, 원본 `presmptPrce`)
- 결정 (다) 7번 열: `category='foreign'`이면 `외자`, 그 외는 `내자`

### 구현·검증 기록 (2026-09-04)

작업 경로는 `E:\구분\나라장터 입찰공고 모니터링`, 브랜치는 `dev001`이다.
기존 `52c4` 작업 트리는 원래 상태로 보존했으며 push·pull·운영 배포도 하지 않았다.

| Task | 커밋 | 검증 내용 |
| --- | --- | --- |
| 0 | `d99c52d` | 운영 DB 읽기 전용 원본 진단 및 열 의미 확정. 코드 변경이 없는 진단이므로 실패 테스트 대신 기존 전체 테스트 확인 |
| 1 | `a656c8e`, `40161bc` | 12열·KST·합집합·합계 테스트 RED → 구현 → GREEN. 누락 금액 `-`, 전체 누락 합계 `-`, 혼합 합계, int64 경계 오류 회귀 보완 |
| 2 | `c0acb2c`, `c4d5eff`, `a2f013d`, `23ecc13` | 0016 열·NULL·권한 계약 RED → 구현 → GREEN. 기존 마이그레이션 불변 |
| 3 | `fb71fe6` | 예약·수동 INSERT, 14열 로더, NULL 및 같은 공고의 비연속 매칭 병합 RED → 구현 → GREEN |
| 4 | `27f647b` | 가로 스크롤·A4 가로 CSS 계약 RED → 구현 → GREEN |
| 5 | `a00d8f2`, `5dbc5de` | 실제 매처 키워드·수집/게시일시·11열·선택 필터 불변성·모바일 공고명 RED → 구현 → GREEN |

모든 구현 커밋은 Task별로 분리했다. Task별 검토와 최종 통합 검토를 수행했고,
마지막 누락 금액 수정도 재검토를 통과했다. 의존성, 기존 마이그레이션 0001~0015,
매칭 로직, WHERE/ORDER BY 및 건수 산정 경로는 유지했다.

#### 완료 게이트

코드 커밋 `40161bc`에서 다음 명령이 모두 종료 코드 0으로 통과했다.

| 환경 | 명령 | 결과 |
| --- | --- | --- |
| Windows / Go 1.27 | `go test ./...` | PASS |
| WSL Linux / Go 1.27 / GCC | `CGO_ENABLED=1 go test -race ./...` | PASS |
| Windows / Go 1.27 | `go vet ./...` | PASS |
| Windows → FreeBSD amd64 | `CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -trimpath -o build/namo-freebsd-amd64 .` | PASS |

교차 빌드 파일은 `build/namo-freebsd-amd64`(22,073,804바이트)다.
SHA-256: `09c1f21a84e6ef3a126c9edb8df05751b369d7feaf1be33e238390df72f7ae86`.
이 빌드는 실제 FreeBSD 실행이나 운영 DB 검증을 대신하지 않는다.

#### 화면·인쇄 확인

- 실제 렌더러와 웹 핸들러에 합성 데이터 30건을 넣어 확인했다. 운영 데이터/계정 변경은 없다.
- 1440px 데스크톱과 390px 모바일에서 페이지 전체 가로 넘침이 없다. 리포트의 넓은 표만
  내부 가로 스크롤되고, 화면 공고 목록은 모바일 라벨 행으로 바뀐다.
- 기본 10건, 20개 설정에서 선택 필터 15건 및 해당 키워드만 표시,
  30개 설정에서 30건 표시를 확인했다. 모바일 공고명 링크 최소 높이는 44px다.
- 합성 리포트 PDF 6쪽에서 12열 머리글 반복과 마지막 30번째 행 포함을 확인했다.
  출력 도구가 Letter 가로(792×612pt)를 사용하므로 **정확한 A4 용지 인쇄 확인은 미완료**다.
  코드의 `@page size:A4 landscape` 및 긴 셀 줄바꿈 계약은 테스트했다.
- UI 정적 검사 결과는 발견 사항 없음이다. 템플릿 검사는 파서 의존성 부재로 정규식 모드였으므로
  이것만으로 브라우저 렌더링을 검증했다고 보지 않는다.

#### 남은 운영 확인과 결정

- PostgreSQL 통합 테스트는 `NAMO_TEST_DATABASE_URL` 또는
  `TEST_POSTGRES_OWNER_URL`/`TEST_POSTGRES_RUNTIME_URL` 부재로 건너뛴 항목이 있다.
  로더·SQL 계약 테스트의 성공을 실제 마이그레이션 성공으로 간주하지 않는다.
- 별도 운영 승인 후 0016 적용, 수동 리포트 생성·다운로드, 동일 범위/시각의 건수 비교,
  실제 A4 인쇄를 확인한다. Task 6 Step 3은 이 때문에 미완료로 남겼다.
- Ruling: 새 스냅샷 열은 DEFAULT/백필 없이 nullable이다. 과거 스냅샷과 저장 HTML을 보존한다.
- Ruling: 출처가 확인된 `공고기관`·`추정가격`을 쓰며 누락값은 `-`다.
- Ruling: 필터 미선택 목록은 활성 필터의 매칭 키워드 합집합, 선택 목록은 선택 필터의 키워드만 표시한다.
- Ruling: 과거 HTML은 재렌더링하지 않는다. 과거 NULL 스냅샷을 새 렌더러로 읽는 경우만 새 셀이 `-`다.

로컬 QA 브라우저는 종료했다. 합성 리포트 보조 서버의 종료 명령은 실행 환경에서 차단되어
`search-history-preview` PID `26736`(127.0.0.1:19887)이 남아 있다. 운영 서비스와 무관한
읽기 전용 샘플이며, 재사용 전에 PID와 실행 파일 경로를 다시 확인해야 한다.
