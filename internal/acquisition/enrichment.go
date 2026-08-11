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
// ajuizamento/sigilo/movimentos), and performs the placeholder+merge: it finds or
// creates the GRADED court_record in the same case, re-points the placeholder's
// intimations onto it, retires the placeholder (SUPERSEDED), and attaches the
// movimentos as docket entries. DATAJUD is enrichment-only — it never discovers —
// so it is not an activatable integration; this event is its sole trigger.

// consumerEnrichment is this listener's identity in processed_event (per-consumer
// dedup, independent of the sync consumer that produced the event).
const consumerEnrichment = "acquisition.enrichment"

// GradedRecordParams find-or-creates the graded court record and refreshes the
// DATAJUD-authoritative fields. It is the enrichment counterpart of
// FindOrCreateCourtRecordParams, minus the entitlement gate (grading an already
// tracked process consumes no new plan slot).
type GradedRecordParams struct {
	TenantID     string
	CaseID       string
	CNJNumber    string
	Degree       string
	Court        string
	Class        string
	Subject      string
	JudgingBody  string
	FiledAt      time.Time
	Secrecy      string
	Completeness float32
	// NextSyncAt seeds the re-poll schedule; it is written only when the graded
	// record is first created (the query ignores it on a refresh), so the scheduler
	// owns re-scheduling thereafter.
	NextSyncAt time.Time
}

// enrichRepo is the narrow persistence port the enrichment use case drives:
// the grade merge (upsert graded + re-point + supersede) and the movimento upsert.
type enrichRepo interface {
	AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error
	UpsertGradedCourtRecord(ctx context.Context, tx database.Tx, params GradedRecordParams) (*CourtRecord, error)
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
// two triggers with one path: a fresh DJEN discovery (degree=UNKNOWN → grade +
// placeholder+merge) and a scheduler re-poll of an already-graded record
// (degree=G1/G2/… → refresh its movimentos). Both fetch and parse outside any
// transaction, then commit in one unit of work; the merge-vs-refresh split falls
// out of whether the graded record differs from the observed one. An observation
// missing the keys a by-number fetch needs is a no-op ack. A fetch fault is
// retryable (asynq re-delivers — a DATAJUD rate-limit is transient); a parse fault
// archives the task (SkipRetry); a process not yet in DATAJUD is a no-op ack.
func (uc *EnrichmentUseCase) OnCourtRecordObserved(ctx context.Context, ev CourtRecordObserved) error {
	if ev.CNJNumber == "" || ev.Court == "" {
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

// applyEnrichment commits the placeholder+merge in one unit of work: dedup the
// event, upsert the graded record, move the placeholder's intimations onto it,
// retire the placeholder, upsert the DATAJUD movimentos as docket entries, and
// emit docket_entry_observed for the new ones — all atomically.
func (uc *EnrichmentUseCase) applyEnrichment(ctx context.Context, ev CourtRecordObserved, graded ParsedCourtRecord, movimentos []ParsedDocketEntry) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		// Take the per-tenant write lock FIRST (before the dedup row write) so this
		// graded-record merge never deadlocks against a concurrent sync slice writing
		// the same tenant's court_records. The DATAJUD fetch already ran outside this tx.
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

		gradedRec, err := uc.repo.UpsertGradedCourtRecord(ctx, tx, GradedRecordParams{
			TenantID:     ev.TenantID,
			CaseID:       ev.CaseID,
			CNJNumber:    graded.CNJNumber,
			Degree:       graded.Degree,
			Court:        graded.Court,
			Class:        graded.Class,
			Subject:      graded.Subject,
			JudgingBody:  graded.JudgingBody,
			FiledAt:      graded.FiledAt,
			Secrecy:      graded.Secrecy,
			Completeness: graded.Completeness,
			// Seeds the re-poll schedule on the FIRST grade (ignored by the query on a
			// refresh, so the scheduler's claim owns re-scheduling afterward).
			NextSyncAt: uc.now().Add(defaultSyncInterval),
		})
		if err != nil {
			return err
		}

		// Placeholder+merge: the observed UNKNOWN record and the graded record are
		// distinct rows (UNKNOWN vs G1/G2). Move the placeholder's intimations onto the
		// graded record and retire the placeholder. DJEN discovery produces no docket
		// entries, so the placeholder never has any to re-point.
		if gradedRec.ID != ev.CourtRecordID {
			if _, err := uc.repo.RepointIntimations(ctx, tx, ev.TenantID, ev.CourtRecordID, gradedRec.ID); err != nil {
				return err
			}
			if err := uc.repo.SupersedeCourtRecord(ctx, tx, ev.TenantID, ev.CourtRecordID); err != nil {
				return err
			}
		}

		newDocket, err := uc.repo.UpsertDocketEntries(ctx, tx, enrichDocketParams(gradedRec.ID, movimentos))
		if err != nil {
			return err
		}
		for _, de := range newDocket {
			if err := uc.outbox.Publish(ctx, tx, newDocketEntryObservedFromEnrich(ev, de)); err != nil {
				return err
			}
		}
		return nil
	})
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
