package health

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor is the retry/timeout core behind WaitAll. These tests exercise it with an
// injected check, so no real Postgres/Redis is needed — the boot-order resilience is
// verified deterministically and fast.

func TestWaitFor_SucceedsImmediately(t *testing.T) {
	var calls atomic.Int32
	check := func(context.Context) error { calls.Add(1); return nil }

	if err := waitFor(context.Background(), check, time.Second, time.Second); err != nil {
		t.Fatalf("expected immediate success, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", got)
	}
}

func TestWaitFor_SucceedsAfterRetries(t *testing.T) {
	var calls atomic.Int32
	// Fails the first two attempts (dependency still coming up), then succeeds.
	check := func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("connection refused")
		}
		return nil
	}

	if err := waitFor(context.Background(), check, time.Second, time.Millisecond); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestWaitFor_TimesOut(t *testing.T) {
	sentinel := errors.New("connection refused")
	check := func(context.Context) error { return sentinel }

	err := waitFor(context.Background(), check, 30*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("timeout error should mention readiness, got %v", err)
	}
	// The last probe error must be preserved so the boot log names the culprit.
	if !errors.Is(err, sentinel) {
		t.Fatalf("timeout error should wrap the last check error, got %v", err)
	}
}
