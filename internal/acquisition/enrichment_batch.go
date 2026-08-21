package acquisition

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/obs"
)

// enrichment_batch.go is the BATCH enrichment job: it replaces the old "1
// court_record_observed → 1 DATAJUD fetch (120/min)" per backfill-discovered process with
// a job in LOTE por (tenant, tribunal). One step scans up to enrichmentBatchScanLimit
// court_records of one tribunal still due, pulls ALL of them in a single _search `terms`
// request (≈1000 processes per request), grades every hit IN PLACE (reusing the same core
// as the single path) and MARKS every scanned record attempted — hit or not. It OWNS the
// import's capture_run ENRICHMENT row (RUNNING while it grades, closed OK/PARTIAL when the
// whole import's scan empties) and RESUMES itself: while work remains it re-enqueues a next
// step (pass+1, ProcessAt=now+carência) in the SAME tx (transactional outbox), so a crash
// resumes and a redelivery is a no-op (the scan reads only records not yet attempted).

// enrichmentBatchScanLimit is the per-step scan/fetch size: one DATAJUD `terms` page caps
// at ~1000 hits, and one request is one limiter token, so a 6.684-process import drains in
// ~7 requests/steps. Not env-tunable in Phase 1 — the ceiling is the DATAJUD page cap.
const enrichmentBatchScanLimit = 1000

// consumerEnrichmentBatch is this job's identity in processed_event: dedup is per (import,
// court, pass) via the step event id, so a redelivered step is a no-op.
const consumerEnrichmentBatch = "acquisition.enrichment_batch"

// CloseEnrichmentRunParams stamps the terminal status + finished_at on an import's
// ENRICHMENT capture row (the deterministic fecho when the scan drains).
type CloseEnrichmentRunParams struct {
	TenantID      string
	BackfillJobID string
	Status        string
	At            time.Time
}

// enrichmentTerminalStatus decides the terminal ENRICHMENT status PURELY from the count of
// REAL parse errors — hits DATAJUD returned but we could not parse. 0 errors → OK
// ("Concluída"), even when some processes were NOT FOUND in the DATAJUD index (0 hits): a
// process the public index simply does not have is EXPECTED, not a failure. Only a real error
// (hit present, unparseable) makes it PARTIAL ("Falha parcial"). Coverage (updated vs. total)
// is informational, not pass/fail.
func enrichmentTerminalStatus(errs int) string {
	if errs > 0 {
		return BackfillStatusPartial
	}
	return SyncStatusOK
}

// batchFetcher pulls MANY processes of one tribunal in as few requests as possible
// (the DATAJUD connector's FetchBatch). The narrow port keeps the job testable without HTTP.
type batchFetcher interface {
	FetchBatch(ctx context.Context, req FetchRequest) ([]RawPayload, error)
}

// batchParser maps a batch DATAJUD payload set into a numeroProcesso→process map AND the
// numeroProcessos SKIPPED by a real parse error (hits present but unparseable — distinct from
// numbers simply absent from the envelope). The DATAJUD parser's ParseBatch. Narrow port, no
// I/O in the test double.
type batchParser interface {
	ParseBatch(ctx context.Context, pages []RawPayload) (map[string]ParsedProcess, []string, error)
}

// enrichBatchRepo is the persistence port the batch job drives: the scan (SelectDue /
// CountRemaining), the attempt marker, the import counter (applied + real errors), the fecho
// (Close / InsertEmpty), and the tenant write lock. Grading itself is delegated to the shared
// EnrichmentUseCase.gradeInTx, which drives its own enrichRepo — so this port carries only what
// the batch orchestration adds on top of the grade.
type enrichBatchRepo interface {
	AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error
	SelectDueForEnrichment(ctx context.Context, tx database.Tx, tenantID, court string, limit int) ([]DueRecord, error)
	CountRemainingDueForJob(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (int, error)
	MarkEnrichmentAttempted(ctx context.Context, tx database.Tx, tenantID string, recordIDs []string, at time.Time) error
	IncrementImportEnrichmentRunBy(ctx context.Context, tx database.Tx, tenantID, backfillJobID string, delta, errs int, at time.Time) error
	GetImportEnrichmentCounter(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (updated, errs int, found bool, err error)
	CountImportDiscoveredRecords(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (int, error)
	CloseImportEnrichmentRun(ctx context.Context, tx database.Tx, params CloseEnrichmentRunParams) (rows int, err error)
	InsertEmptyImportEnrichmentRun(ctx context.Context, tx database.Tx, params CloseEnrichmentRunParams) error
}

// EnrichmentBatchUseCase reacts to enrichment_batch_requested. It owns the batch scan,
// fetch, grade (delegated to grade), the import's ENRICHMENT capture row lifecycle, and the
// self-re-enqueue. grade is the shared enrichment core (gradeInTx); fetcher/parser are the
// DATAJUD batch connector/parser (concrete, injected like the ingestion use case).
type EnrichmentBatchUseCase struct {
	repo    enrichBatchRepo
	grade   *EnrichmentUseCase
	fetcher batchFetcher
	parser  batchParser
	outbox  publisher
	uow     database.UnitOfWork
	now     func() time.Time
}

// NewEnrichmentBatchUseCase wires the batch enrichment job.
func NewEnrichmentBatchUseCase(repo enrichBatchRepo, grade *EnrichmentUseCase, fetcher batchFetcher, parser batchParser, outbox publisher, uow database.UnitOfWork) *EnrichmentBatchUseCase {
	return &EnrichmentBatchUseCase{
		repo:    repo,
		grade:   grade,
		fetcher: fetcher,
		parser:  parser,
		outbox:  outbox,
		uow:     uow,
		now:     time.Now,
	}
}

// OnEnrichmentBatchRequested runs one step of the (tenant, court, import) batch:
//
//  1. Scan up to enrichmentBatchScanLimit due records of this tribunal (system read inside
//     the tenant tx via the scan index).
//  2. If NONE remain for this tribunal, try to CLOSE the import: when the whole import's
//     scan (all tribunals) is empty, stamp the ENRICHMENT row's terminal status by coverage
//     (updated vs. discovered). If other tribunals still have due records, just stop — their
//     own batch steps will do the closing. No re-enqueue for an empty tribunal.
//  3. Otherwise FetchBatch (outside the tx — network) + ParseBatch, then in ONE unit of
//     work: grade every hit IN PLACE, mark EVERY scanned record attempted (hit or not),
//     bump the import counter by the graded count, and re-enqueue the NEXT step — all
//     atomically, so the step is resumable and idempotent.
func (uc *EnrichmentBatchUseCase) OnEnrichmentBatchRequested(ctx context.Context, ev EnrichmentBatchRequested) error {
	if ev.TenantID == "" || ev.Court == "" || ev.BackfillJobID == "" {
		return nil
	}

	ctx, span := obs.Start(ctx, "acquisition.enrichment_batch")
	defer span.End()

	// Read the due batch in a short tenant tx (the scan index serves it). Reading in its own
	// tx keeps the slow FetchBatch OUTSIDE any open transaction, exactly like the single path.
	var due []DueRecord
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		var derr error
		due, derr = uc.repo.SelectDueForEnrichment(ctx, tx, ev.TenantID, ev.Court, enrichmentBatchScanLimit)
		return derr
	})
	if err != nil {
		return err
	}

	if len(due) == 0 {
		return uc.closeIfImportDrained(ctx, ev)
	}

	numbers := make([]string, 0, len(due))
	for _, d := range due {
		numbers = append(numbers, d.CNJNumber)
	}

	pages, err := uc.fetcher.FetchBatch(ctx, FetchRequest{
		Capability: CapabilityFetchBatch,
		Court:      ev.Court,
		CNJNumbers: numbers,
	})
	if err != nil {
		// Retryable infra (rate-limit/transport): asynq re-delivers this step.
		return fmt.Errorf("datajud fetch batch %s (%d): %w", ev.Court, len(numbers), err)
	}
	byNumber, skipped, err := uc.parser.ParseBatch(ctx, pages)
	if err != nil {
		// Only a page whose ENVELOPE is unparseable reaches here (a per-hit failure is captured
		// in `skipped`, not returned). A malformed envelope never parses on retry; archive the
		// step — the scan still holds the unmarked records for a later re-emission. Wrapping
		// SkipRetry mirrors the single path's parse fault.
		return fmt.Errorf("parse datajud batch payload: %v: %w", err, asynq.SkipRetry)
	}

	return uc.applyBatch(ctx, ev, due, byNumber, len(skipped))
}

// applyBatch commits one step in a single unit of work: grade every hit IN PLACE (reusing
// the shared core), mark ALL scanned records attempted (hit or not, so the scan shrinks and
// the fecho can reach 0), bump the import's ENRICHMENT counter by the graded count AND by
// `parseErrs` (real parse errors this step saw), and re-enqueue the next step. All atomic
// under the tenant advisory lock. `parseErrs` — hits DATAJUD returned but we could not parse
// — is what makes the eventual fecho PARTIAL; a number simply NOT in the index (0 hits) is
// not counted here.
func (uc *EnrichmentBatchUseCase) applyBatch(ctx context.Context, ev EnrichmentBatchRequested, due []DueRecord, byNumber map[string]ParsedProcess, parseErrs int) error {
	graded := 0
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		// Lock FIRST: the grade write must not deadlock a concurrent sync on the same tenant,
		// and the counter UPSERT must be race-free per tenant.
		if err := uc.repo.AcquireTenantWriteLock(ctx, tx, ev.TenantID); err != nil {
			return err
		}

		// Dedup the STEP (per import/court/pass). A redelivery of the same step is a no-op —
		// the records it would touch are already marked attempted by the first delivery, so
		// even without this guard it would be a no-op, but dedup avoids re-fetching work and
		// re-enqueuing a duplicate next step.
		already, err := events.NewDedup(tx).SeenOrMark(ctx, consumerEnrichmentBatch, ev.EventID)
		if err != nil {
			return err
		}
		if already {
			return nil
		}

		attempted := make([]string, 0, len(due))
		for _, d := range due {
			attempted = append(attempted, d.ID)

			proc, ok := byNumber[d.CNJNumber]
			if !ok {
				// DATAJUD did not return this number (0 hits — not in the public index), or its
				// hit was skipped by a parse error. Either way it is still marked attempted so the
				// scan makes progress. A 0-hit is EXPECTED and does NOT count as an error (it does
				// not rebaixar the fecho); a real parse error was already counted in `parseErrs`.
				continue
			}
			if proc.Record.Degree == DegreeUnknown {
				continue
			}
			if _, _, gerr := uc.grade.gradeInTx(ctx, tx, ev.TenantID, d.ID, proc.Record, proc.Movimentos); gerr != nil {
				return gerr
			}
			graded++
		}

		// Mark EVERY scanned record attempted (graded or not) — the single write that removes
		// them from the scan index so the next step reads fresh work.
		if err := uc.repo.MarkEnrichmentAttempted(ctx, tx, ev.TenantID, attempted, uc.now()); err != nil {
			return err
		}

		// Bump the import's ENRICHMENT capture row by what this step graded AND by the real parse
		// errors it saw (the first step with any of the two opens the row RUNNING). Skip a step
		// that neither graded nor errored so it never mints a spurious RUNNING row.
		if graded > 0 || parseErrs > 0 {
			if err := uc.repo.IncrementImportEnrichmentRunBy(ctx, tx, ev.TenantID, ev.BackfillJobID, graded, parseErrs, uc.now()); err != nil {
				return err
			}
		}

		// Resume: re-enqueue the NEXT step (pass+1, ProcessAt=now+carência) in the SAME tx via
		// the outbox, so the job is resumable and drains until the scan empties. The next step
		// re-scans — if this was the last batch, it finds nothing and CLOSES the import.
		next := ev.nextEnrichmentBatchStep(uc.now())
		return uc.outbox.Publish(ctx, tx, next)
	})
	if err != nil {
		return err
	}
	if graded > 0 {
		recordEnrichmentBatchApplied(ctx, graded)
	}
	return nil
}

// closeIfImportDrained closes the import's ENRICHMENT capture row when NO record of the
// whole import (any tribunal) remains due — the deterministic fecho: "no untried record
// remains". The terminal status is decided PURELY by the accumulated REAL parse errors (hits
// DATAJUD returned but we could not parse): 0 → OK, >0 → PARTIAL. Processes DATAJUD does not
// have in its public index (0 hits) are EXPECTED — they are marked attempted, drop out of the
// remaining count, and do NOT rebaixar the status. `total` (discovered) and `updated`
// (graded) are read only for the informational nao_encontrados = total-updated log. When the
// import graded nothing AND saw no error (no ENRICHMENT row), it inserts an honest OK terminal
// row. If other tribunals still have due records, it does nothing — their batch steps close.
func (uc *EnrichmentBatchUseCase) closeIfImportDrained(ctx context.Context, ev EnrichmentBatchRequested) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if err := uc.repo.AcquireTenantWriteLock(ctx, tx, ev.TenantID); err != nil {
			return err
		}

		remaining, err := uc.repo.CountRemainingDueForJob(ctx, tx, ev.TenantID, ev.BackfillJobID)
		if err != nil {
			return err
		}
		if remaining > 0 {
			// Other tribunals of this import still have work; their own batch steps own the
			// close. This (empty) tribunal simply stops — no re-enqueue.
			return nil
		}

		total, err := uc.repo.CountImportDiscoveredRecords(ctx, tx, ev.TenantID, ev.BackfillJobID)
		if err != nil {
			return err
		}
		updated, errs, found, err := uc.repo.GetImportEnrichmentCounter(ctx, tx, ev.TenantID, ev.BackfillJobID)
		if err != nil {
			return err
		}

		params := CloseEnrichmentRunParams{
			TenantID:      ev.TenantID,
			BackfillJobID: ev.BackfillJobID,
			// Status is errors-only: not-found-in-DATAJUD is expected, not a failure.
			Status: enrichmentTerminalStatus(errs),
			At:     uc.now(),
		}
		if !found {
			// The import graded nothing AND saw no parse error (no ENRICHMENT row was ever
			// opened): record an honest terminal row (updated=0, errors=0 → OK). ON CONFLICT DO
			// NOTHING guards a late step racing in.
			return uc.repo.InsertEmptyImportEnrichmentRun(ctx, tx, params)
		}
		if _, err := uc.repo.CloseImportEnrichmentRun(ctx, tx, params); err != nil {
			return err
		}
		notFound := total - updated
		if notFound < 0 {
			notFound = 0
		}
		slog.InfoContext(ctx, "acquisition: batch enrichment closed",
			obs.KeyTenantID, ev.TenantID, "backfill_job_id", ev.BackfillJobID,
			"status", params.Status, "graded", updated, "errors", errs, "not_found_in_datajud", notFound)
		return nil
	})
}
