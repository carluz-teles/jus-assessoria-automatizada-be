package deadline

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/httpx"
)

// read.go is the slice's read side: the prazos screen reads that bypass the write
// aggregate (a DTO per query, not the entity). It does NOT reuse the 2c write
// Repository — the reads run on the pool with their own narrow port (readRepo). The
// ReadUseCase is a thin pagination policy over that port: it over-fetches one row to
// tell the handler whether a next page exists, without a separate COUNT for hasMore.

// PrazoView is one row of a process's Prazos tab (GET /v1/processos/:id/prazos). The
// process context is implicit (the :id is the process), so it carries no cnj/court —
// only the prazo itself. DaysLeft is calendar days to EndDate (negative once overdue);
// the urgency styling is the FE's call. HolidaysApplied is the auditable "por quê".
type PrazoView struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	EndDate         time.Time `json:"end_date"`
	DaysLeft        int       `json:"days_left"`
	Counting        string    `json:"counting"`
	Doubled         bool      `json:"doubled"`
	DoubledReason   string    `json:"doubled_reason"`
	Status          string    `json:"status"`
	HolidaysApplied []string  `json:"holidays_applied"`
	IntimationID    string    `json:"intimation_id"`
	Confirmed       bool      `json:"confirmed"`
}

// AgendaPrazoView is one row of the global agenda (GET /v1/prazos): the same prazo
// fields plus the process context (court_record_id/cnj_number/court) the agenda needs
// to link back, since it spans every process of the tenant.
type AgendaPrazoView struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	EndDate         time.Time `json:"end_date"`
	DaysLeft        int       `json:"days_left"`
	Counting        string    `json:"counting"`
	Doubled         bool      `json:"doubled"`
	DoubledReason   string    `json:"doubled_reason"`
	Status          string    `json:"status"`
	HolidaysApplied []string  `json:"holidays_applied"`
	IntimationID    string    `json:"intimation_id"`
	Confirmed       bool      `json:"confirmed"`
	CourtRecordID   string    `json:"court_record_id"`
	CNJNumber       string    `json:"cnj_number"`
	Court           string    `json:"court"`
}

// PrazoDetailView is the audit view of one prazo (GET /v1/prazos/:id): every field the
// "por quê" popover and the auditor need — the full HolidaysApplied, the RulesVersion
// that derived the days, the origin IntimationID, and start/end/days/counting.
type PrazoDetailView struct {
	ID              string    `json:"id"`
	CourtRecordID   string    `json:"court_record_id"`
	Kind            string    `json:"kind"`
	StartDate       time.Time `json:"start_date"`
	EndDate         time.Time `json:"end_date"`
	DaysLeft        int       `json:"days_left"`
	Days            int       `json:"days"`
	Counting        string    `json:"counting"`
	Doubled         bool      `json:"doubled"`
	DoubledReason   string    `json:"doubled_reason"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	HolidaysApplied []string  `json:"holidays_applied"`
	IntimationID    string    `json:"intimation_id"`
	RulesVersion    string    `json:"rules_version"`
	Confirmed       bool      `json:"confirmed"`
	// Confirmation panel fields (migration 0049): the termo inicial the prazo is anchored on, the
	// frozen legal citation snapshot, the lawyer's manual extra days, and the audit stamp (who/
	// when confirmed, with the resolved name).
	AnchorEvent     string     `json:"anchor_event"`
	LegalCitation   string     `json:"legal_citation,omitempty"`
	ManualExtraDays int        `json:"manual_extra_days"`
	ConfirmedByID   string     `json:"confirmed_by_id,omitempty"`
	ConfirmedByName string     `json:"confirmed_by_name,omitempty"`
	ConfirmedAt     *time.Time `json:"confirmed_at"`
	// V1 memória de cálculo (docs/design-motor-de-prazos-v1.md): the full "por que essa data?"
	// trail. CalcMemory/CrossValidation are nil for a pré-V1 prazo (no calc_memory/cross_
	// validation row was ever written) — the LEFT JOINs in GetPrazo degrade gracefully, never an
	// error. PrazoInterno is the persisted internal safety buffer (deadline.prazo_interno,
	// internalBufferBusinessDays business days before EndDate), COALESCEd to EndDate at the SQL
	// layer for a pré-migration row that never had it recomputed.
	Origem             string               `json:"origem,omitempty"`
	Selo               string               `json:"selo,omitempty"`
	ConfirmacaoExigida bool                 `json:"confirmacao_exigida"`
	PrazoInterno       time.Time            `json:"prazo_interno"`
	CalcMemory         *CalcMemoryView      `json:"calc_memory,omitempty"`
	AppliedHoliday     []AppliedHolidayView `json:"applied_holiday,omitempty"`
	CrossValidation    *CrossValidationView `json:"cross_validation,omitempty"`
}

// CalcMemoryView is the deterministic calculation audit trail (calc_memory, V1) — the "por que
// essa data?" answer. Nil on PrazoDetailView when the prazo predates V1 (no row ever written).
type CalcMemoryView struct {
	// ID is internal (feeds ListAppliedHolidaysByCalcMemory) — never serialized.
	ID                      string  `json:"-"`
	PrazoBase               string  `json:"prazo_base"`
	PrazoBaseFonte          string  `json:"prazo_base_fonte"`
	TermoInicialRegra       string  `json:"termo_inicial_regra"`
	DiasUteis               bool    `json:"dias_uteis"`
	DobraMotivo             string  `json:"dobra_motivo,omitempty"`
	TabelaLegalRef          string  `json:"tabela_legal_ref,omitempty"`
	IATipoInferido          string  `json:"ia_tipo_inferido,omitempty"`
	IAConfianca             float64 `json:"ia_confianca,omitempty"`
	CalendarProviderVersion string  `json:"calendar_provider_version,omitempty"`
}

// AppliedHolidayView is one feriado snapshot applied to the calculation (applied_holiday, V1),
// 1:N under CalcMemory.
type AppliedHolidayView struct {
	Data    time.Time `json:"data"`
	Nome    string    `json:"nome,omitempty"`
	Ambito  string    `json:"ambito,omitempty"`
	Comarca string    `json:"comarca,omitempty"`
}

// CrossValidationView is the declared×calculado cross-validation (cross_validation, V1). Nil on
// PrazoDetailView when the prazo had no prazo_declarado to cross-check.
type CrossValidationView struct {
	DataDeclarada time.Time `json:"data_declarada"`
	DataCalculada time.Time `json:"data_calculada"`
	DifDias       int       `json:"dif_dias"`
	Resultado     string    `json:"resultado"`
	CausaProvavel string    `json:"causa_provavel,omitempty"`
	Decisao       string    `json:"decisao,omitempty"`
	DecididoPor   string    `json:"decidido_por,omitempty"`
}

// PrazoSuggestContext is the advisory case context the AI suggester (suggest.go) composes the
// meta-prompt from — NOT a wire DTO (it never serializes to the FE). It carries the prazo's own
// signals (Kind/Days/Counting) plus the richer context erd-ai-advisory.md §3 specializes on: the
// process's Court/Degree/Class/Subject (court_record) and the origin intimação's Type + Text (the
// truncated teor). IntimationID is kept for provenance capture. Class/Subject/IntimationType are
// "" when NULL in the DB and IntimationText is "" only for an empty teor — the composer omits
// whatever comes empty, so a sparse case degrades gracefully instead of producing dangling labels.
//
// AISummary/AIRecommendation/AISummaryGeneratedAt (migration 0036) are the PERSISTED "O que
// aconteceu"/"O que fazer" summary: NULL (AISummaryGeneratedAt == nil) until the suggester's
// first successful LLM call for this prazo, frozen thereafter (sync-on-first-GET, write-once —
// no invalidation path). AISummaryGeneratedAt is the single source of truth for "already
// persisted" the use case branches on; AISummary/AIRecommendation are "" until then.
type PrazoSuggestContext struct {
	Kind           string
	Days           int
	Counting       string
	IntimationID   string
	Court          string
	Degree         string
	Class          string
	Subject        string
	IntimationType string
	IntimationText string

	AISummary            string
	AIRecommendation     string
	AISummaryGeneratedAt *time.Time
}

// TaskView is one row of a task list — BOTH the process's Tasks tab (GET
// /v1/processos/:id/tasks) and the global agenda (GET /v1/tasks, "meus prazos"). It is the
// single wire shape for a task (the write handlers render it too, from the saved entity), so a
// task looks identical however it is read or written. DueDate/CompletedAt are optional
// (null-able) times; the context FKs are "" when the task is avulsa/unassigned. sortDue is the
// keyset sort value (COALESCE(due_date, sentinel)) — unexported so it never serializes; the
// handler reads it to build the next cursor (an undated task's cursor keys off the sentinel).
type TaskView struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Kind        string     `json:"kind,omitempty"`
	Priority    string     `json:"priority,omitempty"` // HIGH|MEDIUM|LOW; "" = sem prioridade (omitted)
	DueDate     *time.Time `json:"due_date"`
	Status      string     `json:"status"`
	// DisplayStatus is the DERIVED presentation status (Aberta|Em execução|Concluída|Atrasada;
	// "" for a DISMISSED task) the cockpit/agenda bucket the row by — additive to the existing
	// wire shape, so cockpit and agenda keep consuming Status while the new tabs read this.
	DisplayStatus  string     `json:"display_status,omitempty"`
	Source         string     `json:"source"`
	AssigneeUserID string     `json:"assignee_user_id,omitempty"`
	DeadlineID     string     `json:"deadline_id,omitempty"`
	IntimationID   string     `json:"intimation_id,omitempty"`
	CourtRecordID  string     `json:"court_record_id,omitempty"`
	CompletedAt    *time.Time `json:"completed_at"`
	// CNJNumber/Court are the process context (ListTasks agenda only, joined off
	// court_record_id) — "" for an avulsa task (no court_record_id) and omitted from the
	// wire shape. ListTasksByProcesso does NOT carry these (the :id in the route already
	// is the process context, mirroring PrazoView).
	CNJNumber string `json:"cnj_number,omitempty"`
	Court     string `json:"court,omitempty"`
	sortDue   time.Time
	// doneItems is the count of ticked checklist items for the row, the raw ingredient the
	// read use case turns into DisplayStatus (any done ⇒ Em execução). Unexported so it never
	// serializes — the FE reads DisplayStatus, not this count.
	doneItems int
}

// newTaskViewFromEntity renders a persisted *Task (the CREATE/PATCH write result) as the shared
// TaskView wire shape — so the write responses and the read models are byte-for-byte the same
// task JSON. sortDue is irrelevant on a write (no keyset), so it is left zero.
func newTaskViewFromEntity(t *Task) TaskView {
	return TaskView{
		ID:             t.ID,
		Title:          t.Title,
		Description:    t.Description,
		Kind:           t.Kind,
		Priority:       t.Priority,
		DueDate:        t.DueDate,
		Status:         string(t.Status),
		Source:         string(t.Source),
		AssigneeUserID: t.AssigneeUserID,
		DeadlineID:     t.DeadlineID,
		IntimationID:   t.IntimationID,
		CourtRecordID:  t.CourtRecordID,
		CompletedAt:    t.CompletedAt,
	}
}

// TasksByProcessoQuery carries the ascending keyset cursor (the last row's sort_due and id)
// plus the process (court_record) whose tasks to read and the tenant. LastDue is the keyset
// sort value — the coalesced due_date (a real date, or the '9999-12-31' sentinel for an undated
// task). The handler fills the min sentinel for a first page.
type TasksByProcessoQuery struct {
	TenantID      string
	CourtRecordID string
	LastDue       string
	LastID        string
	Limit         int
}

// TasksQuery carries the agenda's ascending keyset cursor plus its optional filters: Status ("" =
// all), Assignee ("" = all assignees; = principal.UserID for "meus"), Source ("" = all),
// IntimationID ("" = all; = a specific uuid to list only tasks of that intimação), and a
// due_date window [From, To] (each "" = open bound). The dates/assignee/intimation_id are the
// wire layout; the handler validates them, the repo parses. LastDue is the coalesced sort value
// (see TasksByProcessoQuery).
type TasksQuery struct {
	TenantID     string
	Status       string
	Assignee     string
	Source       string
	IntimationID string
	From         string
	To           string
	LastDue      string
	LastID       string
	Limit        int
}

// Filtered reports whether any agenda filter (Status/Assignee/Source/IntimationID/window) is
// active — the repo uses it to decide when the "X de Y" counter needs the filtered COUNT.
func (q TasksQuery) Filtered() bool {
	return q.Status != "" || q.Assignee != "" || q.Source != "" || q.IntimationID != "" || q.From != "" || q.To != ""
}

// AssigneeOption is one selectable ?assignee: the responsável's name (the chip label) and user
// id (the query-param value). Read-model DTO, off the write path.
type AssigneeOption struct {
	Name string
	ID   string
}

// TasksByProcessoResult is a page of a process's tasks plus its total for the "X de Y" counter.
// There is no filter on the tab, so a single Total carries both sides (mirrors PrazosByProcessoResult).
// Filters is always empty on the tab (no chips).
type TasksByProcessoResult struct {
	Items   []TaskView
	HasMore bool
	Total   int64
	Filters httpx.Filters
}

// TasksResult is a page of the task agenda plus the two totals for "X de Y": TotalCount is the
// current context (filtered by Status/Assignee/Source/window), Total the tenant-wide count.
// Filters is the selectable-options block the envelope renders as chips.
type TasksResult struct {
	Items      []TaskView
	HasMore    bool
	TotalCount int64
	Total      int64
	Filters    httpx.Filters
}

// PrazosByProcessoQuery carries the ascending keyset cursor (the last row's end_date
// and id) plus the process (court_record) whose prazos to read and the tenant. The
// handler fills the min sentinel for a first page.
type PrazosByProcessoQuery struct {
	TenantID      string
	CourtRecordID string
	LastEnd       string
	LastID        string
	Limit         int
}

// PrazosQuery carries the agenda's ascending keyset cursor plus its optional filters:
// Status ("" = all), Kind ("" = all), Court ("" = all) and an end_date window [From, To]
// (each "" = open bound). The dates are the wire layout (2006-01-02); the handler
// validates them, the repo parses. Kind/Court are free text from the envelope's options.
type PrazosQuery struct {
	TenantID string
	Status   string
	Kind     string
	Court    string
	From     string
	To       string
	LastEnd  string
	LastID   string
	Limit    int
}

// Filtered reports whether any agenda filter (Status/Kind/Court/window) is active — the
// repo uses it to decide when the "X de Y" counter needs the filtered COUNT.
func (q PrazosQuery) Filtered() bool {
	return q.Status != "" || q.Kind != "" || q.Court != "" || q.From != "" || q.To != ""
}

// PrazosByProcessoResult is a page of a process's prazos plus its total for the "X de
// Y" counter. There is no filter on the tab, so a single Total carries both sides.
// Filters is always empty on the tab (no chips).
type PrazosByProcessoResult struct {
	Items   []PrazoView
	HasMore bool
	Total   int64
	Filters httpx.Filters
}

// PrazosResult is a page of the agenda plus the two totals for "X de Y": TotalCount is
// the current context (filtered by Status/Kind/Court/window), Total the tenant-wide count.
// Filters is the selectable-options block the envelope renders as chips.
type PrazosResult struct {
	Items      []AgendaPrazoView
	HasMore    bool
	TotalCount int64
	Total      int64
	Filters    httpx.Filters
}

// TaskItemView is one checklist item on the task detail screen (GET /v1/tasks/:id): the
// tickable subtarefa in its ordered slot. DoneAt is omitted while the item is undone.
type TaskItemView struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Position  int        `json:"position"`
	Done      bool       `json:"done"`
	DoneAt    *time.Time `json:"done_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// TaskDetailView is the task detail read model (GET /v1/tasks/:id): the task's own fields, its
// ordered checklist, the {done, total} progress, and the DERIVED display_status. Items is always
// a non-nil slice so an itemless task serializes as [] not null.
type TaskDetailView struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	Description    string         `json:"description,omitempty"`
	Kind           string         `json:"kind,omitempty"`
	Priority       string         `json:"priority,omitempty"` // HIGH|MEDIUM|LOW; "" = sem prioridade (omitted)
	DueDate        *time.Time     `json:"due_date"`
	Status         string         `json:"status"`
	DisplayStatus  string         `json:"display_status,omitempty"`
	Source         string         `json:"source"`
	AssigneeUserID string         `json:"assignee_user_id,omitempty"`
	DeadlineID     string         `json:"deadline_id,omitempty"`
	IntimationID   string         `json:"intimation_id,omitempty"`
	CourtRecordID  string         `json:"court_record_id,omitempty"`
	CompletedAt    *time.Time     `json:"completed_at"`
	Items          []TaskItemView `json:"items"`
	Progress       TaskProgress   `json:"progress"`
}

// TaskCommentView is one message in a task's discussion thread (GET /v1/tasks/:id/comments). The
// author is resolved to a name for display (AuthorName "" when the id is unknown — the FE labels
// it). AuthorUserID is carried so the FE can render "you"/avatar. CreatedAt orders the thread
// (oldest-first).
type TaskCommentView struct {
	ID           string    `json:"id"`
	AuthorUserID string    `json:"author_user_id"`
	AuthorName   string    `json:"author_name,omitempty"`
	Body         string    `json:"body"`
	CreatedAt    time.Time `json:"created_at"`
}

// TaskActivityView is one row of a task's audit log (GET /v1/tasks/:id/activity). EventType is the
// closed set (TASK_CREATED|TITLE_CHANGED|…); the actor is resolved to a name for display. FromValue/
// ToValue are the "de X para Y" a field change renders (both "" for a create/lifecycle/comment).
// CreatedAt orders the log (newest-first).
type TaskActivityView struct {
	ID          string    `json:"id"`
	ActorUserID string    `json:"actor_user_id"`
	ActorName   string    `json:"actor_name,omitempty"`
	EventType   string    `json:"event_type"`
	FromValue   string    `json:"from_value,omitempty"`
	ToValue     string    `json:"to_value,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// PrazosSummary is the prazos KPI read model (GET /v1/prazos/summary), aggregated per tenant
// from deadline.status + days_left. Thresholds (decisão travada, coherent with PrazoView's
// urgency semantics — the gold-<3-dias styling): OPEN/PENDING is "aberto"; among those,
// days_left ≤ 1 is "crítico" and days_left ∈ 0..3 is "vencendo" (both subsets of aberto, not
// disjoint); a future start (start_date > hoje) is "futuro"; days_left < 0 (or MISSED) is
// "vencido"; MET is "cumprido". CANCELLED counts only in total. The buckets deliberately
// OVERLAP (a crítico is also aberto and vencendo) — the FE picks the most urgent label per card.
type PrazosSummary struct {
	Total     int `json:"total"`
	Criticos  int `json:"criticos"`
	Vencendo  int `json:"vencendo"`
	Abertos   int `json:"abertos"`
	Futuros   int `json:"futuros"`
	Vencidos  int `json:"vencidos"`
	Cumpridos int `json:"cumpridos"`
}

// TasksSummary is the tasks KPI read model (GET /v1/tasks/summary), aggregated per tenant using
// the SAME derivation as display_status: abertas = OPEN, not overdue, no item done; em_execucao =
// OPEN, not overdue, some item done; concluidas = DONE; atrasadas = OPEN & overdue. DISMISSED is
// excluded (out of the cockpit), so these four are DISJOINT and sum to the non-dismissed tasks.
type TasksSummary struct {
	Abertas    int `json:"abertas"`
	EmExecucao int `json:"em_execucao"`
	Concluidas int `json:"concluidas"`
	Atrasadas  int `json:"atrasadas"`
}

// readRepo is the narrow read port the ReadUseCase drives — the keyset list reads, the
// counters and the single-prazo detail. It is deliberately separate from the write
// Repository (2c): reads run on the pool, off the transactional write path.
type readRepo interface {
	ListPrazosByProcesso(ctx context.Context, q PrazosByProcessoQuery) ([]PrazoView, error)
	CountPrazosByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	ListPrazos(ctx context.Context, q PrazosQuery) ([]AgendaPrazoView, error)
	ListPrazosByIntimacao(ctx context.Context, tenantID, intimationID string) ([]AgendaPrazoView, error)
	CountPrazos(ctx context.Context, q PrazosQuery) (totalCount, total int64, err error)
	GetPrazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error)
	// ListAppliedHolidaysByCalcMemory reads the 1:N feriados snapshot for one calc_memory,
	// scoped to tenantID (barrier 1). Called by ReadUseCase.Prazo only when GetPrazo returned a
	// CalcMemory (a pré-V1 prazo has none, so this never runs for it). No holidays applied
	// yields an empty slice, never an error.
	ListAppliedHolidaysByCalcMemory(ctx context.Context, tenantID, calcMemoryID string) ([]AppliedHolidayView, error)
	// GetPrazoSuggestContext reads the advisory case context for one prazo (the AI suggester's
	// dedicated read: prazo signals + court_record + intimação teor), scoped to tenantID (barrier
	// 1). A miss is the typed ErrDeadlineNotFound (→ 404).
	GetPrazoSuggestContext(ctx context.Context, tenantID, id string) (PrazoSuggestContext, error)
	ListTasksByProcesso(ctx context.Context, q TasksByProcessoQuery) ([]TaskView, error)
	CountTasksByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	ListTasks(ctx context.Context, q TasksQuery) ([]TaskView, error)
	CountTasks(ctx context.Context, q TasksQuery) (totalCount, total int64, err error)
	// GetTaskDetail reads a task's own fields for the detail view, scoped to tenantID (barrier
	// 1). A miss is ErrTaskNotFound (→ 404). The checklist + progress are separate reads.
	GetTaskDetail(ctx context.Context, tenantID, id string) (TaskDetailView, error)
	// ListTaskItems reads a task's ordered checklist, scoped to (taskID, tenantID). An itemless
	// task yields an empty slice, never an error.
	ListTaskItems(ctx context.Context, tenantID, taskID string) ([]TaskItemView, error)
	// TaskItemProgress reads a task's {done, total} checklist tally, scoped to (taskID, tenantID).
	TaskItemProgress(ctx context.Context, tenantID, taskID string) (TaskProgress, error)
	// ListTaskComments reads a task's discussion thread (oldest-first), scoped to (taskID,
	// tenantID). An empty thread yields an empty slice, never an error.
	ListTaskComments(ctx context.Context, tenantID, taskID string) ([]TaskCommentView, error)
	// ListTaskActivity reads a task's audit log (newest-first), scoped to (taskID, tenantID). An
	// empty log yields an empty slice, never an error.
	ListTaskActivity(ctx context.Context, tenantID, taskID string) ([]TaskActivityView, error)
	// PrazosSummary reads the tenant's prazos KPI counts (single object). No pagination.
	PrazosSummary(ctx context.Context, tenantID string) (PrazosSummary, error)
	// TasksSummary reads the tenant's tasks KPI counts (single object). No pagination.
	TasksSummary(ctx context.Context, tenantID string) (TasksSummary, error)
	// Filter options for the agenda envelopes — the distinct-value reads that back the
	// chips. Each is tenant-scoped and mirrors the list's own predicates.
	ListPrazoKinds(ctx context.Context, tenantID string) ([]string, error)
	ListPrazoCourts(ctx context.Context, tenantID string) ([]string, error)
	ListTaskAssignees(ctx context.Context, tenantID string) ([]AssigneeOption, error)
}

// ReadUseCase serves the prazos screen reads. It is a pagination policy over readRepo:
// it over-fetches one row per page so the handler learns whether more remain without a
// separate COUNT. `now` is the reference day the derived display_status compares due_dates
// against; it defaults to time.Now and is overridable in tests via WithReadClock.
type ReadUseCase struct {
	repo readRepo
	now  func() time.Time
}

// ReadOption configures a ReadUseCase at construction (functional options, like the write
// UseCase) so the clock seam can be injected in tests without breaking the positional call.
type ReadOption func(*ReadUseCase)

// WithReadClock overrides the reference clock the derived display_status uses to decide whether
// a task's due_date is past. Production leaves the default (time.Now); tests pin it so a given
// due_date deterministically buckets as Atrasada or not.
func WithReadClock(now func() time.Time) ReadOption {
	return func(uc *ReadUseCase) { uc.now = now }
}

// NewReadUseCase wires the read use case over a read port (the pool-backed read repo).
func NewReadUseCase(repo readRepo, opts ...ReadOption) *ReadUseCase {
	uc := &ReadUseCase{repo: repo, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// PrazosByProcesso returns up to q.Limit of a process's prazos (soonest first), whether
// a further page exists, and the tab's total. The keyset read over-fetches one row for
// hasMore; the total is a separate COUNT — a small skew under concurrent inserts is fine
// (read model, not aggregate).
func (uc *ReadUseCase) PrazosByProcesso(ctx context.Context, q PrazosByProcessoQuery) (PrazosByProcessoResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListPrazosByProcesso(ctx, q)
	if err != nil {
		return PrazosByProcessoResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	total, err := uc.repo.CountPrazosByProcesso(ctx, q.TenantID, q.CourtRecordID)
	if err != nil {
		return PrazosByProcessoResult{}, err
	}
	return PrazosByProcessoResult{Items: rows, HasMore: hasMore, Total: total, Filters: httpx.Filters{}}, nil
}

// Prazos returns up to q.Limit agenda prazos (soonest first), whether a further page
// exists, and the "X de Y" totals (filtered by Status/Kind/Court/window, plus the
// tenant-wide total). Same over-fetch policy as PrazosByProcesso. The envelope's filter
// options are assembled alongside (the distinct-value reads) so the FE renders the chips
// without a second request.
func (uc *ReadUseCase) Prazos(ctx context.Context, q PrazosQuery) (PrazosResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListPrazos(ctx, q)
	if err != nil {
		return PrazosResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	totalCount, total, err := uc.repo.CountPrazos(ctx, q)
	if err != nil {
		return PrazosResult{}, err
	}
	filters, err := uc.prazosFilters(ctx, q.TenantID)
	if err != nil {
		return PrazosResult{}, err
	}
	return PrazosResult{Items: rows, HasMore: hasMore, TotalCount: totalCount, Total: total, Filters: filters}, nil
}

// prazosFilters assembles the agenda's selectable options: the free-text kind/court
// options from the distinct-value reads. Each key is omitted when it has no options.
func (uc *ReadUseCase) prazosFilters(ctx context.Context, tenantID string) (httpx.Filters, error) {
	kinds, err := uc.repo.ListPrazoKinds(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	courts, err := uc.repo.ListPrazoCourts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	f := httpx.Filters{}
	f.Set("kind", httpx.OptionsFromStrings(kinds)...)
	f.Set("court", httpx.OptionsFromStrings(courts)...)
	return f, nil
}

// PrazosByIntimacao returns the prazo derived from one intimação (the F2 lookup, GET
// /v1/prazos?intimation_id=...), wrapped in the SAME agenda result shape so the handler
// reuses the /prazos envelope. The deadline is 1:1 with the intimação (notification_id
// UNIQUE), so this is 0 or 1 item — no over-fetch/keyset: HasMore is false and the "X de
// Y" totals coincide with the item count.
func (uc *ReadUseCase) PrazosByIntimacao(ctx context.Context, tenantID, intimationID string) (PrazosResult, error) {
	rows, err := uc.repo.ListPrazosByIntimacao(ctx, tenantID, intimationID)
	if err != nil {
		return PrazosResult{}, err
	}
	n := int64(len(rows))
	return PrazosResult{Items: rows, HasMore: false, TotalCount: n, Total: n, Filters: httpx.Filters{}}, nil
}

// Prazo returns one prazo's audit detail, or the repo's typed ErrDeadlineNotFound (→
// 404) when the id resolves to no row in the tenant. PrazoInterno now comes straight off the
// SQL row (GetPrazo COALESCEs the persisted column to EndDate), so no in-memory stamp is
// needed. When the prazo has a calc_memory (V1-derived), it also loads the applied_holiday
// trail — the 1:N companion read GetPrazo deliberately does NOT join (it would multiply the
// deadline row). A pré-V1 prazo (nil CalcMemory) skips this read entirely, never an error.
func (uc *ReadUseCase) Prazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error) {
	p, err := uc.repo.GetPrazo(ctx, tenantID, id)
	if err != nil {
		return PrazoDetailView{}, err
	}

	if p.CalcMemory != nil {
		holidays, err := uc.repo.ListAppliedHolidaysByCalcMemory(ctx, tenantID, p.CalcMemory.ID)
		if err != nil {
			return PrazoDetailView{}, err
		}
		p.AppliedHoliday = holidays
	}
	return p, nil
}

// SuggestContext returns the advisory case context the AI suggester composes the meta-prompt from
// (prazo signals + the process's court_record + the origin intimação's teor), or the repo's typed
// ErrDeadlineNotFound (→ 404) when the id resolves to no row in the tenant. A thin pass-through
// (no pagination): it is a single tenant-scoped read.
func (uc *ReadUseCase) SuggestContext(ctx context.Context, tenantID, id string) (PrazoSuggestContext, error) {
	return uc.repo.GetPrazoSuggestContext(ctx, tenantID, id)
}

// TasksByProcesso returns up to q.Limit of a process's tasks (soonest due first, undated last),
// whether a further page exists, and the tab's total. Same over-fetch policy as PrazosByProcesso:
// it fetches one extra row for hasMore and a separate COUNT for the total.
func (uc *ReadUseCase) TasksByProcesso(ctx context.Context, q TasksByProcessoQuery) (TasksByProcessoResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListTasksByProcesso(ctx, q)
	if err != nil {
		return TasksByProcessoResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	total, err := uc.repo.CountTasksByProcesso(ctx, q.TenantID, q.CourtRecordID)
	if err != nil {
		return TasksByProcessoResult{}, err
	}
	uc.decorateDisplayStatus(rows)
	return TasksByProcessoResult{Items: rows, HasMore: hasMore, Total: total, Filters: httpx.Filters{}}, nil
}

// Tasks returns up to q.Limit agenda tasks (soonest due first, undated last), whether a further
// page exists, and the "X de Y" totals (filtered by Status/Assignee/Source/window, plus the
// tenant-wide total). Same over-fetch policy as Prazos. The envelope's filter options are
// assembled alongside (the distinct-value reads) so the FE renders the chips without a second
// request.
func (uc *ReadUseCase) Tasks(ctx context.Context, q TasksQuery) (TasksResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListTasks(ctx, q)
	if err != nil {
		return TasksResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	totalCount, total, err := uc.repo.CountTasks(ctx, q)
	if err != nil {
		return TasksResult{}, err
	}
	uc.decorateDisplayStatus(rows)
	filters, err := uc.tasksFilters(ctx, q.TenantID)
	if err != nil {
		return TasksResult{}, err
	}
	return TasksResult{Items: rows, HasMore: hasMore, TotalCount: totalCount, Total: total, Filters: filters}, nil
}

// tasksFilters assembles the task agenda's selectable options: the closed source set from the
// entity constants and the free-text assignee options (label==name / value==id) from the
// distinct-value read. Each key is omitted when it has no options.
func (uc *ReadUseCase) tasksFilters(ctx context.Context, tenantID string) (httpx.Filters, error) {
	assignees, err := uc.repo.ListTaskAssignees(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	f := httpx.Filters{}
	f.SetEnum("source", string(SourceAI), string(SourceRule), string(SourceManual))
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

// decorateDisplayStatus fills each row's DERIVED display_status from its (status, done-item
// count, due_date) against the read use case's clock — the SINGLE derivation the detail view and
// the summaries also use, so a task's presentation status is identical however it is read. It
// mutates in place (the rows are the use case's own slice, not shared).
func (uc *ReadUseCase) decorateDisplayStatus(rows []TaskView) {
	now := uc.now()
	for i := range rows {
		progress := TaskProgress{Done: rows[i].doneItems}
		rows[i].DisplayStatus = string(deriveDisplayStatus(TaskStatus(rows[i].Status), progress, rows[i].DueDate, now))
	}
}

// TaskDetail returns the task detail read model (GET /v1/tasks/:id): the task's own fields, its
// ordered checklist, the {done, total} progress and the DERIVED display_status. A miss is the
// repo's typed ErrTaskNotFound (→ 404). The checklist and progress are separate tenant-scoped
// reads, so an itemless task still resolves (empty checklist, {0,0} progress).
func (uc *ReadUseCase) TaskDetail(ctx context.Context, tenantID, id string) (TaskDetailView, error) {
	view, err := uc.repo.GetTaskDetail(ctx, tenantID, id)
	if err != nil {
		return TaskDetailView{}, err
	}
	items, err := uc.repo.ListTaskItems(ctx, tenantID, id)
	if err != nil {
		return TaskDetailView{}, err
	}
	progress, err := uc.repo.TaskItemProgress(ctx, tenantID, id)
	if err != nil {
		return TaskDetailView{}, err
	}
	if items == nil {
		items = []TaskItemView{}
	}
	view.Items = items
	view.Progress = progress
	view.DisplayStatus = string(deriveDisplayStatus(TaskStatus(view.Status), progress, view.DueDate, uc.now()))
	return view, nil
}

// TaskComments returns a task's discussion thread (GET /v1/tasks/:id/comments), oldest-first. It
// guards the task exists in the tenant first (GetTaskDetail → ErrTaskNotFound → 404) so a foreign/
// unknown id is a miss, not a silently-empty thread. A real task with no comments yields [].
func (uc *ReadUseCase) TaskComments(ctx context.Context, tenantID, id string) ([]TaskCommentView, error) {
	if _, err := uc.repo.GetTaskDetail(ctx, tenantID, id); err != nil {
		return nil, err
	}
	comments, err := uc.repo.ListTaskComments(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if comments == nil {
		comments = []TaskCommentView{}
	}
	return comments, nil
}

// TaskActivity returns a task's audit log (GET /v1/tasks/:id/activity), newest-first. Like
// TaskComments it guards the task exists (→ 404 on a miss); a real task with no logged mutation
// yields [] (a historical task not touched since 0055 simply has no rows yet).
func (uc *ReadUseCase) TaskActivity(ctx context.Context, tenantID, id string) ([]TaskActivityView, error) {
	if _, err := uc.repo.GetTaskDetail(ctx, tenantID, id); err != nil {
		return nil, err
	}
	activity, err := uc.repo.ListTaskActivity(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if activity == nil {
		activity = []TaskActivityView{}
	}
	return activity, nil
}

// PrazosSummary returns the tenant's prazos KPI counts (GET /v1/prazos/summary) — a single
// aggregated object, no pagination. It is a thin pass-through: the buckets are derived in SQL
// (the counts are cheap and the thresholds documented at PrazosSummary).
func (uc *ReadUseCase) PrazosSummary(ctx context.Context, tenantID string) (PrazosSummary, error) {
	return uc.repo.PrazosSummary(ctx, tenantID)
}

// TasksSummary returns the tenant's tasks KPI counts (GET /v1/tasks/summary) — a single
// aggregated object, no pagination. The buckets use the same display_status derivation as the
// detail/list views (computed in SQL against CURRENT_DATE, so the tenant-wide aggregate stays
// one query rather than scanning every task in Go).
func (uc *ReadUseCase) TasksSummary(ctx context.Context, tenantID string) (TasksSummary, error) {
	return uc.repo.TasksSummary(ctx, tenantID)
}
