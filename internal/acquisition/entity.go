// Package acquisition is the vertical slice that onboards a tenant onto the
// external data sources it wants monitored. Its only v0 use case is activation:
// POST /v1/acquisition/integrations upserts the tenant's DJEN integration row and
// emits acquisition.integration_activated in the same transaction, so downstream
// slices (sync, backfill) learn the watch went live.
package acquisition

import (
	"encoding/json"
	"time"
)

// Source constants — the data sources involved in acquisition. Only DJEN is
// activatable (ActivateIntegration always targets it — see validation.go).
// DATAJUD is enrichment-only, triggered automatically by court_record_observed,
// never by activation. UPLOAD exists in the schema but is not an automated feed.
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

// Secrecy constants — a court_record's disclosure level (schema default PUBLIC).
// DATAJUD's nivelSigilo (0..N) maps onto these; DJEN discovery never sets it.
const (
	SecrecyPublic     = "PUBLIC"
	SecrecyRestricted = "RESTRICTED"
	SecrecySecret     = "SECRET"
)

// Lifecycle constants — a court_record's process-liveness flag. Only ACTIVE counts
// against the plan ceiling; SUPERSEDED is the UNKNOWN placeholder after a DATAJUD
// grade reveal re-pointed its children to the graded record (placeholder+merge).
// SUSPENDED and ARCHIVED are the remaining lifecycle values the schema stores (the
// KPI summary buckets read them); they are part of the closed set the processos
// list filter exposes.
const (
	LifecycleActive     = "ACTIVE"
	LifecycleSuspended  = "SUSPENDED"
	LifecycleArchived   = "ARCHIVED"
	LifecycleSuperseded = "SUPERSEDED"
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
// what downstream upserts (docket entries, intimations) and the
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

// Intimation type constants — the DJEN communication kind (tipoComunicacao). The
// deadline slice's counting rule branches on it (a citação and an intimação count
// differently). Text validated on the app, per the CHECK-on-app convention.
const (
	IntimationTypeIntimacao   = "INTIMACAO"
	IntimationTypeCitacao     = "CITACAO"
	IntimationTypeComunicacao = "COMUNICACAO"
)

// Intimation status constants — its cancellation lifecycle. A publication lands
// ACTIVE; when the DJEN retracts it (data_cancelamento) the same hash re-arrives
// as CANCELLED and the upsert flips the row, so the deadline slice revokes the
// derived prazo. The column default is ACTIVE.
const (
	IntimationStatusActive    = "ACTIVE"
	IntimationStatusCancelled = "CANCELLED"
)

// Intimation user-status constants — the triagem workflow state the user drives
// from the inbox, stored in the SEPARATE `user_status` column (0030), never the
// DJEN cancellation `status` above. A fresh (or reopened) intimação is PENDING; the
// user resolves or ignores it. The sync upsert never writes this column, so a
// re-observation cannot clobber the user's decision. Text validated on the app
// (CHECK-on-app convention). PENDING is the column default.
const (
	IntimationUserStatusPending  = "PENDING"
	IntimationUserStatusResolved = "RESOLVED"
	IntimationUserStatusIgnored  = "IGNORED"
)

// Urgência constants — the closed set of deadline-proximity filter values for the
// intimações inbox (?urgencia query param). The handler validates against this set;
// the SQL predicate maps each value to a deadline subquery.
const (
	UrgenciaAtraso           = "atraso"             // prazo vencido (days_left < 0, status PENDING|OPEN, user_status != RESOLVED|IGNORED)
	UrgenciaHoje             = "hoje"               // vence hoje (days_left = 0, status PENDING|OPEN, user_status != RESOLVED|IGNORED)
	UrgenciaProximosDoisDias = "proximos_dois_dias" // vence em 1-2 dias (status PENDING|OPEN, user_status != RESOLVED|IGNORED)
	UrgenciaSemana           = "semana"             // vence em 3-7 dias (status PENDING|OPEN, user_status != RESOLVED|IGNORED)
	// UrgenciaEsteMes — prazo no mês corrente: days_left > 7 E end_date <= último dia do
	// mês corrente (status PENDING|OPEN, user_status != RESOLVED|IGNORED). Disjunto de
	// semana (>7 corta o overlap) e de mais_adiante (teto no fim do mês).
	UrgenciaEsteMes = "este_mes"
	// UrgenciaMaisAdiante — prazo estritamente além do mês corrente: end_date > último dia
	// do mês corrente (status PENDING|OPEN, user_status != RESOLVED|IGNORED).
	UrgenciaMaisAdiante = "mais_adiante"
	// UrgenciaSemDataDefinida — intimação sem prazo derivado (deadline IS NULL, user_status
	// != RESOLVED|IGNORED). Disjunto de todos os buckets temporais (que exigem deadline).
	UrgenciaSemDataDefinida = "sem_data_definida"
	// UrgenciaNaoConfirmado is NO LONGER a urgência bucket — it became the server-side
	// "Não confirmadas" triage toggle (separate ?nao_confirmado param, predicate
	// d.status = 'PENDING'). Kept as a documented alias of the chip predicate, not part of
	// the urgência closed set.
	UrgenciaNaoConfirmado = "nao_confirmado"
	// UrgenciaSemProvidencia is REMOVED from the urgência closed set (redesign v1). Kept
	// ONLY as a deprecated alias so legacy deep-links (?urgencia=sem_providencia) degrade to
	// "no filter" instead of a hard 400. It is not offered as a tab and has no SQL bucket.
	UrgenciaSemProvidencia = "sem_providencia"
)

// Party role constants — a party's polo in the process (CHECK-on-app enum). The
// DJEN destinatário's polo maps onto these via partyRoleFromPolo: "A" (ativo =
// autor) → PLAINTIFF, "P" (passivo = réu) → DEFENDANT, anything else → THIRD_PARTY.
const (
	PartyRolePlaintiff  = "PLAINTIFF"
	PartyRoleDefendant  = "DEFENDANT"
	PartyRoleThirdParty = "THIRD_PARTY"
)

// PartySource constants — where a party row came from. v0 derives every party from
// the DJEN; MANUAL is the (future) manual-entry seam.
const (
	PartySourceDJEN   = "DJEN"
	PartySourceManual = "MANUAL"
)

// Party is one party of a process (autor/réu/terceiro), materialized so the cockpit
// can render the AUTOR/RÉU cards. It hangs off the case (shared across graus), deduped
// by (tenant, case, role, name). Document (CPF/CNPJ) is empty in v0 — the DJEN never
// discloses it and we never invent one.
type Party struct {
	ID       string
	TenantID string
	CaseID   string
	Role     string
	Name     string
	Document string
	Source   string
}

// PartyCounsel is one advogado representing a Party (nome + OAB número/UF), deduped by
// (tenant, party, oab, uf). Source records where it came from (DJEN in v0).
type PartyCounsel struct {
	ID       string
	TenantID string
	PartyID  string
	Name     string
	OAB      string
	UF       string
	Source   string
}

// Intimation is one intimação persisted by the sync cycle, deduped within the
// (tenant, case) scope. This slice does not emit an intimation-observed event
// (the deadline slice owns that), so the entity is the persisted shape, not an
// event carrier. Type/Status/SourceURL/CancelledAt/CancelReason are the DJEN
// fields (0014); Recipients is the jsonb list of every addressee with a matched
// flag on the tenant's OAB. Type/SourceURL/CancelReason are empty when the source
// does not disclose them; CancelledAt is the zero time unless status is CANCELLED.
type Intimation struct {
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
	Type            string
	Status          string
	SourceURL       string
	CancelledAt     time.Time
	CancelReason    string
	Recipients      json.RawMessage
}
