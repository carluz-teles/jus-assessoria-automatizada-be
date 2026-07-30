package database

import (
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/jusassessoria/platform/migrations"
)

// TestEmbeddedSource asserts the migration files embed and parse as a valid
// golang-migrate iofs source without touching a live database (the plain green
// gate has no Postgres — real-DB apply is covered by an integration test later).
func TestEmbeddedSource(t *testing.T) {
	t.Parallel()

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	// Close over an embed.FS source is a no-op that cannot fail, but the rule is that
	// returned errors are never silently dropped — surface it as a cleanup failure.
	t.Cleanup(func() {
		if err := src.Close(); err != nil {
			t.Errorf("src.Close: %v", err)
		}
	})

	first, err := src.First()
	if err != nil {
		t.Fatalf("First: %v", err)
	}
	if first != 1 {
		t.Fatalf("first migration version = %d, want 1", first)
	}

	// Version 1 must have a non-empty up and a non-empty down (the pair).
	up, _, err := src.ReadUp(1)
	if err != nil {
		t.Fatalf("ReadUp(1): %v", err)
	}
	t.Cleanup(func() {
		if err := up.Close(); err != nil {
			t.Errorf("up.Close: %v", err)
		}
	})
	if n := mustReadLen(t, up); n == 0 {
		t.Fatal("up migration for version 1 is empty")
	}

	down, _, err := src.ReadDown(1)
	if err != nil {
		t.Fatalf("ReadDown(1): %v", err)
	}
	t.Cleanup(func() {
		if err := down.Close(); err != nil {
			t.Errorf("down.Close: %v", err)
		}
	})
	if n := mustReadLen(t, down); n == 0 {
		t.Fatal("down migration for version 1 is empty")
	}

	// The acquisition slice adds migration 2 (integration.updated_at) and 3
	// (backfill_job integration_id index); the identity onboarding slice adds 4
	// (tenant company cols + app_user.phone); the convites slice adds 5 (the
	// membership table). So 1→2→3→4→5 and 5 is the last — nothing follows it.
	if next, err := src.Next(1); err != nil || next != 2 {
		t.Fatalf("Next(1) = (%d, %v), want (2, nil)", next, err)
	}
	if next, err := src.Next(2); err != nil || next != 3 {
		t.Fatalf("Next(2) = (%d, %v), want (3, nil)", next, err)
	}
	if next, err := src.Next(3); err != nil || next != 4 {
		t.Fatalf("Next(3) = (%d, %v), want (4, nil)", next, err)
	}
	if next, err := src.Next(4); err != nil || next != 5 {
		t.Fatalf("Next(4) = (%d, %v), want (5, nil)", next, err)
	}
	if _, err := src.Next(5); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Next(5) error = %v, want fs.ErrNotExist", err)
	}
}

// TestPgxURL covers the DATABASE_URL -> pgx5:// scheme rewrite the migrate
// pgx/v5 driver requires. Pure and DB-less.
func TestPgxURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "postgres scheme is rewritten",
			in:   "postgres://user:pass@localhost:5432/jus",
			want: "pgx5://user:pass@localhost:5432/jus",
		},
		{
			name: "postgresql scheme is rewritten, query preserved",
			in:   "postgresql://user:pass@host/jus?sslmode=disable",
			want: "pgx5://user:pass@host/jus?sslmode=disable",
		},
		{
			name: "already pgx5 is passed through",
			in:   "pgx5://user@host/jus",
			want: "pgx5://user@host/jus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := pgxURL(tt.in); got != tt.want {
				t.Errorf("pgxURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func mustReadLen(t *testing.T, r io.Reader) int {
	t.Helper()

	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading migration: %v", err)
	}
	return len(b)
}
