//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
)

// TestWatchedOABLifecycle drives the watched_oab liga/desliga lifecycle against real
// Postgres: AddOrEnableWatchedOAB must report needsHistory=true only for a genuinely
// new key, needsCatchUp=true only for a real re-enable (a key that was disabled), and
// both false for an idempotent re-post of an already-enabled key. This is the query
// (0070_watched_oab_lifecycle) the DEV handoff flagged as "the most delicate part of
// the design" and asked to be proven against real Postgres, not just mocked.
func TestWatchedOABLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-woab-lifecycle", 0)
	integrationID := seedIntegration(t, pool, tenantID, "DJEN")

	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)
	const key = "LIFECYCLE-347019|SP"

	// Case A: brand-new key -> needsHistory=true, needsCatchUp=false.
	var row acquisition.WatchedOAB
	var needsHistory, needsCatchUp bool
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		row, needsHistory, needsCatchUp, err = repo.AddOrEnableWatchedOAB(ctx, tx, tenantID, integrationID, key)
		return err
	}); err != nil {
		t.Fatalf("AddOrEnableWatchedOAB (new): %v", err)
	}
	if !needsHistory || needsCatchUp {
		t.Fatalf("new key = {needsHistory:%v needsCatchUp:%v}, want {true false}", needsHistory, needsCatchUp)
	}
	if !row.Enabled || row.OABKey != key {
		t.Fatalf("new row = %+v, want enabled with oab_key %q", row, key)
	}

	// Case B: idempotent re-post of the same (still enabled) key -> both false.
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		_, needsHistory, needsCatchUp, err = repo.AddOrEnableWatchedOAB(ctx, tx, tenantID, integrationID, key)
		return err
	}); err != nil {
		t.Fatalf("AddOrEnableWatchedOAB (idempotent replay): %v", err)
	}
	if needsHistory || needsCatchUp {
		t.Fatalf("idempotent replay = {needsHistory:%v needsCatchUp:%v}, want {false false}", needsHistory, needsCatchUp)
	}

	// Disable it (the Termos toggle-off): DisableWatchedOAB stamps disabled_at.
	var disabled acquisition.WatchedOAB
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		disabled, err = repo.DisableWatchedOAB(ctx, tx, tenantID, integrationID, key)
		return err
	}); err != nil {
		t.Fatalf("DisableWatchedOAB: %v", err)
	}
	if disabled.Enabled {
		t.Fatalf("disabled row still enabled: %+v", disabled)
	}
	if disabled.DisabledAt == nil {
		t.Fatalf("disabled row has no disabled_at: %+v", disabled)
	}

	// A repeat disable is a no-op (0 rows updated) — the repo re-reads instead of
	// clobbering disabled_at, and reports the same state.
	var redisabled acquisition.WatchedOAB
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		redisabled, err = repo.DisableWatchedOAB(ctx, tx, tenantID, integrationID, key)
		return err
	}); err != nil {
		t.Fatalf("DisableWatchedOAB (repeat): %v", err)
	}
	if redisabled.Enabled || redisabled.DisabledAt == nil || !redisabled.DisabledAt.Equal(*disabled.DisabledAt) {
		t.Fatalf("repeat disable changed state: got %+v, want disabled_at unchanged from %+v", redisabled, disabled)
	}

	// Case C: re-enable a DISABLED key -> needsHistory=false, needsCatchUp=true, and
	// catch_up_since is COALESCEd from disabled_at.
	var reenabled acquisition.WatchedOAB
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		reenabled, needsHistory, needsCatchUp, err = repo.AddOrEnableWatchedOAB(ctx, tx, tenantID, integrationID, key)
		return err
	}); err != nil {
		t.Fatalf("AddOrEnableWatchedOAB (re-enable): %v", err)
	}
	if needsHistory || !needsCatchUp {
		t.Fatalf("re-enable = {needsHistory:%v needsCatchUp:%v}, want {false true}", needsHistory, needsCatchUp)
	}
	if !reenabled.Enabled {
		t.Fatalf("re-enabled row still disabled: %+v", reenabled)
	}
	if reenabled.CatchUpSince == nil || !reenabled.CatchUpSince.Equal(*disabled.DisabledAt) {
		t.Fatalf("catch_up_since = %v, want it COALESCEd from disabled_at %v", reenabled.CatchUpSince, disabled.DisabledAt)
	}

	// ClearWatchedOABCatchUp: a compare-and-clear with a WRONG since is a silent no-op
	// (the gap must survive a stale/late close); the CORRECT since clears it.
	wrongSince := reenabled.CatchUpSince.Add(-time.Hour)
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return repo.ClearWatchedOABCatchUp(ctx, tx, tenantID, integrationID, key, wrongSince)
	}); err != nil {
		t.Fatalf("ClearWatchedOABCatchUp (wrong since): %v", err)
	}
	var afterWrongClear acquisition.WatchedOAB
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		afterWrongClear, err = repo.GetWatchedOAB(ctx, tx, tenantID, integrationID, key)
		return err
	}); err != nil {
		t.Fatalf("GetWatchedOAB (after wrong clear): %v", err)
	}
	if afterWrongClear.CatchUpSince == nil {
		t.Fatalf("catch_up_since cleared by a MISMATCHED since — compare-and-clear broken")
	}

	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return repo.ClearWatchedOABCatchUp(ctx, tx, tenantID, integrationID, key, *reenabled.CatchUpSince)
	}); err != nil {
		t.Fatalf("ClearWatchedOABCatchUp (correct since): %v", err)
	}
	var afterClear acquisition.WatchedOAB
	if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		afterClear, err = repo.GetWatchedOAB(ctx, tx, tenantID, integrationID, key)
		return err
	}); err != nil {
		t.Fatalf("GetWatchedOAB (after clear): %v", err)
	}
	if afterClear.CatchUpSince != nil {
		t.Fatalf("catch_up_since still set after a matching compare-and-clear: %v", afterClear.CatchUpSince)
	}

	// GetWatchedOAB on a key that never existed is the typed 404.
	err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		_, err := repo.GetWatchedOAB(ctx, tx, tenantID, integrationID, "LIFECYCLE-NEVER-EXISTED|SP")
		return err
	})
	if !errors.Is(err, acquisition.ErrWatchedOABNotFound) {
		t.Fatalf("GetWatchedOAB on an unknown key: error = %v, want ErrWatchedOABNotFound", err)
	}
}
