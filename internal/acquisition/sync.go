package acquisition

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// sync.go is the consumer of acquisition.sync_requested: one window of an
// integration's history, fetched through a connector, parsed, and folded into
// the consolidation tables. The connector and parser are ports (connector.go);
// this slice injects a stub behind them (stub.go), so the full cycle runs with
// no network I/O. The REAL DJEN/DataJud connector and parser are later slices.

// consumerSync is this listener's identity in processed_event. Dedup is
// per-consumer, so marking an event here never blocks the backfill consumer that
// produced it (consumerBackfill).
const consumerSync = "acquisition.sync"

// Sync run lifecycle values. A run lands RUNNING when it opens and transitions to
// OK (with item tallies) or FAILED (with an error) when it closes.
const (
	SyncStatusRunning = "RUNNING"
	SyncStatusOK      = "OK"
	SyncStatusFailed  = "FAILED"
)

// defaultSyncInterval is how far ahead the next sweep of a court record is placed
// after a successful sync; the scheduler slice (future) reads next_sync_at.
const defaultSyncInterval = 24 * time.Hour

// SyncRunParams is the insert payload for a sync_run opening in RUNNING.
type SyncRunParams struct {
	TenantID         string
	IntegrationID    string
	ConnectorID      string
	ConnectorVersion string
	StartedAt        time.Time
	Status           string
}

// SyncRunOutcome closes a run. Error is empty on OK (the repo writes NULL) and
// carries the failure reason on FAILED.
type SyncRunOutcome struct {
	ID           string
	Status       string
	ItemsNew     int
	ItemsDeduped int
	FinishedAt   time.Time
	Error        string
}

// FindOrCreateCourtRecordParams resolves-or-creates a court record and marks it
// synced in one repo call: the natural key locates it (or seeds a new case +
// record), then Completeness/NextSyncAt are written whether it was new or found.
type FindOrCreateCourtRecordParams struct {
	TenantID     string
	CNJNumber    string
	Degree       string
	Court        string
	Class        string
	Subject      string
	Completeness float32
	NextSyncAt   time.Time
}

// DocketEntryParams is one andamento to upsert (idempotent on court_record+hash).
type DocketEntryParams struct {
	CourtRecordID string
	Hash          string
	OccurredAt    time.Time
	ObservedAt    time.Time
	Source        string
	Fidelity      int
	Text          string
}

// IntimationParams is one intimação to upsert (idempotent on tenant+case+hash).
type IntimationParams struct {
	TenantID        string
	CaseID          string
	CourtRecordID   string
	Hash            string
	MadeAvailableAt time.Time
	PublishedAt     time.Time
	DeadlineStartAt time.Time
	Content         string
	Source          string
}

// syncRepo is the narrow persistence port the sync use case drives.
// *pgRepository satisfies it (and the wider Repository); the use case depends on
// this minimal view so its unit test mocks only these five methods.
// UpsertDocketEntries returns the entries that were ACTUALLY new (the rest were
// deduped by the unique constraint), so the use case can emit an observed event
// for each new one and tally new vs. deduped.
type syncRepo interface {
	InsertSyncRun(ctx context.Context, tx database.Tx, params SyncRunParams) (id string, err error)
	UpdateSyncRun(ctx context.Context, tx database.Tx, outcome SyncRunOutcome) error
	FindOrCreateCourtRecord(ctx context.Context, tx database.Tx, params FindOrCreateCourtRecordParams) (*CourtRecord, error)
	UpsertDocketEntries(ctx context.Context, tx database.Tx, params []DocketEntryParams) (newEntries []DocketEntry, err error)
	UpsertIntimations(ctx context.Context, tx database.Tx, params []IntimationParams) (newCount int, err error)
}

// SyncUseCase reacts to sync_requested by running one fetch→parse→upsert cycle.
// It depends on the narrow syncRepo port, the outbox publisher, the unit of work,
// and the connector/parser ports — never on the concrete pg implementation or a
// real data source. now is a seam: it defaults to time.Now and the unit test
// overrides it for deterministic timestamps.
type SyncUseCase struct {
	repo      syncRepo
	outbox    publisher
	uow       database.UnitOfWork
	connector Connector
	parser    Parser
	now       func() time.Time
}

// syncOption tunes a SyncUseCase at construction. Options are unexported:
// production callers take the defaults, only same-package tests override the clock.
type syncOption func(*SyncUseCase)

// NewSyncUseCase wires the sync use case. The connector and parser are injected
// (a stub in this slice, resolved via the Orchestrator at composition time).
func NewSyncUseCase(repo syncRepo, outbox publisher, uow database.UnitOfWork, connector Connector, parser Parser, opts ...syncOption) *SyncUseCase {
	uc := &SyncUseCase{
		repo:      repo,
		outbox:    outbox,
		uow:       uow,
		connector: connector,
		parser:    parser,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// OnSyncRequested runs the cycle for one window. It opens a sync_run (deduped in
// UoW-1), fetches and parses outside any transaction (no DB, pure connector I/O),
// then commits the effect in UoW-2. A duplicate delivery no-ops after the dedup
// mark. A fetch fault records a FAILED run and acks (the scheduler re-syncs
// later); a parse fault records FAILED and archives the task (SkipRetry — a
// malformed payload never parses on retry).
//
// tenantID comes from the event payload (a trusted internal producer, no Clerk
// token on the worker) and scopes every transaction's RLS.
func (uc *SyncUseCase) OnSyncRequested(ctx context.Context, ev SyncRequested) error {
	syncRunID, seen, err := uc.startRun(ctx, ev)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}

	raw, err := uc.connector.Fetch(ctx, fetchRequestFromEvent(ev))
	if err != nil {
		if ferr := uc.failRun(ctx, ev, syncRunID, err); ferr != nil {
			return ferr
		}
		return nil
	}

	parsed, err := uc.parser.Parse(raw)
	if err != nil {
		if ferr := uc.failRun(ctx, ev, syncRunID, err); ferr != nil {
			return ferr
		}
		// asynq.SkipRetry archives the task instead of retrying a payload that can
		// never parse. The use case owns fetch/parse, so it signals it here; the
		// listener returns the error verbatim (mirrors events.Decode).
		return fmt.Errorf("parse sync payload: %v: %w", err, asynq.SkipRetry)
	}

	return uc.applyResult(ctx, ev, syncRunID, parsed)
}

// startRun is UoW-1: dedup the event and, if it is the first sighting, open the
// sync_run in RUNNING. A seen event commits only the dedup mark and reports
// seen=true so the caller acks without running the cycle.
func (uc *SyncUseCase) startRun(ctx context.Context, ev SyncRequested) (syncRunID string, seen bool, err error) {
	err = uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		already, derr := events.NewDedup(tx).SeenOrMark(ctx, consumerSync, ev.EventID)
		if derr != nil {
			return derr
		}
		if already {
			seen = true
			return nil
		}

		id, ierr := uc.repo.InsertSyncRun(ctx, tx, SyncRunParams{
			TenantID:         ev.TenantID,
			IntegrationID:    ev.IntegrationID,
			ConnectorID:      uc.connector.ID(),
			ConnectorVersion: uc.connector.Version(),
			StartedAt:        uc.now(),
			Status:           SyncStatusRunning,
		})
		if ierr != nil {
			return ierr
		}
		syncRunID = id
		return nil
	})
	return syncRunID, seen, err
}

// failRun is the UoW-2 for a fetch/parse fault: mark the run FAILED with the
// reason and emit sync_failed in the same transaction.
func (uc *SyncUseCase) failRun(ctx context.Context, ev SyncRequested, syncRunID string, cause error) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if err := uc.repo.UpdateSyncRun(ctx, tx, SyncRunOutcome{
			ID:         syncRunID,
			Status:     SyncStatusFailed,
			FinishedAt: uc.now(),
			Error:      cause.Error(),
		}); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newSyncFailed(ev, syncRunID, cause.Error()))
	})
}

// applyResult is the UoW-2 for a successful sync: find-or-create each observed
// court record, upsert its docket entries and intimations (dedup by unique
// constraint), close the run OK with the tallies, and emit the observed events —
// all atomically. court_record_observed fires per observed record;
// docket_entry_observed only for entries that were actually new.
func (uc *SyncUseCase) applyResult(ctx context.Context, ev SyncRequested, syncRunID string, parsed ParsedResult) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		records := make(map[string]*CourtRecord, len(parsed.CourtRecords))
		ordered := make([]*CourtRecord, 0, len(parsed.CourtRecords))
		nextSync := uc.now().Add(defaultSyncInterval)

		for _, pr := range parsed.CourtRecords {
			cr, err := uc.repo.FindOrCreateCourtRecord(ctx, tx, FindOrCreateCourtRecordParams{
				TenantID:     ev.TenantID,
				CNJNumber:    pr.CNJNumber,
				Degree:       pr.Degree,
				Court:        pr.Court,
				Class:        pr.Class,
				Subject:      pr.Subject,
				Completeness: pr.Completeness,
				NextSyncAt:   nextSync,
			})
			if err != nil {
				return err
			}
			records[recordKey(pr.CNJNumber, pr.Degree)] = cr
			ordered = append(ordered, cr)
		}

		docketParams, err := docketParamsFor(parsed.DocketEntries, records)
		if err != nil {
			return err
		}
		newDocket, err := uc.repo.UpsertDocketEntries(ctx, tx, docketParams)
		if err != nil {
			return err
		}

		intimParams, err := intimationParamsFor(ev.TenantID, parsed.Intimations, records)
		if err != nil {
			return err
		}
		if _, err := uc.repo.UpsertIntimations(ctx, tx, intimParams); err != nil {
			return err
		}

		itemsNew := len(newDocket)
		itemsDeduped := len(docketParams) - itemsNew

		if err := uc.repo.UpdateSyncRun(ctx, tx, SyncRunOutcome{
			ID:           syncRunID,
			Status:       SyncStatusOK,
			ItemsNew:     itemsNew,
			ItemsDeduped: itemsDeduped,
			FinishedAt:   uc.now(),
		}); err != nil {
			return err
		}

		return uc.publishObserved(ctx, tx, ev, syncRunID, ordered, newDocket, itemsNew, itemsDeduped)
	})
}

// publishObserved emits, within the caller's tx, one court_record_observed per
// observed record, one docket_entry_observed per NEW entry, and one
// sync_completed to close the run.
func (uc *SyncUseCase) publishObserved(ctx context.Context, tx database.Tx, ev SyncRequested, syncRunID string, records []*CourtRecord, newDocket []DocketEntry, itemsNew, itemsDeduped int) error {
	for _, cr := range records {
		if err := uc.outbox.Publish(ctx, tx, newCourtRecordObserved(ev, syncRunID, cr)); err != nil {
			return err
		}
	}
	for _, de := range newDocket {
		if err := uc.outbox.Publish(ctx, tx, newDocketEntryObserved(ev, syncRunID, de)); err != nil {
			return err
		}
	}
	return uc.outbox.Publish(ctx, tx, newSyncCompleted(ev, syncRunID, itemsNew, itemsDeduped))
}

// docketParamsFor resolves each parsed docket entry to its court_record id via
// the find-or-create map. An entry naming a record not in this result is a
// parser invariant violation and aborts the cycle (fail closed).
func docketParamsFor(entries []ParsedDocketEntry, records map[string]*CourtRecord) ([]DocketEntryParams, error) {
	params := make([]DocketEntryParams, 0, len(entries))
	for _, pd := range entries {
		cr, ok := records[recordKey(pd.CNJNumber, pd.Degree)]
		if !ok {
			return nil, fmt.Errorf("docket entry %q references unknown court record %s/%s", pd.Hash, pd.CNJNumber, pd.Degree)
		}
		params = append(params, DocketEntryParams{
			CourtRecordID: cr.ID,
			Hash:          pd.Hash,
			OccurredAt:    pd.OccurredAt,
			ObservedAt:    pd.ObservedAt,
			Source:        pd.Source,
			Fidelity:      pd.Fidelity,
			Text:          pd.Text,
		})
	}
	return params, nil
}

// intimationParamsFor resolves each parsed intimation to its case/record ids
// via the find-or-create map, under the event's tenant. Same fail-closed rule as
// docket entries.
func intimationParamsFor(tenantID string, intims []ParsedIntimation, records map[string]*CourtRecord) ([]IntimationParams, error) {
	params := make([]IntimationParams, 0, len(intims))
	for _, pn := range intims {
		cr, ok := records[recordKey(pn.CNJNumber, pn.Degree)]
		if !ok {
			return nil, fmt.Errorf("intimation %q references unknown court record %s/%s", pn.Hash, pn.CNJNumber, pn.Degree)
		}
		params = append(params, IntimationParams{
			TenantID:        tenantID,
			CaseID:          cr.CaseID,
			CourtRecordID:   cr.ID,
			Hash:            pn.Hash,
			MadeAvailableAt: pn.MadeAvailableAt,
			PublishedAt:     pn.PublishedAt,
			DeadlineStartAt: pn.DeadlineStartAt,
			Content:         pn.Content,
			Source:          pn.Source,
		})
	}
	return params, nil
}

// fetchRequestFromEvent maps a sync_requested window to a connector FetchRequest.
// Onboarding syncs discover a tenant's processes by OAB over the window.
func fetchRequestFromEvent(ev SyncRequested) FetchRequest {
	return FetchRequest{
		Capability:    CapabilityDiscoverByOAB,
		IntegrationID: ev.IntegrationID,
		WindowFrom:    ev.WindowFrom,
		WindowTo:      ev.WindowTo,
	}
}

// recordKey is the in-memory join key between a parsed record and its docket
// entries/intimations: the natural key (cnj, degree) with a NUL separator so
// no two distinct pairs ever collide.
func recordKey(cnjNumber, degree string) string {
	return cnjNumber + "\x00" + degree
}

// newEventID mints a fresh time-ordered v7 uuid used as an event's idempotency
// key (consumer dedup) and enqueue-dedup TaskID at the relay.
func newEventID() string {
	return uuid.Must(uuid.NewV7()).String()
}

func newCourtRecordObserved(ev SyncRequested, syncRunID string, cr *CourtRecord) CourtRecordObserved {
	return CourtRecordObserved{
		Base:          events.Base{EventID: newEventID(), Aggregate: cr.ID},
		TenantID:      ev.TenantID,
		SyncRunID:     syncRunID,
		CourtRecordID: cr.ID,
		CaseID:        cr.CaseID,
		CNJNumber:     cr.CNJNumber,
		Degree:        cr.Degree,
	}
}

func newDocketEntryObserved(ev SyncRequested, syncRunID string, de DocketEntry) DocketEntryObserved {
	return DocketEntryObserved{
		Base:          events.Base{EventID: newEventID(), Aggregate: de.ID},
		TenantID:      ev.TenantID,
		SyncRunID:     syncRunID,
		CourtRecordID: de.CourtRecordID,
		DocketEntryID: de.ID,
		Hash:          de.Hash,
	}
}

func newSyncCompleted(ev SyncRequested, syncRunID string, itemsNew, itemsDeduped int) SyncCompleted {
	return SyncCompleted{
		Base:          events.Base{EventID: newEventID(), Aggregate: syncRunID},
		TenantID:      ev.TenantID,
		SyncRunID:     syncRunID,
		IntegrationID: ev.IntegrationID,
		BackfillJobID: ev.BackfillJobID,
		SliceIndex:    ev.SliceIndex,
		ItemsNew:      itemsNew,
		ItemsDeduped:  itemsDeduped,
	}
}

func newSyncFailed(ev SyncRequested, syncRunID, reason string) SyncFailed {
	return SyncFailed{
		Base:          events.Base{EventID: newEventID(), Aggregate: syncRunID},
		TenantID:      ev.TenantID,
		SyncRunID:     syncRunID,
		IntegrationID: ev.IntegrationID,
		BackfillJobID: ev.BackfillJobID,
		SliceIndex:    ev.SliceIndex,
		Reason:        reason,
	}
}
