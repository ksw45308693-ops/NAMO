package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"namo/internal/config"
	"namo/internal/model"
	"namo/internal/procurement"
	appweb "namo/internal/web"
)

func TestCollectionReadsCurrentKeyBeforeStartingWork(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{G2BAPIKeyFile: filepath.Join(dir, "key")}
	var seen []string
	run := collectionWithAPIKey(cfg, func(_ context.Context, current config.Config) (CollectionResult, error) {
		seen = append(seen, current.G2BAPIKey)
		return CollectionResult{}, nil
	})
	if _, err := run(context.Background()); !errors.Is(err, config.ErrAPIKeyNotConfigured) || len(seen) != 0 {
		t.Fatal("missing key started collection")
	}
	for _, key := range []string{"key-one", "key%2Btwo%2F%3D"} {
		if err := cfg.APIKeys().Save(key); err != nil {
			t.Fatal(err)
		}
		if _, err := run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] != "key-one" || seen[1] != "key+two/=" {
		t.Fatal("runner cached stale key")
	}
	if cfg.G2BAPIKey != "" {
		t.Fatal("mutated shared config")
	}
}

func TestSchedulerWaitsForAPIKeyWithoutStoppingReports(t *testing.T) {
	reports := 0
	scheduler := newServeScheduler(func(context.Context) (CollectionResult, error) {
		return CollectionResult{}, config.ErrAPIKeyNotConfigured
	}, func(context.Context, time.Time) error { reports++; return nil })
	if err := scheduler.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if reports != 1 {
		t.Fatal("missing API key stopped report job")
	}
}

func TestCollectionSendsEncodedPortalKeyExactlyOnce(t *testing.T) {
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("serviceKey")
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00","resultMsg":"OK"},"body":{"items":[],"totalCount":0,"numOfRows":100,"pageNo":1}}}`))
	}))
	defer server.Close()
	run := collectionWithAPIKey(config.Config{G2BAPIKey: "test+key%2F%3D"}, func(ctx context.Context, cfg config.Config) (CollectionResult, error) {
		client := procurement.NewClient(procurement.Config{BaseURL: server.URL, HTTPClient: server.Client(), ServiceKey: cfg.G2BAPIKey})
		_, err := client.List(ctx, model.CategoryConstruction, procurement.ListQuery{})
		return CollectionResult{}, err
	})
	if _, err := run(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case key := <-requests:
		if key != "test+key/=" {
			t.Fatal("API received a double-encoded key or a lost plus")
		}
	default:
		t.Fatal("collection did not reach API")
	}
}

func TestWebServiceAPIKeyRequiresIdentifiedPlatformAdmin(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := config.APIKeyStore{Path: filepath.Join(dir, "key")}
	s := &WebService{APIKeys: store}
	for _, identity := range []appweb.RequestContext{{}, {Role: "platform_admin"}, {UserID: "tenant", Role: "tenant_admin"}} {
		if err := s.SaveG2BAPIKey(context.Background(), identity, "test-key"); err == nil {
			t.Fatal("unauthorized save")
		}
	}
	if _, err := store.Read(); !errors.Is(err, config.ErrAPIKeyNotConfigured) {
		t.Fatal("unauthorized mutation")
	}
	admin := appweb.RequestContext{UserID: "admin", Role: "platform_admin"}
	queued := false
	s.QueueCollection = func() error { queued = true; return nil }
	if err := s.RunCollection(context.Background(), admin); !errors.Is(err, config.ErrAPIKeyNotConfigured) || queued {
		t.Fatal("queued without a key")
	}
	if err := s.SaveG2BAPIKey(context.Background(), admin, "test-key"); err != nil {
		t.Fatal(err)
	}
	if key, _ := store.Read(); key != "test-key" {
		t.Fatal("key not persisted")
	}
	if err := s.RunCollection(context.Background(), admin); err != nil || !queued {
		t.Fatal("configured collection did not queue")
	}
}
