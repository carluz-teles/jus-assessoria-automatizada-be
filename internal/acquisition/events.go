package acquisition

import "github.com/jusassessoria/platform/lib/events"

// TypeIntegrationActivated is the dotted id of the only event this slice
// produces. Other slices may import IntegrationActivated as the shape they
// consume — it is the only acquisition type allowed to cross a slice boundary.
const TypeIntegrationActivated = "acquisition.integration_activated"

// TypeSyncRequested is the dotted id of the per-slice backfill event the
// backfill listener emits (one per window). The sync slice (a later milestone)
// consumes it to fetch that window; this slice only produces it.
const TypeSyncRequested = "acquisition.sync_requested"

// Type ids of the events the sync cycle produces. sync_completed/sync_failed
// bracket a run; court_record_observed/docket_entry_observed announce what the
// sync saw so downstream slices (consolidation, deadlines, documents) can react.
const (
	TypeSyncCompleted       = "acquisition.sync_completed"
	TypeSyncFailed          = "acquisition.sync_failed"
	TypeCourtRecordObserved = "acquisition.court_record_observed"
	TypeDocketEntryObserved = "acquisition.docket_entry_observed"
)

const aggregateTypeIntegration = "integration"

const aggregateTypeBackfillJob = "backfill_job"

const aggregateTypeSyncRun = "sync_run"

const aggregateTypeCourtRecord = "court_record"

const aggregateTypeDocketEntry = "docket_entry"

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
// window's position in the horizon. Base carries the per-slice event id (the
// consumer's dedup key) and the backfill_job aggregate id.
type SyncRequested struct {
	events.Base
	BackfillJobID string `json:"backfill_job_id"`
	TenantID      string `json:"tenant_id"`
	IntegrationID string `json:"integration_id"`
	SliceIndex    int    `json:"slice_index"`
	WindowFrom    string `json:"window_from"`
	WindowTo      string `json:"window_to"`
}

var _ events.Event = SyncRequested{}

func (SyncRequested) Type() string          { return TypeSyncRequested }
func (SyncRequested) AggregateType() string { return aggregateTypeBackfillJob }

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
}

var _ events.Event = CourtRecordObserved{}

func (CourtRecordObserved) Type() string          { return TypeCourtRecordObserved }
func (CourtRecordObserved) AggregateType() string { return aggregateTypeCourtRecord }

// DocketEntryObserved announces one NEWLY inserted andamento (deduped entries do
// not emit it, so a re-sync of the same window is silent). It carries the entry
// and record ids and the source hash; the deadline/documents slices consume it.
// Aggregate id is the docket_entry id.
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
}

var _ events.Event = SyncFailed{}

func (SyncFailed) Type() string          { return TypeSyncFailed }
func (SyncFailed) AggregateType() string { return aggregateTypeSyncRun }
