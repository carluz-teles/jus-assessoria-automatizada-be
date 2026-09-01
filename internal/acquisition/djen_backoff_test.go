package acquisition

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The breaker.Gate mechanics (exponential backoff, cap, reset, Wait/context) are
// tested once in lib/breaker/breaker_test.go — not re-tested here, that would be
// the exact duplicated-logic-as-duplicated-tests smell Regra nº1 warns about.
// What's DJEN-specific and still worth covering here: parseRetryAfter's parsing and
// that a 429 response actually trips the connector's shared gate end-to-end.

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"seconds", "5", 5 * time.Second},
		{"zero", "0", 0},
		{"negative", "-3", 0},
		{"garbage", "soon", 0},
		{"http-date future", now.Add(30 * time.Second).UTC().Format(http.TimeFormat), 30 * time.Second},
		{"http-date past", now.Add(-30 * time.Second).UTC().Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(tt.in, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestDJENConnector_RateLimited asserts a 429 surfaces a typed, retryable
// RateLimitedError (with the server's Retry-After and the request identity) and arms
// the shared cooldown so the next slice backs off.
func TestDJENConnector_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000))
	_, err := c.Fetch(context.Background(), FetchRequest{
		Capability: CapabilityDiscoverByOAB,
		OABs:       []OABEntry{{Number: "347019", UF: "SP"}},
	})
	if err == nil {
		t.Fatal("want error for HTTP 429, got nil")
	}

	var rle *RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("want *RateLimitedError in the chain, got %v", err)
	}
	if rle.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", rle.Status)
	}
	if rle.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", rle.RetryAfter)
	}
	if rle.OAB != "347019" || rle.UF != "SP" || rle.Page != 1 {
		t.Errorf("identity = %s/%s page %d, want 347019/SP page 1", rle.OAB, rle.UF, rle.Page)
	}
	// A tripped gate blocks Wait even on an already-cancelled context (it has a real
	// deadline to honor); an untripped gate would return nil before ever selecting on
	// ctx.Done(). Only the exported API is used — the gate now lives in lib/breaker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.cooldown.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Error("cooldown gate should be tripped after a 429")
	}
}
