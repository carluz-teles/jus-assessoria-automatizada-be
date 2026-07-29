// Package acquisition is the vertical slice that onboards a tenant onto the
// external data sources it wants monitored. Its only v0 use case is activation:
// POST /v1/acquisition/integrations upserts one integration row per requested
// source and emits acquisition.integration_activated in the same transaction, so
// downstream slices (sync, backfill — future) learn a source went live.
package acquisition

import "time"

// Source constants — the data sources a tenant can subscribe to. Only DJEN and
// DATAJUD are activatable in v0; UPLOAD exists in the schema but is not an
// automated feed, so activation rejects it (see validation.go).
const (
	SourceDJEN    = "DJEN"
	SourceDATAJUD = "DATAJUD"
	SourceUpload  = "UPLOAD"
)

// Status constants — an integration's lifecycle. Activation always lands on
// ACTIVE; the degraded/failed/disabled states are set by later sync slices.
const (
	StatusActive   = "ACTIVE"
	StatusDegraded = "DEGRADED"
	StatusDisabled = "DISABLED"
)

// Scope is what an integration watches: the OAB registrations (and, later, tax
// ids) whose processes the source should surface. It is stored as the jsonb
// `scope` column and travels verbatim in the activation event payload, so its
// json tags are load-bearing.
type Scope struct {
	OAB   []string `json:"oab"`
	TaxID []string `json:"taxId,omitempty"`
}

// Integration is a tenant's subscription to one data source (not a court). The
// id is the internal uuid; CredentialRef points to a secret vault entry and is
// always empty (SQL NULL) in v0 — it is never accepted from the request body.
type Integration struct {
	ID            string
	TenantID      string
	Source        string
	Scope         Scope
	CredentialRef string
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
