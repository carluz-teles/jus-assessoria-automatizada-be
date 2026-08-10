package acquisition

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A frozen clock keeps the gate's arithmetic deterministic — no real sleeps.
func frozenGate(at time.Time) *cooldownGate {
	return &cooldownGate{now: func() time.Time { return at }}
}

func TestCooldownGate_ExponentialAndCap(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	g := frozenGate(base)

	// A fresh gate imposes no wait.
	if d := g.until.Sub(base); d > 0 {
		t.Fatalf("fresh gate should not be in cooldown, until-now=%s", d)
	}

	// Without a Retry-After the pause doubles per consecutive block.
	for i, want := range []time.Duration{
		djenCooldownBase, djenCooldownBase << 1, djenCooldownBase << 2, djenCooldownBase << 3,
	} {
		if got := g.trip(0); got != want {
			t.Fatalf("trip #%d = %s, want %s", i+1, got, want)
		}
	}

	// It keeps escalating but never exceeds the cap (and never overflows negative).
	for i := 0; i < 12; i++ {
		if got := g.trip(0); got <= 0 || got > djenCooldownMax {
			t.Fatalf("escalated trip out of bounds: %s (cap %s)", got, djenCooldownMax)
		}
	}

	// A clean page resets the streak, so the next block starts from the base again.
	g.reset()
	if got := g.trip(0); got != djenCooldownBase {
		t.Fatalf("post-reset trip = %s, want %s", got, djenCooldownBase)
	}
}

func TestCooldownGate_RetryAfterWinsButIsCapped(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	g := frozenGate(base)

	if got := g.trip(5 * time.Second); got != 5*time.Second {
		t.Fatalf("Retry-After pause = %s, want 5s", got)
	}
	if got := g.trip(2 * time.Minute); got != djenCooldownMax {
		t.Fatalf("over-cap Retry-After pause = %s, want cap %s", got, djenCooldownMax)
	}
}

func TestCooldownGate_Wait(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	now := base
	g := &cooldownGate{now: func() time.Time { return now }}

	// No cooldown → returns immediately.
	if err := g.wait(context.Background()); err != nil {
		t.Fatalf("wait with no cooldown: %v", err)
	}

	// After a trip, advancing the clock past the deadline clears the wait.
	g.trip(0) // until = base + djenCooldownBase
	now = base.Add(djenCooldownBase + time.Second)
	if err := g.wait(context.Background()); err != nil {
		t.Fatalf("wait after deadline passed: %v", err)
	}
}

func TestCooldownGate_WaitRespectsContext(t *testing.T) {
	t.Parallel()
	// Real clock so an active cooldown has a real (short) deadline; a cancelled ctx
	// must unblock wait immediately instead of sleeping it out.
	g := newCooldownGate()
	g.trip(0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait(cancelled ctx) = %v, want context.Canceled", err)
	}
}

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
	if c.cooldown.until.Sub(c.cooldown.now()) <= 0 {
		t.Error("cooldown gate should be tripped after a 429")
	}
}
