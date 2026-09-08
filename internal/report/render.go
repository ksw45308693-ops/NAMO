// Package report renders deterministic standalone HTML procurement reports.
package report

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Match struct {
	RuleName string
	Reasons  []string
}

type Notice struct {
	ID, Title, Category, Agency, Region, SourceURL string
	SourceKind                                     string
	Keywords                                       []string
	Amount                                         int64
	Deadline, PostedAt, CollectedAt, RecordedAt    time.Time
	Matches                                        []Match
}

type Document struct {
	TenantName, ScheduleName string
	Trigger                  string
	DueAt, WindowEnd         time.Time
	WindowStart              *time.Time
	Notices                  []Notice
}

type reportView struct {
	TenantName, ScheduleName, Trigger string
	Window, DueAt                     string
	Total                             int
	Construction, Service             int
	Goods, Foreign                    int
	Notices                           []noticeView
	QueryDate, QueryKinds             string
	QueryKeywords, QueryFilters       string
	AmountTotal                       string
	Rows                              []rowView
}

type noticeView struct {
	ID, Title, Category, Agency, Region, SourceURL string
	Amount, Deadline                               string
	Link                                           template.URL
	HasLink                                        bool
	Matches                                        []matchView
}

type matchView struct {
	RuleName string
	Reasons  []string
}

type rowView struct {
	Ordinal                                 int
	Kind, Date, Clock, Keyword              string
	Category, Trade                         string
	Title, Agency, PostedAt, Amount, Record string
	Link                                    template.URL
	HasLink                                 bool
}

var kst = time.FixedZone("KST", 9*60*60)

var reportTemplate = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>입찰공고 보고서</title><style>
body{margin:0;background:#f3f6f8;color:#162331;font:14px/1.55 Arial,sans-serif}.report{max-width:1360px;margin:0 auto;padding:28px}.header{background:#092a4a;color:#fff;padding:24px;border-radius:10px}.header h1{margin:0;font-size:24px}.header p{margin:5px 0 0;color:#bcecff}.meta,.summary,.query,.notice{background:#fff;border:1px solid #d9e2e8;border-radius:8px;margin-top:16px;padding:18px}.meta{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.label{color:#527080;font-size:12px}.value{font-weight:600}.counts{display:flex;flex-wrap:wrap;gap:8px}.count{background:#e6f7f7;color:#0b5d68;border-radius:999px;padding:4px 10px}.query dl{display:flex;flex-wrap:wrap;gap:8px 20px;margin:8px 0 0}.query div{display:flex;gap:6px}.query dt{color:#527080}.query dd{margin:0}.grid{background:#fff;border:1px solid #d9e2e8;border-radius:8px;margin-top:16px;padding:18px}.grid-head{display:flex;justify-content:space-between;gap:12px;font-weight:600;margin-bottom:10px}.grid-scroll{overflow-x:auto}.grid table{border-collapse:collapse;width:100%;font-size:12px;white-space:nowrap}.grid th,.grid td{border:1px solid #d9e2e8;padding:5px 7px;text-align:left;vertical-align:top}.grid thead th{background:#F0F1F3;color:#17243A}.grid td.number{text-align:right;font-variant-numeric:tabular-nums}.grid td.name{white-space:normal;min-width:260px}.evidence{margin-top:16px}.evidence>summary{cursor:pointer;font-weight:600}.notice h2{font-size:18px;margin:0 0 8px}.notice dl{display:grid;grid-template-columns:100px 1fr;gap:5px 12px;margin:0}.notice dt{color:#527080}.notice dd{margin:0}.matches{margin:12px 0 0;padding-left:18px}.matches li{margin:5px 0}.source{overflow-wrap:anywhere}.empty{color:#527080}@media print{@page{size:A4 landscape;margin:10mm}body{background:#fff}.report{max-width:none;padding:0}.header,.meta,.summary,.query,.notice{break-inside:avoid}.grid{break-inside:auto}.grid tr{break-inside:avoid}.grid-scroll{overflow:visible}.grid table{min-width:0;table-layout:fixed;font-size:8px;white-space:normal}.grid th,.grid td{padding:3px 4px;white-space:normal;overflow-wrap:anywhere}.grid td.name{min-width:0}}
</style></head><body><main class="report"><header class="header"><h1>입찰공고 보고서</h1><p>{{.TenantName}} · {{.ScheduleName}}</p></header>
<section class="meta"><div><div class="label">실행 구분</div><div class="value">{{.Trigger}}</div></div><div><div class="label">일치 조회 기간</div><div class="value">{{.Window}}</div></div><div><div class="label">기준 시각</div><div class="value">{{.DueAt}}</div></div><div><div class="label">일치 공고</div><div class="value">총 {{.Total}}건</div></div></section>
<section class="summary"><strong>분류별 현황</strong><div class="counts"><span class="count">공사 {{.Construction}}</span><span class="count">용역 {{.Service}}</span><span class="count">물품 {{.Goods}}</span><span class="count">외자 {{.Foreign}}</span></div></section>
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
{{if .Notices}}<details class="evidence"><summary>매칭 근거 카드</summary>{{range .Notices}}<article class="notice"><h2>{{.Title}}</h2><dl><dt>분류</dt><dd>{{.Category}}</dd><dt>기관</dt><dd>{{.Agency}}</dd><dt>지역</dt><dd>{{.Region}}</dd><dt>추정 금액</dt><dd>{{.Amount}}</dd><dt>마감</dt><dd>{{.Deadline}}</dd><dt>원문</dt><dd class="source">{{if .HasLink}}<a href="{{.Link}}">{{.SourceURL}}</a>{{else}}{{.SourceURL}}{{end}}</dd></dl>{{if .Matches}}<ul class="matches">{{range .Matches}}<li><strong>{{.RuleName}}</strong>{{if .Reasons}}<ul>{{range .Reasons}}<li>{{.}}</li>{{end}}</ul>{{end}}</li>{{end}}</ul>{{else}}<p class="empty">일치 규칙 없음</p>{{end}}</article>{{end}}</details>{{else}}<section class="summary empty">새로 일치한 공고가 없습니다.</section>{{end}}
</main></body></html>`))

// BuildHTML returns a complete self-contained Korean HTML report.
func BuildHTML(doc Document) ([]byte, error) {
	view := reportView{
		TenantName:   doc.TenantName,
		ScheduleName: doc.ScheduleName,
		Trigger:      doc.Trigger,
		DueAt:        formatTime(doc.DueAt),
		Window:       formatWindow(doc.WindowStart, doc.WindowEnd),
		Total:        len(doc.Notices),
		Notices:      make([]noticeView, 0, len(doc.Notices)),
		Rows:         make([]rowView, 0, len(doc.Notices)),
	}
	var amountTotal int64
	var kinds, keywords, filters []string
	for index, notice := range doc.Notices {
		countCategory(&view, notice.Category)
		entry := noticeView{
			ID: notice.ID, Title: notice.Title, Category: categoryLabel(notice.Category), Agency: notice.Agency, Region: notice.Region,
			SourceURL: notice.SourceURL, Amount: formatAmount(notice.Amount), Deadline: formatDeadline(notice.Deadline),
			Matches: make([]matchView, 0, len(notice.Matches)),
		}
		if validSourceURL(notice.SourceURL) {
			entry.HasLink = true
			entry.Link = template.URL(notice.SourceURL)
		}
		for _, match := range notice.Matches {
			entry.Matches = append(entry.Matches, matchView{RuleName: match.RuleName, Reasons: match.Reasons})
		}
		view.Notices = append(view.Notices, entry)
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
	var output bytes.Buffer
	if err := reportTemplate.Execute(&output, view); err != nil {
		return nil, fmt.Errorf("render report: %w", err)
	}
	return output.Bytes(), nil
}

func countCategory(view *reportView, category string) {
	switch category {
	case "construction":
		view.Construction++
	case "service":
		view.Service++
	case "goods":
		view.Goods++
	case "foreign":
		view.Foreign++
	}
}

func categoryLabel(category string) string {
	switch category {
	case "construction":
		return "공사"
	case "service":
		return "용역"
	case "goods":
		return "물품"
	case "foreign":
		return "외자"
	default:
		return category
	}
}

func formatWindow(start *time.Time, end time.Time) string {
	if start == nil {
		return "최초 수집 이후 ~ " + formatTime(end)
	}
	return formatTime(*start) + " ~ " + formatTime(end)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format("2006-01-02 15:04 MST")
}

func formatDeadline(value time.Time) string {
	if value.IsZero() {
		return "마감 미정"
	}
	return formatTime(value)
}

func formatAmount(value int64) string {
	if value == 0 {
		return "미정"
	}
	return groupDigits(value) + "원"
}

func groupDigits(value int64) string {
	text := strconv.FormatInt(value, 10)
	prefix := ""
	if strings.HasPrefix(text, "-") {
		prefix, text = "-", text[1:]
	}
	for index := len(text) - 3; index > 0; index -= 3 {
		text = text[:index] + "," + text[index:]
	}
	return prefix + text
}

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

func dashJoin(values []string) string {
	return dash(strings.Join(values, ", "))
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func validSourceURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
}
