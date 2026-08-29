package court

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

// court.connection_state_changed — the ERD's own name (§8) for the event the FE
// reacts to (polling or a live subscription) after Connect resolves asynchronously.
// Carries only the new status, never a secret or the error's full detail beyond a
// short human-readable message.

const (
	aggregateTypeCourtConnection = "court_connection"
	typeConnectionStateChanged   = "court.connection_state_changed"
)

type connectionStateChanged struct {
	events.Base
	TenantID     string `json:"tenant_id"`
	ConnectionID string `json:"connection_id"`
	Status       string `json:"status"`
}

// newConnectionStateChanged mints a FRESH event id per call (uuid v4, not a
// deterministic fact-derived key) — unlike a one-time terminal fact (certificate
// revoked), a connection's status can legitimately change to the SAME value many
// times over its life (e.g. ERROR → retry → ERROR again), and each occurrence is a
// distinct notification worth delivering, not a duplicate to dedup away.
func newConnectionStateChanged(tenantID, connectionID string, status Status) connectionStateChanged {
	return connectionStateChanged{
		Base:         events.Base{EventID: uuid.NewString(), Aggregate: connectionID},
		TenantID:     tenantID,
		ConnectionID: connectionID,
		Status:       string(status),
	}
}

func (connectionStateChanged) AggregateType() string { return aggregateTypeCourtConnection }
func (connectionStateChanged) Type() string          { return typeConnectionStateChanged }
