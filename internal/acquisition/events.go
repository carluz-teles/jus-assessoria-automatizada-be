package acquisition

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

// TypeIntegrationActivated is the dotted id of the only event this slice
// produces. Other slices may import IntegrationActivated as the shape they
// consume — it is the only acquisition type allowed to cross a slice boundary.
const TypeIntegrationActivated = "acquisition.integration_activated"

// TypeSyncRequested is the dotted id of the per-slice backfill event the
// backfill listener emits (one per window). The sync slice (a later milestone)
// consumes it to fetch that window; this slice only produces it.
const TypeSyncRequested = "acquisition.sync_requested"

// TypeBackfillFinished is the dotted id of the terminal backfill event: the
// completion counter emits it exactly once, when the LAST slice of a job closes
// (the counter reaches total_slices), carrying the final tally and status. Its
// aggregate is the backfill_job.
const TypeBackfillFinished = "acquisition.backfill_finished"

// TypeDiarioRequested is the dotted id of the national-ingestion work event: one
// tribunal's diário for one day. The scheduler (IngestionScheduler) fans a day into
// one per tribunal; the ingestion consumer (IngestionUseCase) fetches and lands it.
// Routing it through the outbox → relay → "ingestao" queue is what gives each
// tribunal INDEPENDENT retry + DLQ, so a 429 on one court no longer truncates the
// national sweep the way the old synchronous loop did.
const TypeDiarioRequested = "acquisition.diario_requested"

// Type ids of the events the sync cycle produces. sync_completed/sync_failed
// bracket a run; court_record_observed/docket_entry_observed announce what the
// sync saw so downstream slices (consolidation, deadlines, documents) can react.
const (
	TypeSyncCompleted       = "acquisition.sync_completed"
	TypeSyncFailed          = "acquisition.sync_failed"
	TypeCourtRecordObserved = "acquisition.court_record_observed"
	TypeDocketEntryObserved = "acquisition.docket_entry_observed"
)

// Type ids of the intimation events the sync cycle produces in the SAME tx as the
// intimation upsert: observed for a newly landed intimação, cancelled when the DJEN
// retracts one (ACTIVE → CANCELLED). The deadline slice consumes them to open/revoke
// a prazo; it never reads back the acquisition tables (slices talk only by event).
const (
	TypeIntimationObserved  = "acquisition.intimation.observed"
	TypeIntimationCancelled = "acquisition.intimation.cancelled"
)

// TypeEnrichmentBatchRequested is the dotted id of the INTERNAL, SELF-RE-ENQUEUING job
// that DRIVES o enriquecimento DATAJUD em LOTE por (tenant, tribunal). Ele é dono do
// ciclo de vida da linha capture_run ENRICHMENT de uma importação: cada passo varre até
// ~1000 court_records ainda não enriquecidos do tribunal, busca-os em UM request _search
// (terms), grada todos e MARCA todos como tentados; enquanto sobrar registro a varrer ele
// se re-agenda (pass+1, ProcessAt=now+carência), e quando a varredura da importação inteira
// esvazia ele FECHA a linha ENRICHMENT (OK/PARTIAL por cobertura). Produzido na descoberta
// (sync.go, um por tribunal distinto) E consumido por este slice (nenhum outro slice o vê).
const TypeEnrichmentBatchRequested = "acquisition.enrichment_batch_requested"

const aggregateTypeIntegration = "integration"

const aggregateTypeBackfillJob = "backfill_job"

const aggregateTypeSyncRun = "sync_run"

const aggregateTypeCourtRecord = "court_record"

const aggregateTypeDocketEntry = "docket_entry"

const aggregateTypeDiario = "diario"

const aggregateTypeIntimation = "intimation"

// IntegrationActivated is emitted, in the same transaction as the upsert, when a
// source is activated or its scope meaningfully changes. The payload carries
// exactly what a consumer needs to start watching: which integration, which
// tenant, which source, and the scope to monitor. Base adds the event id (used
// for consumer dedup) and the aggregate id.
type IntegrationActivated struct {
	events.Base
	IntegrationID string `json:"integration_id"`
	TenantID      string `json:"tenant_id"`
	Source        string `json:"source"`
	Scope         Scope  `json:"scope"`
}

var _ events.Event = IntegrationActivated{}

func (IntegrationActivated) Type() string          { return TypeIntegrationActivated }
func (IntegrationActivated) AggregateType() string { return aggregateTypeIntegration }

// SyncRequested asks the sync slice to fetch one window of an integration's
// history. The backfill listener emits one per slice of the onboarding horizon,
// in the same transaction as the backfill_job insert. The window bounds are
// bare dates (matching the backfill_job date columns); SliceIndex is the
// window's position in the horizon. Source names the data source (DJEN, DATAJUD,
// …) so the sync consumer resolves the matching connector via the Orchestrator.
// Base carries the per-slice event id (the consumer's dedup key) and the
// backfill_job aggregate id.
type SyncRequested struct {
	events.Base
	BackfillJobID string `json:"backfill_job_id"`
	TenantID      string `json:"tenant_id"`
	IntegrationID string `json:"integration_id"`
	Source        string `json:"source"`
	SliceIndex    int    `json:"slice_index"`
	WindowFrom    string `json:"window_from"`
	WindowTo      string `json:"window_to"`
	// Scope denormalizes the integration's watch scope (the OABs) onto the event,
	// so the sync consumer builds the connector's discovery FetchRequest without
	// reading back the integration row.
	Scope Scope `json:"scope"`
	// CatchUpOABKey/CatchUpSince mark this slice as a re-enable catch-up sync (one
	// watched_oab, not a backfill): when set, the closing sync_completed/sync_failed
	// (below) carries them back so OnSyncCompleted can compare-and-clear the OAB's
	// pending catch_up_since. Empty for a normal backfill/scheduled slice.
	CatchUpOABKey string `json:"catch_up_oab_key,omitempty"`
	CatchUpSince  string `json:"catch_up_since,omitempty"`
}

var _ events.Event = SyncRequested{}

func (SyncRequested) Type() string          { return TypeSyncRequested }
func (SyncRequested) AggregateType() string { return aggregateTypeBackfillJob }

// DiarioRequested asks the ingestion consumer to fetch ONE tribunal's diário for
// ONE day and land it in the publication store. The scheduler emits one per tribunal
// per day (system-scoped, no tenant): the national firehose, fanned across the worker
// fleet. Day is a bare date (2006-01-02), matching the publication window. Base carries
// a fresh per-emission event id (the relay's enqueue-dedup TaskID) so a later day's
// re-request of the same tribunal is a NEW task, not deduped — the daily self-heal.
// There is no consumer dedup: the store insert is idempotent (ON CONFLICT hash), so a
// redelivery re-fetches and re-lands harmlessly, which is exactly what lets a transient
// fault retry.
type DiarioRequested struct {
	events.Base
	Tribunal string `json:"tribunal"`
	Day      string `json:"day"`
}

var _ events.Event = DiarioRequested{}

func (DiarioRequested) Type() string          { return TypeDiarioRequested }
func (DiarioRequested) AggregateType() string { return aggregateTypeDiario }

// CourtRecordObserved announces that a sync saw a court record this run — a
// brand-new one or a re-observation. It carries the record's identity and the
// case it hangs on, so the consolidation slice can link/merge and the read
// models can refresh. Aggregate id is the court_record id.
type CourtRecordObserved struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	SyncRunID     string `json:"sync_run_id"`
	CourtRecordID string `json:"court_record_id"`
	CaseID        string `json:"case_id"`
	CNJNumber     string `json:"cnj_number"`
	Degree        string `json:"degree"`
	// Court is the tribunal sigla — the DATAJUD enrichment consumer needs it to pick
	// the per-tribunal index (api_publica_<sigla>) when it fetches by number.
	Court string `json:"court"`
	// BackfillJobID denormalizes the origin backfill onto the event when this record
	// was discovered by an onboarding backfill (empty for a live sync or a scheduler
	// re-poll). The enrichment consumer reads it to SUPPRESS the per-andamento
	// docket_entry_observed for backfill-originated records: their only consumer is the
	// notifications aviso, which is already silenced during onboarding (the single
	// import_finished aviso stands in for the burst), so emitting them only floods the
	// outbox. The andamentos are still persisted — only the notify-trigger event is skipped.
	BackfillJobID string `json:"backfill_job_id,omitempty"`
}

var _ events.Event = CourtRecordObserved{}

func (CourtRecordObserved) Type() string          { return TypeCourtRecordObserved }
func (CourtRecordObserved) AggregateType() string { return aggregateTypeCourtRecord }

// DocketEntryObserved announces one NEWLY inserted andamento (deduped entries do
// not emit it, so a re-sync of the same window is silent). It carries the entry
// and record ids and the source hash. TWO slices consume it, on two different asynq
// servers, so the relay FANS IT OUT (lib/events queuesFor): notifications (main server →
// new-andamento aviso) and deadline (dedicated server → reconcile a MISSED/OPEN prazo
// when a movimento de resposta lands). Aggregate id is the docket_entry id.
type DocketEntryObserved struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	SyncRunID     string `json:"sync_run_id"`
	CourtRecordID string `json:"court_record_id"`
	DocketEntryID string `json:"docket_entry_id"`
	Hash          string `json:"hash"`
}

var _ events.Event = DocketEntryObserved{}

func (DocketEntryObserved) Type() string          { return TypeDocketEntryObserved }
func (DocketEntryObserved) AggregateType() string { return aggregateTypeDocketEntry }

// IntimationObserved announces one NEWLY landed intimação (a re-sync of the same
// window is silent — deduped rows do not emit it). UF is DENORMALIZED here by the
// producer (derived from Court via ufFromTribunal) so the deadline slice — which
// never imports acquisition — receives the state ready for its holiday-calendar
// lookup. DeadlineStartAt is the wire date (2006-01-02) the deadline derivation
// starts from. Aggregate id is the intimation id.
type IntimationObserved struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	IntimationID  string `json:"intimation_id"`
	CourtRecordID string `json:"court_record_id"`
	CaseID        string `json:"case_id"`
	// IntimationType is the DJEN type (INTIMACAO/CITACAO/COMUNICACAO). The Go field
	// avoids the name Type — that is the events.Event method; the JSON stays "type".
	IntimationType  string `json:"type"`
	Court           string `json:"court"`
	UF              string `json:"uf"`
	DeadlineStartAt string `json:"deadline_start_at"`
	// PrazoDeclarado is the declared day count extracted deterministically from the
	// intimação's teor (extractPrazoDeclarado), e.g. "5 dias" — "" when the teor carries
	// no recognizable prazo mention (the deadline slice then derives it purely from the
	// V0 rule table, never guessing).
	PrazoDeclarado string `json:"prazo_declarado"`
}

var _ events.Event = IntimationObserved{}

func (IntimationObserved) Type() string          { return TypeIntimationObserved }
func (IntimationObserved) AggregateType() string { return aggregateTypeIntimation }

// IntimationCancelled announces that a previously ACTIVE intimação transitioned to
// CANCELLED on this upsert (the DJEN brought a data_cancelamento). The deadline slice
// consumes it to revoke the derived prazo, so a phantom deadline never survives a
// retraction. Reason is the DJEN motivo_cancelamento (empty when it did not disclose
// one). Aggregate id is the intimation id.
type IntimationCancelled struct {
	events.Base
	TenantID     string `json:"tenant_id"`
	IntimationID string `json:"intimation_id"`
	Reason       string `json:"reason"`
}

var _ events.Event = IntimationCancelled{}

func (IntimationCancelled) Type() string          { return TypeIntimationCancelled }
func (IntimationCancelled) AggregateType() string { return aggregateTypeIntimation }

// SyncCompleted closes a successful run with its item tallies (new vs. deduped),
// so the backfill slice can advance its slice counters (a later sub-slice).
// Aggregate id is the sync_run id.
type SyncCompleted struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	SyncRunID     string `json:"sync_run_id"`
	IntegrationID string `json:"integration_id"`
	BackfillJobID string `json:"backfill_job_id"`
	SliceIndex    int    `json:"slice_index"`
	ItemsNew      int    `json:"items_new"`
	ItemsDeduped  int    `json:"items_deduped"`
	// CatchUpOABKey/CatchUpSince are carried through from the closing SyncRequested
	// (empty for a normal backfill/scheduled slice) when this run closed a re-enable
	// catch-up sync — see SyncRequested. OnSyncCompleted uses them to compare-and-clear
	// the OAB's pending catch_up_since.
	CatchUpOABKey string `json:"catch_up_oab_key,omitempty"`
	CatchUpSince  string `json:"catch_up_since,omitempty"`
}

var _ events.Event = SyncCompleted{}

func (SyncCompleted) Type() string          { return TypeSyncCompleted }
func (SyncCompleted) AggregateType() string { return aggregateTypeSyncRun }

// SyncFailed closes a failed run with a human-readable reason (a fetch or parse
// fault). The backfill slice counts it toward slices_error; the reason is
// operational context, never the raw wrapped error. Aggregate id is the
// sync_run id.
type SyncFailed struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	SyncRunID     string `json:"sync_run_id"`
	IntegrationID string `json:"integration_id"`
	BackfillJobID string `json:"backfill_job_id"`
	SliceIndex    int    `json:"slice_index"`
	Reason        string `json:"reason"`
	// CatchUpOABKey/CatchUpSince mirror SyncCompleted's — carried through so a FAILED
	// catch-up sync is identifiable, though OnSyncFailed deliberately does NOT clear
	// catch_up_since (the gap stays marked so the next toggle retries it).
	CatchUpOABKey string `json:"catch_up_oab_key,omitempty"`
	CatchUpSince  string `json:"catch_up_since,omitempty"`
}

var _ events.Event = SyncFailed{}

func (SyncFailed) Type() string          { return TypeSyncFailed }
func (SyncFailed) AggregateType() string { return aggregateTypeSyncRun }

// BackfillFinished closes an onboarding backfill. The completion counter emits it
// once, in the same transaction as the RUNNING → terminal status update, when the
// last slice's sync_completed/sync_failed lands. It carries the final tally so a
// consumer (notification, read models) can report the outcome without re-reading
// the job. Status is COMPLETED when every slice succeeded, PARTIAL when at least
// one failed. Aggregate id is the backfill_job id.
type BackfillFinished struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	BackfillJobID string `json:"backfill_job_id"`
	IntegrationID string `json:"integration_id"`
	TotalSlices   int    `json:"total_slices"`
	SlicesOK      int    `json:"slices_ok"`
	SlicesError   int    `json:"slices_error"`
	Status        string `json:"status"`
}

var _ events.Event = BackfillFinished{}

func (BackfillFinished) Type() string          { return TypeBackfillFinished }
func (BackfillFinished) AggregateType() string { return aggregateTypeBackfillJob }

// TypeWatchedOABReenabled is the dotted id of the event ToggleWatchedOAB publishes when
// turning capture back ON for an OAB that was disabled: it asks the backfill use case to
// catch the OAB up over the downtime window (disabled_at..now), never the full historical
// horizon. Produced by domain.go's ToggleWatchedOAB, consumed by backfill.go.
const TypeWatchedOABReenabled = "acquisition.watched_oab_reenabled"

const aggregateTypeWatchedOAB = "watched_oab"

// watchedOABNamespace namespaces the aggregate id v5 of watched_oab_reenabled (the outbox
// requires aggregate_id uuid NOT NULL, and (tenant|integration|oab_key) is not a uuid) —
// same pattern as enrichmentBatchNamespace, a distinct namespace constant per event family.
var watchedOABNamespace = uuid.MustParse("f1c7a9d3-4b2e-4a8f-9c6d-2e5b8a3f7c19")

// WatchedOABReenabled is emitted, in the same tx as the enable write, when the Termos
// toggle turns capture back ON for a previously-disabled OAB (a REAL re-enable — see
// AddOrEnableWatchedOAB's was_enabled branch). CatchUpSince is the OAB's disabled_at
// (the gap the catch-up sync must cover). Aggregate id is a stable v5 of
// (tenant|integration|oab_key).
type WatchedOABReenabled struct {
	events.Base
	TenantID      string    `json:"tenant_id"`
	IntegrationID string    `json:"integration_id"`
	Source        string    `json:"source"`
	OABKey        string    `json:"oab_key"`
	CatchUpSince  time.Time `json:"catch_up_since"`
}

var _ events.Event = WatchedOABReenabled{}

func (WatchedOABReenabled) Type() string          { return TypeWatchedOABReenabled }
func (WatchedOABReenabled) AggregateType() string { return aggregateTypeWatchedOAB }

// newWatchedOABReenabled builds the event, minting a fresh v7 event id (the consumer's
// dedup key) and deriving the stable v5 aggregate from (tenant|integration|oab_key) via
// the same namespaced-uuid pattern enrichmentBatchAggregate uses.
func newWatchedOABReenabled(tenantID, integrationID, source, oabKey string, catchUpSince time.Time) WatchedOABReenabled {
	return WatchedOABReenabled{
		Base:          events.Base{EventID: newEventID(), Aggregate: watchedOABAggregate(tenantID, integrationID, oabKey)},
		TenantID:      tenantID,
		IntegrationID: integrationID,
		Source:        source,
		OABKey:        oabKey,
		CatchUpSince:  catchUpSince,
	}
}

// watchedOABAggregate derives the stable v5 aggregate uuid from (tenant|integration|oab_key).
func watchedOABAggregate(tenantID, integrationID, oabKey string) string {
	return uuid.NewSHA1(watchedOABNamespace, []byte(tenantID+"|"+integrationID+"|"+oabKey)).String()
}

// enrichmentBatchDelay is the carência between os passos de um enriquecimento em lote: o
// job se re-agenda com ProcessAt=now+delay pra dar tempo do lote anterior (grade+supersede)
// commitar e sumir do índice de varredura antes do próximo SELECT. Curto — o gargalo é o
// request DATAJUD (1/limiter), não o intervalo — mas não-zero, pra o self-re-enqueue não
// virar um hot-loop contra o Postgres.
const enrichmentBatchDelay = 2 * time.Second

// enrichmentBatchNamespace namespaces o aggregate id v5 do enrichment_batch_requested
// (o outbox exige aggregate_id uuid NOT NULL, e (tenant|court|job) não é um uuid).
var enrichmentBatchNamespace = uuid.MustParse("b3a4c2e1-9f7d-4c6b-8a1e-5d3f2b7c9a04")

// EnrichmentBatchRequested is the self-re-enqueuing batch enrichment job for one
// (tenant, tribunal) of one import. TenantID is the scope channel every tenant-scoped
// consumer needs (o outbox não carrega tenant). Court names the tribunal whose index the
// batch queries; BackfillJobID is the import whose ENRICHMENT capture row this job owns.
// Pass is the step counter: it makes a FRESH EventID per re-enqueue ("enrich-batch:{job}:
// {court}:{pass}"), because the relay's asynq TaskID is unique GLOBAL per event id — a
// stable id would make the scheduled follow-up collide with the just-acked step and get
// dropped. processAt is the ETA of the NEXT step, kept UNEXPORTED so it never serializes:
// it feeds ProcessAt() at publish (the relay writes outbox.process_at) and is irrelevant
// to the consumer, which re-reads the scan by (tenant, court).
type EnrichmentBatchRequested struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	Court         string `json:"court"`
	BackfillJobID string `json:"backfill_job_id"`
	Pass          int    `json:"pass"`
	processAt     time.Time
}

var (
	_ events.Event          = EnrichmentBatchRequested{}
	_ events.ScheduledEvent = EnrichmentBatchRequested{}
)

func (EnrichmentBatchRequested) Type() string          { return TypeEnrichmentBatchRequested }
func (EnrichmentBatchRequested) AggregateType() string { return aggregateTypeBackfillJob }

// ProcessAt opts each STEP into scheduled delivery: the first emission (from discovery)
// runs immediately (zero processAt → deliver now); a self-re-enqueue carries now+carência
// so the previous batch's commit clears the scan first (repo directive: ETA work is a
// scheduled task, not polling).
func (e EnrichmentBatchRequested) ProcessAt() (time.Time, bool) {
	return e.processAt, !e.processAt.IsZero()
}

// newEnrichmentBatchRequested builds the FIRST step of a (tenant, court, import) batch,
// emitted on discovery — immediate (zero processAt). The aggregate is a stable v5 of
// (tenant|court|job) so the outbox's uuid NOT NULL is satisfied.
func newEnrichmentBatchRequested(tenantID, court, backfillJobID string) EnrichmentBatchRequested {
	return EnrichmentBatchRequested{
		Base:          events.Base{EventID: enrichmentBatchEventID(backfillJobID, court, 0), Aggregate: enrichmentBatchAggregate(tenantID, court, backfillJobID)},
		TenantID:      tenantID,
		Court:         court,
		BackfillJobID: backfillJobID,
		Pass:          0,
	}
}

// nextEnrichmentBatchStep builds the NEXT step from the current one: a fresh EventID
// (pass+1) so the relay's global TaskID does not drop it, and ProcessAt=now+carência so
// the previous batch's commit clears the scan first.
func (e EnrichmentBatchRequested) nextEnrichmentBatchStep(at time.Time) EnrichmentBatchRequested {
	return EnrichmentBatchRequested{
		Base:          events.Base{EventID: enrichmentBatchEventID(e.BackfillJobID, e.Court, e.Pass+1), Aggregate: e.Aggregate},
		TenantID:      e.TenantID,
		Court:         e.Court,
		BackfillJobID: e.BackfillJobID,
		Pass:          e.Pass + 1,
		processAt:     at.Add(enrichmentBatchDelay),
	}
}

// enrichmentBatchEventID is the per-step idempotency key/TaskID: unique per (import,
// court, pass), so each self-re-enqueued step is a distinct asynq task AND a distinct
// consumer-dedup key (a redelivery of the SAME step is still deduped).
func enrichmentBatchEventID(backfillJobID, court string, pass int) string {
	return fmt.Sprintf("enrich-batch:%s:%s:%d", backfillJobID, court, pass)
}

// enrichmentBatchAggregate derives the stable v5 aggregate uuid from (tenant|court|job).
func enrichmentBatchAggregate(tenantID, court, backfillJobID string) string {
	return uuid.NewSHA1(enrichmentBatchNamespace, []byte(tenantID+"|"+court+"|"+backfillJobID)).String()
}
