// Package database holds shared persistence infrastructure injected into the
// vertical slices. Here it is the golang-migrate runner that applies the
// versioned schema at boot — see docs/erd-backend.md 5b.1 (boot order) and 5b.3
// (only the api binary migrates; the workers assume the schema is ready).
package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// database driver
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/jusassessoria/platform/migrations"
)

// Up applies every pending migration against databaseURL and returns nil when the
// schema is already current (migrate.ErrNoChange). Only the api binary calls this,
// at boot (docs/erd-backend.md 5b.3).
//
// golang-migrate v4's Up is not context-aware, so ctx cannot be threaded into the
// migration itself; it is honored at the boundary — a boot context already
// cancelled short-circuits before we take the database lock.
func Up(ctx context.Context, databaseURL string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("migration context cancelled: %w", err)
	}

	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("opening embedded migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, pgxURL(databaseURL))
	if err != nil {
		return fmt.Errorf("building migrator: %w", err)
	}
	// migrate opens its own pgx pool for the migration; release it on return so it
	// does not outlive this call. Close reports the source and database close errors
	// separately; the migration outcome is the primary return, so these are logged,
	// not returned (single-handling rule).
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			slog.Warn("closing migrator", "src_err", srcErr, "db_err", dbErr)
		}
	}()

	// Version before applying, for an informative boot log. golang-migrate is silent
	// on success, so without this the api log gives no sign the schema was applied —
	// making "did migrations run?" unanswerable from the outside. ErrNilVersion means
	// a fresh database (nothing applied yet).
	before, _, verr := m.Version()

	switch err := m.Up(); {
	case errors.Is(err, migrate.ErrNoChange):
		slog.Info("migrations: schema already current", "version", before)
		return nil
	case err != nil:
		return fmt.Errorf("applying migrations: %w", err)
	}

	after, _, _ := m.Version()
	if errors.Is(verr, migrate.ErrNilVersion) {
		slog.Info("migrations: applied on empty schema", "to_version", after)
	} else {
		slog.Info("migrations: applied", "from_version", before, "to_version", after)
	}
	return nil
}

// pgxURL rewrites a standard DATABASE_URL (postgres:// or postgresql://) to the
// pgx5:// scheme that the migrate pgx/v5 database driver registers. Any other
// scheme is passed through untouched.
func pgxURL(databaseURL string) string {
	switch {
	case strings.HasPrefix(databaseURL, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(databaseURL, "postgresql://")
	case strings.HasPrefix(databaseURL, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(databaseURL, "postgres://")
	default:
		return databaseURL
	}
}
