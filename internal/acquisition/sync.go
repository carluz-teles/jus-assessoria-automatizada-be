package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/obs"
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

// SyncRunParams is the insert payload for a sync_run opening in RUNNING. EventID
// is the sync_requested event that opened it — persisted so a re-delivery can
// locate and resume a run that never closed (FindSyncRunByEventID). WindowFrom/To
// stamp the slice's date window (wire format 2006-01-02; empty → NULL) so the
// reconciliations read can show which window each execution covered.
type SyncRunParams struct {
	TenantID         string
	IntegrationID    string
	ConnectorID      string
	ConnectorVersion string
	StartedAt        time.Time
	Status           string
	EventID          string
	WindowFrom       string
	WindowTo         string
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
	// JudgingBody is the órgão julgador the source disclosed (empty when it did
	// not). It is refreshed on every sync, but COALESCEd in the repo so a sync that
	// omits it keeps the value a prior sync learned.
	JudgingBody string
	// ActiveProcessLimit is the tenant's ceiling on ACTIVE processes (the billing
	// entitlement), resolved by the use case and passed down so the repo can gate a
	// NEW record against it. The repo only compares the count to this number; it
	// never decides where the number comes from. A create is refused with
	// ErrProcessLimitReached when the tenant's ACTIVE count already meets it.
	ActiveProcessLimit int
}

// DocketEntryParams is one andamento to upsert (idempotent on court_record+hash).
// TPUCode/Complements are the DATAJUD movimento classification (zero/empty for
// sources that do not classify the entry).
type DocketEntryParams struct {
	CourtRecordID string
	Hash          string
	OccurredAt    time.Time
	ObservedAt    time.Time
	Source        string
	Fidelity      int
	Text          string
	TPUCode       int
	Complements   json.RawMessage
}

// IntimationParams is one intimação to upsert (ON CONFLICT DO UPDATE on
// tenant+case+hash). Type/Status/SourceURL/CancelledAt/CancelReason are the DJEN
// fields; Status defaults to ACTIVE (intimationParamsFor fills it) and drives the
// upsert's DO UPDATE when the source retracts a publication. Recipients is the
// jsonb addressee list carried verbatim to the column (empty → the '[]' default).
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
	Type            string
	Status          string
	SourceURL       string
	CancelledAt     time.Time
	CancelReason    string
	Recipients      json.RawMessage
}

// syncRepo is the narrow persistence port the sync use case drives.
// *pgRepository satisfies it (and the wider Repository); the use case depends on
// this minimal view so its unit test mocks only these five methods.
// UpsertDocketEntries returns the entries that were ACTUALLY new (the rest were
// deduped by the unique constraint), so the use case can emit an observed event
// for each new one and tally new vs. deduped.
type syncRepo interface {
	InsertSyncRun(ctx context.Context, tx database.Tx, params SyncRunParams) (id string, err error)
	FindSyncRunByEventID(ctx context.Context, tx database.Tx, eventID string) (*SyncRun, error)
	UpdateSyncRun(ctx context.Context, tx database.Tx, outcome SyncRunOutcome) (closed bool, err error)
	FindOrCreateCourtRecord(ctx context.Context, tx database.Tx, params FindOrCreateCourtRecordParams) (*CourtRecord, error)
	UpsertDocketEntries(ctx context.Context, tx database.Tx, params []DocketEntryParams) (newEntries []DocketEntry, err error)
	UpsertIntimations(ctx context.Context, tx database.Tx, params []IntimationParams) (newCount int, err error)
}

// EntitlementChecker resolves a tenant's ceiling on ACTIVE processes — the v0
// billing entitlement (active_process_limit). It is defined HERE, in the consumer,
// and implemented in the billing slice (an adapter injected only at the worker's
// composition root), so acquisition never imports billing and vice versa — the same
// consumer-defines-the-port rule the docs apply to routes, here applied to a
// synchronous read across slices. It takes no tx: the read need not share the
// court_record transaction (a small overshoot under concurrent syncs is accepted in
// v0). A tenant with no subscription resolves to 0 (fail-closed) inside the adapter,
// never as an error — the use case only ever receives an int (or a real infra error).
type EntitlementChecker interface {
	ActiveProcessLimit(ctx context.Context, tenantID string) (int, error)
}

// unlimitedEntitlement is the default EntitlementChecker a SyncUseCase falls back to
// when none is injected: it imposes no ceiling. Production ALWAYS wires the billing
// adapter through WithEntitlementChecker at the worker's composition root; this
// default only keeps same-package tests that do not exercise gating source-compatible
// (mirrors the now:time.Now seam).
type unlimitedEntitlement struct{}

func (unlimitedEntitlement) ActiveProcessLimit(context.Context, string) (int, error) {
	return math.MaxInt, nil
}

// SyncUseCase reacts to sync_requested by running one fetch→parse→upsert cycle.
// It depends on the narrow syncRepo port, the outbox publisher, the unit of work,
// the connector orchestrator, and the parser port — never on the concrete pg
// implementation or a real data source. The connector is resolved per event from
// the orchestrator by the event's source (a DATAJUD event runs under the DATAJUD
// connector, a DJEN one under DJEN), so one use case serves every source. now is a
// seam: it defaults to time.Now and the unit test overrides it for deterministic
// timestamps.
type SyncUseCase struct {
	repo         syncRepo
	outbox       publisher
	uow          database.UnitOfWork
	orchestrator *Orchestrator
	parser       Parser
	checker      EntitlementChecker
	now          func() time.Time
}

// syncOption tunes a SyncUseCase at construction. The clock override stays
// unexported (same-package tests only); WithEntitlementChecker is exported so the
// worker's composition root can inject the billing adapter across the slice boundary.
type syncOption func(*SyncUseCase)

// WithEntitlementChecker injects the billing entitlement port the cycle consults to
// gate NEW court records against the tenant's active_process_limit. Without it the
// use case imposes no ceiling (unlimitedEntitlement); cmd/worker-ingestao wires the
// real billing adapter here.
func WithEntitlementChecker(c EntitlementChecker) syncOption {
	return func(uc *SyncUseCase) { uc.checker = c }
}

// NewSyncUseCase wires the sync use case. The orchestrator resolves the connector
// per event by source (stubs in this slice); the parser is injected (a stub here).
// The entitlement checker defaults to no ceiling and is overridden with the billing
// adapter via WithEntitlementChecker at the worker.
func NewSyncUseCase(repo syncRepo, outbox publisher, uow database.UnitOfWork, orchestrator *Orchestrator, parser Parser, opts ...syncOption) *SyncUseCase {
	uc := &SyncUseCase{
		repo:         repo,
		outbox:       outbox,
		uow:          uow,
		orchestrator: orchestrator,
		parser:       parser,
		checker:      unlimitedEntitlement{},
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// OnSyncRequested runs the cycle for one window. It opens a sync_run (deduped in
// UoW-1), fetches and parses outside the sync transaction (the parser may read
// shared reference data — the DJEN parser derives CPC-224 dates from the holiday
// calendar — but never the sync tables), then commits the effect in UoW-2. A
// duplicate delivery no-ops after the dedup mark. A fetch fault records a FAILED
// run and acks (the scheduler re-syncs later); a parse fault records FAILED and
// archives the task (SkipRetry — a malformed payload never parses on retry).
//
// tenantID comes from the event payload (a trusted internal producer, no Clerk
// token on the worker) and scopes every transaction's RLS. The connector is
// resolved from the event's source before opening the run (so the sync_run audit
// row is stamped with the connector that actually serves this source); an unknown
// source is the typed ErrConnectorNotFound — a misconfigured worker — which stays
// retryable so a redeploy that registers the connector heals it.
func (uc *SyncUseCase) OnSyncRequested(ctx context.Context, ev SyncRequested) error {
	connector, err := uc.orchestrator.ConnectorFor(ev.Source)
	if err != nil {
		return fmt.Errorf("resolve connector for source %q: %w", ev.Source, err)
	}

	// Stamp the flow's identity onto the consumer span (opened by events.Observe) so
	// EVERY sync — success or failure — is filterable by tenant/integration/source/
	// window in New Relic. Domain keys on the span, generic ones on the middleware.
	enrichSyncSpan(ctx, ev)

	syncRunID, seen, err := uc.startRun(ctx, ev, connector)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}

	raw, err := connector.Fetch(ctx, fetchRequestFromEvent(ev))
	if err != nil {
		// A fetch fault is transient by nature (a DJEN WAF 403, a flaky court): retry
		// via asynq with backoff while the retry budget remains, so a burst-triggered
		// block heals on a later, spaced-out attempt. Only when retries are exhausted
		// close the run FAILED (so the backfill counter still advances) and ack. In a
		// unit test (no asynq context) both counters are 0, so it closes FAILED at
		// once — the original behavior.
		retry, _ := asynq.GetRetryCount(ctx)
		maxRetry, _ := asynq.GetMaxRetry(ctx)
		if retry < maxRetry {
			return fmt.Errorf("djen fetch %q (retrying): %w", ev.Source, err)
		}
		// Retries exhausted: this failure is HANDLED here (run marked FAILED, sync_failed
		// emitted, task ack'd) — so it is logged HERE, once, with the keys to find it. The
		// generic middleware "event failed" line does not fire because we ack (return nil).
		slog.WarnContext(ctx, "acquisition: sync fetch exhausted retries; run marked FAILED",
			append(syncLogArgs(ev, syncRunID), "error", err.Error())...)
		if ferr := uc.failRun(ctx, ev, syncRunID, err); ferr != nil {
			return ferr
		}
		return nil
	}

	parsed, err := uc.parser.Parse(ctx, raw)
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
// sync_run in RUNNING (stamped with the resolved connector and the event id). On a
// re-delivery of an already-marked event it does NOT blindly no-op: it reconciles
// the run the event opened (resolveSeenRun) — a run left RUNNING by a crashed prior
// attempt is resumed (seen=false, its id returned), a closed one is a no-op ack.
func (uc *SyncUseCase) startRun(ctx context.Context, ev SyncRequested, connector Connector) (syncRunID string, seen bool, err error) {
	err = uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		already, derr := events.NewDedup(tx).SeenOrMark(ctx, consumerSync, ev.EventID)
		if derr != nil {
			return derr
		}
		if already {
			syncRunID, seen, derr = uc.resolveSeenRun(ctx, tx, ev)
			return derr
		}

		id, ierr := uc.repo.InsertSyncRun(ctx, tx, SyncRunParams{
			TenantID:         ev.TenantID,
			IntegrationID:    ev.IntegrationID,
			ConnectorID:      connector.ID(),
			ConnectorVersion: connector.Version(),
			StartedAt:        uc.now(),
			Status:           SyncStatusRunning,
			EventID:          ev.EventID,
			WindowFrom:       ev.WindowFrom,
			WindowTo:         ev.WindowTo,
		})
		if ierr != nil {
			return ierr
		}
		syncRunID = id
		return nil
	})
	return syncRunID, seen, err
}

// resolveSeenRun reconciles a re-delivery of an already-marked event with the
// sync_run it opened. A RUNNING run means a prior attempt died between the dedup
// mark (UoW-1) and the close, leaving no committed effect (fetch/parse are I/O-only
// and applyResult/failRun are single atomic transactions) — so the cycle RESUMES:
// seen=false returns the existing run id and the caller re-runs fetch→parse→close
// against it, opening no second run and duplicating no outbox. A closed run
// (OK/FAILED), or none at all (defensive), is a no-op ack (seen=true).
func (uc *SyncUseCase) resolveSeenRun(ctx context.Context, tx database.Tx, ev SyncRequested) (syncRunID string, seen bool, err error) {
	run, err := uc.repo.FindSyncRunByEventID(ctx, tx, ev.EventID)
	if errors.Is(err, ErrSyncRunNotFound) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if run.Status == SyncStatusRunning {
		return run.ID, false, nil
	}
	return "", true, nil
}

// failRun is the UoW-2 for a fetch/parse fault: mark the run FAILED with the
// reason and emit sync_failed in the same transaction. The close is a
// compare-and-swap (UpdateSyncRun guards on status=RUNNING): if a concurrent
// execution already closed this run, closed is false and sync_failed is NOT
// re-published — the winning execution already emitted the terminal event.
func (uc *SyncUseCase) failRun(ctx context.Context, ev SyncRequested, syncRunID string, cause error) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		closed, err := uc.repo.UpdateSyncRun(ctx, tx, SyncRunOutcome{
			ID:         syncRunID,
			Status:     SyncStatusFailed,
			FinishedAt: uc.now(),
			Error:      cause.Error(),
		})
		if err != nil {
			return err
		}
		if !closed {
			return nil
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
	// The onboarding backfill (BackfillJobID set) is NOT gated by the active-process
	// ceiling — a product decision: a high-volume OAB must import its whole history,
	// and gating would silently drop most of it, arbitrarily by slice ordering. The
	// gate still guards any non-backfill discovery (BackfillJobID ""). When it applies,
	// resolve the entitlement ONCE per cycle (not per record), OUTSIDE the tx — a read
	// on the billing pool that must not hold this tx's connection; a small overshoot
	// under concurrent syncs is accepted (v0). A real error fails the whole cycle (the
	// run stays RUNNING; asynq re-delivers and resolveSeenRun resumes it); only
	// ErrSubscriptionNotFound is folded to limit 0 by the adapter, never surfaced here.
	limit := math.MaxInt
	if ev.BackfillJobID == "" {
		resolved, err := uc.checker.ActiveProcessLimit(ctx, ev.TenantID)
		if err != nil {
			return fmt.Errorf("resolve active process limit for tenant %s: %w", ev.TenantID, err)
		}
		limit = resolved
	}

	var (
		tally     syncTally
		committed bool // this execution is the one that closed the run OK
	)
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		records := make(map[string]*CourtRecord, len(parsed.CourtRecords))
		ordered := make([]*CourtRecord, 0, len(parsed.CourtRecords))
		blocked := make(map[string]bool)
		nextSync := uc.now().Add(defaultSyncInterval)

		for _, pr := range parsed.CourtRecords {
			cr, err := uc.repo.FindOrCreateCourtRecord(ctx, tx, FindOrCreateCourtRecordParams{
				TenantID:           ev.TenantID,
				CNJNumber:          pr.CNJNumber,
				Degree:             pr.Degree,
				Court:              pr.Court,
				Class:              pr.Class,
				Subject:            pr.Subject,
				Completeness:       pr.Completeness,
				JudgingBody:        pr.JudgingBody,
				NextSyncAt:         nextSync,
				ActiveProcessLimit: limit,
			})
			if errors.Is(err, ErrProcessLimitReached) {
				// Expected, not a failure: the tenant is at its plan ceiling and this is a
				// brand-new process. Skip it (and its docket/intimation children below),
				// keep folding the rest of the batch, and let the run close OK. Disparo de
				// notification.requested avisando o tenant fica para uma fatia futura.
				slog.WarnContext(ctx, "acquisition: active process limit reached; skipping new court record",
					"tenant_id", ev.TenantID,
					"cnj_number", pr.CNJNumber,
					"court", pr.Court,
					"degree", pr.Degree,
				)
				blocked[recordKey(pr.CNJNumber, pr.Degree)] = true
				continue
			}
			if err != nil {
				return err
			}
			records[recordKey(pr.CNJNumber, pr.Degree)] = cr
			ordered = append(ordered, cr)
		}

		docketParams, err := docketParamsFor(parsed.DocketEntries, records, blocked)
		if err != nil {
			return err
		}
		newDocket, err := uc.repo.UpsertDocketEntries(ctx, tx, docketParams)
		if err != nil {
			return err
		}

		intimParams, err := intimationParamsFor(ev.TenantID, parsed.Intimations, records, blocked)
		if err != nil {
			return err
		}
		if _, err := uc.repo.UpsertIntimations(ctx, tx, intimParams); err != nil {
			return err
		}

		itemsNew := len(newDocket)
		itemsDeduped := len(docketParams) - itemsNew

		closed, err := uc.repo.UpdateSyncRun(ctx, tx, SyncRunOutcome{
			ID:           syncRunID,
			Status:       SyncStatusOK,
			ItemsNew:     itemsNew,
			ItemsDeduped: itemsDeduped,
			FinishedAt:   uc.now(),
		})
		if err != nil {
			return err
		}
		if !closed {
			// A concurrent execution already closed this run (its status is no longer
			// RUNNING, so the CAS affected zero rows). The upserts above were idempotent
			// no-ops against the effect it committed; skip the observed/completed events
			// so they — and the backfill slice count they drive — fire exactly once.
			return nil
		}

		tally = syncTally{
			CourtRecords: len(ordered),
			Intimations:  len(intimParams),
			DocketNew:    itemsNew,
			Deduped:      itemsDeduped,
			Blocked:      len(blocked),
		}
		committed = true
		return uc.publishObserved(ctx, tx, ev, syncRunID, ordered, newDocket, itemsNew, itemsDeduped)
	})
	if err != nil {
		return err
	}
	// Milestone, only for the execution that actually closed the run (spans tell the
	// happy-path flow; this one INFO per closed slice gives the counts at a glance).
	if committed {
		uc.logSyncCompleted(ctx, ev, syncRunID, tally)
	}
	return nil
}

// syncTally is what one successful sync slice produced, for the completion span
// attributes and the milestone log.
type syncTally struct {
	CourtRecords int
	Intimations  int
	DocketNew    int
	Deduped      int
	Blocked      int
}

// syncLogArgs are the correlation keys every sync log carries so a failure is
// findable — filter by tenant, integration, source, window or run and you land on
// it. trace_id/span_id ride along automatically (the consumer span is active).
func syncLogArgs(ev SyncRequested, syncRunID string) []any {
	return []any{
		obs.KeyTenantID, ev.TenantID,
		"integration_id", ev.IntegrationID,
		"source", ev.Source,
		"sync_run_id", syncRunID,
		"window_from", ev.WindowFrom,
		"window_to", ev.WindowTo,
	}
}

// enrichSyncSpan stamps the flow's identity onto the active consumer span so every
// sync (success or failure) is filterable by these dimensions in the backend.
func enrichSyncSpan(ctx context.Context, ev SyncRequested) {
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(obs.KeyTenantID, ev.TenantID),
		attribute.String("integration_id", ev.IntegrationID),
		attribute.String("source", ev.Source),
		attribute.String("window_from", ev.WindowFrom),
		attribute.String("window_to", ev.WindowTo),
		attribute.Int("slice_index", ev.SliceIndex),
		attribute.Int("oab_count", len(ev.Scope.OAB)),
	)
}

// logSyncCompleted records the completion counts on the span and emits the one
// milestone INFO line for a closed sync slice.
func (uc *SyncUseCase) logSyncCompleted(ctx context.Context, ev SyncRequested, syncRunID string, t syncTally) {
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("court_records", t.CourtRecords),
		attribute.Int("intimations", t.Intimations),
		attribute.Int("docket_new", t.DocketNew),
		attribute.Int("deduped", t.Deduped),
		attribute.Int("blocked", t.Blocked),
	)
	slog.InfoContext(ctx, "acquisition: sync completed",
		append(syncLogArgs(ev, syncRunID),
			"court_records", t.CourtRecords,
			"intimations", t.Intimations,
			"docket_new", t.DocketNew,
			"deduped", t.Deduped,
			"blocked", t.Blocked,
		)...)
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
// the find-or-create map. An entry whose record was gated by the entitlement limit
// (blocked) is dropped silently — its record was never created, so it has nothing to
// hang on. An entry naming a record that is neither created nor blocked is a parser
// invariant violation and aborts the cycle (fail closed).
func docketParamsFor(entries []ParsedDocketEntry, records map[string]*CourtRecord, blocked map[string]bool) ([]DocketEntryParams, error) {
	params := make([]DocketEntryParams, 0, len(entries))
	for _, pd := range entries {
		key := recordKey(pd.CNJNumber, pd.Degree)
		if blocked[key] {
			continue
		}
		cr, ok := records[key]
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
// via the find-or-create map, under the event's tenant. Same blocked-drop and
// fail-closed rules as docket entries.
func intimationParamsFor(tenantID string, intims []ParsedIntimation, records map[string]*CourtRecord, blocked map[string]bool) ([]IntimationParams, error) {
	params := make([]IntimationParams, 0, len(intims))
	for _, pn := range intims {
		key := recordKey(pn.CNJNumber, pn.Degree)
		if blocked[key] {
			continue
		}
		cr, ok := records[key]
		if !ok {
			return nil, fmt.Errorf("intimation %q references unknown court record %s/%s", pn.Hash, pn.CNJNumber, pn.Degree)
		}
		status := pn.Status
		if status == "" {
			// The source did not disclose a status: a fresh publication is ACTIVE (the
			// column default). Filling it here keeps the upsert's EXCLUDED.status honest.
			status = IntimationStatusActive
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
			Type:            pn.Type,
			Status:          status,
			SourceURL:       pn.SourceURL,
			CancelledAt:     pn.CancelledAt,
			CancelReason:    pn.CancelReason,
			Recipients:      pn.Recipients,
		})
	}
	return params, nil
}

// fetchRequestFromEvent maps a sync_requested window to a connector FetchRequest.
// Onboarding syncs discover a tenant's processes by OAB over the window, so the
// event's denormalized scope is split into the (number, UF) pairs the connector
// queries.
func fetchRequestFromEvent(ev SyncRequested) FetchRequest {
	return FetchRequest{
		Capability:    CapabilityDiscoverByOAB,
		IntegrationID: ev.IntegrationID,
		WindowFrom:    ev.WindowFrom,
		WindowTo:      ev.WindowTo,
		OABs:          oabEntriesFromScope(ev.Scope),
	}
}

// oabEntriesFromScope splits each scope OAB — a validated "UF+number" string
// (e.g. "SP123456", regex ^[A-Z]{2}\d{1,6}$) — into its (number, UF) parts, the
// shape the connector's OAB discovery drives. A malformed entry (too short to
// carry both parts) is skipped rather than yielding a bogus pair.
func oabEntriesFromScope(scope Scope) []OABEntry {
	entries := make([]OABEntry, 0, len(scope.OAB))
	for _, oab := range scope.OAB {
		if len(oab) < 3 {
			continue
		}
		entries = append(entries, OABEntry{UF: oab[:2], Number: oab[2:]})
	}
	return entries
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
		Court:         cr.Court,
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
