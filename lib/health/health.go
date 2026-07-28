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
