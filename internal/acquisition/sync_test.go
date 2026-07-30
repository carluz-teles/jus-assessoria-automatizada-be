package acquisition

import (
	"context"
	"errors"
	"testing"

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
// preset payload, or fetchErr to exercise the fetch-failure path.
type stubConnector struct {
	payload    RawPayload
	fetchErr   error
	fetchCalls int
}

func (c *stubConnector) ID() string                 { return "test-conn" }
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

func (p *stubParser) Parse(RawPayload) (ParsedResult, error) {
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

	findCalls  int
	findParams []FindOrCreateCourtRecordParams

	docketCalls    int
	docketParams   []DocketEntryParams
	docketNewCount int

	intimCalls  int
	intimParams []IntimationParams
	intimNew    int

	updates []SyncRunOutcome
}

func (s *stubSyncRepo) InsertSyncRun(_ context.Context, _ database.Tx, p SyncRunParams) (string, error) {
	s.insertCalls++
	s.lastInsert = p
	return s.syncRunID, nil
}

func (s *stubSyncRepo) UpdateSyncRun(_ context.Context, _ database.Tx, o SyncRunOutcome) error {
	s.updates = append(s.updates, o)
	return nil
}

func (s *stubSyncRepo) FindOrCreateCourtRecord(_ context.Context, _ database.Tx, p FindOrCreateCourtRecordParams) (*CourtRecord, error) {
	s.findCalls++
	s.findParams = append(s.findParams, p)
	return &CourtRecord{
		ID:        "rec-" + p.CNJNumber + "-" + p.Degree,
		TenantID:  p.TenantID,
		CaseID:    "case-" + p.CNJNumber + "-" + p.Degree,
		CNJNumber: p.CNJNumber,
		Degree:    p.Degree,
		Court:     p.Court,
	}, nil
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

// syncRequestedEvent builds a valid sync_requested event for the default tenant.
func syncRequestedEvent() SyncRequested {
	return SyncRequested{
		Base:          events.Base{EventID: "sync-evt-1", Aggregate: "job-1"},
		BackfillJobID: "job-1",
		TenantID:      testTenant,
		IntegrationID: "integ-1",
		SliceIndex:    0,
		WindowFrom:    "2024-01-01",
		WindowTo:      "2024-01-08",
	}
}

// countByType tallies published events by their dotted type id.
func countByType(published []events.Event) map[string]int {
	counts := map[string]int{}
	for _, ev := range published {
		counts[ev.Type()]++
	}
	return counts
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
	uc := NewSyncUseCase(repo, outbox, uow, conn, parser)

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
	uc := NewSyncUseCase(repo, outbox, uow, conn, parser)

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
	uc := NewSyncUseCase(repo, outbox, uow, conn, parser)

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
	uc := NewSyncUseCase(repo, outbox, uow, conn, parser)

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
	uc := NewSyncUseCase(repo, outbox, uow, conn, parser)

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
	uc := NewSyncUseCase(repo, outbox, uow, conn, parser)

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
