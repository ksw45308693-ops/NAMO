package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"namo/internal/config"
	appweb "namo/internal/web"
)

func TestAsyncCollectionTriggerReturnsImmediatelyAndOutlivesRequest(t *testing.T) {
	serviceCtx, stopService := context.WithCancel(context.Background())
	defer stopService()

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	trigger, err := newAsyncCollectionTrigger(serviceCtx, func(ctx context.Context) (CollectionResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return CollectionResult{}, ctx.Err()
		}
		close(finished)
		return CollectionResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	service := &WebService{QueueCollection: trigger.Trigger, APIKeys: config.APIKeyStore{Fallback: "test-key"}}
	returned := make(chan error, 1)
	go func() {
		returned <- service.RunCollection(requestCtx, appweb.RequestContext{UserID: "admin", Role: "platform_admin"})
	}()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("RunCollection() error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("RunCollection blocked on the long-running collection")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued collection did not start")
	}

	cancelRequest()
	select {
	case <-finished:
		t.Fatal("request cancellation stopped the service-owned collection")
	default:
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("collection did not finish after release")
	}
}

func TestAsyncCollectionTriggerUsesServiceCancellation(t *testing.T) {
	serviceCtx, stopService := context.WithCancel(context.Background())
	started := make(chan struct{})
	finished := make(chan error, 1)
	trigger, err := newAsyncCollectionTrigger(serviceCtx, func(ctx context.Context) (CollectionResult, error) {
		close(started)
		<-ctx.Done()
		finished <- ctx.Err()
		return CollectionResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := trigger.Trigger(); err != nil {
		t.Fatal(err)
	}
	<-started
	stopService()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runner cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service cancellation did not stop collection")
	}
	if err := trigger.Trigger(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Trigger() after shutdown error = %v", err)
	}
}

func TestAsyncCollectionTriggerCoalescesWhileRunning(t *testing.T) {
	serviceCtx, stopService := context.WithCancel(context.Background())
	defer stopService()
	started := make(chan struct{})
	release := make(chan struct{})
	calls := make(chan struct{}, 2)
	trigger, err := newAsyncCollectionTrigger(serviceCtx, func(context.Context) (CollectionResult, error) {
		calls <- struct{}{}
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return CollectionResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := trigger.Trigger(); err != nil {
		t.Fatal(err)
	}
	<-started
	for range 5 {
		if err := trigger.Trigger(); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if got := len(calls); got != 1 {
		t.Fatalf("collection calls = %d, want 1", got)
	}
}
