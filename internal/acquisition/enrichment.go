package acquisition

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// enrichment.go is the consumer of acquisition.court_record_observed: when DJEN
// discovers a process it lands a court_record with degree=UNKNOWN (DJEN never
// discloses the grau). This use case fetches that process from DATAJUD by number,
// which reveals the grade and the court's own view (classe/assunto/órgão/
// ajuizamento/sigilo/movimentos), and GRADES THE PLACEHOLDER IN PLACE: it mutates
// the existing court_record (ev.CourtRecordID) to the real degree + DATAJUD fields
// on the SAME id, so the intimations, deadlines and docket already anchored to it
// stay correct — zero orphan, zero duplicate (FIX B). The one row it cannot mutate
// in place is a rare pre-existing graded record at that grade (the (tenant, cnj,
// degree) UNIQUE): there it MERGES the placeholder's intimations onto the graded
// record and retires the placeholder. DATAJUD is enrichment-only — it never
// discovers — so it is not an activatable integration; this event is its sole trigger.

// consumerEnrichment is this listener's identity in processed_event (per-consumer
// dedup, independent of the sync consumer that produced the event).
const consumerEnrichment = "acquisition.enrichment"

// GradeParams grades ONE court_record in place: it names the row to mutate
// (CourtRecordID — the DJEN UNKNOWN placeholder) and the DATAJUD-authoritative
// fields to write onto that same id. It is not entitlement-gated (grading an
// already-tracked process consumes no new plan slot). NextSyncAt seeds the re-poll
// schedule only when the row still has none; an existing schedule (the scheduler's
// claim) is preserved.
type GradeParams struct {
	TenantID      string
	CourtRecordID string
	Degree        string
	Court         string
	Class         string
	Subject       string
	JudgingBody   string
	FiledAt       time.Time
	Secrecy       string
	Completeness  float32
	NextSyncAt    time.Time
	// Lifecycle is the process-liveness state derived from DATAJUD movimentos by
	// lifecycleFromMovimentos. The SQL UPDATE uses CASE to ensure an already-SUPERSEDED
	// row is never downgraded to ACTIVE/ARCHIVED/SUSPENDED: superseding is a structural
	// merge operation, not a lifecycle transition, and must be preserved.
	Lifecycle string
}

// enrichRepo is the narrow persistence port the enrichment use case drives: grading
// the placeholder in place (UpdateCourtRecordGrade), the conflict lookup that guards
// the (tenant, cnj, degree) UNIQUE (GetCourtRecordByKey), the merge fallback for that
// rare conflict (re-point + supersede), and the movimento upsert.
type enrichRepo interface {
	AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error
	GetCourtRecordByKey(ctx context.Context, tx database.Tx, tenantID, cnjNumber, degree string) (record *CourtRecord, found bool, err error)
	UpdateCourtRecordGrade(ctx context.Context, tx database.Tx, params GradeParams) (*CourtRecord, error)
	RepointIntimations(ctx context.Context, tx database.Tx, tenantID, fromRecordID, toRecordID string) (moved int, err error)
	SupersedeCourtRecord(ctx context.Context, tx database.Tx, tenantID, recordID string) error
	UpsertDocketEntries(ctx context.Context, tx database.Tx, params []DocketEntryParams) (newEntries []DocketEntry, err error)
}

// EnrichmentUseCase reacts to court_record_observed by running one DATAJUD
// enrichment. It depends on the narrow enrichRepo, the outbox, the unit of work,
// the connector orchestrator (resolves the DATAJUD connector), and the parser port
// (a ParserSet routing to the DATAJUD parser) — never on a concrete data source.
type EnrichmentUseCase struct {
	repo         enrichRepo
	outbox       publisher
	uow          database.UnitOfWork
	orchestrator *Orchestrator
	parser       Parser
	now          func() time.Time
}

// NewEnrichmentUseCase wires the enrichment use case.
func NewEnrichmentUseCase(repo enrichRepo, outbox publisher, uow database.UnitOfWork, orchestrator *Orchestrator, parser Parser) *EnrichmentUseCase {
	return &EnrichmentUseCase{
		repo:         repo,
		outbox:       outbox,
		uow:          uow,
		orchestrator: orchestrator,
		parser:       parser,
		now:          time.Now,
	}
}

// OnCourtRecordObserved enriches one observed court record via DATAJUD. It serves
// two triggers with one path: a fresh DJEN discovery (degree=UNKNOWN → grade the
// placeholder in place) and a scheduler re-poll of an already-graded record
// (degree=G1/G2/… → refresh its fields + movimentos). Both fetch and parse outside
// any transaction, then commit in one unit of work; either way the record's id is
// stable, so its children stay anchored. An observation
// missing the keys a by-number fetch needs is a no-op ack. A fetch fault is
// retryable (asynq re-delivers — a DATAJUD rate-limit is transient); a parse fault
// archives the task (SkipRetry); a process not yet in DATAJUD is a no-op ack.
func (uc *EnrichmentUseCase) OnCourtRecordObserved(ctx context.Context, ev CourtRecordObserved) error {
	if ev.CNJNumber == "" || ev.Court == "" {
		return nil
	}
	// A backfill-discovered record is enriched by the BATCH job (one _search per tribunal),
	// not per-record: sync.go emits one enrichment_batch_requested per distinct tribunal of
	// the window, and the batch owns the grade + the import's ENRICHMENT capture row. Running
	// the single fetch here too would double-enrich (the same process pulled twice) and race
	// the batch's counter, so a backfill-originated observation is a no-op here. Live sync and
	// scheduler re-poll (BackfillJobID == "") stay on this per-record fetch — Phase 1 does not
	// touch the re-poll path. This early return is the smaller blast radius than suppressing
	// court_record_observed in sync.go (that event's shape/consumers are unchanged).
	if ev.BackfillJobID != "" {
		return nil
	}

	connector, err := uc.orchestrator.ConnectorFor(SourceDATAJUD)
	if err != nil {
		return fmt.Errorf("resolve connector for source %q: %w", SourceDATAJUD, err)
	}

	raw, err := connector.Fetch(ctx, FetchRequest{
		Capability: CapabilityFetchByNumber,
		CNJNumber:  ev.CNJNumber,
		Court:      ev.Court,
	})
	if err != nil {
		// Retryable infra (rate-limit/transport): let asynq re-deliver.
		return fmt.Errorf("datajud fetch %s/%s: %w", ev.Court, ev.CNJNumber, err)
	}

	parsed, err := uc.parser.Parse(ctx, raw)
	if err != nil {
		return fmt.Errorf("parse datajud payload: %v: %w", err, asynq.SkipRetry)
	}
	if len(parsed.CourtRecords) == 0 {
		// The process is not (yet) indexed by DATAJUD — nothing to grade. Ack; a
		// scheduler re-poll (next_sync_at) retries the enrichment later.
		return nil
	}

	graded := parsed.CourtRecords[0]
	if graded.Degree == DegreeUnknown {
		// DATAJUD did not disclose a grade either — nothing to do. Ack.
		return nil
	}
	if ev.Degree != DegreeUnknown && graded.Degree != ev.Degree {
		// Re-poll of a graded record, but DATAJUD's top hit is a DIFFERENT grade (a
		// multi-instance process, e.g. G1 + G2). v0 tracks one grade per record; skip
		// to avoid a spurious supersede. Multi-instance is a v1 concern (case_link).
		slog.WarnContext(ctx, "acquisition: datajud top hit grade differs from record; skipping resync",
			"court_record_id", ev.CourtRecordID, "record_degree", ev.Degree, "hit_degree", graded.Degree)
		return nil
	}

	return uc.applyEnrichment(ctx, ev, graded, parsed.DocketEntries)
}

// applyEnrichment commits the in-place grade in one unit of work: dedup the event,
// resolve which record to grade (the placeholder itself, or — on the rare grade
// conflict — a pre-existing graded record it merges into), grade that record,
// upsert the DATAJUD movimentos as docket entries, and emit docket_entry_observed
// for the new ones — all atomically.
func (uc *EnrichmentUseCase) applyEnrichment(ctx context.Context, ev CourtRecordObserved, graded ParsedCourtRecord, movimentos []ParsedDocketEntry) error {
	applied := false
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		// Take the per-tenant write lock FIRST (before the dedup row write) so this
		// grade write never deadlocks against a concurrent sync slice writing the same
		// tenant's court_records. The DATAJUD fetch already ran outside this tx. Holding
		// it also makes the read-then-write conflict check below race-free per tenant.
		if err := uc.repo.AcquireTenantWriteLock(ctx, tx, ev.TenantID); err != nil {
			return err
		}

		already, err := events.NewDedup(tx).SeenOrMark(ctx, consumerEnrichment, ev.EventID)
		if err != nil {
			return err
		}
		if already {
			return nil
		}

		_, newDocket, err := uc.gradeInTx(ctx, tx, ev.TenantID, ev.CourtRecordID, graded, movimentos)
		if err != nil {
			return err
		}
		// This path only ever runs for a LIVE sync / scheduler re-poll: OnCourtRecordObserved
		// early-returns for backfill records (BackfillJobID != ""), which the batch job owns.
		// So the per-andamento docket_entry_observed is always emitted here — real-time avisos
		// (new_andamento) and the deadline reconcile fan-out both depend on it. The import's
		// ENRICHMENT capture row is bumped by the batch job, not here.
		evs := make([]events.Event, 0, len(newDocket))
		for _, de := range newDocket {
			evs = append(evs, newDocketEntryObservedFromEnrich(ev, de))
		}
		if err := uc.outbox.PublishBatch(ctx, tx, evs); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return err
	}
	if applied {
		recordEnrichmentApplied(ctx)
	}
	return nil
}

// gradeInTx applies one process's grade IN PLACE inside the caller's tx and upserts its
// movimentos as docket entries, returning the graded record and the docket rows that were
// actually new. It is the shared core of BOTH enrichment paths — the single-fetch consumer
// (applyEnrichment) and the batch job (OnEnrichmentBatchRequested) — so the FIX B grade +
// the rare (tenant, cnj, degree) merge fallback live in exactly one place. courtRecordID is
// the row to grade (the DJEN placeholder, or a batch-due record); graded/movimentos are the
// DATAJUD-parsed process. It does NOT dedup, take the lock, or emit events — the caller owns
// the tx boundary and the event/audit decisions.
func (uc *EnrichmentUseCase) gradeInTx(ctx context.Context, tx database.Tx, tenantID, courtRecordID string, graded ParsedCourtRecord, movimentos []ParsedDocketEntry) (*CourtRecord, []DocketEntry, error) {
	// FIX B — grade the record IN PLACE. The row we mutate keeps courtRecordID, so its
	// intimations/deadlines/docket stay anchored (no orphan, no duplicate). Mutating the
	// degree would violate the (tenant, cnj, degree) UNIQUE only if a DIFFERENT record
	// already holds this grade; detect that first and merge into it instead. In the common
	// grade (no existing graded record) and the scheduler/batch refresh (the record IS the
	// graded record), the target is simply courtRecordID.
	target := courtRecordID
	existing, found, err := uc.repo.GetCourtRecordByKey(ctx, tx, tenantID, graded.CNJNumber, graded.Degree)
	if err != nil {
		return nil, nil, err
	}
	if found && existing.ID != courtRecordID {
		// Rare conflict: a graded record already holds this (tenant, cnj, degree). Merge the
		// placeholder INTO it — move its intimations onto the graded record and retire the
		// placeholder. DJEN discovery produces no docket entries, so the placeholder never
		// has any to re-point; the DATAJUD movimentos below land on the graded record.
		if _, err := uc.repo.RepointIntimations(ctx, tx, tenantID, courtRecordID, existing.ID); err != nil {
			return nil, nil, err
		}
		if err := uc.repo.SupersedeCourtRecord(ctx, tx, tenantID, courtRecordID); err != nil {
			return nil, nil, err
		}
		target = existing.ID
	}

	gradedRec, err := uc.repo.UpdateCourtRecordGrade(ctx, tx, GradeParams{
		TenantID:      tenantID,
		CourtRecordID: target,
		Degree:        graded.Degree,
		Court:         graded.Court,
		Class:         graded.Class,
		Subject:       graded.Subject,
		JudgingBody:   graded.JudgingBody,
		FiledAt:       graded.FiledAt,
		Secrecy:       graded.Secrecy,
		Completeness:  graded.Completeness,
		// Seeds the re-poll schedule when the row has none yet (COALESCE keeps an existing
		// schedule, so the scheduler's claim owns re-scheduling afterward).
		NextSyncAt: uc.now().Add(defaultSyncInterval),
		// Lifecycle derived from movimentos; the SQL guards SUPERSEDED (never downgrades it).
		// Falls back to the existing value when graded.Lifecycle is empty.
		Lifecycle: graded.Lifecycle,
	})
	if err != nil {
		return nil, nil, err
	}

	newDocket, err := uc.repo.UpsertDocketEntries(ctx, tx, enrichDocketParams(gradedRec.ID, movimentos))
	if err != nil {
		return nil, nil, err
	}
	return gradedRec, newDocket, nil
}

// enrichDocketParams binds every DATAJUD movimento to the graded record. Unlike
// the discovery path there is no record map — a DATAJUD fetch returns one process,
// so all movimentos hang on the single graded record.
func enrichDocketParams(recordID string, movimentos []ParsedDocketEntry) []DocketEntryParams {
	params := make([]DocketEntryParams, 0, len(movimentos))
	for _, pd := range movimentos {
		params = append(params, DocketEntryParams{
			CourtRecordID: recordID,
			Hash:          pd.Hash,
			OccurredAt:    pd.OccurredAt,
			ObservedAt:    pd.ObservedAt,
			Source:        pd.Source,
			Fidelity:      pd.Fidelity,
			Text:          pd.Text,
			TPUCode:       pd.TPUCode,
			Complements:   pd.Complements,
		})
	}
	return params
}

// newDocketEntryObservedFromEnrich announces one NEWLY inserted movimento. It
// carries no sync_run id — enrichment is not a sync run — so SyncRunID is empty,
// distinguishing an enrichment-sourced andamento from a discovery-sourced one.
func newDocketEntryObservedFromEnrich(ev CourtRecordObserved, de DocketEntry) DocketEntryObserved {
	return DocketEntryObserved{
		Base:          events.Base{EventID: newEventID(), Aggregate: de.ID},
		TenantID:      ev.TenantID,
		CourtRecordID: de.CourtRecordID,
		DocketEntryID: de.ID,
		Hash:          de.Hash,
	}
}
