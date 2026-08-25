package acquisition

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- pure functions ----------------------------------------------------------

// AC1: the slice count is ceil(horizon/window), with a non-positive horizon or
// window collapsing to zero.
func TestCalculateSlices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		horizon int
		window  int
		want    int
	}{
		{name: "one year in weeks rounds up", horizon: 365, window: 7, want: 53},
		{name: "zero horizon has no slices", horizon: 0, window: 7, want: 0},
		{name: "exactly one window", horizon: 7, window: 7, want: 1},
		{name: "short remainder still one window", horizon: 6, window: 7, want: 1},
		{name: "two windows", horizon: 14, window: 7, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := calculateSlices(tt.horizon, tt.window); got != tt.want {
				t.Errorf("calculateSlices(%d, %d) = %d, want %d", tt.horizon, tt.window, got, tt.want)
			}
		})
	}
}

// AC2: the 30-day lean-ingestion horizon in 7-day windows tiles into 5 consecutive
// slices with no gap or overlap; the first starts at `from` and the last ends exactly
// at `to` (30 = 4*7 + 2, so the last slice is the 2-day remainder).
func TestBuildSyncWindows_HorizonInWeeks(t *testing.T) {
	t.Parallel()

	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, BackfillHorizonDays)

	windows := buildSyncWindows(from, to, BackfillWindowDays)

	want := calculateSlices(BackfillHorizonDays, BackfillWindowDays)
	if len(windows) != want {
		t.Fatalf("len(windows) = %d, want %d", len(windows), want)
	}
	if !windows[0].From.Equal(from) {
		t.Errorf("windows[0].From = %s, want %s", windows[0].From, from)
	}
	if !windows[len(windows)-1].To.Equal(to) {
		t.Errorf("windows[last].To = %s, want %s", windows[len(windows)-1].To, to)
	}
	for i := 0; i < len(windows)-1; i++ {
		if !windows[i].To.Equal(windows[i+1].From) {
			t.Errorf("gap/overlap: windows[%d].To = %s, windows[%d].From = %s",
				i, windows[i].To, i+1, windows[i+1].From)
		}
		if !windows[i].From.Before(windows[i].To) {
			t.Errorf("windows[%d] not forward: From = %s, To = %s", i, windows[i].From, windows[i].To)
		}
	}
}

// buildSyncWindows edge cases: empty/short/inverted ranges.
func TestBuildSyncWindows_Edges(t *testing.T) {
	t.Parallel()

	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		from    time.Time
		to      time.Time
		window  int
		wantLen int
	}{
		{name: "empty range yields nothing", from: from, to: from, window: 7, wantLen: 0},
		{name: "inverted range yields nothing", from: from, to: from.AddDate(0, 0, -7), window: 7, wantLen: 0},
		{name: "non-positive window yields nothing", from: from, to: from.AddDate(0, 0, 30), window: 0, wantLen: 0},
		{name: "short range clamps to one window", from: from, to: from.AddDate(0, 0, 6), window: 7, wantLen: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildSyncWindows(tt.from, tt.to, tt.window)
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen == 1 && !got[0].To.Equal(tt.to) {
				t.Errorf("clamped window To = %s, want %s", got[0].To, tt.to)
			}
		})
	}
}

// --- use case test doubles ---------------------------------------------------

// stubTx is a database.Tx whose only exercised method is Exec (the dedup
// insert). rows is what its CommandTag reports affected: 1 = first sighting,
// 0 = duplicate. Query/QueryRow are never reached (repo and outbox are mocked).
type stubTx struct{ rows int64 }

func (s stubTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", s.rows)), nil
}
func (stubTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (stubTx) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// stubBackfillUoW runs fn with a fixed tx and records the tenant it was scoped to.
type stubBackfillUoW struct {
	tx       database.Tx
	tenantID string
	calls    int
}

func (u *stubBackfillUoW) Do(_ context.Context, tenantID string, fn func(database.Tx) error) error {
	u.calls++
	u.tenantID = tenantID
	return fn(u.tx)
}

func (u *stubBackfillUoW) DoSystem(_ context.Context, fn func(database.Tx) error) error {
	u.calls++
	return fn(u.tx)
}

// watchedOABState is one stubBackfillRepo-tracked watched_oab row: enabled +
// the catch_up_since a disable/re-enable cycle carries, mirroring the real
// UpsertWatchedOAB's COALESCE(catch_up_since, disabled_at) semantics closely
// enough to drive AddOrEnableWatchedOAB's needsHistory/needsCatchUp branches.
type watchedOABState struct {
	enabled      bool
	catchUpSince *time.Time
}

// stubBackfillRepo tracks a per-oabKey watched state and records the insert. For
// the completion-counter tests it also holds a mutable job state: each increment
// bumps a counter and returns the post-bump tallies (mirroring the real
// UPDATE ... RETURNING), or ErrBackfillJobNotFound when notFound is set.
type stubBackfillRepo struct {
	watched     map[string]*watchedOABState
	addCalls    int
	calledKeys  []string // oabKeys handed to AddOrEnableWatchedOAB, in call order
	insertCalls int
	lastInsert  BackfillJobParams

	clearCalls     int
	lastClearKey   string
	lastClearSince time.Time

	// counter path
	counters       BackfillCounters // current job tallies; increments mutate & return
	notFound       bool             // simulate an RLS/tenant miss on increment
	incOKCalls     int
	incErrorCalls  int
	finalizeCalls  int
	finalizeStatus string
}

// preEnable seeds oabKey as already watched and enabled — the "re-post of an
// existing OAB" starting state.
func (s *stubBackfillRepo) preEnable(oabKey string) {
	if s.watched == nil {
		s.watched = map[string]*watchedOABState{}
	}
	s.watched[oabKey] = &watchedOABState{enabled: true}
}

// preDisable seeds oabKey as watched but disabled, with catch_up_since already
// pending since `since` — the "toggled off, not yet re-enabled" starting state.
func (s *stubBackfillRepo) preDisable(oabKey string, since time.Time) {
	if s.watched == nil {
		s.watched = map[string]*watchedOABState{}
	}
	s.watched[oabKey] = &watchedOABState{enabled: false, catchUpSince: &since}
}

func (s *stubBackfillRepo) AddOrEnableWatchedOAB(_ context.Context, _ database.Tx, _, integrationID, oabKey string) (WatchedOAB, bool, bool, error) {
	s.addCalls++
	s.calledKeys = append(s.calledKeys, oabKey)
	if s.watched == nil {
		s.watched = map[string]*watchedOABState{}
	}
	st, existed := s.watched[oabKey]
	if !existed {
		s.watched[oabKey] = &watchedOABState{enabled: true}
		return WatchedOAB{OABKey: oabKey, IntegrationID: integrationID, Enabled: true}, true, false, nil
	}
	if !st.enabled {
		st.enabled = true
		row := WatchedOAB{OABKey: oabKey, IntegrationID: integrationID, Enabled: true, CatchUpSince: st.catchUpSince}
		return row, false, true, nil
	}
	return WatchedOAB{OABKey: oabKey, IntegrationID: integrationID, Enabled: true}, false, false, nil
}

func (s *stubBackfillRepo) ClearWatchedOABCatchUp(_ context.Context, _ database.Tx, _, _, oabKey string, since time.Time) error {
	s.clearCalls++
	s.lastClearKey = oabKey
	s.lastClearSince = since
	if st, ok := s.watched[oabKey]; ok && st.catchUpSince != nil && st.catchUpSince.Equal(since) {
		st.catchUpSince = nil
	}
	return nil
}

func (s *stubBackfillRepo) InsertBackfillJob(_ context.Context, _ database.Tx, p BackfillJobParams) (string, error) {
	s.insertCalls++
	s.lastInsert = p
	return "job-1", nil
}

func (s *stubBackfillRepo) IncrementBackfillSlicesOK(context.Context, database.Tx, string, string) (BackfillCounters, error) {
	s.incOKCalls++
	if s.notFound {
		return BackfillCounters{}, ErrBackfillJobNotFound
	}
	s.counters.SlicesOK++
	return s.counters, nil
}

func (s *stubBackfillRepo) IncrementBackfillSlicesError(context.Context, database.Tx, string, string) (BackfillCounters, error) {
	s.incErrorCalls++
	if s.notFound {
		return BackfillCounters{}, ErrBackfillJobNotFound
	}
	s.counters.SlicesError++
	return s.counters, nil
}

func (s *stubBackfillRepo) FinalizeBackfillJob(_ context.Context, _ database.Tx, _, _, status string) error {
	s.finalizeCalls++
	s.finalizeStatus = status
	s.counters.Status = status
	return nil
}

// withHorizon overrides the horizon/window (same-package test seam) to exercise
// the zero-slice edge deterministically.
func withHorizon(horizonDays, windowDays int) backfillOption {
	return func(uc *BackfillUseCase) {
		uc.horizonDays = horizonDays
		uc.windowDays = windowDays
	}
}

func activatedEvent() IntegrationActivated {
	return IntegrationActivated{
		Base:          events.Base{EventID: "evt-1", Aggregate: "integ-1"},
		IntegrationID: "integ-1",
		TenantID:      testTenant,
		Source:        SourceDJEN,
		Scope:         Scope{OAB: []string{"SP347019", "MG198988"}},
	}
}

type stubHistoryMatcher struct {
	calls  int
	tenant string
}

func (m *stubHistoryMatcher) MatchTenantSince(_ context.Context, tenantID string, _ time.Time) error {
	m.calls++
	m.tenant = tenantID
	return nil
}

// TestBackfillUseCase_CutoverMatchesHistory: with a history matcher wired (the bulk
// cutover), a fresh activation populates watched_oab and catches the tenant up from
// the store — and does NOT fire the per-OAB backfill (no job insert, no sync slices).
func TestBackfillUseCase_CutoverMatchesHistory(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	history := &stubHistoryMatcher{}
	// historyDays mirrors the production cutover knob (onboardingHistoryDays), now 30 to
	// match the lean 30-day backfill horizon. The value is irrelevant to this test's
	// assertions but is kept in sync so the test reflects the actual policy.
	uc := NewBackfillUseCase(repo, outbox, uow, WithHistoryMatcher(history, 30))

	if err := uc.OnIntegrationActivated(context.Background(), activatedEvent()); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}

	if repo.insertCalls != 0 {
		t.Errorf("backfill job inserts = %d, want 0 (cutover skips the per-OAB backfill)", repo.insertCalls)
	}
	if outbox.calls != 0 {
		t.Errorf("sync_requested emitted = %d, want 0 (no per-OAB slices)", outbox.calls)
	}
	if len(repo.calledKeys) != 2 {
		t.Errorf("watched OABs upserted = %v, want 2 (still populated on cutover)", repo.calledKeys)
	}
	if history.calls != 1 || history.tenant != testTenant {
		t.Errorf("history catch-up = {calls:%d tenant:%q}, want {1 %q}", history.calls, history.tenant, testTenant)
	}
}

// --- use case tests ----------------------------------------------------------

// First activation of an integration: one RUNNING job with the horizon's slice
// count of total slices and the same number of sync_requested events, all in a
// single unit of work scoped to the payload's tenant. With the lean 30/7 horizon
// that is 5 slices (indices 0..4).
func TestBackfillUseCase_FirstActivation(t *testing.T) {
	t.Parallel()

	wantSlices := calculateSlices(BackfillHorizonDays, BackfillWindowDays)

	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnIntegrationActivated(context.Background(), activatedEvent()); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}

	if uow.calls != 1 {
		t.Fatalf("unit of work runs = %d, want 1", uow.calls)
	}
	if uow.tenantID != testTenant {
		t.Fatalf("uow tenantID = %q, want %q", uow.tenantID, testTenant)
	}
	if repo.insertCalls != 1 {
		t.Fatalf("inserts = %d, want 1", repo.insertCalls)
	}
	// Activation populates the national-match index from the scope (normalized keys),
	// both brand-new (needsHistory) on a first activation.
	if got := repo.calledKeys; len(got) != 2 || got[0] != "347019|SP" || got[1] != "198988|MG" {
		t.Fatalf("watched OABs upserted = %v, want [347019|SP 198988|MG]", got)
	}
	if repo.lastInsert.TotalSlices != wantSlices || repo.lastInsert.Status != BackfillStatusRunning {
		t.Fatalf("job = {slices:%d status:%q}, want {%d RUNNING}", repo.lastInsert.TotalSlices, repo.lastInsert.Status, wantSlices)
	}
	if outbox.calls != wantSlices {
		t.Fatalf("published = %d, want %d", outbox.calls, wantSlices)
	}

	// Recent-first emission: slice_index stays chronological (0..N-1), but the NEWEST
	// window is published FIRST so its live-deadline intimações land in the first
	// parallel batch. So published[0] carries the highest index (N-1) and the last
	// published carries 0; every slice carries the job id and dated bounds.
	first, ok := outbox.published[0].(SyncRequested)
	if !ok {
		t.Fatalf("published[0] type = %T, want SyncRequested", outbox.published[0])
	}
	if first.SliceIndex != wantSlices-1 || first.BackfillJobID != "job-1" || first.Type() != TypeSyncRequested {
		t.Fatalf("first published slice = %+v, want newest (index %d) emitted first", first, wantSlices-1)
	}
	if first.Source != SourceDJEN {
		t.Fatalf("first slice source = %q, want %q (carried through from the activation)", first.Source, SourceDJEN)
	}
	if first.WindowFrom == "" || first.WindowTo == "" {
		t.Fatalf("first slice has empty window bounds: %+v", first)
	}
	last := outbox.published[wantSlices-1].(SyncRequested)
	if last.SliceIndex != 0 {
		t.Fatalf("last published slice index = %d, want 0 (oldest emitted last)", last.SliceIndex)
	}
}

// A duplicate delivery (SeenOrMark reports seen) is a no-op: the guard is never
// consulted, nothing is inserted or published, and the task acks without error.
func TestBackfillUseCase_DuplicateEventNoOps(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // 0 rows affected = already seen
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnIntegrationActivated(context.Background(), activatedEvent()); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}
	if repo.addCalls != 0 {
		t.Fatalf("watched_oab upserts on a seen event = %d, want 0", repo.addCalls)
	}
	if repo.insertCalls != 0 || outbox.calls != 0 {
		t.Fatalf("seen event caused writes: inserts=%d publishes=%d", repo.insertCalls, outbox.calls)
	}
}

// An identical re-post of an already-fully-watched scope (every OAB already enabled)
// is a no-op: the event is marked processed but no job or slice is produced. This is
// the idempotent-replay case AddOrEnableWatchedOAB's was_enabled=true branch reports.
func TestBackfillUseCase_IdempotentRepostNoOps(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{}
	repo.preEnable("347019|SP")
	repo.preEnable("198988|MG")
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnIntegrationActivated(context.Background(), activatedEvent()); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}
	if repo.addCalls != 2 {
		t.Fatalf("watched_oab upserts consulted = %d, want 2", repo.addCalls)
	}
	if repo.insertCalls != 0 || outbox.calls != 0 {
		t.Fatalf("idempotent re-post caused writes: inserts=%d publishes=%d", repo.insertCalls, outbox.calls)
	}
}

// THE BUG FIX (acceptance criterion): integration_activated re-delivered with a scope
// that adds a NEW OAB to an already-active integration must backfill ONLY that new
// OAB's history — not the whole scope again (duplicate work), and not nothing (the
// original bug: only the very first activation ever fired the backfill). SP347019 is
// already watched+enabled; MG198988 is new.
func TestBackfillUseCase_NewOABAddedToActiveIntegration_BackfillsOnlyDelta(t *testing.T) {
	t.Parallel()

	wantSlices := calculateSlices(BackfillHorizonDays, BackfillWindowDays)

	repo := &stubBackfillRepo{}
	repo.preEnable("347019|SP") // already watched — must NOT be re-scanned
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnIntegrationActivated(context.Background(), activatedEvent()); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}

	if repo.addCalls != 2 {
		t.Fatalf("watched_oab upserts = %d, want 2 (both OABs in the scope)", repo.addCalls)
	}
	if repo.insertCalls != 1 {
		t.Fatalf("backfill job inserts = %d, want 1 (one delta job for the new OAB)", repo.insertCalls)
	}
	if repo.lastInsert.TriggerReason != TriggerReasonOABAdded {
		t.Fatalf("job trigger_reason = %q, want %q", repo.lastInsert.TriggerReason, TriggerReasonOABAdded)
	}
	if len(repo.lastInsert.TriggerOABs) != 1 || repo.lastInsert.TriggerOABs[0] != "MG198988" {
		t.Fatalf("job trigger_oabs = %v, want exactly [MG198988] (the delta), not the pre-existing SP347019", repo.lastInsert.TriggerOABs)
	}
	if outbox.calls != wantSlices {
		t.Fatalf("published = %d, want %d (one delta backfill's slices)", outbox.calls, wantSlices)
	}
	for _, ev := range outbox.published {
		req, ok := ev.(SyncRequested)
		if !ok {
			t.Fatalf("published event type = %T, want SyncRequested", ev)
		}
		if len(req.Scope.OAB) != 1 || req.Scope.OAB[0] != "MG198988" {
			t.Fatalf("slice scope = %v, want exactly [MG198988] (the delta) — SP347019 must not be re-scanned", req.Scope.OAB)
		}
	}
}

// Edge: a zero-slice horizon creates a COMPLETED job and emits no slices.
func TestBackfillUseCase_ZeroSlicesCompletesImmediately(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow, withHorizon(0, BackfillWindowDays))

	if err := uc.OnIntegrationActivated(context.Background(), activatedEvent()); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}
	if repo.insertCalls != 1 {
		t.Fatalf("inserts = %d, want 1", repo.insertCalls)
	}
	if repo.lastInsert.TotalSlices != 0 || repo.lastInsert.Status != BackfillStatusCompleted {
		t.Fatalf("job = {slices:%d status:%q}, want {0 COMPLETED}", repo.lastInsert.TotalSlices, repo.lastInsert.Status)
	}
	if outbox.calls != 0 {
		t.Fatalf("published = %d, want 0 (nothing to sync)", outbox.calls)
	}
}

// --- completion counter (sync_completed / sync_failed) -----------------------

func syncCompletedEvent(backfillJobID string) SyncCompleted {
	return SyncCompleted{
		Base:          events.Base{EventID: "evt-sc-1", Aggregate: "run-1"},
		TenantID:      testTenant,
		SyncRunID:     "run-1",
		IntegrationID: "integ-1",
		BackfillJobID: backfillJobID,
		SliceIndex:    2,
	}
}

func syncFailedEvent(backfillJobID string) SyncFailed {
	return SyncFailed{
		Base:          events.Base{EventID: "evt-sf-1", Aggregate: "run-1"},
		TenantID:      testTenant,
		SyncRunID:     "run-1",
		IntegrationID: "integ-1",
		BackfillJobID: backfillJobID,
		SliceIndex:    2,
		Reason:        "fetch timeout",
	}
}

// D3: a sync_completed with no backfill job (a standalone re-sync) is acked
// before any transaction opens — no unit of work, no increment, no publish.
func TestBackfillUseCase_SyncCompleted_NoBackfillJobNoOps(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncCompleted(context.Background(), syncCompletedEvent("")); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v", err)
	}
	if uow.calls != 0 {
		t.Fatalf("unit of work runs = %d, want 0 (standalone sync)", uow.calls)
	}
	if repo.incOKCalls != 0 || outbox.calls != 0 {
		t.Fatalf("standalone sync caused work: increments=%d publishes=%d", repo.incOKCalls, outbox.calls)
	}
}

// D4: a non-final successful slice increments slices_ok and does NOT finalize —
// no FinalizeBackfillJob, no backfill_finished.
func TestBackfillUseCase_SyncCompleted_NotLastSlice(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{counters: BackfillCounters{TotalSlices: 3, Status: BackfillStatusRunning}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncCompleted(context.Background(), syncCompletedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v", err)
	}
	if repo.incOKCalls != 1 {
		t.Fatalf("slices_ok increments = %d, want 1", repo.incOKCalls)
	}
	if repo.finalizeCalls != 0 || outbox.calls != 0 {
		t.Fatalf("non-final slice finalized: finalize=%d publishes=%d", repo.finalizeCalls, outbox.calls)
	}
}

// D5: a failed slice increments slices_error (not slices_ok).
func TestBackfillUseCase_SyncFailed_IncrementsError(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{counters: BackfillCounters{TotalSlices: 3, Status: BackfillStatusRunning}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncFailed(context.Background(), syncFailedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncFailed() error = %v", err)
	}
	if repo.incErrorCalls != 1 || repo.incOKCalls != 0 {
		t.Fatalf("increments = {ok:%d error:%d}, want {0 1}", repo.incOKCalls, repo.incErrorCalls)
	}
	if repo.finalizeCalls != 0 || outbox.calls != 0 {
		t.Fatalf("non-final slice finalized: finalize=%d publishes=%d", repo.finalizeCalls, outbox.calls)
	}
}

// D6: the last slice with zero errors finalizes COMPLETED and emits exactly one
// backfill_finished carrying the final tally.
func TestBackfillUseCase_SyncCompleted_LastSliceCompletes(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{counters: BackfillCounters{TotalSlices: 3, SlicesOK: 2, Status: BackfillStatusRunning}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncCompleted(context.Background(), syncCompletedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v", err)
	}
	if repo.finalizeCalls != 1 || repo.finalizeStatus != BackfillStatusCompleted {
		t.Fatalf("finalize = {calls:%d status:%q}, want {1 COMPLETED}", repo.finalizeCalls, repo.finalizeStatus)
	}
	// Finalize now emits exactly ONE event: backfill_finished. The ENRICHMENT capture row's
	// fecho is owned by the batch enrichment job (which closes it when its scan drains), so no
	// scheduled close is emitted here anymore.
	if outbox.calls != 1 {
		t.Fatalf("published = %d, want 1 (backfill_finished only)", outbox.calls)
	}
	fin, ok := findPublished[BackfillFinished](outbox.published)
	if !ok {
		t.Fatalf("no BackfillFinished published; got %v", outbox.published)
	}
	if fin.Status != BackfillStatusCompleted || fin.Type() != TypeBackfillFinished {
		t.Fatalf("finished event = {status:%q type:%q}, want {COMPLETED %s}", fin.Status, fin.Type(), TypeBackfillFinished)
	}
	if fin.TotalSlices != 3 || fin.SlicesOK != 3 || fin.SlicesError != 0 {
		t.Fatalf("finished tally = {total:%d ok:%d err:%d}, want {3 3 0}", fin.TotalSlices, fin.SlicesOK, fin.SlicesError)
	}
	if fin.BackfillJobID != "job-1" || fin.TenantID != testTenant || fin.IntegrationID != "integ-1" {
		t.Fatalf("finished ids = %+v, unexpected", fin)
	}
	if fin.AggregateID() != "job-1" {
		t.Fatalf("aggregate id = %q, want the backfill_job id", fin.AggregateID())
	}
}

// D7: the last slice when at least one failed finalizes PARTIAL.
func TestBackfillUseCase_SyncFailed_LastSlicePartial(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{counters: BackfillCounters{TotalSlices: 3, SlicesOK: 2, Status: BackfillStatusRunning}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncFailed(context.Background(), syncFailedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncFailed() error = %v", err)
	}
	if repo.finalizeCalls != 1 || repo.finalizeStatus != BackfillStatusPartial {
		t.Fatalf("finalize = {calls:%d status:%q}, want {1 PARTIAL}", repo.finalizeCalls, repo.finalizeStatus)
	}
	if outbox.calls != 1 {
		t.Fatalf("published = %d, want 1 (backfill_finished only)", outbox.calls)
	}
	fin, ok := findPublished[BackfillFinished](outbox.published)
	if !ok {
		t.Fatalf("no BackfillFinished published; got %v", outbox.published)
	}
	if fin.Status != BackfillStatusPartial || fin.SlicesError != 1 || fin.SlicesOK != 2 {
		t.Fatalf("finished event = {status:%q ok:%d err:%d}, want {PARTIAL 2 1}", fin.Status, fin.SlicesOK, fin.SlicesError)
	}
}

// D8: a duplicate delivery (SeenOrMark reports seen) is a no-op — no increment,
// no finalize, no publish, and the task acks.
func TestBackfillUseCase_SyncCompleted_DuplicateNoOps(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{counters: BackfillCounters{TotalSlices: 3, SlicesOK: 2, Status: BackfillStatusRunning}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // 0 rows affected = already seen
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncCompleted(context.Background(), syncCompletedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v", err)
	}
	if repo.incOKCalls != 0 || repo.finalizeCalls != 0 || outbox.calls != 0 {
		t.Fatalf("seen event caused work: increments=%d finalize=%d publishes=%d",
			repo.incOKCalls, repo.finalizeCalls, outbox.calls)
	}
}

// D9: finalize is idempotent — if the increment reads back a non-RUNNING status
// (the job was already finalized), the counter does NOT re-finalize or re-emit,
// even when the tally has reached the total.
func TestBackfillUseCase_SyncCompleted_AlreadyFinalizedNoReEmit(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{counters: BackfillCounters{TotalSlices: 3, SlicesOK: 2, Status: BackfillStatusCompleted}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncCompleted(context.Background(), syncCompletedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v", err)
	}
	if repo.finalizeCalls != 0 || outbox.calls != 0 {
		t.Fatalf("re-finalized an already-finalized job: finalize=%d publishes=%d", repo.finalizeCalls, outbox.calls)
	}
}

// D9b: a vanished/invisible job (ErrBackfillJobNotFound from the increment) is a
// no-op ack — no finalize, no publish, no error.
func TestBackfillUseCase_SyncCompleted_JobNotFoundNoOps(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{notFound: true}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncCompleted(context.Background(), syncCompletedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v, want nil (no-op ack)", err)
	}
	if repo.finalizeCalls != 0 || outbox.calls != 0 {
		t.Fatalf("not-found job caused work: finalize=%d publishes=%d", repo.finalizeCalls, outbox.calls)
	}
}

// D10: the unit of work is scoped to the event's tenant (RLS barrier 2).
func TestBackfillUseCase_SyncCompleted_UoWScopedToTenant(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{counters: BackfillCounters{TotalSlices: 3, Status: BackfillStatusRunning}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnSyncCompleted(context.Background(), syncCompletedEvent("job-1")); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v", err)
	}
	if uow.calls != 1 {
		t.Fatalf("unit of work runs = %d, want 1", uow.calls)
	}
	if uow.tenantID != testTenant {
		t.Fatalf("uow tenantID = %q, want %q", uow.tenantID, testTenant)
	}
}

// --- watched_oab re-enable catch-up (OnWatchedOABReenabled) -------------------

func reenabledEvent(since time.Time) WatchedOABReenabled {
	return WatchedOABReenabled{
		Base:          events.Base{EventID: "evt-reenable-1"},
		TenantID:      testTenant,
		IntegrationID: "integ-1",
		Source:        SourceDJEN,
		OABKey:        "347019|SP",
		CatchUpSince:  since,
	}
}

// Legacy path (no history matcher): a re-enable fires exactly one standalone
// catch-up sync scoped to [since, today] for just that OAB — no backfill_job.
func TestBackfillUseCase_OnWatchedOABReenabled_Legacy_FiresCatchUpSync(t *testing.T) {
	t.Parallel()

	since := time.Now().AddDate(0, 0, -3)
	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnWatchedOABReenabled(context.Background(), reenabledEvent(since)); err != nil {
		t.Fatalf("OnWatchedOABReenabled() error = %v", err)
	}
	if repo.insertCalls != 0 {
		t.Fatalf("backfill job inserts = %d, want 0 (catch-up is standalone)", repo.insertCalls)
	}
	if outbox.calls != 1 {
		t.Fatalf("published = %d, want 1", outbox.calls)
	}
	req, ok := outbox.published[0].(SyncRequested)
	if !ok {
		t.Fatalf("published type = %T, want SyncRequested", outbox.published[0])
	}
	if req.BackfillJobID != "" {
		t.Fatalf("catch-up sync backfill_job_id = %q, want empty (standalone)", req.BackfillJobID)
	}
	if req.CatchUpOABKey != "347019|SP" || req.CatchUpSince != since.Format(time.RFC3339Nano) {
		t.Fatalf("catch-up identity = {key:%q since:%q}, want {347019|SP %q}", req.CatchUpOABKey, req.CatchUpSince, since.Format(time.RFC3339Nano))
	}
	if len(req.Scope.OAB) != 1 || req.Scope.OAB[0] != "SP347019" {
		t.Fatalf("catch-up scope = %v, want exactly [SP347019]", req.Scope.OAB)
	}
}

// Cutover path (history matcher wired): a re-enable matches the tenant from the
// stored firehose since the OAB's disabled_at — no per-OAB DJEN call.
func TestBackfillUseCase_OnWatchedOABReenabled_Cutover_MatchesHistory(t *testing.T) {
	t.Parallel()

	since := time.Now().AddDate(0, 0, -3)
	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	history := &stubHistoryMatcher{}
	uc := NewBackfillUseCase(repo, outbox, uow, WithHistoryMatcher(history, 30))

	if err := uc.OnWatchedOABReenabled(context.Background(), reenabledEvent(since)); err != nil {
		t.Fatalf("OnWatchedOABReenabled() error = %v", err)
	}
	if outbox.calls != 0 {
		t.Fatalf("published = %d, want 0 (cutover skips the per-OAB sync)", outbox.calls)
	}
	if history.calls != 1 || history.tenant != testTenant {
		t.Fatalf("history catch-up = {calls:%d tenant:%q}, want {1 %q}", history.calls, history.tenant, testTenant)
	}
}

// A duplicate delivery is a no-op under its OWN consumer dedup space (distinct from
// consumerBackfill) — no publish, no error.
func TestBackfillUseCase_OnWatchedOABReenabled_DuplicateNoOps(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // 0 rows affected = already seen
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnWatchedOABReenabled(context.Background(), reenabledEvent(time.Now())); err != nil {
		t.Fatalf("OnWatchedOABReenabled() error = %v", err)
	}
	if outbox.calls != 0 {
		t.Fatalf("seen event caused a publish: %d", outbox.calls)
	}
}

// --- catch-up clearing on the closing sync (OnSyncCompleted/OnSyncFailed) -----

// A SUCCESSFUL catch-up sync (CatchUpOABKey set, no backfill job) clears the OAB's
// pending catch_up_since via the compare-and-clear.
func TestBackfillUseCase_SyncCompleted_ClearsCatchUp(t *testing.T) {
	t.Parallel()

	// RFC3339Nano round-trips full precision (unlike plain RFC3339, which truncates
	// to whole seconds and used to make the compare-and-clear below silently never
	// match a real Postgres timestamptz — see newCatchUpSyncRequested).
	since := time.Now().AddDate(0, 0, -3)
	repo := &stubBackfillRepo{}
	repo.preDisable("347019|SP", since)
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	ev := SyncCompleted{
		Base:          events.Base{EventID: "evt-catchup-1"},
		TenantID:      testTenant,
		SyncRunID:     "run-catchup-1",
		IntegrationID: "integ-1",
		CatchUpOABKey: "347019|SP",
		CatchUpSince:  since.Format(time.RFC3339Nano),
	}
	if err := uc.OnSyncCompleted(context.Background(), ev); err != nil {
		t.Fatalf("OnSyncCompleted() error = %v", err)
	}
	if repo.clearCalls != 1 {
		t.Fatalf("catch-up clears = %d, want 1", repo.clearCalls)
	}
	if repo.lastClearKey != "347019|SP" || !repo.lastClearSince.Equal(since) {
		t.Fatalf("clear args = {key:%q since:%s}, want {347019|SP %s}", repo.lastClearKey, repo.lastClearSince, since)
	}
	// No backfill job attached — the counter path must stay untouched.
	if repo.incOKCalls != 0 || repo.finalizeCalls != 0 {
		t.Fatalf("counter path touched by a job-less catch-up: inc=%d finalize=%d", repo.incOKCalls, repo.finalizeCalls)
	}
}

// A FAILED catch-up sync deliberately does NOT clear catch_up_since — the gap stays
// marked so the next toggle retries it.
func TestBackfillUseCase_SyncFailed_DoesNotClearCatchUp(t *testing.T) {
	t.Parallel()

	since := time.Now().AddDate(0, 0, -3)
	repo := &stubBackfillRepo{}
	repo.preDisable("347019|SP", since)
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	ev := SyncFailed{
		Base:          events.Base{EventID: "evt-catchup-fail-1"},
		TenantID:      testTenant,
		SyncRunID:     "run-catchup-2",
		IntegrationID: "integ-1",
		CatchUpOABKey: "347019|SP",
		CatchUpSince:  since.Format(time.RFC3339Nano),
		Reason:        "fetch timeout",
	}
	if err := uc.OnSyncFailed(context.Background(), ev); err != nil {
		t.Fatalf("OnSyncFailed() error = %v", err)
	}
	if repo.clearCalls != 0 {
		t.Fatalf("catch-up clears on a FAILED sync = %d, want 0", repo.clearCalls)
	}
}

// catchUpTriggerReason: the sync_run trigger label is OAB_REENABLED only when the
// event actually carries a catch-up OAB — an ordinary sync (daily/backfill-windowed/
// scheduler) has no single OAB to attribute and must stay "".
func TestCatchUpTriggerReason(t *testing.T) {
	t.Parallel()
	if got := catchUpTriggerReason("347019|SP"); got != TriggerReasonOABReenabled {
		t.Fatalf("catchUpTriggerReason(oab) = %q, want %q", got, TriggerReasonOABReenabled)
	}
	if got := catchUpTriggerReason(""); got != "" {
		t.Fatalf("catchUpTriggerReason(\"\") = %q, want \"\"", got)
	}
}
