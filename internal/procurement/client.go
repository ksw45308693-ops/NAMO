package procurement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"namo/internal/model"
)

const (
	OfficialBaseURL    = "https://apis.data.go.kr/1230000/ad/BidPublicInfoService"
	defaultTimeout     = 20 * time.Second
	maxResponseBytes   = 8 << 20
	maxPageSize        = 999
	defaultMaxPages    = 1000
	regionCacheLimit   = 500
	regionPageSize     = 100
	maxWarningRawBytes = 4096
)

var (
	ErrEncodedServiceKey = errors.New("service key must be the decoded portal key, not URL-encoded")
	ErrResponseTooLarge  = errors.New("나라장터 API response exceeds limit")
	ErrLookupBudget      = errors.New("participant region lookup budget exhausted")
	ErrInvalidRequest    = errors.New("나라장터 API request could not be constructed")
	ErrHTTPTransport     = errors.New("나라장터 API transport failed")
)

// CallBudget is consumed immediately before every real HTTP attempt. Production
// uses a PostgreSQL-backed implementation so retries and processes share a cap.
type CallBudget interface {
	Take(context.Context) error
}

// Config.ServiceKey must be the decoded portal key. The client URL-encodes it once.
type Config struct {
	BaseURL, ServiceKey           string
	HTTPClient                    *http.Client
	RetryAttempts                 int
	RetryBaseDelay, RetryMaxDelay time.Duration
	MaxBodyBytes                  int64
	Wait                          func(context.Context, time.Duration) error
	Warning                       func(FieldWarning)
	LookupBudget                  int
	MaxPages                      int
	CallBudget                    CallBudget
}
type Client struct {
	config      Config
	regions     map[string]string
	regionKeys  []string
	regionCalls map[string]*regionCall
	lookups     int
	mu          sync.Mutex
}

type regionCall struct {
	done   chan struct{}
	region string
	err    error
}
type ListQuery struct {
	StartDate, EndDate time.Time
	PageSize           int
}
type ServiceError struct{ Code, Message string }

func (e *ServiceError) Error() string { return "나라장터 API " + e.Code + ": " + e.Message }

type MalformedResponseError struct{ Message string }

func (e *MalformedResponseError) Error() string {
	return "malformed 나라장터 API response: " + e.Message
}

type LookupBudgetError struct {
	Limit, Used            int
	BidNumber, BidSequence string
}

func (e *LookupBudgetError) Error() string {
	return fmt.Sprintf("participant region lookup budget exhausted (%d/%d) for %s/%s", e.Used, e.Limit, e.BidNumber, e.BidSequence)
}

func (e *LookupBudgetError) Unwrap() error { return ErrLookupBudget }

type IncompletePageError struct {
	Page, Expected, Received, TotalCount int
}

func (e *IncompletePageError) Error() string {
	return fmt.Sprintf("나라장터 API page %d is incomplete: received %d of %d expected items (total %d)", e.Page, e.Received, e.Expected, e.TotalCount)
}

type RepeatedPageError struct{ Page, FirstPage int }

func (e *RepeatedPageError) Error() string {
	return fmt.Sprintf("나라장터 API page %d repeats page %d", e.Page, e.FirstPage)
}

type MaxPageError struct{ Page, MaxPages, TotalCount int }

func (e *MaxPageError) Error() string {
	return fmt.Sprintf("나라장터 API requires page %d beyond maximum %d (total %d)", e.Page, e.MaxPages, e.TotalCount)
}

type FieldWarning struct {
	Page, Item  int
	Field, Code string
	RawJSON     json.RawMessage
}

func NewClient(c Config) *Client {
	if c.BaseURL == "" {
		c.BaseURL = OfficialBaseURL
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
	clone := *c.HTTPClient
	if clone.Timeout == 0 {
		clone.Timeout = defaultTimeout
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.HTTPClient = &clone
	if c.RetryAttempts < 1 {
		c.RetryAttempts = 3
	}
	if c.RetryBaseDelay <= 0 {
		c.RetryBaseDelay = 100 * time.Millisecond
	}
	if c.RetryMaxDelay <= 0 {
		c.RetryMaxDelay = time.Second
	}
	if c.MaxBodyBytes <= 0 || c.MaxBodyBytes > maxResponseBytes {
		c.MaxBodyBytes = maxResponseBytes
	}
	if c.Wait == nil {
		c.Wait = waitContext
	}
	if c.LookupBudget <= 0 {
		c.LookupBudget = 500
	}
	if c.MaxPages <= 0 {
		c.MaxPages = defaultMaxPages
	}
	return &Client{config: c, regions: map[string]string{}, regionCalls: map[string]*regionCall{}}
}
func (c *Client) List(ctx context.Context, category model.Category, q ListQuery) ([]model.Notice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.Contains(c.config.ServiceKey, "%") {
		return nil, ErrEncodedServiceKey
	}
	op, ok := operationFor(category)
	if !ok {
		return nil, fmt.Errorf("unsupported procurement category %q", category)
	}
	size := q.PageSize
	if size < 1 {
		size = 100
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	var out []model.Notice
	seenPages := make(map[[sha256.Size]byte]int)
	totalCount := -1
	for page := 1; ; page++ {
		if page > c.config.MaxPages {
			return nil, &MaxPageError{Page: page, MaxPages: c.config.MaxPages, TotalCount: totalCount}
		}
		payload, err := c.requestPage(ctx, op, q, page, size)
		if err != nil {
			return nil, err
		}
		items, err := payload.Response.Body.items()
		if err != nil {
			return nil, err
		}
		if totalCount < 0 {
			totalCount = int(payload.Response.Body.TotalCount)
		}
		expected := totalCount - (page-1)*size
		if expected > size {
			expected = size
		}
		if expected < 0 {
			expected = 0
		}
		if len(items) < expected {
			code := "incomplete_page"
			if len(items) == 0 {
				code = "empty_page"
			}
			c.warn(FieldWarning{Page: page, Field: "items", Code: code})
			return nil, &IncompletePageError{Page: page, Expected: expected, Received: len(items), TotalCount: totalCount}
		}
		if len(items) > 0 {
			signature := pageSignature(items)
			if firstPage, ok := seenPages[signature]; ok {
				return nil, &RepeatedPageError{Page: page, FirstPage: firstPage}
			}
			seenPages[signature] = page
		}
		for i, raw := range items {
			safeRaw := sanitizedRawJSON(raw, c.config.ServiceKey)
			var item apiNotice
			if json.Unmarshal(raw, &item) != nil {
				c.warn(FieldWarning{Page: page, Item: i + 1, Field: "item", Code: "malformed_item", RawJSON: safeRaw})
				continue
			}
			item.sanitize(c.config.ServiceKey)
			notice, warnings, valid := item.notice(category, safeRaw)
			for _, w := range warnings {
				w.Page, w.Item = page, i+1
				c.warn(w)
			}
			if valid {
				out = append(out, notice)
			}
		}
		if totalCount == 0 || page*size >= totalCount {
			return out, nil
		}
	}
}
func (c *Client) requestPage(ctx context.Context, op operation, q ListQuery, page, size int) (apiResponse, error) {
	params := make(url.Values)
	params.Set("inqryDiv", "1")
	params.Set("pageNo", strconv.Itoa(page))
	params.Set("numOfRows", strconv.Itoa(size))
	if !q.StartDate.IsZero() {
		params.Set("inqryBgnDt", q.StartDate.In(korea).Format("200601021504"))
	}
	if !q.EndDate.IsZero() {
		params.Set("inqryEndDt", q.EndDate.In(korea).Format("200601021504"))
	}
	return c.request(ctx, op.path, params)
}

func (c *Client) request(ctx context.Context, path string, params url.Values) (apiResponse, error) {
	if err := ctx.Err(); err != nil {
		return apiResponse{}, err
	}
	if strings.Contains(c.config.ServiceKey, "%") {
		return apiResponse{}, ErrEncodedServiceKey
	}
	var last error
	for attempt := 0; attempt < c.config.RetryAttempts; attempt++ {
		p, retry, after, err := c.doRequest(ctx, path, params)
		if err == nil {
			return p, nil
		}
		last = err
		if !retry || attempt+1 == c.config.RetryAttempts {
			break
		}
		delay := backoff(c.config.RetryBaseDelay, c.config.RetryMaxDelay, attempt)
		if after > c.config.RetryMaxDelay {
			after = c.config.RetryMaxDelay
		}
		if after > delay {
			delay = after
		}
		if err := c.config.Wait(ctx, delay); err != nil {
			return apiResponse{}, err
		}
	}
	return apiResponse{}, last
}
func (c *Client) doRequest(ctx context.Context, path string, params url.Values) (apiResponse, bool, time.Duration, error) {
	endpoint, err := url.Parse(strings.TrimRight(c.config.BaseURL, "/") + "/" + path)
	if err != nil {
		return apiResponse{}, false, 0, ErrInvalidRequest
	}
	v := endpoint.Query()
	for key, values := range params {
		v.Del(key)
		for _, value := range values {
			v.Add(key, value)
		}
	}
	v.Set("serviceKey", c.config.ServiceKey)
	v.Set("type", "json")
	endpoint.RawQuery = v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return apiResponse{}, false, 0, ErrInvalidRequest
	}
	if c.config.CallBudget != nil {
		if err := c.config.CallBudget.Take(ctx); err != nil {
			return apiResponse{}, false, 0, err
		}
	}
	res, err := c.config.HTTPClient.Do(req)
	if err != nil {
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return apiResponse{}, false, 0, ctxErr
		}
		return apiResponse{}, true, 0, ErrHTTPTransport
	}
	if res == nil || res.Body == nil {
		return apiResponse{}, false, 0, &MalformedResponseError{Message: "missing HTTP response body"}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return apiResponse{}, res.StatusCode == 429 || res.StatusCode >= 500, retryAfter(res), fmt.Errorf("나라장터 API HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, c.config.MaxBodyBytes+1))
	if err != nil {
		return apiResponse{}, true, 0, errors.New("나라장터 API response read failed")
	}
	if int64(len(data)) > c.config.MaxBodyBytes {
		return apiResponse{}, false, 0, ErrResponseTooLarge
	}
	var p apiResponse
	if json.Unmarshal(data, &p) != nil {
		return apiResponse{}, false, 0, &MalformedResponseError{Message: "invalid JSON"}
	}
	if err := p.validate(); err != nil {
		return apiResponse{}, false, 0, err
	}
	h := p.Response.Header
	if h.Code != "00" && h.Code != "0000" {
		e := &ServiceError{Code: redactSecret(h.Code, c.config.ServiceKey), Message: redactSecret(h.Message, c.config.ServiceKey)}
		return apiResponse{}, transientCode(h.Code), 0, e
	}
	if p.Response.Body == nil {
		return apiResponse{}, false, 0, &MalformedResponseError{Message: "missing response body"}
	}
	return p, false, 0, nil
}
func (c *Client) warn(w FieldWarning) {
	if c.config.Warning != nil {
		c.config.Warning(w)
	}
}

// LookupRegion caches a finite result by number and sequence. Invoke it only for new/revised notices; misses consume quota.
func (c *Client) LookupRegion(ctx context.Context, bidNumber, bidSequence string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.Contains(c.config.ServiceKey, "%") {
		return "", ErrEncodedServiceKey
	}
	key := bidNumber + "\x00" + bidSequence
	c.mu.Lock()
	region, ok := c.regions[key]
	if ok {
		c.mu.Unlock()
		return region, nil
	}
	if call, pending := c.regionCalls[key]; pending {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.region, call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if c.lookups >= c.config.LookupBudget {
		err := &LookupBudgetError{Limit: c.config.LookupBudget, Used: c.lookups, BidNumber: bidNumber, BidSequence: bidSequence}
		c.mu.Unlock()
		return "", err
	}
	c.lookups++
	call := &regionCall{done: make(chan struct{})}
	c.regionCalls[key] = call
	c.mu.Unlock()

	region, err := c.fetchRegion(ctx, bidNumber, bidSequence)
	c.mu.Lock()
	if err == nil {
		c.cacheRegionLocked(key, region)
	}
	call.region, call.err = region, err
	delete(c.regionCalls, key)
	close(call.done)
	c.mu.Unlock()
	return region, err
}

func (c *Client) fetchRegion(ctx context.Context, bidNumber, bidSequence string) (string, error) {
	params := make(url.Values)
	params.Set("inqryDiv", "2")
	params.Set("pageNo", "1")
	params.Set("numOfRows", strconv.Itoa(regionPageSize))
	params.Set("bidNtceNo", bidNumber)
	params.Set("bidNtceOrd", bidSequence)
	payload, err := c.request(ctx, "getBidPblancListInfoPrtcptPsblRgn", params)
	if err != nil {
		return "", err
	}
	items, err := payload.Response.Body.items()
	if err != nil {
		return "", err
	}
	total := int(payload.Response.Body.TotalCount)
	expected := min(total, regionPageSize)
	if len(items) < expected {
		return "", &IncompletePageError{Page: 1, Expected: expected, Received: len(items), TotalCount: total}
	}
	var regions []string
	for _, raw := range items {
		var item struct {
			Region string `json:"prtcptPsblRgnNm"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return "", &MalformedResponseError{Message: "invalid region item"}
		}
		if value := model.NormalizeText(item.Region); value != "" {
			regions = append(regions, value)
		}
	}
	sort.Strings(regions)
	unique := regions[:0]
	for _, value := range regions {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	region := strings.Join(unique, ", ")
	return region, nil
}

func (c *Client) cacheRegionLocked(key, region string) {
	if len(c.regions) >= regionCacheLimit {
		delete(c.regions, c.regionKeys[0])
		c.regionKeys = c.regionKeys[1:]
	}
	c.regions[key] = region
	c.regionKeys = append(c.regionKeys, key)
}
func waitContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func retryAfter(r *http.Response) time.Duration {
	if s, e := strconv.Atoi(r.Header.Get("Retry-After")); e == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if when, e := http.ParseTime(r.Header.Get("Retry-After")); e == nil && when.After(time.Now()) {
		return time.Until(when)
	}
	return 0
}
func backoff(base, max time.Duration, attempt int) time.Duration {
	for i := 0; i < attempt && base < max; i++ {
		base *= 2
	}
	if base > max {
		return max
	}
	return base
}
func transientCode(code string) bool { return code == "01" || code == "05" || code == "23" }

func pageSignature(items []json.RawMessage) [sha256.Size]byte {
	hash := sha256.New()
	for _, item := range items {
		_, _ = hash.Write(item)
		_, _ = hash.Write([]byte{0})
	}
	var signature [sha256.Size]byte
	copy(signature[:], hash.Sum(nil))
	return signature
}

type operation struct{ path string }

func operationFor(c model.Category) (operation, bool) {
	m := map[model.Category]operation{model.CategoryConstruction: {"getBidPblancListInfoCnstwk"}, model.CategoryService: {"getBidPblancListInfoServc"}, model.CategoryGoods: {"getBidPblancListInfoThng"}, model.CategoryForeign: {"getBidPblancListInfoFrgcpt"}}
	op, ok := m[c]
	return op, ok
}

type apiResponse struct {
	Response *apiEnvelope `json:"response"`
}
type apiEnvelope struct {
	Header *apiHeader `json:"header"`
	Body   *apiBody   `json:"body"`
}
type apiHeader struct {
	Code    string `json:"resultCode"`
	Message string `json:"resultMsg"`
}
type apiBody struct {
	Items      json.RawMessage `json:"items"`
	TotalCount flexInt         `json:"totalCount"`
}

type flexInt int

func (n *flexInt) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("invalid count")
	}
	text := string(raw)
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		text = value
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return fmt.Errorf("invalid count")
	}
	if int64(int(value)) != value {
		return fmt.Errorf("count overflow")
	}
	*n = flexInt(value)
	return nil
}

func (p apiResponse) validate() error {
	if p.Response == nil || p.Response.Header == nil || p.Response.Header.Code == "" {
		return &MalformedResponseError{Message: "missing response envelope, header, or result code"}
	}
	return nil
}
func (b *apiBody) items() ([]json.RawMessage, error) {
	if b == nil {
		return nil, &MalformedResponseError{Message: "missing response body"}
	}
	if len(b.Items) == 0 || string(b.Items) == "null" {
		return nil, nil
	}
	var c struct {
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(b.Items, &c) == nil && len(c.Item) > 0 {
		return itemSlice(c.Item)
	}
	return itemSlice(b.Items)
}
func itemSlice(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		return items, nil
	}
	var one json.RawMessage
	if json.Unmarshal(raw, &one) == nil && len(one) > 0 && one[0] == '{' {
		return []json.RawMessage{one}, nil
	}
	return nil, &MalformedResponseError{Message: "invalid items"}
}

type apiNotice struct {
	BidNumber         string `json:"bidNtceNo"`
	BidSequence       string `json:"bidNtceOrd"`
	Title             string `json:"bidNtceNm"`
	Agency            string `json:"ntceInsttNm"`
	ParticipantRegion string `json:"prtcptPsblRgnNm"`
	SourceURL         string `json:"bidNtceDtlUrl"`
	Amount            string `json:"presmptPrce"`
	PostedAt          string `json:"bidNtceDt"`
	Deadline          string `json:"bidClseDt"`
}

func (n *apiNotice) sanitize(secrets ...string) {
	n.BidNumber = sanitizedNoticeField(n.BidNumber, secrets...)
	n.BidSequence = sanitizedNoticeField(n.BidSequence, secrets...)
	n.Title = sanitizedNoticeField(n.Title, secrets...)
	n.Agency = sanitizedNoticeField(n.Agency, secrets...)
	n.ParticipantRegion = sanitizedNoticeField(n.ParticipantRegion, secrets...)
	n.SourceURL = sanitizedSourceURL(n.SourceURL, secrets...)
}

func sanitizedNoticeField(value string, secrets ...string) string {
	for _, secret := range secrets {
		value = redactSecret(value, secret)
	}
	return value
}

func sanitizedSourceURL(value string, secrets ...string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if sanitizedNoticeField(value, secrets...) != value {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		if containsSensitiveURLMarker(value) {
			return ""
		}
		return value
	}
	for key := range parsed.Query() {
		if sensitiveFieldName(key) {
			return ""
		}
	}
	return value
}

func containsSensitiveURLMarker(value string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(value, "_", ""), "-", ""))
	for _, marker := range []string{"servicekey=", "apikey=", "password=", "token="} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func (n apiNotice) notice(c model.Category, raw json.RawMessage) (model.Notice, []FieldWarning, bool) {
	amount, amountOK := parseAmount(n.Amount)
	postedAt, postedOK := parseTime(n.PostedAt)
	deadline, deadlineOK := parseTime(n.Deadline)
	valid := amountOK && postedOK && deadlineOK
	notice := model.Notice{Category: c, BidNumber: n.BidNumber, BidSequence: n.BidSequence, Title: model.NormalizeText(n.Title), Agency: model.NormalizeText(n.Agency), Region: model.NormalizeText(n.ParticipantRegion), SourceURL: model.NormalizeText(n.SourceURL), Amount: amount, PostedAt: postedAt, Deadline: deadline, RawJSON: raw}
	var ws []FieldWarning
	if strings.TrimSpace(n.Amount) != "" && !amountOK {
		ws = append(ws, FieldWarning{Field: "amount", Code: "invalid_amount", RawJSON: raw})
	}
	if strings.TrimSpace(n.PostedAt) != "" && !postedOK {
		ws = append(ws, FieldWarning{Field: "posted_at", Code: "invalid_time", RawJSON: raw})
	}
	if strings.TrimSpace(n.Deadline) != "" && !deadlineOK {
		ws = append(ws, FieldWarning{Field: "deadline", Code: "invalid_time", RawJSON: raw})
	}
	if err := notice.ValidateSource(); err != nil {
		valid = false
		var source *model.SourceValidationError
		if errors.As(err, &source) {
			ws = append(ws, FieldWarning{Field: source.Field, Code: "missing_required", RawJSON: raw})
		}
	}
	if notice.Region == "" {
		ws = append(ws, FieldWarning{Field: "region", Code: "unavailable", RawJSON: raw})
	}
	return notice, ws, valid
}
func parseAmount(value string) (int64, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, ",", ""))
	if value == "" {
		return 0, true
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	return amount, err == nil && amount >= 0
}

func parseTime(s string) (time.Time, bool) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, true
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "200601021504", time.RFC3339} {
		if p, e := time.ParseInLocation(layout, strings.TrimSpace(s), korea); e == nil {
			return p, true
		}
	}
	return time.Time{}, false
}

func sanitizedRawJSON(raw json.RawMessage, secrets ...string) json.RawMessage {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		encoded, _ := json.Marshal("invalid JSON omitted")
		return encoded
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		encoded, _ := json.Marshal("invalid JSON omitted")
		return encoded
	}
	value = scrubSecretFields(value, secrets)
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"omitted":true}`)
	}
	if len(encoded) <= maxWarningRawBytes {
		return encoded
	}
	sum := sha256.Sum256(encoded)
	prefix := string(encoded[:512])
	bounded, _ := json.Marshal(map[string]any{
		"truncated": true,
		"sha256":    fmt.Sprintf("%x", sum[:]),
		"prefix":    prefix,
	})
	return bounded
}

func scrubSecretFields(value any, secrets []string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveFieldName(key) {
				delete(typed, key)
				continue
			}
			typed[key] = scrubSecretFields(child, secrets)
		}
	case []any:
		for index, child := range typed {
			typed[index] = scrubSecretFields(child, secrets)
		}
	case string:
		for _, secret := range secrets {
			typed = redactSecret(typed, secret)
		}
		return typed
	}
	return value
}

func sensitiveFieldName(value string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(value, "_", ""), "-", ""))
	return strings.Contains(normalized, "servicekey") || strings.Contains(normalized, "apikey") || strings.Contains(normalized, "password") || strings.Contains(normalized, "token")
}

func redactSecret(value, secret string) string {
	if secret == "" {
		return value
	}
	for _, candidate := range []string{
		secret,
		url.QueryEscape(secret),
		url.PathEscape(secret),
		strings.ToLower(url.QueryEscape(secret)),
		strings.ToLower(url.PathEscape(secret)),
	} {
		if candidate != "" {
			value = strings.ReplaceAll(value, candidate, "[redacted]")
		}
	}
	return value
}

var korea = time.FixedZone("KST", 9*60*60)
