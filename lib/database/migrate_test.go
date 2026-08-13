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
	// membership table); the acquisition rename adds 6 (notification → intimation);
	// the billing slice adds 7 (the subscription table); the notifications slice
	// adds 8 (notification + notification_delivery); the sync hardening adds 9
	// (sync_run.event_id); the tenant-phone slice adds 10 (tenant.phone); the
	// tenant-email slice adds 11 (tenant.email).
	// The holiday slice adds 12 (the holiday table) and 13 (the state-holiday
	// seed); the DJEN connector adds 14 (intimation DJEN fields + court_record
	// judging_body); the DATAJUD enrichment adds 15 (court_record.filed_at); the
	// re-poll scheduler adds 16 (court_record RLS system escape hatch).
	// So 1→…→15→16 and 16 is the last — nothing follows it.
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
	if next, err := src.Next(5); err != nil || next != 6 {
		t.Fatalf("Next(5) = (%d, %v), want (6, nil)", next, err)
	}
	if next, err := src.Next(6); err != nil || next != 7 {
		t.Fatalf("Next(6) = (%d, %v), want (7, nil)", next, err)
	}
	if next, err := src.Next(7); err != nil || next != 8 {
		t.Fatalf("Next(7) = (%d, %v), want (8, nil)", next, err)
	}
	if next, err := src.Next(8); err != nil || next != 9 {
		t.Fatalf("Next(8) = (%d, %v), want (9, nil)", next, err)
	}
	if next, err := src.Next(9); err != nil || next != 10 {
		t.Fatalf("Next(9) = (%d, %v), want (10, nil)", next, err)
	}
	if next, err := src.Next(10); err != nil || next != 11 {
		t.Fatalf("Next(10) = (%d, %v), want (11, nil)", next, err)
	}
	if next, err := src.Next(11); err != nil || next != 12 {
		t.Fatalf("Next(11) = (%d, %v), want (12, nil)", next, err)
	}
	if next, err := src.Next(12); err != nil || next != 13 {
		t.Fatalf("Next(12) = (%d, %v), want (13, nil)", next, err)
	}
	if next, err := src.Next(13); err != nil || next != 14 {
		t.Fatalf("Next(13) = (%d, %v), want (14, nil)", next, err)
	}
	if next, err := src.Next(14); err != nil || next != 15 {
		t.Fatalf("Next(14) = (%d, %v), want (15, nil)", next, err)
	}
	if next, err := src.Next(15); err != nil || next != 16 {
		t.Fatalf("Next(15) = (%d, %v), want (16, nil)", next, err)
	}
	if next, err := src.Next(16); err != nil || next != 17 {
		t.Fatalf("Next(16) = (%d, %v), want (17, nil)", next, err)
	}
	if next, err := src.Next(17); err != nil || next != 18 {
		t.Fatalf("Next(17) = (%d, %v), want (18, nil)", next, err)
	}
	if next, err := src.Next(18); err != nil || next != 19 {
		t.Fatalf("Next(18) = (%d, %v), want (19, nil)", next, err)
	}
	if next, err := src.Next(19); err != nil || next != 20 {
		t.Fatalf("Next(19) = (%d, %v), want (20, nil)", next, err)
	}
	if next, err := src.Next(20); err != nil || next != 21 {
		t.Fatalf("Next(20) = (%d, %v), want (21, nil)", next, err)
	}
	if next, err := src.Next(21); err != nil || next != 22 {
		t.Fatalf("Next(21) = (%d, %v), want (22, nil)", next, err)
	}
	if next, err := src.Next(22); err != nil || next != 23 {
		t.Fatalf("Next(22) = (%d, %v), want (23, nil)", next, err)
	}
	if next, err := src.Next(23); err != nil || next != 24 {
		t.Fatalf("Next(23) = (%d, %v), want (24, nil)", next, err)
	}
	if next, err := src.Next(24); err != nil || next != 25 {
		t.Fatalf("Next(24) = (%d, %v), want (25, nil)", next, err)
	}
	if next, err := src.Next(25); err != nil || next != 26 {
		t.Fatalf("Next(25) = (%d, %v), want (26, nil)", next, err)
	}
	if next, err := src.Next(26); err != nil || next != 27 {
		t.Fatalf("Next(26) = (%d, %v), want (27, nil)", next, err)
	}
	if next, err := src.Next(27); err != nil || next != 28 {
		t.Fatalf("Next(27) = (%d, %v), want (28, nil)", next, err)
	}
	if next, err := src.Next(28); err != nil || next != 29 {
		t.Fatalf("Next(28) = (%d, %v), want (29, nil)", next, err)
	}
	if next, err := src.Next(29); err != nil || next != 30 {
		t.Fatalf("Next(29) = (%d, %v), want (30, nil)", next, err)
	}
	if next, err := src.Next(30); err != nil || next != 31 {
		t.Fatalf("Next(30) = (%d, %v), want (31, nil)", next, err)
	}
	if next, err := src.Next(31); err != nil || next != 32 {
		t.Fatalf("Next(31) = (%d, %v), want (32, nil)", next, err)
	}
	if _, err := src.Next(32); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Next(32) error = %v, want fs.ErrNotExist", err)
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
