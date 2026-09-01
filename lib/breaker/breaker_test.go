package breaker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGate_Wait_NoOpWithoutCooldown(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	g := New(2*time.Second, 30*time.Second)
	is.NoError(g.Wait(context.Background()))
}

func TestGate_TripThenWait_BlocksUntilDeadline(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	clock := func() time.Time { return now }
	g := New(2*time.Second, 30*time.Second, WithClock(func() time.Time { return clock() }))

	d := g.Trip(0)
	is.Equal(2*time.Second, d, "first block uses base with no shift")

	now = now.Add(2 * time.Second)
	is.NoError(g.Wait(context.Background()), "deadline already passed for the new now")
}

func TestGate_Trip_ExponentialBackoffCappedAtMax(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	g := New(2*time.Second, 10*time.Second, WithClock(func() time.Time { return now }))

	is.Equal(2*time.Second, g.Trip(0), "1st block: base<<0")
	now = now.Add(2 * time.Second)
	is.Equal(4*time.Second, g.Trip(0), "2nd block: base<<1")
	now = now.Add(4 * time.Second)
	is.Equal(8*time.Second, g.Trip(0), "3rd block: base<<2")
	now = now.Add(8 * time.Second)
	is.Equal(10*time.Second, g.Trip(0), "4th block: base<<3=16s capped at max")
}

func TestGate_Trip_RetryAfterWinsOverExponential(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	g := New(2*time.Second, 30*time.Second)
	is.Equal(5*time.Second, g.Trip(5*time.Second))
}

func TestGate_Trip_RetryAfterStillCappedAtMax(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	g := New(2*time.Second, 10*time.Second)
	is.Equal(10*time.Second, g.Trip(1*time.Hour))
}

func TestGate_Reset_RestartsBackoffFromBase(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	g := New(2*time.Second, 30*time.Second, WithClock(func() time.Time { return now }))

	g.Trip(0)
	now = now.Add(2 * time.Second)
	is.Equal(4*time.Second, g.Trip(0), "2nd consecutive block doubles")

	g.Reset()
	now = now.Add(4 * time.Second)
	is.Equal(2*time.Second, g.Trip(0), "after reset, next block is base again")
}

func TestGate_Wait_HonorsContextCancellation(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	g := New(1*time.Hour, 1*time.Hour)
	g.Trip(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := g.Wait(ctx)
	require.Error(t, err)
	is.ErrorIs(err, context.DeadlineExceeded)
}

func TestGate_Now_ReflectsInjectedClock(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	fixed := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	g := New(2*time.Second, 30*time.Second, WithClock(func() time.Time { return fixed }))
	is.Equal(fixed, g.Now())
}
