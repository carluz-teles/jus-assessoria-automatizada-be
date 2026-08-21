package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// fakeBatchRepo records the batch job's orchestration calls and answers the scan from a
// preset. It drives the fecho (Count/Close/InsertEmpty) and the counter (IncrementBy) too.
type fakeBatchRepo struct {
	due          []DueRecord // returned by the first SelectDueForEnrichment, then drained to nil
	selectCalls  int
	remaining    int // CountRemainingDueForJob result
	discovered   int // CountImportDiscoveredRecords result
	counter      int // GetImportEnrichmentCounter updated value
	errCounter   int // GetImportEnrichmentCounter errors value (drives OK vs PARTIAL)
	counterFound bool

	markedIDs     []string
	incrementByBy int // accumulated graded delta
	incrementErrs int // accumulated real-error delta
	incrementRuns int
	closeStatus   string
	closeCalls    int
	insertEmpty   int
	insertStatus  string
}

func (r *fakeBatchRepo) AcquireTenantWriteLock(_ context.Context, _ database.Tx, _ string) error {
	return nil
}

func (r *fakeBatchRepo) SelectDueForEnrichment(_ context.Context, _ database.Tx, _, _ string, _ int) ([]DueRecord, error) {
	r.selectCalls++
	if r.selectCalls == 1 {
		return r.due, nil
	}
	return nil, nil // subsequent scans are empty (the batch drained the tribunal)
}

func (r *fakeBatchRepo) CountRemainingDueForJob(_ context.Context, _ database.Tx, _, _ string) (int, error) {
	return r.remaining, nil
}

func (r *fakeBatchRepo) MarkEnrichmentAttempted(_ context.Context, _ database.Tx, _ string, ids []string, _ time.Time) error {
	r.markedIDs = append(r.markedIDs, ids...)
	return nil
}

func (r *fakeBatchRepo) IncrementImportEnrichmentRunBy(_ context.Context, _ database.Tx, _, _ string, delta, errs int, _ time.Time) error {
	r.incrementRuns++
	r.incrementByBy += delta
	r.incrementErrs += errs
	return nil
}

func (r *fakeBatchRepo) GetImportEnrichmentCounter(_ context.Context, _ database.Tx, _, _ string) (int, int, bool, error) {
	return r.counter, r.errCounter, r.counterFound, nil
}

func (r *fakeBatchRepo) CountImportDiscoveredRecords(_ context.Context, _ database.Tx, _, _ string) (int, error) {
	return r.discovered, nil
}

func (r *fakeBatchRepo) CloseImportEnrichmentRun(_ context.Context, _ database.Tx, p CloseEnrichmentRunParams) (int, error) {
	r.closeCalls++
	r.closeStatus = p.Status
	return 1, nil
}

func (r *fakeBatchRepo) InsertEmptyImportEnrichmentRun(_ context.Context, _ database.Tx, p CloseEnrichmentRunParams) error {
	r.insertEmpty++
	r.insertStatus = p.Status
	return nil
}

// fakeBatchFetcher/Parser stand in for the DATAJUD batch connector+parser. The parser
// returns a preset map keyed by numeroProcesso (a missing number = no hit).
type fakeBatchFetcher struct {
	calls   int
	gotNums []string
}

func (f *fakeBatchFetcher) FetchBatch(_ context.Context, req FetchRequest) ([]RawPayload, error) {
	f.calls++
	f.gotNums = req.CNJNumbers
	return []RawPayload{{Source: SourceDATAJUD}}, nil
}

type fakeBatchParser struct {
	byNumber map[string]ParsedProcess
	skipped  []string // numeroProcessos skipped by a real parse error
}

func (p *fakeBatchParser) ParseBatch(_ context.Context, _ []RawPayload) (map[string]ParsedProcess, []string, error) {
	return p.byNumber, p.skipped, nil
}

func batchEvent() EnrichmentBatchRequested {
	return newEnrichmentBatchRequested(testTenant, "TJSP", "backfill-1")
}

// gradeUC builds a shared enrichment use case whose gradeInTx the batch reuses, backed by a
// fakeEnrichRepo (grade-in-place, no conflict) unless the caller sets one up.
func gradeUC(repo enrichRepo) *EnrichmentUseCase {
	return NewEnrichmentUseCase(repo, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}}, NewOrchestrator(), NewDATAJUDParser())
}

// TestEnrichmentBatch_ScanFetchApply proves one step: it scans the tribunal, fetches by the
// due numbers, grades every hit IN PLACE, marks ALL scanned records attempted (hit or not),
// bumps the import counter by the graded count, and re-enqueues the next step.
func TestEnrichmentBatch_ScanFetchApply(t *testing.T) {
	t.Parallel()

	repo := &fakeBatchRepo{
		due: []DueRecord{
			{ID: "rec-a", CNJNumber: "111", Court: "TJSP"},
			{ID: "rec-b", CNJNumber: "222", Court: "TJSP"}, // DATAJUD will NOT return this one
		},
		remaining: 5, // still work left → will re-enqueue, not close
	}
	fetcher := &fakeBatchFetcher{}
	parser := &fakeBatchParser{byNumber: map[string]ParsedProcess{
		"111": {Record: ParsedCourtRecord{CNJNumber: "111", Degree: "G1", Court: "TJSP"}},
	}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 1}}
	grade := gradeUC(&fakeEnrichRepo{})

	uc := NewEnrichmentBatchUseCase(repo, grade, fetcher, parser, outbox, uow)
	if err := uc.OnEnrichmentBatchRequested(context.Background(), batchEvent()); err != nil {
		t.Fatalf("OnEnrichmentBatchRequested: %v", err)
	}

	if fetcher.calls != 1 || len(fetcher.gotNums) != 2 {
		t.Errorf("fetch = %d calls, %d numbers; want 1 call on both due numbers", fetcher.calls, len(fetcher.gotNums))
	}
	// EVERY scanned record marked attempted — the hit AND the no-hit.
	if len(repo.markedIDs) != 2 {
		t.Errorf("marked attempted = %v, want both rec-a and rec-b", repo.markedIDs)
	}
	// Only the graded one bumps the applied counter; the no-hit (222) is NOT an error (not in
	// the index), so errors stays 0.
	if repo.incrementRuns != 1 || repo.incrementByBy != 1 || repo.incrementErrs != 0 {
		t.Errorf("counter bump = {runs:%d by:%d errs:%d}, want {1 1 0}", repo.incrementRuns, repo.incrementByBy, repo.incrementErrs)
	}
	// Still work remaining → re-enqueues the next step, does NOT close.
	if got := countByType(outbox.published)[TypeEnrichmentBatchRequested]; got != 1 {
		t.Errorf("next-step re-enqueue = %d, want 1", got)
	}
	if repo.closeCalls != 0 || repo.insertEmpty != 0 {
		t.Errorf("must not close while work remains: close=%d insertEmpty=%d", repo.closeCalls, repo.insertEmpty)
	}
}

// TestEnrichmentBatch_ClosesOKWhenNotAllFound proves the CORRECTED semantics (the real import
// case): the scan drains with SOME processes not found in DATAJUD (updated < discovered) but
// ZERO real parse errors → the fecho is OK ("Concluída"), NOT PARTIAL. Not-in-the-index is
// expected, not a failure.
func TestEnrichmentBatch_ClosesOKWhenNotAllFound(t *testing.T) {
	t.Parallel()

	repo := &fakeBatchRepo{
		due:          nil, // nothing due for this tribunal
		remaining:    0,   // and nothing due for the whole import → close
		discovered:   10,
		counter:      7, // only 7 of 10 graded (3 simply not in DATAJUD)…
		errCounter:   0, // …but ZERO real errors → OK
		counterFound: true,
	}
	outbox := &fakeOutbox{}
	uc := NewEnrichmentBatchUseCase(repo, gradeUC(&fakeEnrichRepo{}), &fakeBatchFetcher{}, &fakeBatchParser{}, outbox, &stubBackfillUoW{tx: stubTx{rows: 1}})

	if err := uc.OnEnrichmentBatchRequested(context.Background(), batchEvent()); err != nil {
		t.Fatalf("OnEnrichmentBatchRequested: %v", err)
	}
	if repo.closeCalls != 1 || repo.closeStatus != SyncStatusOK {
		t.Errorf("close = {calls:%d status:%q}, want {1 OK} (not-found is not a failure)", repo.closeCalls, repo.closeStatus)
	}
	if got := countByType(outbox.published)[TypeEnrichmentBatchRequested]; got != 0 {
		t.Errorf("empty scan must NOT re-enqueue, got %d", got)
	}
}

// TestEnrichmentBatch_ClosesPartialOnRealErrors proves PARTIAL is driven ONLY by real parse
// errors: the scan drains, everything discovered was found, but the accumulated errors > 0
// (hits DATAJUD returned but we could not parse) → PARTIAL.
func TestEnrichmentBatch_ClosesPartialOnRealErrors(t *testing.T) {
	t.Parallel()

	repo := &fakeBatchRepo{
		remaining:    0,
		discovered:   10,
		counter:      8,
		errCounter:   2, // 2 real parse errors → PARTIAL
		counterFound: true,
	}
	uc := NewEnrichmentBatchUseCase(repo, gradeUC(&fakeEnrichRepo{}), &fakeBatchFetcher{}, &fakeBatchParser{}, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}})

	if err := uc.OnEnrichmentBatchRequested(context.Background(), batchEvent()); err != nil {
		t.Fatalf("OnEnrichmentBatchRequested: %v", err)
	}
	if repo.closeStatus != BackfillStatusPartial {
		t.Errorf("close status = %q, want PARTIAL (real parse errors)", repo.closeStatus)
	}
}

// TestEnrichmentBatch_EmptyImportInsertsHonestRow proves an import that graded nothing AND saw
// no error (no ENRICHMENT row) inserts an honest OK terminal row (0 errors → OK, even with
// discovered>0 — those processes simply are not in DATAJUD).
func TestEnrichmentBatch_EmptyImportInsertsHonestRow(t *testing.T) {
	t.Parallel()

	repo := &fakeBatchRepo{remaining: 0, discovered: 3, counterFound: false} // no counter row
	uc := NewEnrichmentBatchUseCase(repo, gradeUC(&fakeEnrichRepo{}), &fakeBatchFetcher{}, &fakeBatchParser{}, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}})

	if err := uc.OnEnrichmentBatchRequested(context.Background(), batchEvent()); err != nil {
		t.Fatalf("OnEnrichmentBatchRequested: %v", err)
	}
	if repo.insertEmpty != 1 || repo.closeCalls != 0 {
		t.Errorf("empty import: insertEmpty=%d close=%d, want 1/0", repo.insertEmpty, repo.closeCalls)
	}
	// 0 graded, 0 errors → OK (the 3 discovered simply are not in DATAJUD, not a failure).
	if repo.insertStatus != SyncStatusOK {
		t.Errorf("insert status = %q, want OK (0 errors, not-found is not a failure)", repo.insertStatus)
	}
}

// TestEnrichmentBatch_SkippedHitsBumpErrors proves a step whose parser reported skipped hits
// (real parse errors) increments capture_run.errors by the skip count — even for records that
// were also graded — so the eventual fecho lands PARTIAL.
func TestEnrichmentBatch_SkippedHitsBumpErrors(t *testing.T) {
	t.Parallel()

	repo := &fakeBatchRepo{
		due: []DueRecord{
			{ID: "rec-a", CNJNumber: "111", Court: "TJSP"},
			{ID: "rec-b", CNJNumber: "222", Court: "TJSP"}, // a hit came back but was unparseable
		},
		remaining: 5,
	}
	parser := &fakeBatchParser{
		byNumber: map[string]ParsedProcess{
			"111": {Record: ParsedCourtRecord{CNJNumber: "111", Degree: "G1", Court: "TJSP"}},
		},
		skipped: []string{"222"}, // 222 returned a hit we could not parse → real error
	}
	uc := NewEnrichmentBatchUseCase(repo, gradeUC(&fakeEnrichRepo{}), &fakeBatchFetcher{}, parser, &fakeOutbox{}, &stubBackfillUoW{tx: stubTx{rows: 1}})

	if err := uc.OnEnrichmentBatchRequested(context.Background(), batchEvent()); err != nil {
		t.Fatalf("OnEnrichmentBatchRequested: %v", err)
	}
	// 1 graded, 1 real error → both counters bumped in the same step.
	if repo.incrementByBy != 1 || repo.incrementErrs != 1 {
		t.Errorf("counter bump = {graded:%d errs:%d}, want {1 1}", repo.incrementByBy, repo.incrementErrs)
	}
	// Both scanned records marked attempted (graded and skipped-error alike).
	if len(repo.markedIDs) != 2 {
		t.Errorf("marked attempted = %v, want both records", repo.markedIDs)
	}
}

// TestEnrichmentBatch_OtherTribunalStillDueDoesNotClose proves an empty tribunal whose import
// still has due records in OTHER tribunals does NOT close — those tribunals' steps will.
func TestEnrichmentBatch_OtherTribunalStillDueDoesNotClose(t *testing.T) {
	t.Parallel()

	repo := &fakeBatchRepo{due: nil, remaining: 4} // this tribunal empty, but import has work
	outbox := &fakeOutbox{}
	uc := NewEnrichmentBatchUseCase(repo, gradeUC(&fakeEnrichRepo{}), &fakeBatchFetcher{}, &fakeBatchParser{}, outbox, &stubBackfillUoW{tx: stubTx{rows: 1}})

	if err := uc.OnEnrichmentBatchRequested(context.Background(), batchEvent()); err != nil {
		t.Fatalf("OnEnrichmentBatchRequested: %v", err)
	}
	if repo.closeCalls != 0 || repo.insertEmpty != 0 {
		t.Errorf("must not close while other tribunals have work: close=%d insertEmpty=%d", repo.closeCalls, repo.insertEmpty)
	}
}

// TestEnrichmentBatch_DuplicateStepIsNoOp proves a redelivered step (SeenOrMark reports seen)
// grades nothing and re-enqueues nothing — no loop.
func TestEnrichmentBatch_DuplicateStepIsNoOp(t *testing.T) {
	t.Parallel()

	repo := &fakeBatchRepo{
		due:       []DueRecord{{ID: "rec-a", CNJNumber: "111", Court: "TJSP"}},
		remaining: 5,
	}
	parser := &fakeBatchParser{byNumber: map[string]ParsedProcess{
		"111": {Record: ParsedCourtRecord{CNJNumber: "111", Degree: "G1", Court: "TJSP"}},
	}}
	outbox := &fakeOutbox{}
	uow := &stubBackfillUoW{tx: stubTx{rows: 0}} // 0 rows = already seen
	uc := NewEnrichmentBatchUseCase(repo, gradeUC(&fakeEnrichRepo{}), &fakeBatchFetcher{}, parser, outbox, uow)

	if err := uc.OnEnrichmentBatchRequested(context.Background(), batchEvent()); err != nil {
		t.Fatalf("OnEnrichmentBatchRequested: %v", err)
	}
	if len(repo.markedIDs) != 0 || repo.incrementRuns != 0 {
		t.Errorf("duplicate step did work: marked=%v increments=%d", repo.markedIDs, repo.incrementRuns)
	}
	if got := countByType(outbox.published)[TypeEnrichmentBatchRequested]; got != 0 {
		t.Errorf("duplicate step re-enqueued (%d), want 0", got)
	}
}

// TestEnrichmentBatchStep_FreshEventIDPerPass proves the self-re-enqueue mints a NEW event id
// per pass (so the relay's global TaskID does not drop the follow-up) while the aggregate is
// stable, and the next step opts into a future ProcessAt.
func TestEnrichmentBatchStep_FreshEventIDPerPass(t *testing.T) {
	t.Parallel()

	step0 := newEnrichmentBatchRequested(testTenant, "TJSP", "job-1")
	if _, sched := step0.ProcessAt(); sched {
		t.Error("the first step must deliver immediately (no ProcessAt)")
	}

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	step1 := step0.nextEnrichmentBatchStep(now)
	if step1.EventID == step0.EventID {
		t.Errorf("step event id did not change across passes: %q", step1.EventID)
	}
	if step1.Aggregate != step0.Aggregate {
		t.Errorf("aggregate must be stable across passes: %q vs %q", step1.Aggregate, step0.Aggregate)
	}
	if step1.Pass != 1 {
		t.Errorf("pass = %d, want 1", step1.Pass)
	}
	at, sched := step1.ProcessAt()
	if !sched || !at.Equal(now.Add(enrichmentBatchDelay)) {
		t.Errorf("next step ProcessAt = (%s, %v), want now+delay scheduled", at, sched)
	}
}
