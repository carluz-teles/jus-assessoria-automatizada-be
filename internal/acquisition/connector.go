package acquisition

import (
	"context"
	"time"
)

// connector.go is the source-side seam of the sync cycle: the ports a
// data-source connector and its payload parser satisfy, plus the parsed shape
// the sync use case upserts. The REAL DJEN/DataJud connector and parser are
// future sub-slices; this slice ships only a stub (stub.go) behind these ports,
// so the whole fetch→parse→upsert cycle runs end to end without any network I/O.

// Capability is a discrete thing a connector can do. A connector advertises its
// set via Capabilities(); the orchestrator/use case picks a capability per fetch
// (window discovery vs. a single-number pull). Values are text, validated by
// the CHECK-on-app convention, never a Go enum on the wire.
type Capability string

const (
	// CapabilityFetchByNumber pulls one process by its CNJ number.
	CapabilityFetchByNumber Capability = "FETCH_BY_NUMBER"
	// CapabilityDiscoverByOAB discovers a tenant's processes by OAB over a window
	// — the mode an onboarding backfill slice drives.
	CapabilityDiscoverByOAB Capability = "DISCOVER_BY_OAB"
)

// FetchRequest is everything a connector needs to fetch one window of history:
// which capability to exercise, which integration it serves, and the date bounds
// (bare dates, matching the sync_requested payload). It carries no credentials —
// the connector resolves those from its own configuration (credential_ref).
type FetchRequest struct {
	Capability    Capability
	IntegrationID string
	WindowFrom    string
	WindowTo      string
}

// RawPayload is a connector's opaque output and the parser's input: the raw
// bytes fetched from the source, tagged with the connector identity so the sync
// run can be audited (connector_id/version) and the parser can refuse a payload
// it does not understand. Persisting Body to object storage is a later sub-slice
// (raw_payload_refs stays empty here).
type RawPayload struct {
	ConnectorID      string
	ConnectorVersion string
	Source           string
	Body             []byte
}

// Connector is the port a data source implements. ID/Version identify it in the
// sync_run audit row; Capabilities advertises what it can do; Fetch performs one
// pull. A Fetch failure is retryable infra by nature, but the sync use case
// records it as a FAILED run and acks (the scheduler re-syncs later), rather than
// burning asynq retries.
type Connector interface {
	ID() string
	Version() string
	Capabilities() []Capability
	Fetch(ctx context.Context, req FetchRequest) (RawPayload, error)
}

// ParsedResult is the connector-agnostic view a parser produces from a
// RawPayload: the court records observed in this window, their docket entries,
// and any notifications. The sync use case upserts each list and emits the
// observed events. The lists are keyed to each other by (CNJNumber, Degree):
// a docket entry/notification names the record it belongs to, which the use case
// resolves to a court_record id after FindOrCreateCourtRecord.
type ParsedResult struct {
	CourtRecords  []ParsedCourtRecord
	DocketEntries []ParsedDocketEntry
	Notifications []ParsedNotification
}

// ParsedCourtRecord is one court record as the source reports it. CNJNumber and
// Degree are its natural key (unique per tenant); Court is required by the
// schema; Completeness is the source's confidence that the record is fully
// populated (0..1), written on every sync.
type ParsedCourtRecord struct {
	CNJNumber    string
	Degree       string
	Court        string
	Class        string
	Subject      string
	Completeness float32
}

// ParsedDocketEntry is one andamento. Hash is the source-computed dedup key
// (unique per court_record); the use case inserts ON CONFLICT DO NOTHING and
// emits docket_entry_observed only for the rows that were actually new.
type ParsedDocketEntry struct {
	CNJNumber  string
	Degree     string
	Hash       string
	OccurredAt time.Time
	ObservedAt time.Time
	Source     string
	Fidelity   int
	Text       string
}

// ParsedNotification is one intimação. Hash dedups within the (tenant, case)
// scope; the three dates are already derived by the parser (the deadline slice
// consumes them later). It belongs to the record named by (CNJNumber, Degree).
type ParsedNotification struct {
	CNJNumber       string
	Degree          string
	Hash            string
	MadeAvailableAt time.Time
	PublishedAt     time.Time
	DeadlineStartAt time.Time
	Content         string
	Source          string
}

// Parser is the port that turns a RawPayload into a ParsedResult. CanParse lets
// the orchestrator match a payload to the parser that understands it; a Parse
// failure is terminal (a malformed payload never parses on retry), so the sync
// use case records FAILED and archives the task (asynq.SkipRetry).
type Parser interface {
	CanParse(p RawPayload) bool
	Parse(p RawPayload) (ParsedResult, error)
}
