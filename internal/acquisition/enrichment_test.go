package acquisition

import (
	"context"
	"os"
	"testing"
	"time"

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

	// oldLifecycle is the PRE-update lifecycle UpdateCourtRecordGrade reports (Achado 2).
	// "" by default — never equals ARCHIVED/SUPERSEDED, so existing tests that do not care
	// about the transition see no spurious court_record_archived event.
	oldLifecycle string

	repointFrom  string
	repointTo    string
	repointCalls int

	repointDeadlinesFrom  string
	repointDeadlinesTo    string
	repointDeadlinesCalls int

	supersedeID   string
	supersedeCall int
	// supersedeAlreadyDone simulates a replay against an already-SUPERSEDED placeholder
	// (the guarded UPDATE's "0 rows" case): when true, SupersedeCourtRecord reports NO
	// transition, so gradeInTx must not emit court_record_superseded.
	supersedeAlreadyDone bool

	docketParams []DocketEntryParams

	enrichRunCalls      int
	enrichRunBackfillID string

	// Responsável re-sync on merge (gradeInTx's CascadeCaseResponsibleToIntimations +
	// GetCaseAssignedUser). caseAssignedID backs the GetCaseAssignedUser read; the rest
	// capture what the cascade call was given.
	caseAssignedID  *string
	caseAssignedErr error
	cascadeCalls    int
	cascadeCaseID   string
	cascadeUser     *string
}

func (r *fakeEnrichRepo) AcquireTenantWriteLock(_ context.Context, _ database.Tx, _ string) error {
	return nil
}

func (r *fakeEnrichRepo) GetCourtRecordByKey(_ context.Context, _ database.Tx, _, _, _ string) (*CourtRecord, bool, error) {
	r.getByKeyCalls++
	return r.existing, r.existingFound, nil
}

func (r *fakeEnrichRepo) UpdateCourtRecordGrade(_ context.Context, _ database.Tx, p GradeParams) (*GradedCourtRecord, error) {
	r.updateCalls++
	r.updateParams = p
	// Mirrors the real SQL's CASE: a SUPERSEDED old lifecycle is sticky; otherwise the new
	// grade's lifecycle wins when non-empty, falling back to the old one (COALESCE).
	newLifecycle := r.oldLifecycle
	switch {
	case r.oldLifecycle == LifecycleSuperseded:
		newLifecycle = LifecycleSuperseded
	case p.Lifecycle != "":
		newLifecycle = p.Lifecycle
	}
	// Grade in place: the returned record keeps the id it was asked to mutate.
	return &GradedCourtRecord{ID: p.CourtRecordID, OldLifecycle: r.oldLifecycle, Lifecycle: newLifecycle}, nil
}

func (r *fakeEnrichRepo) RepointIntimations(_ context.Context, _ database.Tx, _, from, to string) (int, error) {
	r.repointCalls++
	r.repointFrom, r.repointTo = from, to
	return 1, nil
}

func (r *fakeEnrichRepo) RepointDeadlines(_ context.Context, _ database.Tx, _, from, to string) (int, error) {
	r.repointDeadlinesCalls++
	r.repointDeadlinesFrom, r.repointDeadlinesTo = from, to
	return 1, nil
}

func (r *fakeEnrichRepo) SupersedeCourtRecord(_ context.Context, _ database.Tx, _, id string) (bool, error) {
	r.supersedeCall++
	r.supersedeID = id
	return !r.supersedeAlreadyDone, nil
}

func (r *fakeEnrichRepo) UpsertDocketEntries(_ context.Context, _ database.Tx, params []DocketEntryParams) ([]DocketEntry, error) {
	r.docketParams = params
	out := make([]DocketEntry, 0, len(params))
	for _, p := range params {
		out = append(out, DocketEntry{ID: "de-" + p.Hash, CourtRecordID: p.CourtRecordID, Hash: p.Hash})
	}
	return out, nil
}

func (r *fakeEnrichRepo) CascadeCaseResponsibleToIntimations(_ context.Context, _ database.Tx, _, caseID string, assignedUserID *string) (int64, error) {
	r.cascadeCalls++
	r.cascadeCaseID = caseID
	r.cascadeUser = assignedUserID
	return 0, nil
}

func (r *fakeEnrichRepo) GetCaseAssignedUser(_ context.Context, _ database.Tx, _, _ string) (*string, error) {
	return r.caseAssignedID, r.caseAssignedErr
}

func (r *fakeEnrichRepo) IncrementImportEnrichmentRun(_ context.Context, _ database.Tx, _, backfillJobID string, _ time.Time) error {
	r.enrichRunCalls++
	r.enrichRunBackfillID = backfillJobID
	return nil
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
	// A live enrichment (no BackfillJobID) has no import to attribute to → the import's
	// ENRICHMENT capture row is NOT bumped (the batch job owns that counter now).
	if repo.enrichRunCalls != 0 {
		t.Errorf("IncrementImportEnrichmentRun calls = %d, want 0 (single path never bumps the counter)", repo.enrichRunCalls)
	}
}

// TestEnrichment_BackfillIsNoOp proves the anti-double guard: a record discovered by an
// onboarding backfill (BackfillJobID set) is enriched by the BATCH job (one _search per
// tribunal), so the per-record consumer early-returns — no fetch, no grade, no docket.
// This is what prevents double-enrichment (the same process pulled twice).
func TestEnrichment_BackfillIsNoOp(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{}
	conn := &stubConnector{id: "datajud", payload: RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}}
	orch := NewOrchestrator()
	orch.Register(SourceDATAJUD, conn)
	uc := NewEnrichmentUseCase(repo, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}}, orch, NewDATAJUDParser())

	ev := placeholderObserved()
	ev.BackfillJobID = "backfill-9" // discovered by an onboarding backfill → batch owns it

	if err := uc.OnCourtRecordObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}
	if conn.fetchCalls != 0 || repo.updateCalls != 0 {
		t.Errorf("backfill record must be a no-op in the per-record path: fetches=%d updateCalls=%d, want 0/0",
			conn.fetchCalls, repo.updateCalls)
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

// TestEnrichment_ConflictMerge_ReassignsResponsibleToTargetCase proves the merge branch
// re-syncs the DESTINATION case's responsável onto the intimações RepointIntimations just
// moved: after the merge, GetCaseAssignedUser is read for existing.CaseID (the target
// case, NOT the placeholder's origin case) and CascadeCaseResponsibleToIntimations is
// called with exactly that (caseID, assignee) pair.
func TestEnrichment_ConflictMerge_ReassignsResponsibleToTargetCase(t *testing.T) {
	t.Parallel()

	targetUser := "user-target"
	repo := &fakeEnrichRepo{
		existing:       &CourtRecord{ID: "graded-existing", Degree: "G1", CaseID: "case-target"},
		existingFound:  true,
		caseAssignedID: &targetUser,
	}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	payload := RawPayload{Source: SourceDATAJUD, Body: datajudFixtureBytes(t)}

	err := enrichmentUnderTest(repo, outbox, uow, payload).OnCourtRecordObserved(context.Background(), placeholderObserved())
	if err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}

	if repo.cascadeCalls != 1 {
		t.Fatalf("cascade calls = %d, want 1", repo.cascadeCalls)
	}
	if repo.cascadeCaseID != "case-target" {
		t.Errorf("cascade caseID = %q, want the DESTINATION case %q", repo.cascadeCaseID, "case-target")
	}
	if repo.cascadeUser == nil || *repo.cascadeUser != targetUser {
		t.Errorf("cascade user = %v, want %q (read from GetCaseAssignedUser on the target case)", repo.cascadeUser, targetUser)
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

	// A LIVE re-poll (no BackfillJobID) redelivered: the dedup guard makes it a no-op.
	ev := placeholderObserved()

	if err := enrichmentUnderTest(repo, &fakeOutbox{}, uow, payload).OnCourtRecordObserved(context.Background(), ev); err != nil {
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

// ─── Achado 2 (lifecycle reconciliation, fatia 2a) — gradeInTx's transition detection ───
// These call gradeInTx directly (unexported, same-package test) since the transition
// logic lives entirely inside it — no fetch/parse plumbing needed.

// TestGradeInTx_ArchivedTransition_PublishesEvent proves the core detection: a REAL
// ARCHIVED transition (old lifecycle was NOT archived, the new grade IS) publishes
// exactly one acquisition.court_record_archived keyed on the graded record's id.
func TestGradeInTx_ArchivedTransition_PublishesEvent(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{oldLifecycle: LifecycleActive}
	outbox := &fakeOutbox{}
	uc := NewEnrichmentUseCase(repo, outbox, nil, nil, nil)

	graded := ParsedCourtRecord{CNJNumber: "cnj-1", Degree: "G1", Lifecycle: LifecycleArchived}
	if _, _, err := uc.gradeInTx(context.Background(), stubTx{}, testTenant, "cr-1", graded, nil); err != nil {
		t.Fatalf("gradeInTx: %v", err)
	}

	if got := countByType(outbox.published)[TypeCourtRecordArchived]; got != 1 {
		t.Fatalf("court_record_archived events = %d, want 1", got)
	}
	for _, ev := range outbox.published {
		archived, ok := ev.(CourtRecordArchived)
		if !ok {
			continue
		}
		if archived.CourtRecordID != "cr-1" || archived.TenantID != testTenant {
			t.Errorf("event = %+v, want court_record_id=cr-1 tenant=%s", archived, testTenant)
		}
	}
}

// TestGradeInTx_ArchivedNoChange_NoEvent proves idempotency: a re-poll that finds the
// record ALREADY archived (old==new==ARCHIVED) must NOT re-publish — no event storm on
// every scheduler re-poll of an already-concluded process.
func TestGradeInTx_ArchivedNoChange_NoEvent(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{oldLifecycle: LifecycleArchived}
	outbox := &fakeOutbox{}
	uc := NewEnrichmentUseCase(repo, outbox, nil, nil, nil)

	graded := ParsedCourtRecord{CNJNumber: "cnj-1", Degree: "G1", Lifecycle: LifecycleArchived}
	if _, _, err := uc.gradeInTx(context.Background(), stubTx{}, testTenant, "cr-1", graded, nil); err != nil {
		t.Fatalf("gradeInTx: %v", err)
	}

	if got := countByType(outbox.published)[TypeCourtRecordArchived]; got != 0 {
		t.Errorf("re-poll with unchanged ARCHIVED lifecycle published %d court_record_archived events, want 0", got)
	}
}

// TestGradeInTx_NonArchivedTransition_NoEvent proves the event is scoped to the
// ARCHIVED transition specifically: an ACTIVE→SUSPENDED (or any other) change is a real
// transition but not the one Achado 2 cares about, so it publishes no
// court_record_archived either.
func TestGradeInTx_NonArchivedTransition_NoEvent(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{oldLifecycle: LifecycleActive}
	outbox := &fakeOutbox{}
	uc := NewEnrichmentUseCase(repo, outbox, nil, nil, nil)

	graded := ParsedCourtRecord{CNJNumber: "cnj-1", Degree: "G1", Lifecycle: LifecycleSuspended}
	if _, _, err := uc.gradeInTx(context.Background(), stubTx{}, testTenant, "cr-1", graded, nil); err != nil {
		t.Fatalf("gradeInTx: %v", err)
	}

	if got := countByType(outbox.published)[TypeCourtRecordArchived]; got != 0 {
		t.Errorf("ACTIVE→SUSPENDED published %d court_record_archived events, want 0", got)
	}
}

// TestGradeInTx_MergeSupersedes_RepointsDeadlinesAndPublishesEvent proves the merge
// path's Achado 2 addition: RepointDeadlines runs alongside RepointIntimations (before
// SupersedeCourtRecord, same tx — the production leak this closes: deadlines orphaned on
// a SUPERSEDED placeholder), and a REAL supersede (the guarded UPDATE touched a row)
// publishes exactly one acquisition.court_record_superseded keyed on the RETIRING
// placeholder's id.
func TestGradeInTx_MergeSupersedes_RepointsDeadlinesAndPublishesEvent(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{
		existing:      &CourtRecord{ID: "graded-existing", Degree: "G1"},
		existingFound: true,
	}
	outbox := &fakeOutbox{}
	uc := NewEnrichmentUseCase(repo, outbox, nil, nil, nil)

	graded := ParsedCourtRecord{CNJNumber: "cnj-1", Degree: "G1"}
	if _, _, err := uc.gradeInTx(context.Background(), stubTx{}, testTenant, "placeholder-1", graded, nil); err != nil {
		t.Fatalf("gradeInTx: %v", err)
	}

	if repo.repointDeadlinesCalls != 1 || repo.repointDeadlinesFrom != "placeholder-1" || repo.repointDeadlinesTo != "graded-existing" {
		t.Errorf("RepointDeadlines = %d calls, %s→%s; want 1, placeholder-1→graded-existing",
			repo.repointDeadlinesCalls, repo.repointDeadlinesFrom, repo.repointDeadlinesTo)
	}
	if got := countByType(outbox.published)[TypeCourtRecordSuperseded]; got != 1 {
		t.Fatalf("court_record_superseded events = %d, want 1", got)
	}
	for _, ev := range outbox.published {
		superseded, ok := ev.(CourtRecordSuperseded)
		if !ok {
			continue
		}
		if superseded.CourtRecordID != "placeholder-1" || superseded.TenantID != testTenant {
			t.Errorf("event = %+v, want court_record_id=placeholder-1 tenant=%s", superseded, testTenant)
		}
	}
}

// TestGradeInTx_MergeReplay_NoSupersededEvent proves the guard's idempotency: a
// redelivery against an already-SUPERSEDED placeholder (SupersedeCourtRecord's guarded
// UPDATE touches 0 rows) must NOT re-publish court_record_superseded.
func TestGradeInTx_MergeReplay_NoSupersededEvent(t *testing.T) {
	t.Parallel()

	repo := &fakeEnrichRepo{
		existing:             &CourtRecord{ID: "graded-existing", Degree: "G1"},
		existingFound:        true,
		supersedeAlreadyDone: true,
	}
	outbox := &fakeOutbox{}
	uc := NewEnrichmentUseCase(repo, outbox, nil, nil, nil)

	graded := ParsedCourtRecord{CNJNumber: "cnj-1", Degree: "G1"}
	if _, _, err := uc.gradeInTx(context.Background(), stubTx{}, testTenant, "placeholder-1", graded, nil); err != nil {
		t.Fatalf("gradeInTx: %v", err)
	}

	if got := countByType(outbox.published)[TypeCourtRecordSuperseded]; got != 0 {
		t.Errorf("replay published %d court_record_superseded events, want 0", got)
	}
}
