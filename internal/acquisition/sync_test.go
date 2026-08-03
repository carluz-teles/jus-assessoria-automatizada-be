package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- sync use case test doubles ----------------------------------------------
//
// These reuse stubTx and fakeOutbox from backfill_test.go (same package): stubTx
// drives SeenOrMark's rows-affected (1 = first sighting, 0 = duplicate) and
// stubBackfillUoW runs fn per Do while recording the tenant it was scoped to. The
// sync-specific doubles below are the narrow syncRepo and the connector/parser
// ports.

// stubConnector is the connector port under the use case's control: it returns a
// preset payload, or fetchErr to exercise the fetch-failure path. id defaults to
// "test-conn"; set it to prove the use case ran under a specific connector (the id
// is stamped on the sync_run).
type stubConnector struct {
	id         string
	payload    RawPayload
	fetchErr   error
	fetchCalls int
}

func (c *stubConnector) ID() string {
	if c.id == "" {
		return "test-conn"
	}
	return c.id
}
func (c *stubConnector) Version() string            { return "test-v1" }
func (c *stubConnector) Capabilities() []Capability { return []Capability{CapabilityDiscoverByOAB} }

func (c *stubConnector) Fetch(_ context.Context, _ FetchRequest) (RawPayload, error) {
	c.fetchCalls++
	if c.fetchErr != nil {
		return RawPayload{}, c.fetchErr
	}
	return c.payload, nil
}

// stubParser is the parser port: it returns a preset ParsedResult, or parseErr to
// exercise the parse-failure path.
type stubParser struct {
	result     ParsedResult
	parseErr   error
	parseCalls int
}

func (p *stubParser) CanParse(RawPayload) bool { return true }

func (p *stubParser) Parse(context.Context, RawPayload) (ParsedResult, error) {
	p.parseCalls++
	if p.parseErr != nil {
		return ParsedResult{}, p.parseErr
	}
	return p.result, nil
}

// stubSyncRepo records every sync-cycle call and answers from preset knobs.
// docketNewCount is how many of the upserted docket entries it reports as new
// (the rest are deduped by the unique constraint); intimNew is the analog for
// intimations.
type stubSyncRepo struct {
	syncRunID string

	insertCalls int
	lastInsert  SyncRunParams

	// FindSyncRunByEventID knobs: findByEventRun is returned when set, else
	// findByEventErr (default ErrSyncRunNotFound is what a real miss looks like).
	findByEventRun   *SyncRun
	findByEventErr   error
	findByEventCalls int

	findCalls  int
	findParams []FindOrCreateCourtRecordParams

	// Entitlement gating knobs (fatia 5-A). existingKeys are treated as a HIT
	// (reobservation) — returned regardless of the limit, mirroring the real repo's
	// err==nil branch. Every other record is a MISS, gated against
	// params.ActiveProcessLimit using activeCount as the tenant's simulated ACTIVE
	// tally: count >= limit ⇒ ErrProcessLimitReached, else the record is created and
	// activeCount bumped — exactly the repo's MISS-branch count-and-compare.
	activeCount  int
	existingKeys map[string]bool

	docketCalls    int
	docketParams   []DocketEntryParams
	docketNewCount int

	intimCalls  int
	intimParams []IntimationParams
	intimNew    int

	updates []SyncRunOutcome
	// closedRuns models the sync_run's status=RUNNING compare-and-swap: the first
	// close of a given run id wins (closed=true), any later close of the SAME id
	// finds it already closed (closed=false) — exactly what the real UPDATE ...
	// WHERE status='RUNNING' RETURNING id reports under a concurrent redelivery.
	closedRuns map[string]bool
}

func (s *stubSyncRepo) InsertSyncRun(_ context.Context, _ database.Tx, p SyncRunParams) (string, error) {
	s.insertCalls++
	s.lastInsert = p
	return s.syncRunID, nil
}

func (s *stubSyncRepo) FindSyncRunByEventID(_ context.Context, _ database.Tx, _ string) (*SyncRun, error) {
	s.findByEventCalls++
	if s.findByEventErr != nil {
		return nil, s.findByEventErr
	}
	if s.findByEventRun != nil {
		return s.findByEventRun, nil
	}
	return nil, ErrSyncRunNotFound
}

func (s *stubSyncRepo) UpdateSyncRun(_ context.Context, _ database.Tx, o SyncRunOutcome) (bool, error) {
	s.updates = append(s.updates, o)
	if s.closedRuns == nil {
		s.closedRuns = map[string]bool{}
	}
	if s.closedRuns[o.ID] {
		return false, nil // already closed (CAS misses): a concurrent execution won.
	}
	s.closedRuns[o.ID] = true
	return true, nil
}

func (s *stubSyncRepo) FindOrCreateCourtRecord(_ context.Context, _ database.Tx, p FindOrCreateCourtRecordParams) (*CourtRecord, error) {
	s.findCalls++
	s.findParams = append(s.findParams, p)

	if s.existingKeys[recordKey(p.CNJNumber, p.Degree)] {
		return stubCourtRecord(p), nil // HIT — reobservation, never gated.
	}
	if s.activeCount >= p.ActiveProcessLimit {
		return nil, ErrProcessLimitReached // MISS at/over the ceiling — gated.
	}
	s.activeCount++
	return stubCourtRecord(p), nil
}

// stubCourtRecord builds the CourtRecord the stub returns for a resolved record,
// keyed deterministically off the natural key so tests can predict ids.
func stubCourtRecord(p FindOrCreateCourtRecordParams) *CourtRecord {
	return &CourtRecord{
		ID:        "rec-" + p.CNJNumber + "-" + p.Degree,
		TenantID:  p.TenantID,
		CaseID:    "case-" + p.CNJNumber + "-" + p.Degree,
		CNJNumber: p.CNJNumber,
		Degree:    p.Degree,
		Court:     p.Court,
	}
}

func (s *stubSyncRepo) UpsertDocketEntries(_ context.Context, _ database.Tx, params []DocketEntryParams) ([]DocketEntry, error) {
	s.docketCalls++
	s.docketParams = params

	n := s.docketNewCount
	if n > len(params) {
		n = len(params)
	}
	out := make([]DocketEntry, 0, n)
	for i := range n {
		p := params[i]
		out = append(out, DocketEntry{
			ID:            "docket-" + p.Hash,
			CourtRecordID: p.CourtRecordID,
			Hash:          p.Hash,
			OccurredAt:    p.OccurredAt,
			ObservedAt:    p.ObservedAt,
			Source:        p.Source,
			Fidelity:      p.Fidelity,
			Text:          p.Text,
		})
	}
	return out, nil
}

func (s *stubSyncRepo) UpsertIntimations(_ context.Context, _ database.Tx, params []IntimationParams) (int, error) {
	s.intimCalls++
	s.intimParams = params
	return s.intimNew, nil
}

// syncRequestedEvent builds a valid sync_requested event for the default tenant,
// sourced from DJEN (so orchestratorWith(SourceDJEN, …) resolves its connector).
func syncRequestedEvent() SyncRequested {
	return SyncRequested{
		Base:          events.Base{EventID: "sync-evt-1", Aggregate: "job-1"},
		BackfillJobID: "job-1",
		TenantID:      testTenant,
		IntegrationID: "integ-1",
		Source:        SourceDJEN,
		SliceIndex:    0,
		WindowFrom:    "2024-01-01",
		WindowTo:      "2024-01-08",
	}
}

// orchestratorWith registers a single connector under source — the minimal
// orchestrator the sync use case needs so ConnectorFor(source) resolves it.
func orchestratorWith(source string, c Connector) *Orchestrator {
	o := NewOrchestrator()
	o.Register(source, c)
	return o
}

// countByType tallies published events by their dotted type id.
func countByType(published []events.Event) map[string]int {
	counts := map[string]int{}
	for _, ev := range published {
		counts[ev.Type()]++
	}
	return counts
}

// stubEntitlementChecker is the EntitlementChecker under the test's control: it
// answers limit (or err) for every tenant and records how it was called so a test
// proves the cycle consults it exactly once, scoped to the event's tenant.
type stubEntitlementChecker struct {
	limit      int
	err        error
	calls      int
	lastTenant string
}

func (c *stubEntitlementChecker) ActiveProcessLimit(_ context.Context, tenantID string) (int, error) {
	c.calls++
	c.lastTenant = tenantID
	if c.err != nil {
		return 0, c.err
	}
	return c.limit, nil
}

// twoRecordFixture is a parsed result naming TWO distinct court records (A and B),
// each with its own docket entry and intimation. It lets a gating test create one
// record and block the other, then assert the blocked record's children are dropped
// while the created one's are still folded in.
func twoRecordFixture(source string) ParsedResult {
	const cnjA = "0000001-11.2024.8.26.0100"
	const cnjB = "0000002-22.2024.8.26.0100"
	occurred := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	observed := time.Date(2024, 1, 11, 8, 0, 0, 0, time.UTC)
	made := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)

	record := func(cnj string) ParsedCourtRecord {
		return ParsedCourtRecord{CNJNumber: cnj, Degree: DegreeG1, Court: "TJSP", Completeness: 0.5}
	}
	docket := func(cnj, hash string) ParsedDocketEntry {
		return ParsedDocketEntry{
			CNJNumber:  cnj,
			Degree:     DegreeG1,
			Hash:       hash,
			OccurredAt: occurred,
			ObservedAt: observed,
			Source:     source,
			Fidelity:   100,
			Text:       "andamento",
		}
	}
	intim := func(cnj, hash string) ParsedIntimation {
		return ParsedIntimation{
			CNJNumber:       cnj,
			Degree:          DegreeG1,
			Hash:            hash,
			MadeAvailableAt: made,
			PublishedAt:     made,
			DeadlineStartAt: made,
			Content:         "intimação",
			Source:          source,
		}
	}

	return ParsedResult{
		CourtRecords:  []ParsedCourtRecord{record(cnjA), record(cnjB)},
		DocketEntries: []ParsedDocketEntry{docket(cnjA, "docket-A"), docket(cnjB, "docket-B")},
		Intimations:   []ParsedIntimation{intim(cnjA, "notif-A"), intim(cnjB, "notif-B")},
	}
}

// --- use case tests ----------------------------------------------------------

// U3: the first delivery runs the full cycle — sync_run RUNNING→OK, one
// find-or-create, both docket entries upserted, one intimation upserted, and
// the outbox carries court_record_observed×1, docket_entry_observed×2,
// sync_completed×1. The whole cycle is scoped to the event's tenant.
func TestSyncUseCase_FirstDelivery_RunsFullCycle(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 2}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	ev := syncRequestedEvent()
	if err := uc.OnSyncRequested(context.Background(), ev); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	if repo.insertCalls != 1 || repo.lastInsert.Status != SyncStatusRunning {
		t.Fatalf("sync_run insert = {calls:%d status:%q}, want {1 RUNNING}", repo.insertCalls, repo.lastInsert.Status)
	}
	if repo.lastInsert.ConnectorID != conn.ID() || repo.lastInsert.ConnectorVersion != conn.Version() {
		t.Fatalf("sync_run connector = {%q %q}, want {%q %q}",
			repo.lastInsert.ConnectorID, repo.lastInsert.ConnectorVersion, conn.ID(), conn.Version())
	}
	if repo.lastInsert.EventID != ev.EventID {
		t.Fatalf("sync_run event_id = %q, want %q (the event that opened the run)", repo.lastInsert.EventID, ev.EventID)
	}
	if len(repo.updates) != 1 || repo.updates[0].Status != SyncStatusOK {
		t.Fatalf("sync_run updates = %+v, want one OK", repo.updates)
	}
	if repo.updates[0].ItemsNew != 2 || repo.updates[0].ItemsDeduped != 0 {
		t.Fatalf("tallies = {new:%d deduped:%d}, want {2 0}", repo.updates[0].ItemsNew, repo.updates[0].ItemsDeduped)
	}
	if repo.findCalls != 1 {
		t.Fatalf("FindOrCreateCourtRecord calls = %d, want 1", repo.findCalls)
	}
	if repo.docketCalls != 1 || len(repo.docketParams) != 2 {
		t.Fatalf("docket upsert = {calls:%d entries:%d}, want {1 2}", repo.docketCalls, len(repo.docketParams))
	}
	if repo.intimCalls != 1 || len(repo.intimParams) != 1 {
		t.Fatalf("intimation upsert = {calls:%d entries:%d}, want {1 1}", repo.intimCalls, len(repo.intimParams))
	}

	counts := countByType(outbox.published)
	if counts[TypeCourtRecordObserved] != 1 || counts[TypeDocketEntryObserved] != 2 || counts[TypeSyncCompleted] != 1 {
		t.Fatalf("outbox by type = %v, want court_record_observed:1 docket_entry_observed:2 sync_completed:1", counts)
	}
	if counts[TypeSyncFailed] != 0 {
		t.Fatalf("sync_failed emitted %d times on success, want 0", counts[TypeSyncFailed])
	}

	if uow.tenantID != ev.TenantID {
		t.Fatalf("uow tenantID = %q, want %q", uow.tenantID, ev.TenantID)
	}
}

// U4: a duplicate delivery (SeenOrMark reports seen) is a no-op — no fetch, no
// parse, no run, no upserts, no outbox — and acks without error.
func TestSyncUseCase_DuplicateDelivery_NoOps(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 2}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // 0 rows affected = already seen
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	if conn.fetchCalls != 0 || parser.parseCalls != 0 {
		t.Fatalf("seen event drove the cycle: fetch=%d parse=%d", conn.fetchCalls, parser.parseCalls)
	}
	if repo.insertCalls != 0 || len(repo.updates) != 0 || repo.docketCalls != 0 {
		t.Fatalf("seen event caused writes: insert=%d updates=%d docket=%d", repo.insertCalls, len(repo.updates), repo.docketCalls)
	}
	if outbox.calls != 0 {
		t.Fatalf("seen event published %d events, want 0", outbox.calls)
	}
}

// U5: a fetch fault records a FAILED run and emits sync_failed, performs no
// upserts, and acks (err nil) — the scheduler re-syncs later.
func TestSyncUseCase_FetchError_FailsAndAcks(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{syncRunID: "run-1"}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{fetchErr: errors.New("connector unreachable")}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("OnSyncRequested() error = %v, want nil (fetch fault is acked)", err)
	}

	if parser.parseCalls != 0 {
		t.Fatalf("parser reached %d times after a fetch fault, want 0", parser.parseCalls)
	}
	if len(repo.updates) != 1 || repo.updates[0].Status != SyncStatusFailed {
		t.Fatalf("sync_run updates = %+v, want one FAILED", repo.updates)
	}
	if repo.updates[0].Error == "" {
		t.Fatal("FAILED run has empty error reason, want the fetch cause")
	}
	if repo.findCalls != 0 || repo.docketCalls != 0 {
		t.Fatalf("fetch fault caused upserts: find=%d docket=%d", repo.findCalls, repo.docketCalls)
	}
	counts := countByType(outbox.published)
	if counts[TypeSyncFailed] != 1 || counts[TypeSyncCompleted] != 0 || counts[TypeCourtRecordObserved] != 0 {
		t.Fatalf("outbox by type = %v, want only sync_failed:1", counts)
	}
}

// U6: a parse fault records a FAILED run, emits sync_failed, and returns an error
// wrapping asynq.SkipRetry (a malformed payload never parses on retry).
func TestSyncUseCase_ParseError_FailsAndSkipsRetry(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{syncRunID: "run-1"}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{parseErr: errors.New("unrecognized payload")}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	err := uc.OnSyncRequested(context.Background(), syncRequestedEvent())
	if err == nil {
		t.Fatal("OnSyncRequested() error = nil, want a parse failure")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("error = %v, want it to wrap asynq.SkipRetry", err)
	}
	if len(repo.updates) != 1 || repo.updates[0].Status != SyncStatusFailed {
		t.Fatalf("sync_run updates = %+v, want one FAILED", repo.updates)
	}
	if repo.docketCalls != 0 {
		t.Fatalf("parse fault caused %d docket upserts, want 0", repo.docketCalls)
	}
	counts := countByType(outbox.published)
	if counts[TypeSyncFailed] != 1 || counts[TypeSyncCompleted] != 0 {
		t.Fatalf("outbox by type = %v, want only sync_failed:1", counts)
	}
}

// U7: with one of the two docket entries already present (repo reports 1 new, 1
// deduped), the run closes with tallies {new:1 deduped:1} and docket_entry_observed
// fires only for the new one.
func TestSyncUseCase_DocketDedup_ObservesOnlyNew(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 1}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	if len(repo.updates) != 1 || repo.updates[0].ItemsNew != 1 || repo.updates[0].ItemsDeduped != 1 {
		t.Fatalf("tallies = %+v, want {new:1 deduped:1}", repo.updates)
	}
	counts := countByType(outbox.published)
	if counts[TypeDocketEntryObserved] != 1 {
		t.Fatalf("docket_entry_observed = %d, want 1 (only the new entry)", counts[TypeDocketEntryObserved])
	}
	if counts[TypeCourtRecordObserved] != 1 || counts[TypeSyncCompleted] != 1 {
		t.Fatalf("outbox by type = %v, want court_record_observed:1 sync_completed:1", counts)
	}
}

// U8: the transaction's RLS scope is the event's tenant, not a hardcoded value.
func TestSyncUseCase_RLSScopedToEventTenant(t *testing.T) {
	t.Parallel()

	const tenant = "tenant-rls-9"
	repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 2}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	ev := syncRequestedEvent()
	ev.TenantID = tenant
	if err := uc.OnSyncRequested(context.Background(), ev); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	if uow.tenantID != tenant {
		t.Fatalf("uow tenantID = %q, want %q", uow.tenantID, tenant)
	}
	if repo.lastInsert.TenantID != tenant {
		t.Fatalf("sync_run tenantID = %q, want %q", repo.lastInsert.TenantID, tenant)
	}
}

// U9: a re-delivery of an already-marked event whose sync_run is still RUNNING (a
// prior attempt died between UoW-1 and the close) RESUMES the cycle — it opens no
// second run, re-runs fetch→parse→apply, and closes the SAME run (run-stuck) OK.
func TestSyncUseCase_RedeliveryRunningRun_Resumes(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{
		docketNewCount: 2,
		// SeenOrMark reports "already" (rows=0), and the run this event opened is
		// still RUNNING — the crashed-mid-cycle state.
		findByEventRun: &SyncRun{ID: "run-stuck", Status: SyncStatusRunning},
	}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // 0 rows affected = already seen
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	if repo.insertCalls != 0 {
		t.Fatalf("insertCalls = %d, want 0 (resume reuses the stuck run, opens no new one)", repo.insertCalls)
	}
	if conn.fetchCalls != 1 || parser.parseCalls != 1 {
		t.Fatalf("resume did not re-run the cycle: fetch=%d parse=%d, want 1 1", conn.fetchCalls, parser.parseCalls)
	}
	if len(repo.updates) != 1 || repo.updates[0].Status != SyncStatusOK || repo.updates[0].ID != "run-stuck" {
		t.Fatalf("sync_run updates = %+v, want one OK closing run-stuck", repo.updates)
	}
	counts := countByType(outbox.published)
	if counts[TypeSyncCompleted] != 1 || counts[TypeCourtRecordObserved] != 1 || counts[TypeDocketEntryObserved] != 2 {
		t.Fatalf("outbox by type = %v, want sync_completed:1 court_record_observed:1 docket_entry_observed:2", counts)
	}
}

// U10: a re-delivery of an already-marked event whose sync_run is already CLOSED
// (OK or FAILED) is a no-op ack — no fetch, no new run, no re-close, no outbox —
// so a closed run is never reopened and its outbox is never duplicated.
func TestSyncUseCase_RedeliveryClosedRun_NoOps(t *testing.T) {
	t.Parallel()

	for _, status := range []string{SyncStatusOK, SyncStatusFailed} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			repo := &stubSyncRepo{
				docketNewCount: 2,
				findByEventRun: &SyncRun{ID: "run-closed", Status: status},
			}
			outbox := &fakeOutbox{}
			uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // already seen
			conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
			parser := &stubParser{result: stubFixture(SourceDJEN)}
			uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

			if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
				t.Fatalf("OnSyncRequested() error = %v", err)
			}

			if conn.fetchCalls != 0 || parser.parseCalls != 0 {
				t.Fatalf("closed-run redelivery drove the cycle: fetch=%d parse=%d", conn.fetchCalls, parser.parseCalls)
			}
			if repo.insertCalls != 0 || len(repo.updates) != 0 {
				t.Fatalf("closed-run redelivery wrote the run: insert=%d updates=%d", repo.insertCalls, len(repo.updates))
			}
			if outbox.calls != 0 {
				t.Fatalf("closed-run redelivery published %d events, want 0", outbox.calls)
			}
		})
	}
}

// U13: two executions race to close the SAME run (the original attempt is still
// in flight when a redelivery resumes it — both read the run RUNNING in UoW-1 and
// both reach the close in UoW-2). The status=RUNNING compare-and-swap lets exactly
// ONE win: it publishes sync_completed and the observed events; the loser's close
// affects zero rows (closed=false) and publishes NOTHING — so the terminal event
// (and the backfill slice count it drives) fires exactly once. Without the guard
// both would publish, double-counting the backfill slice.
func TestSyncUseCase_ConcurrentClose_PublishesCompletedOnce(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{
		syncRunID:      "run-1",
		docketNewCount: 2,
		// A redelivery resolving the same run finds it still RUNNING (the original
		// attempt has not committed its close yet).
		findByEventRun: &SyncRun{ID: "run-1", Status: SyncStatusRunning},
	}
	outbox := &fakeOutbox{}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	orch := orchestratorWith(SourceDJEN, conn)

	// Execution A — the first delivery opens run-1 and closes it OK (CAS wins).
	ucA := NewSyncUseCase(repo, outbox, &stubBackfillUoW{tx: stubTx{rows: 1}}, orch, parser)
	if err := ucA.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("first execution error = %v", err)
	}
	// Execution B — a concurrent redelivery resumes the SAME run-1 and races to
	// close it, but the run is already OK, so the CAS misses.
	ucB := NewSyncUseCase(repo, outbox, &stubBackfillUoW{tx: stubTx{rows: 0}}, orch, parser)
	if err := ucB.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("redelivery execution error = %v", err)
	}

	if len(repo.updates) != 2 {
		t.Fatalf("UpdateSyncRun attempts = %d, want 2 (both executions tried to close run-1)", len(repo.updates))
	}
	counts := countByType(outbox.published)
	if counts[TypeSyncCompleted] != 1 {
		t.Fatalf("sync_completed = %d, want 1 (published exactly once despite the race)", counts[TypeSyncCompleted])
	}
	if counts[TypeCourtRecordObserved] != 1 || counts[TypeDocketEntryObserved] != 2 {
		t.Fatalf("observed events = %v, want court_record_observed:1 docket_entry_observed:2 (only the winner emits them)", counts)
	}
}

// U14: the same race on the FAILURE path — two executions both hit a fetch fault
// and race to mark the SAME run FAILED. The CAS lets one win and publish
// sync_failed; the loser's close misses (closed=false) and publishes nothing, so
// sync_failed fires exactly once.
func TestSyncUseCase_ConcurrentClose_PublishesFailedOnce(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{
		syncRunID:      "run-1",
		findByEventRun: &SyncRun{ID: "run-1", Status: SyncStatusRunning},
	}
	outbox := &fakeOutbox{}
	conn := &stubConnector{fetchErr: errors.New("connector unreachable")}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	orch := orchestratorWith(SourceDJEN, conn)

	ucA := NewSyncUseCase(repo, outbox, &stubBackfillUoW{tx: stubTx{rows: 1}}, orch, parser)
	if err := ucA.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("first execution error = %v", err)
	}
	ucB := NewSyncUseCase(repo, outbox, &stubBackfillUoW{tx: stubTx{rows: 0}}, orch, parser)
	if err := ucB.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("redelivery execution error = %v", err)
	}

	if len(repo.updates) != 2 {
		t.Fatalf("UpdateSyncRun attempts = %d, want 2 (both executions tried to fail run-1)", len(repo.updates))
	}
	counts := countByType(outbox.published)
	if counts[TypeSyncFailed] != 1 {
		t.Fatalf("sync_failed = %d, want 1 (published exactly once despite the race)", counts[TypeSyncFailed])
	}
	if counts[TypeSyncCompleted] != 0 {
		t.Fatalf("sync_completed = %d on a failed cycle, want 0", counts[TypeSyncCompleted])
	}
}

// U11: the connector is resolved per event by the integration's source — a DJEN
// event runs under the DJEN connector, a DATAJUD event under DATAJUD (proven by
// the distinct connector id stamped on the sync_run).
func TestSyncUseCase_ResolvesConnectorBySource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		wantID string
	}{
		{name: "djen event uses djen connector", source: SourceDJEN, wantID: "conn-djen"},
		{name: "datajud event uses datajud connector", source: SourceDATAJUD, wantID: "conn-datajud"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			djen := &stubConnector{id: "conn-djen", payload: RawPayload{ConnectorID: stubConnectorID}}
			datajud := &stubConnector{id: "conn-datajud", payload: RawPayload{ConnectorID: stubConnectorID}}
			orch := NewOrchestrator()
			orch.Register(SourceDJEN, djen)
			orch.Register(SourceDATAJUD, datajud)

			repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 2}
			outbox := &fakeOutbox{}
			uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
			parser := &stubParser{result: stubFixture(tt.source)}
			uc := NewSyncUseCase(repo, outbox, uow, orch, parser)

			ev := syncRequestedEvent()
			ev.Source = tt.source
			if err := uc.OnSyncRequested(context.Background(), ev); err != nil {
				t.Fatalf("OnSyncRequested() error = %v", err)
			}

			if repo.lastInsert.ConnectorID != tt.wantID {
				t.Fatalf("sync_run connector_id = %q, want %q (connector for %s)", repo.lastInsert.ConnectorID, tt.wantID, tt.source)
			}
		})
	}
}

// U12: an event whose source has no registered connector fails with the typed
// ErrConnectorNotFound and opens no run (the connector is resolved before UoW-1).
func TestSyncUseCase_UnknownSource_TypedError(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{syncRunID: "run-1"}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	parser := &stubParser{result: stubFixture(SourceDJEN)}
	// Orchestrator with DJEN registered, but the event names an unregistered source.
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser)

	ev := syncRequestedEvent()
	ev.Source = SourceDATAJUD
	err := uc.OnSyncRequested(context.Background(), ev)
	if !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrConnectorNotFound", err)
	}
	if repo.insertCalls != 0 || conn.fetchCalls != 0 {
		t.Fatalf("unknown source opened work: insert=%d fetch=%d, want 0 0", repo.insertCalls, conn.fetchCalls)
	}
}

// --- entitlement gating tests (fatia 5-A) -----------------------------------

// AC1: below the limit, every brand-new court record is created — the current
// behavior is preserved. The checker is consulted exactly once per cycle, scoped to
// the event's tenant (not once per record).
func TestSyncUseCase_BelowLimit_CreatesAllRecords(t *testing.T) {
	t.Parallel()

	repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 2}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: twoRecordFixture(SourceDJEN)}
	checker := &stubEntitlementChecker{limit: 10}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser,
		WithEntitlementChecker(checker))

	if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	if checker.calls != 1 || checker.lastTenant != testTenant {
		t.Fatalf("checker = {calls:%d tenant:%q}, want {1 %q} (resolved once, scoped to the event tenant)",
			checker.calls, checker.lastTenant, testTenant)
	}
	if repo.findCalls != 2 || repo.activeCount != 2 {
		t.Fatalf("find/create = {calls:%d active:%d}, want {2 2} (both records created under the limit)",
			repo.findCalls, repo.activeCount)
	}
	if len(repo.updates) != 1 || repo.updates[0].Status != SyncStatusOK {
		t.Fatalf("sync_run updates = %+v, want one OK", repo.updates)
	}
	if len(repo.docketParams) != 2 || len(repo.intimParams) != 2 {
		t.Fatalf("children = {docket:%d intim:%d}, want {2 2} (no record blocked)", len(repo.docketParams), len(repo.intimParams))
	}
	counts := countByType(outbox.published)
	if counts[TypeCourtRecordObserved] != 2 || counts[TypeSyncCompleted] != 1 {
		t.Fatalf("outbox = %v, want court_record_observed:2 sync_completed:1", counts)
	}
}

// AC2: at the limit, a brand-new record (a MISS) is NOT created — the cycle logs,
// skips it AND its docket/intimation children, folds the rest of the batch, and the
// sync_run still closes OK (a block is expected, never an abort).
func TestSyncUseCase_AtLimit_BlocksNewRecordAndClosesOK(t *testing.T) {
	t.Parallel()

	// limit 1, no records yet: the first new record (A) is created and pushes the
	// tally to 1; the second (B) is at the ceiling and is blocked.
	repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 2}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: twoRecordFixture(SourceDJEN)}
	checker := &stubEntitlementChecker{limit: 1}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser,
		WithEntitlementChecker(checker))

	if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("OnSyncRequested() error = %v, want nil (a block is expected, not a failure)", err)
	}

	// Both records were attempted, but only one created.
	if repo.findCalls != 2 || repo.activeCount != 1 {
		t.Fatalf("find/create = {calls:%d active:%d}, want {2 1} (B blocked at the ceiling)", repo.findCalls, repo.activeCount)
	}
	// The run still closes OK — the cycle is not aborted.
	if len(repo.updates) != 1 || repo.updates[0].Status != SyncStatusOK {
		t.Fatalf("sync_run updates = %+v, want one OK (a block must not abort the run)", repo.updates)
	}
	// Only the created record's children are folded; the blocked record's docket
	// entry and intimation are dropped (NOT treated as a parser-invariant abort).
	if len(repo.docketParams) != 1 || len(repo.intimParams) != 1 {
		t.Fatalf("children = {docket:%d intim:%d}, want {1 1} (blocked record's children dropped)", len(repo.docketParams), len(repo.intimParams))
	}
	if repo.docketParams[0].Hash != "docket-A" || repo.intimParams[0].Hash != "notif-A" {
		t.Fatalf("kept children = {docket:%q intim:%q}, want the created record A's", repo.docketParams[0].Hash, repo.intimParams[0].Hash)
	}
	counts := countByType(outbox.published)
	if counts[TypeCourtRecordObserved] != 1 || counts[TypeSyncCompleted] != 1 {
		t.Fatalf("outbox = %v, want court_record_observed:1 sync_completed:1 (only the created record observed)", counts)
	}
}

// AC4: a reobservation (a HIT — the CNJ+degree already exists) is NEVER gated, even
// when the tenant is already at (or over) its limit: the record is returned and its
// docket entries are processed. Only the brand-new record in the same batch is blocked.
func TestSyncUseCase_Reobservation_NotGatedEvenAtLimit(t *testing.T) {
	t.Parallel()

	// Ceiling reached (limit 0). Record A is already known (a HIT), record B is new.
	repo := &stubSyncRepo{
		syncRunID:      "run-1",
		docketNewCount: 2,
		existingKeys:   map[string]bool{recordKey("0000001-11.2024.8.26.0100", DegreeG1): true},
	}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: twoRecordFixture(SourceDJEN)}
	checker := &stubEntitlementChecker{limit: 0}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser,
		WithEntitlementChecker(checker))

	if err := uc.OnSyncRequested(context.Background(), syncRequestedEvent()); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	// A (reobservation) is processed despite the ceiling; B (new) is blocked.
	if len(repo.docketParams) != 1 || repo.docketParams[0].Hash != "docket-A" {
		t.Fatalf("docket params = %+v, want only the reobserved record A's docket entry", repo.docketParams)
	}
	if len(repo.intimParams) != 1 || repo.intimParams[0].Hash != "notif-A" {
		t.Fatalf("intim params = %+v, want only the reobserved record A's intimation", repo.intimParams)
	}
	counts := countByType(outbox.published)
	if counts[TypeCourtRecordObserved] != 1 {
		t.Fatalf("court_record_observed = %d, want 1 (the reobserved record, not the blocked new one)", counts[TypeCourtRecordObserved])
	}
	if len(repo.updates) != 1 || repo.updates[0].Status != SyncStatusOK {
		t.Fatalf("sync_run updates = %+v, want one OK", repo.updates)
	}
}

// AC6: a genuine checker error (NOT ErrSubscriptionNotFound, which the adapter folds
// to limit 0) is a real fault — it propagates, never silenced: the cycle reaches no
// record, closes no run (it stays RUNNING for a later resume), and publishes nothing.
func TestSyncUseCase_CheckerError_FailsCycle(t *testing.T) {
	t.Parallel()

	billingDown := errors.New("billing unavailable")
	repo := &stubSyncRepo{syncRunID: "run-1", docketNewCount: 2}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	conn := &stubConnector{payload: RawPayload{ConnectorID: stubConnectorID}}
	parser := &stubParser{result: twoRecordFixture(SourceDJEN)}
	checker := &stubEntitlementChecker{err: billingDown}
	uc := NewSyncUseCase(repo, outbox, uow, orchestratorWith(SourceDJEN, conn), parser,
		WithEntitlementChecker(checker))

	err := uc.OnSyncRequested(context.Background(), syncRequestedEvent())
	if !errors.Is(err, billingDown) {
		t.Fatalf("error = %v, want it to wrap the checker failure (a genuine checker error must not be silenced)", err)
	}
	if repo.findCalls != 0 {
		t.Fatalf("FindOrCreateCourtRecord called %d times after a checker fault, want 0", repo.findCalls)
	}
	if len(repo.updates) != 0 {
		t.Fatalf("sync_run updates = %+v, want none (the run stays RUNNING for a resume)", repo.updates)
	}
	if outbox.calls != 0 {
		t.Fatalf("checker fault published %d events, want 0", outbox.calls)
	}
}
