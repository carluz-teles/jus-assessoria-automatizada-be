package database

import (
	"context"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/lib/config"
)

// NewPool builds the application's pgx connection pool from cfg.DatabaseURL. It
// wires otelpgx as the pool tracer so every query emits a span (docs 4b.7), and
// pings once to fail fast at boot (docs 5b.1) — a process that cannot reach the
// database must die on the way up, not three hours into serving requests.
//
// The caller owns the pool's lifetime and MUST Close it on shutdown. Any failure
// is returned as an infra error via WrapInfra.
func NewPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, WrapInfra(err)
	}
	pc.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, WrapInfra(err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, WrapInfra(err)
	}

	return pool, nil
}
