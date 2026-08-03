package acquisition

import (
	"context"
	"os"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// fakeEnrichRepo records the enrichment persistence calls and returns a graded
// record with a fixed id distinct from the placeholder, so the merge path runs.
type fakeEnrichRepo struct {
	gradedParams  GradedRecordParams
	gradedCalls   int
	repointFrom   string
	repointTo     string
	repointCalls  int
	supersedeID   string
	supersedeCall int
	docketParams  []DocketEntryParams
}

func (r *fakeEnrichRepo) UpsertGradedCourtRecord(_ context.Context, _ database.Tx, p GradedRecordParams) (*CourtRecord, error) {
	r.gradedCalls++
	r.gradedParams = p
	return &CourtRecord{ID: "graded-1", TenantID: p.TenantID, CaseID: p.CaseID, CNJNumber: p.CNJNumber, Degree: p.Degree, Court: p.Court}, nil
}

func (r *fakeEnrichRepo) RepointIntimations(_ context.Context, _ database.Tx, _, from, to string) (int, error) {
	r.repointCalls++
	r.repointFrom, r.repointTo = from, to
	return 1, nil
}

func (r *fakeEnrichRepo) SupersedeCourtRecord(_ context.Context, _ database.Tx, _, id string) error {
	r.supersedeCall++
	r.supersedeID = id
	return nil
}

func (r *fakeEnrichRepo) UpsertDocketEntries(_ context.Context, _ database.Tx, params []DocketEntryParams) ([]DocketEntry, error) {
	r.docketParams = params
	out := make([]DocketEntry, 0, len(params))
	for _, p := range params {
		out = append(out, DocketEntry{ID: "de-" + p.Hash, CourtRecordID: p.CourtRecordID, Hash: p.Hash})
	}
	return out, nil
}

func datajudFixtureBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/datajud_search.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func placeholderObserved() CourtRecordObserved {
	return CourtRecordObserved{
		Base:          events.Base{EventID: "enrich-evt-1", Aggregate: "unknown-1"},
		TenantID:      testTenant,
		CourtRecordID: "unknown-1",
		CaseID:        "case-1",
		CNJNumber:     "50007978720168210156",
		Degree:        DegreeUnknown,
		Court:         "TJRS",
	}
}

func enrichmentUnderTest(repo enrichRepo, outbox publisher, uow database.UnitOfWork, payload RawPayload) *EnrichmentUseCase {
	orch := NewOrchestrator()
	orch.Register(SourceDATAJUD, &stubConnector{id: "datajud", payload: payload})
	return NewEnrichmentUseCase(repo, outbox, uow, orch, NewDATAJUDParser())
}

// TestEnrichment_PlaceholderMerge is the happy path: a DJEN placeholder is graded,
// its intimations re-pointed, the placeholder superseded, and the movimentos land
// as docket entries with an observed event each.
func TestEnrichment_PlaceholderMerge(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}} // first sighting
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	err := enrichmentUnderTest(repo, outbox, uow, payload).OnCourtRecordObserved(context.Background(), placeholderObserved())
	if err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}

	if repo.gradedCalls != 1 || repo.gradedParams.Degree != "G1" {
		t.Errorf("graded upsert = %d calls, degree %q; want 1 call, G1", repo.gradedCalls, repo.gradedParams.Degree)
	}
	if repo.repointCalls != 1 || repo.repointFrom != "unknown-1" || repo.repointTo != "graded-1" {
		t.Errorf("repoint = %d calls, %s→%s; want 1, unknown-1→graded-1", repo.repointCalls, repo.repointFrom, repo.repointTo)
	}
	if repo.supersedeCall != 1 || repo.supersedeID != "unknown-1" {
		t.Errorf("supersede = %d calls, id %q; want 1, unknown-1", repo.supersedeCall, repo.supersedeID)
	}
	if len(repo.docketParams) == 0 {
		t.Fatal("no movimentos upserted as docket entries")
	}
	for _, p := range repo.docketParams {
		if p.CourtRecordID != "graded-1" {
			t.Errorf("docket attached to %q, want the graded record", p.CourtRecordID)
		}
	}
	if got := countByType(outbox.published)[TypeDocketEntryObserved]; got != len(repo.docketParams) {
		t.Errorf("docket_entry_observed events = %d, want %d", got, len(repo.docketParams))
	}
}

// TestEnrichment_SkipsMissingKeys proves the guard: an observation without the
// number/court a by-number fetch needs triggers no fetch and no persistence.
func TestEnrichment_SkipsMissingKeys(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	conn := &stubConnector{id: "datajud", payload: RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}}
	orch := NewOrchestrator()
	orch.Register(SourceDATAJUD, conn)
	uc := NewEnrichmentUseCase(repo, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}}, orch, NewDATAJUDParser())

	ev := placeholderObserved()
	ev.Court = "" // missing tribunal → cannot fetch

	if err := uc.OnCourtRecordObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}
	if conn.fetchCalls != 0 || repo.gradedCalls != 0 {
		t.Errorf("guard failed: fetches=%d gradedCalls=%d, want 0/0", conn.fetchCalls, repo.gradedCalls)
	}
}

// TestEnrichment_SkipsGradeMismatch proves a re-poll whose DATAJUD top hit is a
// DIFFERENT grade than the record (multi-instance, v0 out of scope) is fetched but
// not merged — no spurious grade/supersede.
func TestEnrichment_SkipsGradeMismatch(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	conn := &stubConnector{id: "datajud", payload: RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}}
	orch := NewOrchestrator()
	orch.Register(SourceDATAJUD, conn)
	uc := NewEnrichmentUseCase(repo, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}}, orch, NewDATAJUDParser())

	ev := placeholderObserved()
	ev.Degree = DegreeG2 // record is G2, but the fixture's hit is G1 → mismatch

	if err := uc.OnCourtRecordObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}
	if conn.fetchCalls != 1 {
		t.Errorf("expected a fetch before the mismatch check, got %d", conn.fetchCalls)
	}
	if repo.gradedCalls != 0 {
		t.Errorf("grade mismatch still merged (gradedCalls=%d), want 0", repo.gradedCalls)
	}
}

// TestEnrichment_ResyncRefresh proves the re-poll path: a graded record observed
// again (same grade) refreshes its movimentos WITHOUT re-pointing or superseding
// (the graded record is itself the target).
func TestEnrichment_ResyncRefresh(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	ev := placeholderObserved()
	ev.Degree = "G1"              // already graded
	ev.CourtRecordID = "graded-1" // and IS the record the fake upsert returns

	if err := enrichmentUnderTest(repo, outbox, uow, payload).OnCourtRecordObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}
	if repo.gradedCalls != 1 {
		t.Errorf("graded refresh = %d, want 1", repo.gradedCalls)
	}
	if repo.repointCalls != 0 || repo.supersedeCall != 0 {
		t.Errorf("resync must not re-point/supersede: repoint=%d supersede=%d", repo.repointCalls, repo.supersedeCall)
	}
	if len(repo.docketParams) == 0 {
		t.Error("resync should refresh movimentos as docket entries")
	}
}

// TestEnrichment_DuplicateIsNoOp proves a re-delivery (SeenOrMark reports seen)
// skips the merge even though the fetch (outside the tx) still ran.
func TestEnrichment_DuplicateIsNoOp(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // 0 rows affected = already seen
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	if err := enrichmentUnderTest(repo, &fakeOutbox{}, uow, payload).OnCourtRecordObserved(context.Background(), placeholderObserved()); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}
	if repo.gradedCalls != 0 {
		t.Errorf("duplicate delivery still merged (gradedCalls=%d), want 0", repo.gradedCalls)
	}
}

// TestEnrichment_NoHitsIsAck proves a process not yet in DATAJUD is a no-op ack.
func TestEnrichment_NoHitsIsAck(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	payload := RawPayload{Source: SourceDATAJUD, Body: []byte(`{"hits":{"hits":[]}}`)}

	if err := enrichmentUnderTest(repo, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}}, payload).OnCourtRecordObserved(context.Background(), placeholderObserved()); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}
	if repo.gradedCalls != 0 {
		t.Errorf("empty DATAJUD result still graded (gradedCalls=%d), want 0", repo.gradedCalls)
	}
}
