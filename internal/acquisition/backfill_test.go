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

// AC2: a 365-day horizon in 7-day windows tiles into 53 consecutive slices with
// no gap or overlap; the first starts at `from` and the last ends exactly at `to`.
func TestBuildSyncWindows_YearInWeeks(t *testing.T) {
	t.Parallel()

	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, BackfillHorizonDays)

	windows := buildSyncWindows(from, to, BackfillWindowDays)

	if len(windows) != 53 {
		t.Fatalf("len(windows) = %d, want 53", len(windows))
	}
	if !windows[0].From.Equal(from) {
		t.Errorf("windows[0].From = %s, want %s", windows[0].From, from)
	}
	if !windows[52].To.Equal(to) {
		t.Errorf("windows[52].To = %s, want %s", windows[52].To, to)
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

// stubBackfillRepo answers the guard from a preset flag and records the insert.
// For the completion-counter tests it also holds a mutable job state: each
// increment bumps a counter and returns the post-bump tallies (mirroring the
// real UPDATE ... RETURNING), or ErrBackfillJobNotFound when notFound is set.
type stubBackfillRepo struct {
	exists      bool
	existsCalls int
	insertCalls int
	lastInsert  BackfillJobParams

	// counter path
	counters       BackfillCounters // current job tallies; increments mutate & return
	notFound       bool             // simulate an RLS/tenant miss on increment
	incOKCalls     int
	incErrorCalls  int
	finalizeCalls  int
	finalizeStatus string
}

func (s *stubBackfillRepo) BackfillJobExistsByIntegration(context.Context, database.Tx, string) (bool, error) {
	s.existsCalls++
	return s.exists, nil
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
	}
}

// --- use case tests ----------------------------------------------------------

// First activation of an integration: one RUNNING job with 53 total slices and
// 53 sync_requested events, all in a single unit of work scoped to the payload's
// tenant.
func TestBackfillUseCase_FirstActivation(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{exists: false}
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
	if repo.lastInsert.TotalSlices != 53 || repo.lastInsert.Status != BackfillStatusRunning {
		t.Fatalf("job = {slices:%d status:%q}, want {53 RUNNING}", repo.lastInsert.TotalSlices, repo.lastInsert.Status)
	}
	if outbox.calls != 53 {
		t.Fatalf("published = %d, want 53", outbox.calls)
	}

	// The slices are indexed 0..52 and carry the job id and dated bounds.
	first, ok := outbox.published[0].(SyncRequested)
	if !ok {
		t.Fatalf("published[0] type = %T, want SyncRequested", outbox.published[0])
	}
	if first.SliceIndex != 0 || first.BackfillJobID != "job-1" || first.Type() != TypeSyncRequested {
		t.Fatalf("first slice = %+v, unexpected", first)
	}
	if first.Source != SourceDJEN {
		t.Fatalf("first slice source = %q, want %q (carried through from the activation)", first.Source, SourceDJEN)
	}
	if first.WindowFrom == "" || first.WindowTo == "" {
		t.Fatalf("first slice has empty window bounds: %+v", first)
	}
	last := outbox.published[52].(SyncRequested)
	if last.SliceIndex != 52 {
		t.Fatalf("last slice index = %d, want 52", last.SliceIndex)
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
	if repo.existsCalls != 0 {
		t.Fatalf("guard consulted %d times on a seen event, want 0", repo.existsCalls)
	}
	if repo.insertCalls != 0 || outbox.calls != 0 {
		t.Fatalf("seen event caused writes: inserts=%d publishes=%d", repo.insertCalls, outbox.calls)
	}
}

// A re-activation (a job already exists for the integration) is guarded: the
// event is marked processed but no second job or slice is produced.
func TestBackfillUseCase_ReactivationGuarded(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{exists: true}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	uc := NewBackfillUseCase(repo, outbox, uow)

	if err := uc.OnIntegrationActivated(context.Background(), activatedEvent()); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}
	if repo.existsCalls != 1 {
		t.Fatalf("guard consulted %d times, want 1", repo.existsCalls)
	}
	if repo.insertCalls != 0 || outbox.calls != 0 {
		t.Fatalf("guarded re-activation caused writes: inserts=%d publishes=%d", repo.insertCalls, outbox.calls)
	}
}

// Edge: a zero-slice horizon creates a COMPLETED job and emits no slices.
func TestBackfillUseCase_ZeroSlicesCompletesImmediately(t *testing.T) {
	t.Parallel()

	repo := &stubBackfillRepo{exists: false}
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
	if outbox.calls != 1 {
		t.Fatalf("published = %d, want 1 backfill_finished", outbox.calls)
	}
	fin, ok := outbox.published[0].(BackfillFinished)
	if !ok {
		t.Fatalf("published[0] type = %T, want BackfillFinished", outbox.published[0])
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
		t.Fatalf("published = %d, want 1 backfill_finished", outbox.calls)
	}
	fin := outbox.published[0].(BackfillFinished)
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
