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

// Degree constants — a court_record's instance level, part of its natural key
// (tenant, cnj_number, degree). UNKNOWN is the safe default when the source does
// not disclose the degree.
const (
	DegreeG1       = "G1"
	DegreeG2       = "G2"
	DegreeJE       = "JE"
	DegreeSuperior = "SUPERIOR"
	DegreeUnknown  = "UNKNOWN"
)

// SyncRun is the auditable record of one sync execution: which connector ran,
// under which integration, and its outcome. It lands RUNNING at the start of the
// cycle and transitions to OK (with the item counters) or FAILED (with the
// error) at the end, in a second transaction. CourtRecordID is empty on OAB
// discovery (the run is not yet tied to a single record).
type SyncRun struct {
	ID               string
	TenantID         string
	CourtRecordID    string
	IntegrationID    string
	ConnectorID      string
	ConnectorVersion string
	Status           string
	ItemsNew         int
	ItemsDeduped     int
	StartedAt        time.Time
	FinishedAt       time.Time
}

// CourtRecord is what the court knows about a process at one degree — the
// FindOrCreate result the sync cycle keys everything else on. ID and CaseID are
// what downstream upserts (docket entries, notifications) and the
// court_record_observed event need; the rest of the schema's columns are not
// materialized into the entity in this slice.
type CourtRecord struct {
	ID        string
	TenantID  string
	CaseID    string
	CNJNumber string
	Degree    string
	Court     string
}

// DocketEntry is one andamento persisted by the sync cycle. The use case builds
// it only for entries that were actually inserted (the new set), so it can emit
// docket_entry_observed for each — ID is the freshly assigned row id.
type DocketEntry struct {
	ID            string
	CourtRecordID string
	Hash          string
	OccurredAt    time.Time
	ObservedAt    time.Time
	Source        string
	Fidelity      int
	Text          string
}

// Notification is one intimação persisted by the sync cycle, deduped within the
// (tenant, case) scope. This slice does not emit a notification-observed event
// (the deadline slice owns that), so the entity is the persisted shape, not an
// event carrier.
type Notification struct {
	ID              string
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
