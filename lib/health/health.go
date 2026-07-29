// Package health is the boot-time dependency probe every binary runs at step 2
// of the boot lifecycle (docs/erd-backend.md §5b.1): before wiring pools or
// serving traffic, confirm Postgres and Redis are reachable and fail fast if
// not. A process that cannot reach its dependencies must die on the way up, not
// three hours into serving requests.
//
// These are liveness probes, not the full readiness endpoint — the real
// /health that reports per-dependency status lands with the docker/integration
// slice. Here CheckAll returns a single wrapped error naming which dependency is
// down so the boot log points straight at the culprit.
package health

import (
	"context"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/lib/config"
)

// checkTimeout bounds the whole probe. A down dependency should surface in
// seconds, not hang the boot indefinitely on a TCP connect.
const checkTimeout = 5 * time.Second

// CheckAll verifies every external dependency the process needs is reachable and
// returns the first failure, wrapped with the dependency name. It applies its
// own short timeout so a black-holed host cannot stall the boot.
func CheckAll(ctx context.Context, cfg config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	if err := checkPostgres(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("health: postgres unreachable: %w", err)
	}

	if err := checkRedis(cfg.RedisURL); err != nil {
		return fmt.Errorf("health: redis unreachable: %w", err)
	}

	return nil
}

// waitTimeout / waitBackoff bound WaitAll: retry CheckAll for up to ~1 min, pausing
// between attempts. Wide enough to absorb a datastore that boots alongside this
// process; short enough that a genuinely-down dependency still fails the boot fast.
const (
	waitTimeout = 60 * time.Second
	waitBackoff = 2 * time.Second
)

// WaitAll polls CheckAll with a fixed backoff until every dependency is reachable or
// waitTimeout elapses. In an orchestrated environment (Railway, compose, k8s) a
// process and its datastores start together, so a one-shot probe makes boot ORDER a
// race: a worker that boots a second before Postgres accepts connections would die on
// the way up (docs §5b.1). Retrying for a bounded window turns "dependency not up YET"
// into a short wait, while still failing fast when a dependency is genuinely down.
func WaitAll(ctx context.Context, cfg config.Config) error {
	return waitFor(ctx, func(c context.Context) error { return CheckAll(c, cfg) }, waitTimeout, waitBackoff)
}

// waitFor is WaitAll's testable core: it retries check until it returns nil or the
// timeout elapses, sleeping backoff between attempts. Split out so the retry/timeout
// logic is unit-tested without real datastores (check is injected).
func waitFor(ctx context.Context, check func(context.Context) error, timeout, backoff time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		lastErr := check(ctx)
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health: dependencies not ready after %s: %w", timeout, lastErr)
		case <-time.After(backoff):
		}
	}
}

// checkPostgres opens a short-lived pool and pings it. The pool is closed before
// returning — this is a probe, not the application pool (database.NewPool owns
// that, later in boot).
func checkPostgres(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	return pool.Ping(ctx)
}

// checkRedis pings Redis through asynq's own client, so the probe speaks the
// exact connection settings (URI, DB, TLS) the workers and relay will use. The
// client is closed immediately; asynq.Client.Ping carries no context, but the
// underlying redis client dials with its own connect timeout.
func checkRedis(redisURL string) error {
	opt, err := asynq.ParseRedisURI(redisURL)
	if err != nil {
		return err
	}

	client := asynq.NewClient(opt)
	defer client.Close()

	return client.Ping()
}
