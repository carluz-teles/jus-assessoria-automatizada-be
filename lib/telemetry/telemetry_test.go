package telemetry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/telemetry"
)

// TestSetup is a smoke test of the wiring, with no live collector. The core
// guarantee is lazy gRPC: Setup must succeed and return a usable shutdown even
// though nothing listens at the endpoint (New never dials).
//
// Shutdown is bounded but not asserted error-free: the metric reader performs a
// final flush on shutdown, which does dial the (dead) endpoint and returns an
// infra error. What we require is that shutdown returns promptly within the
// deadline — no hang, no panic — and that any error is a typed apperr infra
// error, never a raw leak. Against a live collector the flush succeeds and
// shutdown returns nil.
func TestSetup(t *testing.T) {
	cfg := config.Config{
		OTELEndpoint: "localhost:4317",
		Env:          "test",
	}

	shutdown, err := telemetry.Setup(context.Background(), cfg, "test")
	if err != nil {
		t.Fatalf("Setup returned error (gRPC should be lazy): %v", err)
	}
	if shutdown == nil {
		t.Fatal("Setup returned nil shutdown")
	}

	// Short deadline bounds the flush attempt so the test cannot hang.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- shutdown(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			return // clean shutdown (e.g. if a collector happened to answer)
		}
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Kind != apperr.KindInfra {
			t.Fatalf("shutdown error must be a typed infra AppError, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return within 5s")
	}
}
