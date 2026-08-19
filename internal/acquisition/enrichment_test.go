package acquisition

import (
	"context"
	"os"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// fakeEnrichRepo records the enrichment persistence calls. GetCourtRecordByKey
// answers the grade-conflict lookup: by default a miss (found=false → grade in
// place); set existing/existingFound to simulate a pre-existing graded record (→ the
// merge path). UpdateCourtRecordGrade echoes back a record keyed on the SAME id it was
// asked to mutate, so a test can assert the id never changed.
type fakeEnrichRepo struct {
	existing      *CourtRecord // GetCourtRecordByKey result
	existingFound bool
	getByKeyCalls int

	updateParams GradeParams
	updateCalls  int

	repointFrom  string
	repointTo    string
	repointCalls int

	supersedeID   string
	supersedeCall int

	docketParams []DocketEntryParams
}

func (r *fakeEnrichRepo) AcquireTenantWriteLock(_ context.Context, _ database.Tx, _ string) error {
	return nil
}

func (r *fakeEnrichRepo) GetCourtRecordByKey(_ context.Context, _ database.Tx, _, _, _ string) (*CourtRecord, bool, error) {
	r.getByKeyCalls++
	return r.existing, r.existingFound, nil
}

func (r *fakeEnrichRepo) UpdateCourtRecordGrade(_ context.Context, _ database.Tx, p GradeParams) (*CourtRecord, error) {
	r.updateCalls++
	r.updateParams = p
	// Grade in place: the returned record keeps the id it was asked to mutate.
	return &CourtRecord{ID: p.CourtRecordID, TenantID: p.TenantID, Degree: p.Degree, Court: p.Court}, nil
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

// TestEnrichment_GraduatesInPlace is the FIX B happy path: a DJEN placeholder is
// graded IN PLACE — UpdateCourtRecordGrade is called on the SAME id (ev.CourtRecordID),
// the degree becomes the real grade, and NO record is created, re-pointed or
// superseded. The movimentos land as docket entries on that same id with an observed
// event each.
func TestEnrichment_GraduatesInPlace(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{} // no existing graded record → grade in place
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}} // first sighting
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	err := enrichmentUnderTest(repo, outbox, uow, payload).OnCourtRecordObserved(context.Background(), placeholderObserved())
	if err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}

	if repo.updateCalls != 1 || repo.updateParams.CourtRecordID != "unknown-1" || repo.updateParams.Degree != "G1" {
		t.Errorf("grade update = %d calls, id %q degree %q; want 1 call on unknown-1 → G1",
			repo.updateCalls, repo.updateParams.CourtRecordID, repo.updateParams.Degree)
	}
	// Lifecycle is derived from the fixture's movimentos (no code 22/25) → ACTIVE, and
	// is passed through to the update params (the SQL CASE then guards SUPERSEDED).
	if repo.updateParams.Lifecycle != LifecycleActive {
		t.Errorf("grade update lifecycle = %q; want ACTIVE (fixture has no terminal/suspension code)", repo.updateParams.Lifecycle)
	}
	if repo.repointCalls != 0 || repo.supersedeCall != 0 {
		t.Errorf("common grade must not re-point/supersede: repoint=%d supersede=%d", repo.repointCalls, repo.supersedeCall)
	}
	if len(repo.docketParams) == 0 {
		t.Fatal("no movimentos upserted as docket entries")
	}
	for _, p := range repo.docketParams {
		if p.CourtRecordID != "unknown-1" {
			t.Errorf("docket attached to %q, want the graded-in-place record unknown-1", p.CourtRecordID)
		}
	}
	if got := countByType(outbox.published)[TypeDocketEntryObserved]; got != len(repo.docketParams) {
		t.Errorf("docket_entry_observed events = %d, want %d", got, len(repo.docketParams))
	}
}

// TestEnrichment_BackfillSuppressesDocketEvents proves the outbox-flood fix: when the
// observed record came from an onboarding backfill (BackfillJobID set), the enrichment
// still GRADES the record and PERSISTS the movimentos as docket entries, but emits NO
// docket_entry_observed — the per-andamento aviso is silenced during onboarding anyway,
// so the events would only flood the relay and starve the backfill's own sync_completed.
func TestEnrichment_BackfillSuppressesDocketEvents(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	ev := placeholderObserved()
	ev.BackfillJobID = "backfill-1" // discovered by an onboarding backfill

	if err := enrichmentUnderTest(repo, outbox, uow, payload).OnCourtRecordObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}

	// The record is still graded and the movimentos still land as docket entries…
	if repo.updateCalls != 1 {
		t.Errorf("grade update = %d calls; want 1 (grading happens regardless of backfill)", repo.updateCalls)
	}
	if len(repo.docketParams) == 0 {
		t.Fatal("movimentos must still be persisted as docket entries during a backfill")
	}
	// …but NO docket_entry_observed is emitted for a backfill-originated record.
	if got := countByType(outbox.published)[TypeDocketEntryObserved]; got != 0 {
		t.Errorf("docket_entry_observed events = %d, want 0 (suppressed for backfill)", got)
	}
}

// TestEnrichment_ConflictMerges proves the rare fallback: when a graded record already
// holds this (tenant, cnj, degree) — grading the placeholder in place would violate the
// UNIQUE — the placeholder's intimations are re-pointed onto the existing graded record,
// the placeholder is superseded, and the grade/movimentos land on the existing record.
func TestEnrichment_ConflictMerges(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{
		existing:      &CourtRecord{ID: "graded-existing", Degree: "G1"},
		existingFound: true,
	}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	err := enrichmentUnderTest(repo, outbox, uow, payload).OnCourtRecordObserved(context.Background(), placeholderObserved())
	if err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}

	if repo.repointCalls != 1 || repo.repointFrom != "unknown-1" || repo.repointTo != "graded-existing" {
		t.Errorf("repoint = %d calls, %s→%s; want 1, unknown-1→graded-existing", repo.repointCalls, repo.repointFrom, repo.repointTo)
	}
	if repo.supersedeCall != 1 || repo.supersedeID != "unknown-1" {
		t.Errorf("supersede = %d calls, id %q; want 1, unknown-1", repo.supersedeCall, repo.supersedeID)
	}
	if repo.updateCalls != 1 || repo.updateParams.CourtRecordID != "graded-existing" {
		t.Errorf("grade update = %d calls on id %q; want 1 on graded-existing", repo.updateCalls, repo.updateParams.CourtRecordID)
	}
	for _, p := range repo.docketParams {
		if p.CourtRecordID != "graded-existing" {
			t.Errorf("docket attached to %q, want the existing graded record", p.CourtRecordID)
		}
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
	if conn.fetchCalls != 0 || repo.updateCalls != 0 {
		t.Errorf("guard failed: fetches=%d updateCalls=%d, want 0/0", conn.fetchCalls, repo.updateCalls)
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
	if repo.updateCalls != 0 {
		t.Errorf("grade mismatch still graded (updateCalls=%d), want 0", repo.updateCalls)
	}
}

// TestEnrichment_ResyncRefresh proves the re-poll path: a graded record observed
// again (same grade) refreshes its fields/movimentos WITHOUT changing the degree,
// re-pointing or superseding — GetCourtRecordByKey resolves the record to ITSELF, so
// the in-place update targets the same id.
func TestEnrichment_ResyncRefresh(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{
		existing:      &CourtRecord{ID: "graded-1", Degree: "G1"}, // the key resolves to the record itself
		existingFound: true,
	}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	ev := placeholderObserved()
	ev.Degree = "G1"              // already graded
	ev.CourtRecordID = "graded-1" // and IS the record the key lookup returns

	if err := enrichmentUnderTest(repo, outbox, uow, payload).OnCourtRecordObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}
	if repo.updateCalls != 1 || repo.updateParams.CourtRecordID != "graded-1" || repo.updateParams.Degree != "G1" {
		t.Errorf("graded refresh = %d calls on id %q degree %q; want 1 on graded-1 → G1",
			repo.updateCalls, repo.updateParams.CourtRecordID, repo.updateParams.Degree)
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
	if repo.updateCalls != 0 {
		t.Errorf("duplicate delivery still graded (updateCalls=%d), want 0", repo.updateCalls)
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
	if repo.updateCalls != 0 {
		t.Errorf("empty DATAJUD result still graded (updateCalls=%d), want 0", repo.updateCalls)
	}
}
