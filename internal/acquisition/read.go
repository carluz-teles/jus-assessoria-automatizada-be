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
}

// IntimacaoView is one row of the intimações inbox: the intimation plus its court
// record's number/court/degree. ContentPreview is a truncation of the (often long,
// HTML) teor — the inbox shows a summary, the detail screen the full content.
type IntimacaoView struct {
	ID              string    `json:"id"`
	CNJNumber       string    `json:"cnj_number"`
	Court           string    `json:"court"`
	Degree          string    `json:"degree"`
	Type            string    `json:"type"`
	Status          string    `json:"status"`      // DJEN cancellation lifecycle (ACTIVE|CANCELLED)
	UserStatus      string    `json:"user_status"` // triagem state (PENDING|RESOLVED|IGNORED)
	Source          string    `json:"source"`
	SourceURL       string    `json:"source_url"`
	MadeAvailableAt time.Time `json:"made_available_at"`
	PublishedAt     time.Time `json:"published_at"`
	DeadlineStartAt time.Time `json:"deadline_start_at"`
	ContentPreview  string    `json:"content_preview"`
}

// IntimacaoDetailView is the deep-link detail of one intimation (GET
// /v1/intimacoes/:id). It embeds the full IntimacaoView (so every list field the FE
// already renders is present — additive, nothing removed) and adds the detail-only
// extras the inbox row omits: the FULL teor (not the truncated preview), the court
// record's órgão julgador, and the addressee list. Recipients is the jsonb column
// forwarded verbatim (a list of {name, oab, matched}); it defaults to an empty array,
// never JSON null.
type IntimacaoDetailView struct {
	IntimacaoView
	Content     string          `json:"content"`      // FULL teor (untruncated), for the detail screen
	JudgingBody string          `json:"judging_body"` // court_record.judging_body (órgão julgador)
	Recipients  json.RawMessage `json:"recipients"`   // destinatários (jsonb array), verbatim
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
// /v1/intimacoes/summary): the tenant's intimation counts bucketed by triagem state.
// EmAnalise and Criticas have no source yet (Fase 3 / prazo derivation) — always 0;
// see SummarizeIntimacoes.
type IntimacoesSummaryView struct {
	Total      int64 `json:"total"`
	Pendentes  int64 `json:"pendentes"`
	EmAnalise  int64 `json:"em_analise"`
	Resolvidas int64 `json:"resolvidas"`
	Ignoradas  int64 `json:"ignoradas"`
	Criticas   int64 `json:"criticas"`
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
}

// Filtered reports whether any list filter (search included) is active.
func (q IntimacoesQuery) Filtered() bool {
	return q.Search != "" || q.Type != "" || q.UserStatus != "" || q.Court != ""
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

type IntimacoesResult struct {
	Items      []IntimacaoView
	HasMore    bool
	TotalCount int64
	Total      int64
	Filters    httpx.Filters
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
}

// ReadUseCase serves the screen reads. It is a pagination policy over readRepo: it
// over-fetches one row per page so the handler learns whether more remain without a
// separate COUNT.
type ReadUseCase struct {
	repo readRepo
}

// NewReadUseCase wires the read use case (share the slice's repo with the writer).
func NewReadUseCase(repo readRepo) *ReadUseCase {
	return &ReadUseCase{repo: repo}
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
// assembled alongside.
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
	courts, err := uc.repo.ListIntimacaoCourts(ctx, q.TenantID)
	if err != nil {
		return IntimacoesResult{}, err
	}
	f := httpx.Filters{}
	f.Set("court", httpx.OptionsFromStrings(courts)...)
	f.SetEnum("type", IntimationTypeIntimacao, IntimationTypeCitacao, IntimationTypeComunicacao)
	f.SetEnum("user_status", IntimationUserStatusPending, IntimationUserStatusResolved, IntimationUserStatusIgnored)
	return IntimacoesResult{Items: rows, HasMore: hasMore, TotalCount: totalCount, Total: total, Filters: f}, nil
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
