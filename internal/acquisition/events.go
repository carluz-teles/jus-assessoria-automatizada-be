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

const aggregateTypeIntegration = "integration"

const aggregateTypeBackfillJob = "backfill_job"

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
