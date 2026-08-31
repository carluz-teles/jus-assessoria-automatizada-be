// Package breaker is a shared circuit-breaker for external egress that gets
// rate-limited or goes unstable under load: on a block it pushes forward a single
// deadline that every concurrent caller waits on before its next request, so a
// whole connector backs off together instead of independent retries piling onto an
// already-struggling endpoint. Extracted from internal/acquisition's DJEN connector
// (the first user) once a second connector (lib/eproc) needed the identical shape —
// duplicating it again would be the kind of split-brain logic the repo's Regra nº1
// forbids.
package breaker

import (
	"context"
	"sync"
	"time"
)

// Gate is the circuit-breaker itself. The pause grows exponentially per consecutive
// block (base<<n, capped at max) and resets on the first clean response after a
// block. Safe for concurrent use. Build it with New; the zero value is not usable.
type Gate struct {
	mu          sync.Mutex
	until       time.Time
	consecutive int
	now         func() time.Time
	base        time.Duration
	max         time.Duration
}

// Option tunes a Gate at construction.
type Option func(*Gate)

// WithClock overrides the time source (tests). A nil func keeps time.Now.
func WithClock(now func() time.Time) Option {
	return func(g *Gate) {
		if now != nil {
			g.now = now
		}
	}
}

// New builds a Gate that grows its pause from base up to max (exponential,
// base<<consecutive-blocks, capped at max).
func New(base, max time.Duration, opts ...Option) *Gate {
	g := &Gate{now: time.Now, base: base, max: max}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Now reports the Gate's time source — callers use it to compute a baseline for
// parsing a server's Retry-After header against the same clock the Gate itself uses.
func (g *Gate) Now() time.Time {
	return g.now()
}

// Wait blocks until the current cooldown deadline passes or ctx is cancelled. It is
// a no-op on the common path (no active cooldown).
func (g *Gate) Wait(ctx context.Context) error {
	g.mu.Lock()
	d := g.until.Sub(g.now())
	g.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Trip extends the shared cooldown after a block and returns the pause applied. A
// caller-supplied retryAfter (e.g. a server's Retry-After header) wins when
// positive; otherwise the pause is exponential in the consecutive-block count.
// Either way it is capped at max so a caller is never held back too long. The shift
// is clamped so it can never overflow.
func (g *Gate) Trip(retryAfter time.Duration) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()

	d := retryAfter
	if d <= 0 {
		shift := g.consecutive
		if shift > 5 {
			shift = 5
		}
		d = g.base << shift
	}
	if d <= 0 || d > g.max {
		d = g.max
	}
	g.consecutive++

	if until := g.now().Add(d); until.After(g.until) {
		g.until = until
	}
	return d
}

// Reset clears the consecutive-block streak after a clean response, so the next
// block starts its backoff from base again.
func (g *Gate) Reset() {
	g.mu.Lock()
	g.consecutive = 0
	g.mu.Unlock()
}
