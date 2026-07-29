package acquisition

import "github.com/jusassessoria/platform/lib/events"

// TypeIntegrationActivated is the dotted id of the only event this slice
// produces. Other slices may import IntegrationActivated as the shape they
// consume — it is the only acquisition type allowed to cross a slice boundary.
const TypeIntegrationActivated = "acquisition.integration_activated"

const aggregateTypeIntegration = "integration"

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
