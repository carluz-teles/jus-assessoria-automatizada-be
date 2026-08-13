package acquisition

import (
	"context"
	"encoding/json"
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

// OABEntry is one OAB registration in a connector's discovery scope: the bare
// number and the two-letter UF (seccional) it belongs to. The DJEN discovers a
// tenant's processes per (number, UF), so the pair — not a combined string — is
// what a fetch drives.
type OABEntry struct {
	Number string
	UF     string
}

// FetchRequest is everything a connector needs to fetch one window of history:
// which capability to exercise, which integration it serves, the date bounds
// (bare dates, matching the sync_requested payload), and — for OAB discovery —
// the OAB registrations to query. It carries no credentials — the connector
// resolves those from its own configuration (credential_ref).
type FetchRequest struct {
	Capability    Capability
	IntegrationID string
	WindowFrom    string
	WindowTo      string
	OABs          []OABEntry
	// CNJNumber and Court drive FETCH_BY_NUMBER (DATAJUD enrichment): the process to
	// pull and the tribunal whose index holds it (the DATAJUD alias is per-tribunal).
	// Both are empty for DISCOVER_BY_OAB.
	CNJNumber string
	Court     string
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
// and any intimations. The sync use case upserts each list and emits the
// observed events. The lists are keyed to each other by (CNJNumber, Degree):
// a docket entry/intimation names the record it belongs to, which the use case
// resolves to a court_record id after FindOrCreateCourtRecord.
type ParsedResult struct {
	CourtRecords  []ParsedCourtRecord
	DocketEntries []ParsedDocketEntry
	Intimations   []ParsedIntimation
	// Parties are the process's partes (autor/réu/terceiro) with their advogados, as
	// the source discloses them (DJEN `destinatarios` + `destinatarioadvogados`). Like
	// the other lists they are keyed to a record by (CNJNumber, Degree); the use case
	// resolves that to a case_id and upserts them idempotently. DATAJUD never carries
	// parties (LGPD), so only the DJEN parser fills this.
	Parties []ParsedParty
}

// ParsedParty is one party of a process as the source discloses it, with the
// advogados the same communication addressed. Role is the polo→role mapping
// (A→PLAINTIFF, P→DEFENDANT, else THIRD_PARTY); Name is the destinatário's nome.
// It belongs to the record named by (CNJNumber, Degree) and, downstream, to that
// record's case. The DJEN does not link an advogado to a specific party, so Counsels
// are the communication's advogados attributed to this party by the ingestion's
// polo-inference rule (see the note in sync.go) — never invented, and CPF/CNPJ is
// never fabricated (the party's document stays absent until manual entry).
type ParsedParty struct {
	CNJNumber string
	Degree    string
	Role      string
	Name      string
	Counsels  []ParsedCounsel
}

// ParsedCounsel is one advogado representing a ParsedParty: nome + OAB (número, UF)
// as the DJEN `destinatarioadvogados.advogado` discloses them.
type ParsedCounsel struct {
	Name string
	OAB  string
	UF   string
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
	// JudgingBody is the órgão julgador the source disclosed (DJEN nomeOrgao /
	// DATAJUD orgaoJulgador); empty when it did not.
	JudgingBody string
	// FiledAt (DATAJUD dataAjuizamento) and Secrecy (from nivelSigilo) are disclosed
	// only by DATAJUD enrichment; FiledAt is the zero time and Secrecy is empty when
	// the source (DJEN discovery) does not carry them.
	FiledAt time.Time
	Secrecy string
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
	// TPUCode is the movimento's code in the Tabela Processual Unificada (DATAJUD
	// movimento.codigo); Complements carries its complementosTabelados as raw jsonb.
	// Zero/empty when the source does not classify the entry.
	TPUCode     int
	Complements json.RawMessage
}

// ParsedIntimation is one intimação. Hash dedups within the (tenant, case)
// scope; the three dates are already derived by the parser (the deadline slice
// consumes them later). It belongs to the record named by (CNJNumber, Degree).
// Type/Status/SourceURL/CancelledAt/CancelReason are the DJEN fields (0014):
// Status is ACTIVE on a fresh publication and CANCELLED when the source retracts
// it (then CancelledAt/CancelReason are set). Recipients is the jsonb list of
// every addressee with a matched flag on the tenant's OAB, carried verbatim to
// the column. Type/SourceURL/CancelReason are empty when the source omits them.
type ParsedIntimation struct {
	CNJNumber       string
	Degree          string
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

// Parser is the port that turns a RawPayload into a ParsedResult. CanParse lets
// a ParserSet match a payload to the parser that understands it; a Parse failure
// is terminal (a malformed payload never parses on retry), so the sync use case
// records FAILED and archives the task (asynq.SkipRetry). Parse takes a context
// because a real parser may need I/O to finish the mapping — the DJEN parser
// derives the publication/deadline dates through lib/calendar (holiday lookups),
// outside the sync transaction.
type Parser interface {
	CanParse(p RawPayload) bool
	Parse(ctx context.Context, p RawPayload) (ParsedResult, error)
}

// ParserSet is the parser counterpart of the Orchestrator: it resolves the first
// registered parser that CanParse a payload and delegates to it. It is itself a
// Parser, so the sync use case still depends on the single Parser port while the
// composition wires one parser per source (a real DJEN parser next to a stub for
// the sources not yet built). Order is significant — the first match wins — so
// register the specific parsers before any catch-all.
type ParserSet []Parser

var _ Parser = ParserSet(nil)

// CanParse reports whether any member can parse the payload.
func (s ParserSet) CanParse(p RawPayload) bool {
	for _, parser := range s {
		if parser.CanParse(p) {
			return true
		}
	}
	return false
}

// Parse delegates to the first member that CanParse the payload, or returns the
// typed ErrParserNotFound when none claims it (a misconfigured composition — the
// sync use case treats it like any parse fault).
func (s ParserSet) Parse(ctx context.Context, p RawPayload) (ParsedResult, error) {
	for _, parser := range s {
		if parser.CanParse(p) {
			return parser.Parse(ctx, p)
		}
	}
	return ParsedResult{}, ErrParserNotFound
}
