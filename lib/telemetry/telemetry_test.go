package telemetry

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/config"

	"go.opentelemetry.io/otel/log/global"
)

// TestSetup is a smoke test of the wiring, with no live collector. The core
// guarantee is lazy gRPC: Setup must succeed and return a usable shutdown even
// though nothing listens at the endpoint (New never dials). Both security modes
// are exercised — insecure (local collector) and TLS (authenticated backend like
// New Relic, with an api-key header) — and both must return fast without error.
//
// Shutdown is bounded but not asserted error-free: the metric reader and the log
// batch processor perform a final flush on shutdown, which does dial the (dead)
// endpoint and returns an infra error. What we require is that shutdown returns
// promptly within the deadline — no hang, no panic — and that any error is a
// typed apperr infra error, never a raw leak. Against a live collector the flush
// succeeds and shutdown returns nil.
//
// Setup also installs the global LoggerProvider (the slog OTel bridge delegates
// to it), asserted non-nil below.
//
// Not parallel: Setup installs the global otel TracerProvider/MeterProvider, so
// concurrent subtests would race on that process-wide state.
func TestSetup(t *testing.T) {
	tests := []struct {
		name     string
		insecure bool
		headers  string
	}{
		{name: "insecure local collector", insecure: true, headers: ""},
		{name: "tls authenticated backend", insecure: false, headers: "api-key=secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				OTELEndpoint: "localhost:4317",
				OTELInsecure: tt.insecure,
				OTELHeaders:  tt.headers,
				Env:          "test",
			}

			shutdown, err := Setup(context.Background(), cfg, "test")
			if err != nil {
				t.Fatalf("Setup returned error (gRPC should be lazy): %v", err)
			}
			if shutdown == nil {
				t.Fatal("Setup returned nil shutdown")
			}
			// Setup must install a LoggerProvider on the log global so the slog
			// OTel bridge (built earlier at the boot logger step) starts emitting.
			if global.GetLoggerProvider() == nil {
				t.Fatal("Setup did not install a global LoggerProvider")
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
		})
	}
}

// TestParseOTLPHeaders pins the OTLP header-string grammar: comma-separated
// pairs, split on the FIRST '=' (so values may contain '='), spaces trimmed,
// blank/malformed/keyless pairs skipped.
func TestParseOTLPHeaders(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{name: "empty string", raw: "", want: map[string]string{}},
		{name: "single pair", raw: "api-key=abc", want: map[string]string{"api-key": "abc"}},
		{name: "multiple pairs", raw: "a=1,b=2", want: map[string]string{"a": "1", "b": "2"}},
		{name: "spaces trimmed", raw: " a = 1 , b = 2 ", want: map[string]string{"a": "1", "b": "2"}},
		{name: "value keeps inner equals", raw: "k=a=b", want: map[string]string{"k": "a=b"}},
		{name: "malformed pair skipped", raw: "foo", want: map[string]string{}},
		{name: "blank key skipped", raw: "=v,a=1", want: map[string]string{"a": "1"}},
		{name: "trailing comma skipped", raw: "a=1,", want: map[string]string{"a": "1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseOTLPHeaders(tt.raw)
			if !maps.Equal(got, tt.want) {
				t.Errorf("parseOTLPHeaders(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
