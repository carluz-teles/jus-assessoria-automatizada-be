package acquisition

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jusassessoria/platform/lib/httpx"
)

// read.go is the slice's read side: the screen reads that bypass the write
// aggregate (a DTO per query, not the entity). The use case here is a thin
// pagination policy over the repo's keyset reads — it over-fetches one row to tell
// the handler whether a next page exists, without a COUNT.

// NextDeadlineView carries the nearest OPEN/PENDING prazo of a process, embedded as
// a nullable pointer in ProcessoView (nil = JSON null when the process has no open
// prazo). DaysLeft is the calendar-day distance to EndDate (negative = vencido, 0 =
// vence hoje). Kind mirrors deadline.kind (the prazo type string).
type NextDeadlineView struct {
	EndDate  time.Time `json:"end_date"`
	DaysLeft int       `json:"days_left"`
	Kind     string    `json:"kind"`
}

// ProcessoView is one row of the consolidated processes screen: the court record
// plus its most recent andamento. Nullable columns are pointers so an absent value
// serializes as JSON null, not a zero.
type ProcessoView struct {
	ID           string     `json:"id"`
	CaseID       string     `json:"case_id"`
	CNJNumber    string     `json:"cnj_number"`
	Court        string     `json:"court"`
	Degree       string     `json:"degree"`
	Class        string     `json:"class"`
	Subject      string     `json:"subject"`
	JudgingBody  string     `json:"judging_body"`
	FiledAt      *time.Time `json:"filed_at"`
	Secrecy      string     `json:"secrecy"`
	Lifecycle    string     `json:"lifecycle"`
	Completeness float32    `json:"completeness"`
	ClaimValue   *string    `json:"claim_value"` // valor da causa (numeric); nil (JSON null) when unset
	// responsável do processo — assigned at case level (court_case), so it is shared
	// across the process's graus. Both nil (JSON null) when no one is assigned; name
	// is the app_user.name joined in, so the FE renders the header without a second read.
	AssignedUserID   *string    `json:"assigned_user_id"`
	AssignedUserName *string    `json:"assigned_user_name"`
	LastMovementText string     `json:"last_movement_text"`
	LastMovementAt   *time.Time `json:"last_movement_at"`
	// NextDeadline is the nearest OPEN/PENDING prazo of the process (soonest end_date
	// first). Nil (JSON null) when the process has no open/pending prazo. Derived from a
	// correlated subquery in ListProcessos/GetProcesso — no extra round-trip.
	NextDeadline *NextDeadlineView `json:"next_deadline"`
}

// IntimacaoPrazoView carries the derived deadline for one intimation. It is embedded
// as a nullable pointer in IntimacaoView (nil = JSON null when no prazo exists yet).
// Status mirrors deadline.status (PENDING|OPEN|MET|MISSED|CANCELLED). Confirmed is
// true when the prazo was human-confirmed (confirmed_by IS NOT NULL). DaysLeft is the
// calendar-day distance to end_date (negative = vencido, 0 = vence hoje).
type IntimacaoPrazoView struct {
	DeadlineID string    `json:"deadline_id"`
	EndDate    time.Time `json:"end_date"`
	DaysLeft   int       `json:"days_left"`
	Status     string    `json:"status"`
	Confirmed  bool      `json:"confirmed"`
}

// IntimacaoView is one row of the intimações inbox: the intimation plus its court
// record's number/court/degree. ContentPreview is a truncation of the (often long,
// HTML) teor — the inbox shows a summary, the detail screen the full content. Prazo
// is nil (JSON null) when the intimação has no derived deadline yet; non-nil when the
// deadline slice derived one (1:1 per notification_id).
type IntimacaoView struct {
	ID              string              `json:"id"`
	CNJNumber       string              `json:"cnj_number"`
	Class           string              `json:"class"`           // court_record.class (classe processual)
	Subject         string              `json:"subject"`         // court_record.subject (assunto; pode vir vazio)
	CourtRecordID   string              `json:"court_record_id"` // court_record.id (deep-link ao processo)
	Court           string              `json:"court"`
	Degree          string              `json:"degree"`
	Type            string              `json:"type"`
	Status          string              `json:"status"`      // DJEN cancellation lifecycle (ACTIVE|CANCELLED)
	UserStatus      string              `json:"user_status"` // triagem state (PENDING|RESOLVED|IGNORED)
	Source          string              `json:"source"`
	SourceURL       string              `json:"source_url"`
	MadeAvailableAt time.Time           `json:"made_available_at"`
	PublishedAt     time.Time           `json:"published_at"`
	DeadlineStartAt time.Time           `json:"deadline_start_at"`
	ContentPreview  string              `json:"content_preview"`
	Prazo           *IntimacaoPrazoView `json:"prazo"` // nil (JSON null) when no prazo derived yet
	// Análise IA (0051) + responsável (0057, ex-conductor/reviewer). AIAnalyzedAt nil = "não
	// analisada" badge; non-nil = já analisada. AssigneeUserID/Name backam o toggle "Minhas"
	// e o rótulo do responsável na linha, joined via LEFT JOIN app_user — nil = não atribuído.
	AIAnalyzedAt     *time.Time `json:"ai_analyzed_at"`
	AssigneeUserID   *string    `json:"assignee_user_id"`
	AssigneeUserName *string    `json:"assignee_user_name"`
}

// IntimacaoHistoryEntry is one event in the intimation's derived timeline (Histórico
// card). It is assembled in the read use case from fields already fetched by GetIntimacao
// — no extra round-trip, no new table. OccurredAt is the event's timestamp (or date cast
// to midnight UTC for date-only sources); Label is the human-readable description.
type IntimacaoHistoryEntry struct {
	OccurredAt time.Time `json:"occurred_at"`
	Label      string    `json:"label"`
}

// Providência statuses — the lifecycle of one AI-suggested providência. Born SUGGESTED;
// "Aprovar e atribuir" promotes it to APPROVED (after the FE has created the real task),
// "Descartar" moves it to DISCARDED. Only SUGGESTED items carry the actionable buttons in
// the FE (APPROVED/DISCARDED are terminal). text + app-level CHECK (never a DB enum).
const (
	ProvidenciaStatusSuggested = "SUGGESTED"
	ProvidenciaStatusApproved  = "APPROVED"
	ProvidenciaStatusDiscarded = "DISCARDED"
)

// IntimacaoProvidenciaView is one derived providência of the AI analysis: a short
// imperative title (the IA already embeds the legal citation in the title, e.g. "Redigir
// defesa (art. 919, CPC)") plus an actionable description, an IA-suggested responsável and
// due date (both nullable — the model may omit them, then the FE shows "—"), and a lifecycle
// Status (SUGGESTED|APPROVED|DISCARDED). Serialized inside the detail view's ai_providencias
// jsonb array. The suggested_assignee_user_id is one of the firm's real app_user ids (chosen
// by the model from the member list injected into the prompt); the FE resolves the name from
// the row's suggested_assignee_name (or re-resolves via the members directory).
type IntimacaoProvidenciaView struct {
	Title                   string  `json:"title"`
	Description             string  `json:"description"`
	SuggestedAssigneeUserID *string `json:"suggested_assignee_user_id"`
	SuggestedAssigneeName   *string `json:"suggested_assignee_name"`
	DueDate                 *string `json:"due_date"` // "2006-01-02" or null
	Status                  string  `json:"status"`   // SUGGESTED|APPROVED|DISCARDED
	// TaskID is the real task created when the providência was APPROVED (the FE creates the
	// task via POST /v1/tasks, then passes its id to the aprovar endpoint so the approved row
	// can link back to it). Null for SUGGESTED/DISCARDED, or when the client omitted it.
	TaskID *string `json:"task_id"`
}

// IntimacaoDetailView is the deep-link detail of one intimation (GET
// /v1/intimacoes/:id). It embeds the full IntimacaoView (so every list field the FE
// already renders is present — additive, nothing removed) and adds the detail-only
// extras the inbox row omits: the FULL teor (not the truncated preview), the court
// record's órgão julgador, the addressee list, and the derived Histórico timeline.
// Recipients is the jsonb column forwarded verbatim (a list of {name, oab, matched});
// it defaults to an empty array, never JSON null. History is always an initialized
// slice (never null), ordered by occurred_at ASC. The single responsável is already
// carried by the embedded IntimacaoView (AssigneeUserID/Name).
type IntimacaoDetailView struct {
	IntimacaoView
	Content string `json:"content"` // FULL teor (untruncated), for the detail screen
	JudgingBody string `json:"judging_body"` // court_record.judging_body (órgão julgador)
	// DistributionDate é a data de ajuizamento/distribuição (court_record.filed_at
	// — migration 0015). Nullable porque DJEN não carrega (só o enriquecimento
	// DATAJUD preenche). Formato "YYYY-MM-DD" (date puro, sem timezone) — a UI
	// só exibe a linha "Distribuição" quando não-vazio.
	DistributionDate string                  `json:"distribution_date,omitempty"`
	Recipients       json.RawMessage         `json:"recipients"` // destinatários (jsonb array), verbatim
	History          []IntimacaoHistoryEntry `json:"history"`    // timeline derivada, ASC, nunca null

	// Análise IA (0051) — o card "Analisar esta intimação". AISummary vazio com
	// AIAnalyzedAt (no embedded IntimacaoView) non-nil = modo degradado (IA
	// indisponível). AIProvidencias é sempre inicializado (nunca null).
	AISummary      string                     `json:"ai_summary,omitempty"`
	AIProvidencias []IntimacaoProvidenciaView `json:"ai_providencias"`
}

// ProcessosSummaryView is the KPI header of the processes list (GET
// /v1/processos/summary): the tenant's process counts bucketed by court_record
// lifecycle. Baixados has no lifecycle source in v0 (always 0); see SummarizeProcessos.
type ProcessosSummaryView struct {
	Total       int64 `json:"total"`
	EmAndamento int64 `json:"em_andamento"`
	Suspensos   int64 `json:"suspensos"`
	Arquivados  int64 `json:"arquivados"`
	Baixados    int64 `json:"baixados"`
}

// IntimacoesSummaryView is the KPI header of the intimações inbox (GET
// /v1/intimacoes/summary): the tenant's intimation counts. The four urgência cards
// (EmAtraso, VencemHoje, NaoConfirmado, Pendentes) are derived from the deadline
// join. EmAnalise and Criticas have no source yet (always 0) — kept for stability.
type IntimacoesSummaryView struct {
	Total         int64 `json:"total"`
	Pendentes     int64 `json:"pendentes"`
	EmAnalise     int64 `json:"em_analise"`
	Resolvidas    int64 `json:"resolvidas"`
	Ignoradas     int64 `json:"ignoradas"`
	Criticas      int64 `json:"criticas"`
	EmAtraso      int64 `json:"em_atraso"`
	VencemHoje    int64 `json:"vencem_hoje"`
	NaoConfirmado int64 `json:"nao_confirmado"`
}

// AndamentoView is one row of a process's "Andamentos" tab: a docket entry
// (andamento) — when the court acted, when we observed it, the TPU code, and the
// text. TPUCode is a pointer so an absent code serializes as JSON null, not 0.
type AndamentoView struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	ObservedAt time.Time `json:"observed_at"`
	TPUCode    *int      `json:"tpu_code"`
	Text       string    `json:"text"`
	Source     string    `json:"source"`
	Fidelity   int       `json:"fidelity"`
}

// PartyCounselView is one advogado of a party on the cockpit's AUTOR/RÉU cards.
type PartyCounselView struct {
	Name string `json:"name"`
	OAB  string `json:"oab"`
	UF   string `json:"uf"`
}

// PartyView is one party (autor/réu/terceiro) with its advogados, for the cockpit's
// partes cards. Document (CPF/CNPJ) is a pointer so an absent value (always, in v0 —
// the DJEN never discloses it) serializes as JSON null, not "". Counsels is always an
// initialized array (never null) so the FE can map over it unconditionally.
type PartyView struct {
	Name     string             `json:"name"`
	Document *string            `json:"document"`
	Counsels []PartyCounselView `json:"counsels"`
}

// PartesView is the /processos/:id/partes read: the process's parties bucketed by role
// for the cockpit's AUTOR/RÉU cards. Terceiros carries THIRD_PARTY and anything the DJEN
// polo did not map to autor/réu. Each list is always initialized (never null) so a
// process with no discovered parties serializes as three empty arrays.
type PartesView struct {
	Autor     []PartyView `json:"autor"`
	Reu       []PartyView `json:"reu"`
	Terceiros []PartyView `json:"terceiros"`
}

// PartyRow is one party as the read repo returns it — a PartyView plus its role, so the
// read use case buckets it into the PartesView lists. Kept off PartyView (the wire shape)
// because the FE reads role implicitly from which bucket the party lands in.
type PartyRow struct {
	Role string
	PartyView
}

// ProcessosQuery / IntimacoesQuery carry the keyset cursor (the last row's sort key
// and id) and the page size. The handler fills the sentinel for a first page; the
// repo turns them into the query's keyset predicate. The filter fields mirror the
// envelope's selectable options: Court/kind-like free text (any well-formed value),
// Lifecycle/Type/UserStatus closed sets (validated at the handler), Assignee a user
// id ("me" resolved by the handler). An empty filter matches everything.
type ProcessosQuery struct {
	TenantID  string
	LastCNJ   string
	LastID    string
	Limit     int
	Search    string // ?search: ILIKE on cnj_number; "" means no filter
	Court     string // ?court: exact match on the record's court (from ListProcessoCourts)
	Lifecycle string // ?lifecycle: closed set (Lifecycle* consts); "" = default ACTIVE
	Degree    string // ?degree: exact match (from ListProcessoDegrees)
	Assignee  string // ?assignee: the case-level responsável's user id; "" = any
}

// Filtered reports whether any list filter (search included) is active — the repo
// uses it to decide when the "X de Y" counter needs the filtered COUNT.
func (q ProcessosQuery) Filtered() bool {
	return q.Search != "" || q.Court != "" || q.Lifecycle != "" || q.Degree != "" || q.Assignee != ""
}

type IntimacoesQuery struct {
	TenantID          string
	LastMadeAvailable string
	LastID            string
	Limit             int
	Search            string // ?search: ILIKE on the court record's cnj_number; "" means no filter
	Type              string // ?type: closed set (IntimationType* consts); "" = all
	UserStatus        string // ?user_status: closed set (IntimationUserStatus* consts); "" = all
	Court             string // ?court: exact match (from ListIntimacaoCourts); "" = all
	Urgencia          string // ?urgencia: closed set (atraso|hoje|proximos_dois_dias|semana|mais_adiante|nao_confirmado|sem_providencia); "" = all
	Assignee          string // ?assignee: a user id ("me" resolved by the handler); matches condutor OR revisor; "" = any
}

// Filtered reports whether any list filter (search included) is active.
func (q IntimacoesQuery) Filtered() bool {
	return q.Search != "" || q.Type != "" || q.UserStatus != "" || q.Court != "" || q.Urgencia != "" || q.Assignee != ""
}

// AndamentosQuery carries the descending keyset cursor (the last row's occurred_at
// and id) plus the process (court_record) whose andamentos to read and the tenant.
// The handler fills the max sentinel for a first page.
type AndamentosQuery struct {
	TenantID      string
	CourtRecordID string
	LastOccurred  string
	LastID        string
	Limit         int
}

// IntimacoesByProcessoQuery carries the descending keyset cursor (the last row's
// made_available_at and id) plus the process (court_record) whose intimations to read
// and the tenant. Mirrors AndamentosQuery — this tab has no ?search — but the sort key
// is the intimation's made_available_at (a date). The handler fills the max sentinel
// for a first page.
type IntimacoesByProcessoQuery struct {
	TenantID          string
	CourtRecordID     string
	LastMadeAvailable string
	LastID            string
	Limit             int
}

// ProcessosResult / IntimacoesResult are the paginated read plus the two totals for
// the "X de Y" counter: TotalCount is the current context (filtered by Search when
// set), Total the tenant-wide count. HasMore drives the next cursor. Filters is the
// selectable-options block the envelope renders as chips (assembled by the use case).
type ProcessosResult struct {
	Items      []ProcessoView
	HasMore    bool
	TotalCount int64
	Total      int64
	Filters    httpx.Filters
}

// IntimacaoBucketsView carries the per-urgência counts the list envelope exposes in
// the `buckets` object. Each count is the number of intimations that would appear
// when the user selects that urgência filter — computed over the same non-urgência
// filters (type, user_status, court, search) in a single aggregate query, so the
// header badges agree with the list without an N+1 round-trip.
// Atraso/Hoje/ProximosDoisDias/EstaSemana/SemProvidencia are the five tabs the
// master-detail inbox renders (in that order). SemProvidencia counts actionable
// intimações (not yet resolved/ignored) that have not been AI-analyzed yet
// (ai_analyzed_at IS NULL) — the "sem providência" tab, unrelated to whether a
// deadline was derived. MaisAdiante/NaoConfirmado are kept in the struct (not
// removed — other reads/tests still reference them) but are NOT one of the five
// tabs; the FE does not render them as a section.
type IntimacaoBucketsView struct {
	Atraso           int64 `json:"atraso"`
	Hoje             int64 `json:"hoje"`
	ProximosDoisDias int64 `json:"proximos_dois_dias"`
	EstaSemana       int64 `json:"esta_semana"`
	SemProvidencia   int64 `json:"sem_providencia"`
	MaisAdiante      int64 `json:"mais_adiante"`
	NaoConfirmado    int64 `json:"nao_confirmado"`
}

type IntimacoesResult struct {
	Items      []IntimacaoView
	HasMore    bool
	TotalCount int64
	Total      int64
	Filters    httpx.Filters
	Buckets    IntimacaoBucketsView
}

// AssigneeOption is one selectable ?assignee: the responsável's name (the chip label)
// and user id (the query-param value). Read-model DTO, off the write path.
type AssigneeOption struct {
	Name string
	ID   string
}

// AndamentosResult is a page of a process's andamentos plus its total for the "X de
// Y" counter. There is no search on this tab, so the two totals coincide; the read
// use case carries one Total. HasMore drives the next cursor. Filters is always
// empty on the tab (no filter chips).
type AndamentosResult struct {
	Items   []AndamentoView
	HasMore bool
	Total   int64
	Filters httpx.Filters
}

// IntimacoesByProcessoResult is a page of a process's intimations plus its total for
// the "X de Y" counter. Like AndamentosResult (no search on this tab), it carries a
// single Total; HasMore drives the next cursor. Filters is always empty (no chips).
type IntimacoesByProcessoResult struct {
	Items   []IntimacaoView
	HasMore bool
	Total   int64
	Filters httpx.Filters
}

// ImportStatusView is the onboarding backfill state for the FE banner ("importando
// seus processos…"): whether an import is running for the tenant, plus the slice
// tallies for a progress hint. Status NONE (no job ever) keeps the banner hidden.
type ImportStatusView struct {
	Importing   bool   `json:"importing"` // status == RUNNING
	Status      string `json:"status"`    // RUNNING | COMPLETED | PARTIAL | NONE
	TotalSlices int    `json:"total_slices"`
	SlicesOK    int    `json:"slices_ok"`
	SlicesError int    `json:"slices_error"`
}

// importStatusNone is the read-side sentinel for a tenant with no backfill_job — the
// FE banner treats it as "not importing" (never shown). It is NOT a DB status value.
const importStatusNone = "NONE"

// ReconciliationRunView is one row of the reconciliations screen: a sync_run with
// its integration's source and the failure reason (error jsonb message) lifted to
// a nullable string. Window bounds use the wire date format (2006-01-02).
type ReconciliationRunView struct {
	ID            string     `json:"id"`
	Source        string     `json:"source"`
	WindowFrom    string     `json:"window_from"`
	WindowTo      string     `json:"window_to"`
	Status        string     `json:"status"` // RUNNING | OK | FAILED
	ProcessosNew  int        `json:"processos_new"`
	IntimacoesNew int        `json:"intimacoes_new"`
	Error         *string    `json:"error"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

// ReconciliationTotals is the tenant's acquired-so-far tally shown in the
// reconciliations summary (and during an import, its progress line).
type ReconciliationTotals struct {
	CourtRecords int `json:"court_records"`
	Intimations  int `json:"intimations"`
}

// ReconciliationView is one import (backfill_job) on the reconciliations
// screen — the "reconciliação": the processes/intimations its windows discovered
// (summed), the overall date window (janela de prazo geral), the slice tallies and
// the lifecycle. The user drills into it for the per-window detail.
type ReconciliationView struct {
	ID          string     `json:"id"`
	Source      string     `json:"source"`
	Status      string     `json:"status"` // RUNNING | COMPLETED | PARTIAL
	WindowFrom  string     `json:"window_from"`
	WindowTo    string     `json:"window_to"`
	Processos   int        `json:"processos"`
	Intimacoes  int        `json:"intimacoes"`
	TotalSlices int        `json:"total_slices"`
	SlicesOK    int        `json:"slices_ok"`
	SlicesError int        `json:"slices_error"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// ReconciliationsView is the reconciliations screen read: the import banner state,
// the tenant's acquired totals, and one card per import (reconciliation), newest first.
type ReconciliationsView struct {
	Import          ImportStatusView     `json:"import"`
	Totals          ReconciliationTotals `json:"totals"`
	Reconciliations []ReconciliationView `json:"reconciliations"`
}

// ReconciliationDetailView is the deep view of one import: its reconciliation header plus
// every window (sync_run), chronological — the detail screen's table.
type ReconciliationDetailView struct {
	Reconciliation ReconciliationView      `json:"reconciliation"`
	Windows        []ReconciliationRunView `json:"windows"`
}

// CaptureRunView is one row of the "Capturas" screen: one capture (a daily fan-out,
// an enrichment day, or an initial load). Kind is returned raw (DAILY_CAPTURE |
// ENRICHMENT | INITIAL_LOAD) — the FE derives the human label from it. DisplayStatus
// is the BE-derived, user-facing status (see captureDisplayStatus). Nullable columns
// are pointers so an absent value serializes as JSON null, not a zero. DurationSec is
// nil while the capture is still running (no finished_at).
type CaptureRunView struct {
	ID                  string     `json:"id"`
	Source              string     `json:"source"`
	Kind                string     `json:"kind"`
	WindowFrom          *string    `json:"window_from"`
	WindowTo            *string    `json:"window_to"`
	StartedAt           time.Time  `json:"started_at"`
	FinishedAt          *time.Time `json:"finished_at"`
	Status              string     `json:"status"`         // raw: RUNNING|OK|PARTIAL|FAILED|COMPLETED
	DisplayStatus       string     `json:"display_status"` // BE-derived, user-facing
	CourtRecordsNew     int        `json:"court_records_new"`
	IntimationsNew      int        `json:"intimations_new"`
	CourtRecordsUpdated int        `json:"court_records_updated"`
	DeadlinesCreated    int        `json:"deadlines_created"`
	TasksCreated        int        `json:"tasks_created"`
	Errors              int        `json:"errors"`
	DurationSec         *int       `json:"duration_sec"`
	OABCount            int        `json:"oab_count"`
}

// CaptureSummaryView is the KPI header of the "Capturas" screen: the last successful
// capture, today's new intimations and derived deadlines, and the next scheduled run
// (nil — JSON null — when CAPTURE_DAILY_TIME is unset; the FE renders "—" then).
type CaptureSummaryView struct {
	LastCaptureAt         *time.Time `json:"last_capture_at"`
	IntimationsNewToday   int        `json:"intimations_new_today"`
	DeadlinesDerivedToday int        `json:"deadlines_derived_today"`
	NextExecution         *string    `json:"next_execution"`
}

// CapturesView is the whole "Capturas" screen read: the KPI header plus the capture
// rows (newest first, bounded — no pagination in v0).
type CapturesView struct {
	Summary CaptureSummaryView `json:"summary"`
	Runs    []CaptureRunView   `json:"runs"`
}

// captureRunsLimit caps the captures list: one screenful, newest first (an eventual
// "ver mais" pages beyond it — mirrors reconciliationRunsLimit).
const captureRunsLimit = 60

// Capture display statuses — the BE-derived, user-facing labels the FE renders as-is.
const (
	captureDisplayDone     = "Concluída"
	captureDisplayWithWarn = "Concluída com avisos"
	captureDisplayPartial  = "Falha parcial"
	captureDisplayRunning  = "Em andamento"
)

// captureDisplayStatus maps a capture's raw status + error count to the user-facing
// label. OK/COMPLETED with no errors is a clean success; OK/COMPLETED with errors is
// a success with warnings; PARTIAL/FAILED is a partial failure; RUNNING is in flight.
// An unknown status falls back to "Em andamento" (never a blank cell).
func captureDisplayStatus(status string, errs int) string {
	switch status {
	case SyncStatusOK, "COMPLETED":
		if errs > 0 {
			return captureDisplayWithWarn
		}
		return captureDisplayDone
	case "PARTIAL", SyncStatusFailed:
		return captureDisplayPartial
	default:
		return captureDisplayRunning
	}
}

// CaptureRunRow is one capture as the read repo returns it — the raw audit-trail
// columns, before the read use case derives DeadlinesCreated/TasksCreated (per-window
// counts), OABCount, DurationSec and DisplayStatus. Kept off CaptureRunView so the
// derivations live in the use case, not the repo (the repo stays a plain mapper).
type CaptureRunRow struct {
	ID                  string
	Source              string
	Kind                string
	WindowFrom          *string
	WindowTo            *string
	StartedAt           time.Time
	FinishedAt          *time.Time
	Status              string
	CourtRecordsNew     int
	IntimationsNew      int
	CourtRecordsUpdated int
	Errors              int
}

// CaptureSummaryRow is the KPI aggregate as the repo returns it — the two capture_run
// aggregates; the read use case folds in DeadlinesDerivedToday and NextExecution.
type CaptureSummaryRow struct {
	LastCaptureAt       *time.Time
	IntimationsNewToday int
}

// ProcessoLineView / IntimacaoLineView are the compact rows a window's collapse
// lists — the processes/intimations that window first discovered (via sync_run_id).
type ProcessoLineView struct {
	ID        string `json:"id"`
	CNJNumber string `json:"cnj_number"`
	Court     string `json:"court"`
	Degree    string `json:"degree"`
	Class     string `json:"class"`
}

type IntimacaoLineView struct {
	ID              string    `json:"id"`
	CNJNumber       string    `json:"cnj_number"`
	Court           string    `json:"court"`
	Degree          string    `json:"degree"`
	Type            string    `json:"type"`
	Status          string    `json:"status"`
	MadeAvailableAt time.Time `json:"made_available_at"`
}

// SyncRunItemsView is a window's collapse payload: the processes and intimations it
// brought, loaded on expand.
type SyncRunItemsView struct {
	Processos  []ProcessoLineView  `json:"processos"`
	Intimacoes []IntimacaoLineView `json:"intimacoes"`
}

// reconciliationRunsLimit caps the reconciliation list: one screenful of imports, newest
// first (an eventual "ver mais" pages beyond it).
const reconciliationRunsLimit = 60

// WatchedOABView is one monitored OAB returned by the "Termos" settings screen:
// the canonical FE key ("UFNUMBER", e.g. "SP347019") plus the lawyer's most
// frequent name derived from party_counsel — null when no capture exists yet.
type WatchedOABView struct {
	OAB  string  `json:"oab"`
	Name *string `json:"name"`
}

// readRepo is the narrow read port the ReadUseCase drives — the keyset list reads
// and the import-status/reconciliations reads, off the write path.
type readRepo interface {
	ListProcessos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, error)
	GetProcesso(ctx context.Context, tenantID, id string) (ProcessoView, error)
	ListIntimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, error)
	GetIntimacao(ctx context.Context, tenantID, id string) (IntimacaoDetailView, error)
	ListAndamentosByProcesso(ctx context.Context, q AndamentosQuery) ([]AndamentoView, error)
	ListIntimacoesByProcesso(ctx context.Context, q IntimacoesByProcessoQuery) ([]IntimacaoView, error)
	ListPartesByProcesso(ctx context.Context, tenantID, courtRecordID string) ([]PartyRow, error)
	CountProcessos(ctx context.Context, q ProcessosQuery) (totalCount, total int64, err error)
	CountIntimacoes(ctx context.Context, q IntimacoesQuery) (totalCount, total int64, err error)
	// BucketIntimacoes counts intimations per urgência bucket for the list envelope,
	// applying the same non-urgência filters as the list (type, user_status, court,
	// search) so the badges agree with the filtered list without an extra round-trip.
	BucketIntimacoes(ctx context.Context, q IntimacoesQuery) (IntimacaoBucketsView, error)
	// Filter options for the list envelopes — the distinct-value reads that back the
	// chips. Each is tenant-scoped and matches the list's own context predicate.
	ListProcessoCourts(ctx context.Context, tenantID string) ([]string, error)
	ListProcessoDegrees(ctx context.Context, tenantID string) ([]string, error)
	ListProcessoAssignees(ctx context.Context, tenantID string) ([]AssigneeOption, error)
	ListIntimacaoCourts(ctx context.Context, tenantID string) ([]string, error)
	SummarizeProcessos(ctx context.Context, tenantID string) (ProcessosSummaryView, error)
	SummarizeIntimacoes(ctx context.Context, tenantID string) (IntimacoesSummaryView, error)
	CountAndamentosByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	CountIntimacoesByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	GetImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
	GetReconciliationTotals(ctx context.Context, tenantID string) (ReconciliationTotals, error)
	ListReconciliations(ctx context.Context, tenantID string, limit int) ([]ReconciliationView, error)
	GetReconciliation(ctx context.Context, tenantID, jobID string) (ReconciliationView, error)
	ListSyncRunsByJob(ctx context.Context, tenantID, jobID string) ([]ReconciliationRunView, error)
	ListProcessosBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]ProcessoLineView, error)
	ListIntimacoesBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]IntimacaoLineView, error)
	// Captures screen — the audit-trail reads. ListCaptureRuns UNIONs capture_run
	// (daily/enrichment) with backfill_job (initial load); GetCaptureRun is the
	// per-capture detail (one capture_run row). GetCaptureSummary + the Count*
	// derivations back the KPI header and the per-row prazos/tarefas/OABs columns.
	ListCaptureRuns(ctx context.Context, tenantID string, limit int) ([]CaptureRunRow, error)
	GetCaptureRun(ctx context.Context, tenantID, id string) (CaptureRunRow, error)
	GetCaptureSummary(ctx context.Context, tenantID string) (CaptureSummaryRow, error)
	CountDeadlinesCreatedBetween(ctx context.Context, tenantID string, from, to time.Time) (int, error)
	CountTasksCreatedBetween(ctx context.Context, tenantID string, from, to time.Time) (int, error)
	CountDeadlinesCreatedToday(ctx context.Context, tenantID string) (int, error)
	CountWatchedOABsForDJEN(ctx context.Context, tenantID string) (int, error)
	ListWatchedOABsWithName(ctx context.Context, tenantID string) ([]WatchedOABView, error)
	// GetResumoContext assembles the full context for the AI process summary (last 10
	// andamentos, active intimações, open prazos + the court_record identification and
	// any cached ai_resume). Used by ResumoUseCase through the resumoReader port.
	GetResumoContext(ctx context.Context, tenantID, courtRecordID string) (ProcessoResumoCtx, error)
	// GetIntimacaoAnaliseContext assembles the context for the AI intimation analysis (the
	// teor + court record identification). Used by AnaliseUseCase through the analiseReader port.
	GetIntimacaoAnaliseContext(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseCtx, error)
}

// ReadUseCase serves the screen reads. It is a pagination policy over readRepo: it
// over-fetches one row per page so the handler learns whether more remain without a
// separate COUNT.
type ReadUseCase struct {
	repo readRepo
	// captureDailyTime is the optional HH:MM the Captures KPI header exposes as "next
	// execution" (from CAPTURE_DAILY_TIME). Empty = the FE renders "—"; the read use
	// case never fabricates a time from the wall clock.
	captureDailyTime string
}

// NewReadUseCase wires the read use case (share the slice's repo with the writer).
func NewReadUseCase(repo readRepo, opts ...ReadOption) *ReadUseCase {
	uc := &ReadUseCase{repo: repo}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// ReadOption configures the read use case at construction (functional options keep the
// constructor backward-compatible — every existing caller passes just the repo).
type ReadOption func(*ReadUseCase)

// WithCaptureDailyTime sets the "next execution" HH:MM shown on the Captures KPI
// header. An empty string is honest: the header's NextExecution stays nil (FE "—").
func WithCaptureDailyTime(hhmm string) ReadOption {
	return func(uc *ReadUseCase) { uc.captureDailyTime = hhmm }
}

// Processos returns up to q.Limit processes, whether a further page exists, and the
// "X de Y" totals (filtered by the active filters, plus the tenant-wide total). The
// keyset read over-fetches one row for hasMore; the totals are separate COUNTs (one
// when no filter, two when filtered) — a small skew vs the page under concurrent
// inserts is tolerable (read model, not aggregate). The envelope's filter options are
// assembled alongside (the distinct-value reads) so the FE renders the chips without
// a second request.
func (uc *ReadUseCase) Processos(ctx context.Context, q ProcessosQuery) (ProcessosResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListProcessos(ctx, q)
	if err != nil {
		return ProcessosResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	totalCount, total, err := uc.repo.CountProcessos(ctx, q)
	if err != nil {
		return ProcessosResult{}, err
	}
	filters, err := uc.processosFilters(ctx, q.TenantID)
	if err != nil {
		return ProcessosResult{}, err
	}
	return ProcessosResult{Items: rows, HasMore: hasMore, TotalCount: totalCount, Total: total, Filters: filters}, nil
}

// processosFilters assembles the processes screen's selectable options: the closed
// lifecycle set from the entity constants and the free-text court/degree/assignee
// options from the distinct-value reads. Each key is omitted when it has no options.
func (uc *ReadUseCase) processosFilters(ctx context.Context, tenantID string) (httpx.Filters, error) {
	courts, err := uc.repo.ListProcessoCourts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	degrees, err := uc.repo.ListProcessoDegrees(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	assignees, err := uc.repo.ListProcessoAssignees(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	f := httpx.Filters{}
	f.Set("court", httpx.OptionsFromStrings(courts)...)
	f.Set("degree", httpx.OptionsFromStrings(degrees)...)
	f.SetEnum("lifecycle", LifecycleActive, LifecycleSuspended, LifecycleArchived, LifecycleSuperseded)
	f.Set("assignee", assigneeOptions(assignees)...)
	return f, nil
}

// assigneeOptions maps the repo's {name, id} options to the envelope's label==name /
// value==id filter options, skipping an empty id (never selectable).
func assigneeOptions(assignees []AssigneeOption) []httpx.FilterOption {
	opts := make([]httpx.FilterOption, 0, len(assignees))
	for _, a := range assignees {
		if a.ID == "" {
			continue
		}
		opts = append(opts, httpx.FilterOption{Label: a.Name, Value: a.ID})
	}
	return opts
}

// Andamentos returns up to q.Limit of a process's docket entries (newest first),
// whether a further page exists, and the tab's total. Same over-fetch policy as
// Processos: the keyset read over-fetches one row for hasMore, the total is a
// separate COUNT — a small skew under concurrent inserts is fine (read model). The
// tab has no filter chips, so Filters is the empty object.
func (uc *ReadUseCase) Andamentos(ctx context.Context, q AndamentosQuery) (AndamentosResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListAndamentosByProcesso(ctx, q)
	if err != nil {
		return AndamentosResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	total, err := uc.repo.CountAndamentosByProcesso(ctx, q.TenantID, q.CourtRecordID)
	if err != nil {
		return AndamentosResult{}, err
	}
	return AndamentosResult{Items: rows, HasMore: hasMore, Total: total, Filters: httpx.Filters{}}, nil
}

// IntimacoesByProcesso returns up to q.Limit of a process's intimations (newest
// availability first), whether a further page exists, and the tab's total. Same
// over-fetch policy as Andamentos: the keyset read over-fetches one row for hasMore,
// the total is a separate COUNT — a small skew under concurrent inserts is fine (read
// model). The tab has no filter chips, so Filters is the empty object.
func (uc *ReadUseCase) IntimacoesByProcesso(ctx context.Context, q IntimacoesByProcessoQuery) (IntimacoesByProcessoResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListIntimacoesByProcesso(ctx, q)
	if err != nil {
		return IntimacoesByProcessoResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	total, err := uc.repo.CountIntimacoesByProcesso(ctx, q.TenantID, q.CourtRecordID)
	if err != nil {
		return IntimacoesByProcessoResult{}, err
	}
	return IntimacoesByProcessoResult{Items: rows, HasMore: hasMore, Total: total, Filters: httpx.Filters{}}, nil
}

// ImportStatus returns the tenant's latest backfill state — the FE banner reads it
// on load (so it survives a page refresh) and hides on the import_finished push.
func (uc *ReadUseCase) ImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error) {
	return uc.repo.GetImportStatus(ctx, tenantID)
}

// Reconciliations returns the reconciliations screen read: import banner state +
// acquired totals + one reconciliation per import. Three pool reads, no tx — a read
// model, not an aggregate; a small skew between them under concurrent syncs is fine.
func (uc *ReadUseCase) Reconciliations(ctx context.Context, tenantID string) (ReconciliationsView, error) {
	imp, err := uc.repo.GetImportStatus(ctx, tenantID)
	if err != nil {
		return ReconciliationsView{}, err
	}
	totals, err := uc.repo.GetReconciliationTotals(ctx, tenantID)
	if err != nil {
		return ReconciliationsView{}, err
	}
	recons, err := uc.repo.ListReconciliations(ctx, tenantID, reconciliationRunsLimit)
	if err != nil {
		return ReconciliationsView{}, err
	}
	return ReconciliationsView{Import: imp, Totals: totals, Reconciliations: recons}, nil
}

// ReconciliationDetail returns one import's reconciliação header plus every window
// (sync_run) it fanned out, chronological — the detail screen. A miss on the
// reconciliation surfaces as the repo's typed not-found.
func (uc *ReadUseCase) ReconciliationDetail(ctx context.Context, tenantID, jobID string) (ReconciliationDetailView, error) {
	reconciliation, err := uc.repo.GetReconciliation(ctx, tenantID, jobID)
	if err != nil {
		return ReconciliationDetailView{}, err
	}
	windows, err := uc.repo.ListSyncRunsByJob(ctx, tenantID, jobID)
	if err != nil {
		return ReconciliationDetailView{}, err
	}
	return ReconciliationDetailView{Reconciliation: reconciliation, Windows: windows}, nil
}

// Captures returns the "Capturas" screen read: the KPI header + one row per capture
// (daily fan-out, enrichment day, initial load), newest first, bounded. Several pool
// reads, no tx — a read model, not an aggregate; a small skew between them under a
// concurrent capture is fine. Each row's prazos/tarefas/OABs columns are derived here
// (per-window counts) so the repo stays a plain mapper.
func (uc *ReadUseCase) Captures(ctx context.Context, tenantID string) (CapturesView, error) {
	summaryRow, err := uc.repo.GetCaptureSummary(ctx, tenantID)
	if err != nil {
		return CapturesView{}, err
	}
	deadlinesToday, err := uc.repo.CountDeadlinesCreatedToday(ctx, tenantID)
	if err != nil {
		return CapturesView{}, err
	}
	summary := CaptureSummaryView{
		LastCaptureAt:         summaryRow.LastCaptureAt,
		IntimationsNewToday:   summaryRow.IntimationsNewToday,
		DeadlinesDerivedToday: deadlinesToday,
		NextExecution:         uc.nextExecution(),
	}

	rows, err := uc.repo.ListCaptureRuns(ctx, tenantID, captureRunsLimit)
	if err != nil {
		return CapturesView{}, err
	}
	// OABCount is the same tenant-wide DJEN watched-OAB count for every row (a capture
	// watches the tenant's whole OAB set), so read it once and reuse.
	oabCount, err := uc.repo.CountWatchedOABsForDJEN(ctx, tenantID)
	if err != nil {
		return CapturesView{}, err
	}
	views := make([]CaptureRunView, 0, len(rows))
	for _, row := range rows {
		view, err := uc.captureRunView(ctx, tenantID, row, oabCount)
		if err != nil {
			return CapturesView{}, err
		}
		views = append(views, view)
	}
	return CapturesView{Summary: summary, Runs: views}, nil
}

// CaptureDetail returns one capture (a DAILY_CAPTURE or ENRICHMENT capture_run) by id,
// tenant-scoped. The initial load (INITIAL_LOAD) keeps its existing reconciliation
// endpoint. A miss surfaces as the repo's typed 404.
func (uc *ReadUseCase) CaptureDetail(ctx context.Context, tenantID, id string) (CaptureRunView, error) {
	row, err := uc.repo.GetCaptureRun(ctx, tenantID, id)
	if err != nil {
		return CaptureRunView{}, err
	}
	oabCount, err := uc.repo.CountWatchedOABsForDJEN(ctx, tenantID)
	if err != nil {
		return CaptureRunView{}, err
	}
	return uc.captureRunView(ctx, tenantID, row, oabCount)
}

// captureRunView folds one audit-trail row into the wire view: it derives the
// user-facing status, the duration, and the per-window prazos/tarefas counts (over
// [started_at, finished_at]; skipped while the capture is still running — an open
// window has no bounded range to count over). OABCount is passed in (read once per
// screen). A running capture reports zero derived prazos/tarefas and a nil duration.
func (uc *ReadUseCase) captureRunView(ctx context.Context, tenantID string, row CaptureRunRow, oabCount int) (CaptureRunView, error) {
	view := CaptureRunView{
		ID:                  row.ID,
		Source:              row.Source,
		Kind:                row.Kind,
		WindowFrom:          row.WindowFrom,
		WindowTo:            row.WindowTo,
		StartedAt:           row.StartedAt,
		FinishedAt:          row.FinishedAt,
		Status:              row.Status,
		DisplayStatus:       captureDisplayStatus(row.Status, row.Errors),
		CourtRecordsNew:     row.CourtRecordsNew,
		IntimationsNew:      row.IntimationsNew,
		CourtRecordsUpdated: row.CourtRecordsUpdated,
		Errors:              row.Errors,
		OABCount:            oabCount,
	}
	// Só uma captura TERMINAL (OK/COMPLETED/PARTIAL/FAILED) concluiu: uma run "Em andamento"
	// não apresenta finished_at, duração nem contagens finais — mesmo que a linha ENRICHMENT
	// mantenha um finished_at MÓVEL como marca de última atividade (o upsert do contador o
	// avança a cada aplicação). Gate pelo STATUS, não pela presença de finished_at, senão
	// uma run em andamento mostraria "Concluída em: now()" e uma duração/contagem falsas.
	if !isTerminalCaptureStatus(row.Status) {
		view.FinishedAt = nil
		return view, nil
	}
	if row.FinishedAt == nil {
		return view, nil
	}
	sec := int(row.FinishedAt.Sub(row.StartedAt).Seconds())
	view.DurationSec = &sec

	// Prazos derivados / tarefas criadas vêm da DESCOBERTA (das intimações), não do
	// enriquecimento: a linha ENRICHMENT só "atualiza" processos, então não os atribui
	// (pertencem à carga inicial / captura diária que os descobriu). Contá-los na janela do
	// enriquecimento pegaria os prazos da fase de descoberta e inflaria a linha errada.
	if row.Kind == "ENRICHMENT" {
		return view, nil
	}

	deadlines, err := uc.repo.CountDeadlinesCreatedBetween(ctx, tenantID, row.StartedAt, *row.FinishedAt)
	if err != nil {
		return CaptureRunView{}, err
	}
	tasks, err := uc.repo.CountTasksCreatedBetween(ctx, tenantID, row.StartedAt, *row.FinishedAt)
	if err != nil {
		return CaptureRunView{}, err
	}
	view.DeadlinesCreated = deadlines
	view.TasksCreated = tasks
	return view, nil
}

// isTerminalCaptureStatus reports whether a capture's raw status is a concluded state
// (OK/COMPLETED/PARTIAL/FAILED) rather than RUNNING. Only a terminal capture presents a
// finished_at, a duration and finalized derived counts.
func isTerminalCaptureStatus(s string) bool {
	switch s {
	case SyncStatusOK, "COMPLETED", BackfillStatusPartial, SyncStatusFailed:
		return true
	default:
		return false
	}
}

// nextExecution returns the "next execution" HH:MM for the KPI header, or nil when
// CAPTURE_DAILY_TIME is unset — honest: no wall-clock guessing. The FE formats the
// bare "HH:MM" into "amanhã, HH:MM".
func (uc *ReadUseCase) nextExecution() *string {
	if uc.captureDailyTime == "" {
		return nil
	}
	t := uc.captureDailyTime
	return &t
}

// SyncRunItems returns a window's collapse payload: the processes and intimations it
// first discovered. Two pool reads; empty slices when the window brought nothing.
func (uc *ReadUseCase) SyncRunItems(ctx context.Context, tenantID, syncRunID string) (SyncRunItemsView, error) {
	processos, err := uc.repo.ListProcessosBySyncRun(ctx, tenantID, syncRunID)
	if err != nil {
		return SyncRunItemsView{}, err
	}
	intimacoes, err := uc.repo.ListIntimacoesBySyncRun(ctx, tenantID, syncRunID)
	if err != nil {
		return SyncRunItemsView{}, err
	}
	return SyncRunItemsView{Processos: processos, Intimacoes: intimacoes}, nil
}

// Intimacoes returns up to q.Limit intimations (newest availability first), whether a
// further page exists, and the "X de Y" totals (filtered by the active filters plus
// the tenant-wide total). Same shape as Processos; the envelope's filter options are
// assembled alongside. The result also carries Buckets — per-urgência counts that
// respect the same non-urgência filters, so section headers agree with the list.
func (uc *ReadUseCase) Intimacoes(ctx context.Context, q IntimacoesQuery) (IntimacoesResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListIntimacoes(ctx, q)
	if err != nil {
		return IntimacoesResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	totalCount, total, err := uc.repo.CountIntimacoes(ctx, q)
	if err != nil {
		return IntimacoesResult{}, err
	}
	buckets, err := uc.repo.BucketIntimacoes(ctx, q)
	if err != nil {
		return IntimacoesResult{}, err
	}
	courts, err := uc.repo.ListIntimacaoCourts(ctx, q.TenantID)
	if err != nil {
		return IntimacoesResult{}, err
	}
	f := httpx.Filters{}
	f.Set("court", httpx.OptionsFromStrings(courts)...)
	f.SetEnum("type", IntimationTypeIntimacao, IntimationTypeCitacao, IntimationTypeComunicacao)
	f.SetEnum("user_status", IntimationUserStatusPending, IntimationUserStatusResolved, IntimationUserStatusIgnored)
	f.SetEnum("urgencia", UrgenciaAtraso, UrgenciaHoje, UrgenciaProximosDoisDias, UrgenciaSemana, UrgenciaMaisAdiante, UrgenciaNaoConfirmado, UrgenciaSemProvidencia)
	return IntimacoesResult{
		Items: rows, HasMore: hasMore, TotalCount: totalCount, Total: total,
		Filters: f, Buckets: buckets,
	}, nil
}

// Intimacao returns one intimation by id for the FE deep-link (open the detail of an
// intimation not on the loaded inbox page). A plain pool read scoped to the tenant — no
// pagination policy — so it delegates straight to the repo, which maps a miss/foreign
// row to the typed 404 and a non-uuid id to the typed 400.
func (uc *ReadUseCase) Intimacao(ctx context.Context, tenantID, id string) (IntimacaoDetailView, error) {
	return uc.repo.GetIntimacao(ctx, tenantID, id)
}

// ProcessosSummary returns the processes list KPI counts (bucketed by lifecycle) for
// the tenant. A single aggregate read on the pool — a read model, not an aggregate.
func (uc *ReadUseCase) ProcessosSummary(ctx context.Context, tenantID string) (ProcessosSummaryView, error) {
	return uc.repo.SummarizeProcessos(ctx, tenantID)
}

// IntimacoesSummary returns the intimações inbox KPI counts (bucketed by triagem state)
// for the tenant. A single aggregate read on the pool — a read model, not an aggregate.
func (uc *ReadUseCase) IntimacoesSummary(ctx context.Context, tenantID string) (IntimacoesSummaryView, error) {
	return uc.repo.SummarizeIntimacoes(ctx, tenantID)
}

// Processo returns one process by id for the FE deep-link (open the detail of a process
// not on the loaded list page). A plain pool read scoped to the tenant — no pagination
// policy — so it delegates straight to the repo, which maps a miss/foreign row to the
// typed 404 and a non-uuid id to the typed 400.
func (uc *ReadUseCase) Processo(ctx context.Context, tenantID, id string) (ProcessoView, error) {
	return uc.repo.GetProcesso(ctx, tenantID, id)
}

// WatchedOABs returns the tenant's monitored OABs (DJEN) with the most-frequent
// lawyer name derived from party_counsel. OABs without a capture yet carry a nil
// name. Delegates straight to the repo — no pagination, the set is small.
func (uc *ReadUseCase) WatchedOABs(ctx context.Context, tenantID string) ([]WatchedOABView, error) {
	return uc.repo.ListWatchedOABsWithName(ctx, tenantID)
}

// GetResumoContext returns the full process context for the AI summary (identification,
// last 10 andamentos, active intimações, open prazos, cached ai_resume), tenant-scoped.
// It satisfies the resumoReader port the ResumoUseCase drives.
func (uc *ReadUseCase) GetResumoContext(ctx context.Context, tenantID, courtRecordID string) (ProcessoResumoCtx, error) {
	return uc.repo.GetResumoContext(ctx, tenantID, courtRecordID)
}

// GetIntimacaoAnaliseContext returns one intimation's teor + court context for the AI
// analysis (POST /v1/intimacoes/:id/analise), tenant-scoped. It satisfies the analiseReader
// port the AnaliseUseCase drives.
func (uc *ReadUseCase) GetIntimacaoAnaliseContext(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseCtx, error) {
	return uc.repo.GetIntimacaoAnaliseContext(ctx, tenantID, intimationID)
}

// Partes returns the process's parties (behind the court_record :id), bucketed by role
// for the cockpit's AUTOR/RÉU cards. A read model, not an aggregate: one tenant-scoped
// pool read, then the flat rows are folded into the three role lists. A foreign or
// unknown :id yields three empty lists (the repo's read resolves no case), never an
// error — this deep-read has no 404 (the parties tab of an absent process is simply
// empty). Each bucket is initialized so the payload is three arrays, never null.
func (uc *ReadUseCase) Partes(ctx context.Context, tenantID, courtRecordID string) (PartesView, error) {
	rows, err := uc.repo.ListPartesByProcesso(ctx, tenantID, courtRecordID)
	if err != nil {
		return PartesView{}, err
	}
	view := PartesView{
		Autor:     []PartyView{},
		Reu:       []PartyView{},
		Terceiros: []PartyView{},
	}
	for _, row := range rows {
		switch row.Role {
		case PartyRolePlaintiff:
			view.Autor = append(view.Autor, row.PartyView)
		case PartyRoleDefendant:
			view.Reu = append(view.Reu, row.PartyView)
		default:
			view.Terceiros = append(view.Terceiros, row.PartyView)
		}
	}
	return view, nil
}
