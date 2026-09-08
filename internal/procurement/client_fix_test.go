package procurement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"namo/internal/model"
)

func TestRegionLookupRejectsIncompleteResultAndRetriesWithoutCaching(t *testing.T) {
	for _, items := range []string{`[]`, `[{"prtcptPsblRgnNm":"서울"}]`} {
		t.Run(items, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body := items
				if calls > 1 {
					body = `[{"prtcptPsblRgnNm":"서울"},{"prtcptPsblRgnNm":"부산"}]`
				}
				fmt.Fprintf(w, `{"response":{"header":{"resultCode":"00"},"body":{"totalCount":2,"items":%s}}}`, body)
			}))
			defer server.Close()
			client := NewClient(Config{BaseURL: server.URL})
			region, err := client.LookupRegion(context.Background(), "N-1", "00")
			var incomplete *IncompletePageError
			if !errors.As(err, &incomplete) || region != "" {
				t.Fatalf("incomplete region was accepted: region=%q err=%v", region, err)
			}
			region, err = client.LookupRegion(context.Background(), "N-1", "00")
			if err != nil || region != "부산, 서울" || calls != 2 {
				t.Fatalf("retry region=%q calls=%d err=%v", region, calls, err)
			}
		})
	}
}

func TestNewClientUsesOfficialBaseAndRejectsEncodedServiceKey(t *testing.T) {
	if got := NewClient(Config{}).config.BaseURL; got != OfficialBaseURL {
		t.Fatalf("base URL = %q, want official %q", got, OfficialBaseURL)
	}
	_, err := NewClient(Config{ServiceKey: "already%2Fencoded"}).List(context.Background(), model.CategoryGoods, ListQuery{})
	if !errors.Is(err, ErrEncodedServiceKey) {
		t.Fatalf("error = %v, want decoded-key error", err)
	}
}

func TestListUsesHourlyTimestampsAndPageSizeCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("inqryBgnDt") != "202609010900" || q.Get("inqryEndDt") != "202609011000" || q.Get("numOfRows") != "999" || q.Get("inqryDiv") != "1" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		if !strings.Contains(r.URL.RawQuery, "serviceKey=raw%2B%2F%3D") {
			t.Fatalf("service key was not encoded once: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00"},"body":{"items":{"item":[]},"totalCount":0}}}`))
	}))
	defer server.Close()
	_, err := NewClient(Config{BaseURL: server.URL, ServiceKey: "raw+/="}).List(context.Background(), model.CategoryGoods, ListQuery{PageSize: 1001, StartDate: time.Date(2026, 9, 1, 9, 0, 0, 0, korea), EndDate: time.Date(2026, 9, 1, 10, 0, 0, 0, korea)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListAcceptsSingleItemAndWarnsMalformedSibling(t *testing.T) {
	var warnings []FieldWarning
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00"},"body":{"items":{"item":{"bidNtceNo":"N-1","bidNtceOrd":"00","bidNtceNm":"single"}},"totalCount":1}}}`))
	}))
	defer server.Close()
	notices, err := NewClient(Config{BaseURL: server.URL, Warning: func(w FieldWarning) { warnings = append(warnings, w) }}).List(context.Background(), model.CategoryGoods, ListQuery{})
	if err != nil || len(notices) != 1 || !hasWarning(warnings, "region") {
		t.Fatalf("notices=%+v warnings=%+v err=%v", notices, warnings, err)
	}
}

func TestListSkipsInvalidSiblingAndWarnsWhenRegionUnavailable(t *testing.T) {
	var warnings []FieldWarning
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00"},"body":{"items":{"item":[{"bidNtceNo":"","bidNtceOrd":"00","bidNtceNm":"invalid"},{"bidNtceNo":"N-2","bidNtceOrd":"00","bidNtceNm":"valid","ntceInsttNm":"agency"}]},"totalCount":2}}}`))
	}))
	defer server.Close()
	notices, err := NewClient(Config{BaseURL: server.URL, Warning: func(warning FieldWarning) { warnings = append(warnings, warning) }}).List(context.Background(), model.CategoryGoods, ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Region != "" || notices[0].Identity() == "" {
		t.Fatalf("notices = %+v", notices)
	}
	if !hasWarning(warnings, "bid number") || !hasWarning(warnings, "region") {
		t.Fatalf("warnings = %+v", warnings)
	}
}

func TestListRejectsMalformedEnvelopeAndCapsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00"}}}`))
	}))
	defer server.Close()
	_, err := NewClient(Config{BaseURL: server.URL}).List(context.Background(), model.CategoryGoods, ListQuery{})
	var malformed *MalformedResponseError
	if !errors.As(err, &malformed) {
		t.Fatalf("error = %v, want malformed envelope", err)
	}

	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 65))) }))
	defer big.Close()
	_, err = NewClient(Config{BaseURL: big.URL, MaxBodyBytes: 64}).List(context.Background(), model.CategoryGoods, ListQuery{})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want capped body", err)
	}
}

func TestListRetriesTransientEnvelopeAndHonorsCancellation(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"01","resultMsg":"temporary"},"body":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00"},"body":{"items":{"item":[]},"totalCount":0}}}`))
	}))
	defer server.Close()
	_, err := NewClient(Config{BaseURL: server.URL, RetryBaseDelay: time.Millisecond, Wait: func(ctx context.Context, delay time.Duration) error { return nil }}).List(context.Background(), model.CategoryGoods, ListQuery{})
	if err != nil || attempts != 2 {
		t.Fatalf("err = %v, attempts = %d", err, attempts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewClient(Config{BaseURL: server.URL}).List(ctx, model.CategoryGoods, ListQuery{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}

func TestListUsesReasonableRetryAfter(t *testing.T) {
	attempts := 0
	var delay time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00"},"body":{"items":{"item":[]},"totalCount":0}}}`))
	}))
	defer server.Close()
	_, err := NewClient(Config{BaseURL: server.URL, RetryMaxDelay: 3 * time.Second, Wait: func(ctx context.Context, got time.Duration) error { delay = got; return nil }}).List(context.Background(), model.CategoryGoods, ListQuery{})
	if err != nil || delay != 2*time.Second {
		t.Fatalf("err=%v retry delay=%s", err, delay)
	}
}

func hasWarning(warnings []FieldWarning, field string) bool {
	for _, warning := range warnings {
		if warning.Field == field {
			return true
		}
	}
	return false
}
