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
	if next, err := src.Next(32); err != nil || next != 33 {
		t.Fatalf("Next(32) = (%d, %v), want (33, nil)", next, err)
	}
	if next, err := src.Next(33); err != nil || next != 34 {
		t.Fatalf("Next(33) = (%d, %v), want (34, nil)", next, err)
	}
	if next, err := src.Next(34); err != nil || next != 35 {
		t.Fatalf("Next(34) = (%d, %v), want (35, nil)", next, err)
	}
	if next, err := src.Next(35); err != nil || next != 36 {
		t.Fatalf("Next(35) = (%d, %v), want (36, nil)", next, err)
	}
	// The billing local-catalog slice adds 37 (plan), 38 (trial_policy) and 39
	// (subscription's plan_id/custom_price_per_process_cents/trial_ends_at); the
	// notifications preferences slice adds 40 (notification_preference); the process-
	// summary slice adds 41 (court_record.ai_resume) — so 1→…→39→40→41 and 41 is the
	// last — nothing follows it.
	if next, err := src.Next(36); err != nil || next != 37 {
		t.Fatalf("Next(36) = (%d, %v), want (37, nil)", next, err)
	}
	if next, err := src.Next(37); err != nil || next != 38 {
		t.Fatalf("Next(37) = (%d, %v), want (38, nil)", next, err)
	}
	if next, err := src.Next(38); err != nil || next != 39 {
		t.Fatalf("Next(38) = (%d, %v), want (39, nil)", next, err)
	}
	if next, err := src.Next(39); err != nil || next != 40 {
		t.Fatalf("Next(39) = (%d, %v), want (40, nil)", next, err)
	}
	if next, err := src.Next(40); err != nil || next != 41 {
		t.Fatalf("Next(40) = (%d, %v), want (41, nil)", next, err)
	}
	if next, err := src.Next(41); err != nil || next != 42 {
		t.Fatalf("Next(41) = (%d, %v), want (42, nil)", next, err)
	}
	if next, err := src.Next(42); err != nil || next != 43 {
		t.Fatalf("Next(42) = (%d, %v), want (43, nil)", next, err)
	}
	if next, err := src.Next(43); err != nil || next != 44 {
		t.Fatalf("Next(43) = (%d, %v), want (44, nil)", next, err)
	}
	if next, err := src.Next(44); err != nil || next != 45 {
		t.Fatalf("Next(44) = (%d, %v), want (45, nil)", next, err)
	}
	if next, err := src.Next(45); err != nil || next != 46 {
		t.Fatalf("Next(45) = (%d, %v), want (46, nil)", next, err)
	}
	if next, err := src.Next(46); err != nil || next != 47 {
		t.Fatalf("Next(46) = (%d, %v), want (47, nil)", next, err)
	}
	if next, err := src.Next(47); err != nil || next != 48 {
		t.Fatalf("Next(47) = (%d, %v), want (48, nil)", next, err)
	}
	if next, err := src.Next(48); err != nil || next != 49 {
		t.Fatalf("Next(48) = (%d, %v), want (49, nil)", next, err)
	}
	if next, err := src.Next(49); err != nil || next != 50 {
		t.Fatalf("Next(49) = (%d, %v), want (50, nil)", next, err)
	}
	if next, err := src.Next(50); err != nil || next != 51 {
		t.Fatalf("Next(50) = (%d, %v), want (51, nil)", next, err)
	}
	if next, err := src.Next(51); err != nil || next != 52 {
		t.Fatalf("Next(51) = (%d, %v), want (52, nil)", next, err)
	}
	if next, err := src.Next(52); err != nil || next != 53 {
		t.Fatalf("Next(52) = (%d, %v), want (53, nil)", next, err)
	}
	if next, err := src.Next(53); err != nil || next != 54 {
		t.Fatalf("Next(53) = (%d, %v), want (54, nil)", next, err)
	}
	if next, err := src.Next(54); err != nil || next != 55 {
		t.Fatalf("Next(54) = (%d, %v), want (55, nil)", next, err)
	}
	if next, err := src.Next(55); err != nil || next != 56 {
		t.Fatalf("Next(55) = (%d, %v), want (56, nil)", next, err)
	}
	if next, err := src.Next(56); err != nil || next != 57 {
		t.Fatalf("Next(56) = (%d, %v), want (57, nil)", next, err)
	}
	if next, err := src.Next(57); err != nil || next != 58 {
		t.Fatalf("Next(57) = (%d, %v), want (58, nil)", next, err)
	}
	if next, err := src.Next(58); err != nil || next != 59 {
		t.Fatalf("Next(58) = (%d, %v), want (59, nil)", next, err)
	}
	if next, err := src.Next(59); err != nil || next != 60 {
		t.Fatalf("Next(59) = (%d, %v), want (60, nil)", next, err)
	}
	if next, err := src.Next(60); err != nil || next != 61 {
		t.Fatalf("Next(60) = (%d, %v), want (61, nil)", next, err)
	}
	if next, err := src.Next(61); err != nil || next != 62 {
		t.Fatalf("Next(61) = (%d, %v), want (62, nil)", next, err)
	}
	if _, err := src.Next(62); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Next(62) error = %v, want fs.ErrNotExist", err)
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
