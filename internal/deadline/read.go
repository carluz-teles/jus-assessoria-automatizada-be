package deadline

import (
	"context"
	"time"
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
}

// TaskView is one row of a task list — BOTH the process's Tasks tab (GET
// /v1/processos/:id/tasks) and the global agenda (GET /v1/tasks, "meus prazos"). It is the
// single wire shape for a task (the write handlers render it too, from the saved entity), so a
// task looks identical however it is read or written. DueDate/CompletedAt are optional
// (null-able) times; the context FKs are "" when the task is avulsa/unassigned. sortDue is the
// keyset sort value (COALESCE(due_date, sentinel)) — unexported so it never serializes; the
// handler reads it to build the next cursor (an undated task's cursor keys off the sentinel).
type TaskView struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Description    string     `json:"description,omitempty"`
	Kind           string     `json:"kind,omitempty"`
	DueDate        *time.Time `json:"due_date"`
	Status         string     `json:"status"`
	Source         string     `json:"source"`
	AssigneeUserID string     `json:"assignee_user_id,omitempty"`
	DeadlineID     string     `json:"deadline_id,omitempty"`
	IntimationID   string     `json:"intimation_id,omitempty"`
	CourtRecordID  string     `json:"court_record_id,omitempty"`
	CompletedAt    *time.Time `json:"completed_at"`
	sortDue        time.Time
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
// all), Assignee ("" = all assignees; = principal.UserID for "meus"), and a due_date window
// [From, To] (each "" = open bound). The dates/assignee are the wire layout; the handler
// validates them, the repo parses. LastDue is the coalesced sort value (see TasksByProcessoQuery).
type TasksQuery struct {
	TenantID string
	Status   string
	Assignee string
	From     string
	To       string
	LastDue  string
	LastID   string
	Limit    int
}

// TasksByProcessoResult is a page of a process's tasks plus its total for the "X de Y" counter.
// There is no filter on the tab, so a single Total carries both sides (mirrors PrazosByProcessoResult).
type TasksByProcessoResult struct {
	Items   []TaskView
	HasMore bool
	Total   int64
}

// TasksResult is a page of the task agenda plus the two totals for "X de Y": TotalCount is the
// current context (filtered by Status/Assignee/window), Total the tenant-wide count.
type TasksResult struct {
	Items      []TaskView
	HasMore    bool
	TotalCount int64
	Total      int64
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
// Status ("" = all) and an end_date window [From, To] (each "" = open bound). The
// dates are the wire layout (2006-01-02); the handler validates them, the repo parses.
type PrazosQuery struct {
	TenantID string
	Status   string
	From     string
	To       string
	LastEnd  string
	LastID   string
	Limit    int
}

// PrazosByProcessoResult is a page of a process's prazos plus its total for the "X de
// Y" counter. There is no filter on the tab, so a single Total carries both sides.
type PrazosByProcessoResult struct {
	Items   []PrazoView
	HasMore bool
	Total   int64
}

// PrazosResult is a page of the agenda plus the two totals for "X de Y": TotalCount is
// the current context (filtered by Status/window), Total the tenant-wide count.
type PrazosResult struct {
	Items      []AgendaPrazoView
	HasMore    bool
	TotalCount int64
	Total      int64
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
	ListTasksByProcesso(ctx context.Context, q TasksByProcessoQuery) ([]TaskView, error)
	CountTasksByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	ListTasks(ctx context.Context, q TasksQuery) ([]TaskView, error)
	CountTasks(ctx context.Context, q TasksQuery) (totalCount, total int64, err error)
}

// ReadUseCase serves the prazos screen reads. It is a pagination policy over readRepo:
// it over-fetches one row per page so the handler learns whether more remain without a
// separate COUNT.
type ReadUseCase struct {
	repo readRepo
}

// NewReadUseCase wires the read use case over a read port (the pool-backed read repo).
func NewReadUseCase(repo readRepo) *ReadUseCase {
	return &ReadUseCase{repo: repo}
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
	return PrazosByProcessoResult{Items: rows, HasMore: hasMore, Total: total}, nil
}

// Prazos returns up to q.Limit agenda prazos (soonest first), whether a further page
// exists, and the "X de Y" totals (filtered by Status/window, plus the tenant-wide
// total). Same over-fetch policy as PrazosByProcesso.
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
	return PrazosResult{Items: rows, HasMore: hasMore, TotalCount: totalCount, Total: total}, nil
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
	return PrazosResult{Items: rows, HasMore: false, TotalCount: n, Total: n}, nil
}

// Prazo returns one prazo's audit detail, or the repo's typed ErrDeadlineNotFound (→
// 404) when the id resolves to no row in the tenant.
func (uc *ReadUseCase) Prazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error) {
	return uc.repo.GetPrazo(ctx, tenantID, id)
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
	return TasksByProcessoResult{Items: rows, HasMore: hasMore, Total: total}, nil
}

// Tasks returns up to q.Limit agenda tasks (soonest due first, undated last), whether a further
// page exists, and the "X de Y" totals (filtered by Status/Assignee/window, plus the tenant-wide
// total). Same over-fetch policy as Prazos.
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
	return TasksResult{Items: rows, HasMore: hasMore, TotalCount: totalCount, Total: total}, nil
}
