package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCollectionBusyRetriesWrappedBusyError(t *testing.T) {
	calls := 0
	err := retryCollectionBusy(context.Background(), func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("snapshot: %w", ErrCollectionRunning)
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestCollectionBusyRetryStopsOnCancellationAndOtherErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryCollectionBusy(ctx, func() error {
		calls++
		cancel()
		return ErrCollectionRunning
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	want := errors.New("database unavailable")
	err = retryCollectionBusy(context.Background(), func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
}
