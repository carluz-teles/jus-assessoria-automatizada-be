package deadline

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handler.go is the deadline slice's HTTP surface — the prazos screen reads (the Prazos
// tab and the /prazos agenda) PLUS the F2 confirmation write (POST /prazos/confirm, the
// coração do produto, §9). The creation path stays event-driven (listener.go); PATCH/
// met/missed and task CRUD are later fatias. The slice owns its routing; cmd/api only
// composes by calling RegisterV1. tenant_id always comes from the verified principal,
// never the path/query/body.

// reader is the narrow port the Handler uses from the read use case — the keyset-
// paginated prazos reads plus the single-prazo detail.
type reader interface {
	PrazosByProcesso(ctx context.Context, q PrazosByProcessoQuery) (PrazosByProcessoResult, error)
	Prazos(ctx context.Context, q PrazosQuery) (PrazosResult, error)
	PrazosByIntimacao(ctx context.Context, tenantID, intimationID string) (PrazosResult, error)
	Prazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error)
	TasksByProcesso(ctx context.Context, q TasksByProcessoQuery) (TasksByProcessoResult, error)
	Tasks(ctx context.Context, q TasksQuery) (TasksResult, error)
	TaskDetail(ctx context.Context, tenantID, id string) (TaskDetailView, error)
	TaskComments(ctx context.Context, tenantID, id string) ([]TaskCommentView, error)
	TaskActivity(ctx context.Context, tenantID, id string) ([]TaskActivityView, error)
	PrazosSummary(ctx context.Context, tenantID string) (PrazosSummary, error)
	TasksSummary(ctx context.Context, tenantID string) (TasksSummary, error)
}

// writer is the narrow port the Handler uses from the write use case — the F2 confirmation
// PLUS the ajuste manual (PATCH) and the manual transitions (met/missed). It is deliberately
// separate from reader (and from the read use case): the writes run on the transactional
// write path, the reads on the pool.
type writer interface {
	Confirm(ctx context.Context, cmd ConfirmCommand) (ConfirmResult, error)
	Adjust(ctx context.Context, cmd AdjustCommand) (AdjustedDeadline, error)
	Preview(ctx context.Context, cmd PreviewCommand) (PreviewResult, error)
	MarkMet(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error)
	MarkMissed(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error)
	NoDeadline(ctx context.Context, tenantID, userID, deadlineID string) (MarkedDeadline, error)
	Reopen(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error)
	ApurarDivergencia(ctx context.Context, cmd ApurarDivergenciaCommand) (ApuradoDivergencia, error)
	ApurarTipo(ctx context.Context, cmd ApurarTipoCommand) (ApuradoTipo, error)
	CreateTask(ctx context.Context, cmd CreateTaskCommand) (*Task, error)
	UpdateTask(ctx context.Context, cmd UpdateTaskCommand) (*Task, error)
	MarkTaskDone(ctx context.Context, tenantID, userID, taskID string) (TaskTransition, error)
	DismissTask(ctx context.Context, tenantID, userID, taskID string) (TaskTransition, error)
	CreateTaskComment(ctx context.Context, cmd CreateTaskCommentCommand) (*TaskComment, error)
	CreateTaskItem(ctx context.Context, cmd CreateTaskItemCommand) (*TaskItem, error)
	UpdateTaskItem(ctx context.Context, cmd UpdateTaskItemCommand) (*TaskItem, error)
	DeleteTaskItem(ctx context.Context, tenantID, taskID, itemID string) error
}

// Handler is the deadline HTTP surface. It owns its routing; the api only composes by
// calling RegisterV1.
type Handler struct {
	reader    reader
	writer    writer
	suggester suggester
}

// suggester is the narrow port for the on-demand AI task suggestion (GET
// /prazos/:id/suggested-tasks). OPTIONAL — nil when the LLM is unconfigured — so the endpoint
// returns an empty list instead of failing, and the F2 form still works.
type suggester interface {
	SuggestTasks(ctx context.Context, tenantID, prazoID string) (Suggestion, error)
}

// NewHandler wires the handler to the prazos read use case, the F2 confirmation write use
// case, and the (optional) AI task suggester. All are injected as narrow ports so the binary
// composes them (the api mounts this handler once) and tests substitute fakes. suggester may
// be nil when OpenRouter is unconfigured.
func NewHandler(reader reader, writer writer, suggester suggester) *Handler {
	return &Handler{reader: reader, writer: writer, suggester: suggester}
}

// RegisterV1 mounts the prazos routes on the /v1 group: three reads open to any
// authenticated principal of the tenant (scoped to its own prazos), the F2 write
// POST /prazos/confirm, and the ajuste + manual transitions of an already-derived prazo
// (PATCH /prazos/:id, POST /prazos/:id/met, POST /prazos/:id/missed). The api calls this
// once — adding the slice's HTTP surface is one line of composition.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Get("/processos/:id/prazos", h.listPrazosByProcesso)
	r.Get("/prazos", h.listPrazos)
	// The static /prazos/summary is registered BEFORE the /prazos/:id param route so Fiber
	// never captures "summary" as an :id (static beats param at the same depth by declaration order).
	r.Get("/prazos/summary", h.getPrazosSummary)
	r.Get("/prazos/:id", h.getPrazo)
	// AI task suggestion (on-demand): pre-fills the F2 form. Deeper path than /prazos/:id.
	r.Get("/prazos/:id/suggested-tasks", h.getSuggestedTasks)
	r.Post("/prazos/confirm", h.confirmPrazo)
	// The static /prazos/preview is registered BEFORE the /prazos/:id param route (like summary)
	// so Fiber never captures "preview" as an :id.
	r.Post("/prazos/preview", h.previewPrazo)
	r.Patch("/prazos/:id", h.adjustPrazo)
	r.Post("/prazos/:id/met", h.markPrazoMet)
	r.Post("/prazos/:id/missed", h.markPrazoMissed)
	r.Post("/prazos/:id/no-deadline", h.markPrazoNoDeadline)
	r.Post("/prazos/:id/reopen", h.reopenPrazo)
	// V1 apuração (docs/design-motor-de-prazos-v1.md): human decision on an a_apurar prazo.
	r.Post("/prazos/:id/apurar-divergencia", h.apurarDivergencia)
	r.Post("/prazos/:id/apurar-tipo", h.apurarTipo)
	r.Get("/processos/:id/tasks", h.listTasksByProcesso)
	r.Get("/tasks", h.listTasks)
	// Same static-before-param ordering for /tasks/summary vs /tasks/:id.
	r.Get("/tasks/summary", h.getTasksSummary)
	r.Get("/tasks/:id", h.getTask)
	r.Post("/tasks", h.createTask)
	r.Patch("/tasks/:id", h.updateTask)
	r.Post("/tasks/:id/done", h.markTaskDone)
	r.Post("/tasks/:id/dismiss", h.dismissTask)
	// Discussion thread + audit log of a task (§4/§10, the Tarefa detail's Comentários/Atividade tabs).
	r.Get("/tasks/:id/comments", h.listTaskComments)
	r.Post("/tasks/:id/comments", h.createTaskComment)
	r.Get("/tasks/:id/activity", h.listTaskActivity)
	// Checklist / subtarefas of a task (§4/§10).
	r.Post("/tasks/:id/items", h.createTaskItem)
	r.Patch("/tasks/:id/items/:itemId", h.updateTaskItem)
	r.Delete("/tasks/:id/items/:itemId", h.deleteTaskItem)
}

// Keyset sentinels for a first page (no cursor): the ascending scan (soonest vencimento
// first) starts below every row — a date before any real prazo, and the zero uuid.
const (
	minDate  = "0001-01-01"
	zeroUUID = "00000000-0000-0000-0000-000000000000"
)

// Query-param allowlists (docs/erd-backend.md §4e.3): each list route accepts only its
// own set — a client typo or a param belonging to another route is rejected with 400
// instead of being silently ignored. Both prazos and tasks accept ?intimation_id as a
// documented filter; tasks also accepts ?assignee as its special "meus prazos" shortcut.
var (
	prazosListParams = map[string]struct{}{
		"status": {}, "kind": {}, "court": {}, "from": {}, "to": {},
		"limit": {}, "cursor": {}, "intimation_id": {},
	}
	tasksListParams = map[string]struct{}{
		"status": {}, "assignee": {}, "source": {}, "from": {}, "to": {},
		"limit": {}, "cursor": {}, "intimation_id": {},
	}
)

// isKnownTaskSource accepts only the closed task-source set the filter exposes — the
// handler is the app-level CHECK on the ?source param (task.source is a plain text
// column; the closed set lives in the entity constants).
func isKnownTaskSource(v string) bool {
	switch v {
	case string(SourceAI), string(SourceRule), string(SourceManual):
		return true
	default:
		return false
	}
}

// listPrazosByProcesso handles GET /v1/processos/:id/prazos: one process's Prazos tab —
// its prazos ordered by end_date (soonest first), keyset paginated (?limit, ?cursor).
// The :id is the court_record id (the same id /processos returns); tenant_id comes from
// the principal, and the read is tenant-scoped so a foreign :id yields an empty page.
func (h *Handler) listPrazosByProcesso(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastEnd, lastID := minDate, zeroUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastEnd, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.PrazosByProcesso(c.UserContext(), PrazosByProcessoQuery{
		TenantID: tenantID, CourtRecordID: c.Params("id"),
		LastEnd: lastEnd, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newPrazosByProcessoPage(res, limit))
}

// listPrazos handles GET /v1/prazos: the tenant's agenda — every prazo ordered by
// end_date (soonest first), keyset paginated (?limit, ?cursor), optionally filtered by
// ?status (a closed set), ?kind / ?court (free text from the envelope's options) and an
// end_date window ?from/?to (wire date 2006-01-02). A bad status or malformed date is a
// client error → 400; a param outside the route's allowlist is likewise 400. As a
// special case, ?intimation_id (a uuid) narrows to the single prazo of that intimação —
// the F2 lookup — returning the same envelope with 0 or 1 item (a malformed id is a 400).
func (h *Handler) listPrazos(c *fiber.Ctx) error {
	if err := httpx.RejectUnknownParams(c, prazosListParams); err != nil {
		return httpx.WriteError(c, err)
	}
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	if intimationID := c.Query("intimation_id"); intimationID != "" {
		if _, err := uuid.Parse(intimationID); err != nil {
			return httpx.WriteError(c, apperr.NewInvalid("invalid intimation_id (want a uuid)"))
		}
		res, err := h.reader.PrazosByIntimacao(c.UserContext(), tenantID, intimationID)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		return c.Status(fiber.StatusOK).JSON(newPrazosPage(res, limit))
	}

	status := c.Query("status")
	if status != "" && !isKnownStatus(status) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid status filter"))
	}
	from, err := validateOptionalDate(c.Query("from"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	to, err := validateOptionalDate(c.Query("to"))
	if err != nil {
		return httpx.WriteError(c, err)
	}

	lastEnd, lastID := minDate, zeroUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastEnd, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.Prazos(c.UserContext(), PrazosQuery{
		TenantID: tenantID, Status: status, Kind: c.Query("kind"), Court: c.Query("court"),
		From: from, To: to, LastEnd: lastEnd, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newPrazosPage(res, limit))
}

// getPrazo handles GET /v1/prazos/:id: one prazo's audit detail. A miss (or a foreign
// tenant's id) is the repo's typed ErrDeadlineNotFound → 404. The view is the whole
// payload, so it is returned without a list envelope.
func (h *Handler) getPrazo(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Prazo(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// suggestedTasksResponse is the endpoint body: the single Análise object the FE renders — the
// "O que aconteceu" summary, the "O que fazer" recommendation and the suggested tasks
// ({title,kind,description}). It is not paginated (no list envelope): one LLM call, one object.
type suggestedTasksResponse struct {
	Summary        string          `json:"summary"`
	Recommendation string          `json:"recommendation"`
	SuggestedTasks []SuggestedTask `json:"suggested_tasks"`
}

// getSuggestedTasks handles GET /v1/prazos/:id/suggested-tasks: the on-demand AI "Análise" that
// feeds the intimação detail. When the suggester is unconfigured (no LLM key) it returns empty
// strings + an empty list (200) — the section still renders and the lawyer types the tasks. A
// missing/foreign prazo → the use case's ErrDeadlineNotFound → 404; an LLM fault is surfaced typed.
// tenant_id comes from the verified principal.
func (h *Handler) getSuggestedTasks(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	if h.suggester == nil {
		return c.Status(fiber.StatusOK).JSON(suggestedTasksResponse{SuggestedTasks: []SuggestedTask{}})
	}
	sugg, err := h.suggester.SuggestTasks(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	tasks := sugg.Tasks
	if tasks == nil {
		tasks = []SuggestedTask{}
	}
	return c.Status(fiber.StatusOK).JSON(suggestedTasksResponse{
		Summary:        sugg.Summary,
		Recommendation: sugg.Recommendation,
		SuggestedTasks: tasks,
	})
}

// confirmPrazo handles POST /v1/prazos/confirm: the F2 "Aprovar tudo" (§9). It validates
// the body, then confirms the prazo (PENDING→OPEN, recomputed) in one tx, returning 200 with
// the confirmed prazo. It does NOT create tasks — those are managed via POST/PATCH /v1/tasks
// (the "Análise" section). tenant_id and confirmed_by come from the verified principal, never
// the body — so the write cannot be spoofed onto another tenant. A missing prazo for the
// intimação is the use case's typed ErrDeadlineNotFound → 404; a bad body is a 400 with the
// {kind,message,details} envelope.
func (h *Handler) confirmPrazo(c *fiber.Ctx) error {
	var req ConfirmRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		// The route mounts under Auth, so this is defensive: no principal → treat as
		// unauthenticated rather than confirming with an empty tenant.
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	res, err := h.writer.Confirm(c.UserContext(), req.toCommand(p.TenantID, p.UserID))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newConfirmResponse(res))
}

// adjustPrazo handles PATCH /v1/prazos/:id: the F2 ajuste manual (§9). It validates the
// partial body, then recomputes + updates the prazo in one tx, returning 200 with the
// recomputed prazo. tenant_id comes from the principal and the prazo id from the path (never
// the body). A miss is the use case's typed ErrDeadlineNotFound → 404; a terminal prazo is
// ErrDeadlineNotAdjustable → 409; a bad body is a 400 with the {kind,message,details} envelope.
func (h *Handler) adjustPrazo(c *fiber.Ctx) error {
	var req AdjustRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	res, err := h.writer.Adjust(c.UserContext(), req.toAdjustCommand(p.TenantID, p.UserID, c.Params("id")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newAdjustResponse(res))
}

// markPrazoMet handles POST /v1/prazos/:id/met: mark cumprido (§9), OPEN→MET. No body — the
// prazo id comes from the path and tenant_id from the principal. A miss is ErrDeadlineNotFound
// → 404; a non-OPEN prazo is ErrDeadlineNotOpen → 409.
func (h *Handler) markPrazoMet(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.MarkMet(c.UserContext(), p.TenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newMarkResponse(res))
}

// markPrazoMissed handles POST /v1/prazos/:id/missed: mark perdido (§9), OPEN→MISSED. The
// manual counterpart of the D+1 carência auto-miss (4b-ii) — same guard (OPEN only), same
// deadline.missed fact. A miss → 404; a non-OPEN prazo → 409.
func (h *Handler) markPrazoMissed(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.MarkMissed(c.UserContext(), p.TenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newMarkResponse(res))
}

// previewPrazo handles POST /v1/prazos/preview: the confirmation panel's live, non-persisted
// recompute (§3). It validates the body, then recomputes the dates read-only (no UoW). A missing
// intimação is the use case's ErrDeadlineNotFound → 404; a bad body is a 400. tenant_id comes
// from the verified principal.
func (h *Handler) previewPrazo(c *fiber.Ctx) error {
	var req PreviewRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	res, err := h.writer.Preview(c.UserContext(), req.toPreviewCommand(p.TenantID))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newPreviewResponse(res))
}

// markPrazoNoDeadline handles POST /v1/prazos/:id/no-deadline: declare "mera ciência" (§3),
// PENDING|OPEN → NO_DEADLINE. No body — the prazo id comes from the path, tenant_id/confirmed_by
// from the principal. A miss → 404; a terminal prazo (MET/MISSED/CANCELLED) → 409.
func (h *Handler) markPrazoNoDeadline(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.NoDeadline(c.UserContext(), p.TenantID, p.UserID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newMarkResponse(res))
}

// reopenPrazo handles POST /v1/prazos/:id/reopen: revert a mera-ciência declaration (§3),
// NO_DEADLINE → PENDING. No body — the prazo id comes from the path, tenant_id from the principal.
// A miss → 404; a non-NO_DEADLINE prazo → 409.
func (h *Handler) reopenPrazo(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.Reopen(c.UserContext(), p.TenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newMarkResponse(res))
}

// apurarDivergencia handles POST /v1/prazos/:id/apurar-divergencia (V1, docs/design-motor-de-
// prazos-v1.md §"Divergência"): the human resolves a declarado×calculado divergência
// (aceita_declarado | aceita_calculado | ajuste_manual), flipping selo a_apurar → confiavel. A
// miss is ErrDeadlineNotFound → 404; a terminal (MET/CANCELLED) or already-resolved/non-
// divergente prazo is ErrDeadlineNotApuravel/ErrDeadlineNotDivergent → 409.
func (h *Handler) apurarDivergencia(c *fiber.Ctx) error {
	var req ApurarDivergenciaRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	res, err := h.writer.ApurarDivergencia(c.UserContext(), req.toApurarDivergenciaCommand(p.TenantID, p.UserID, c.Params("id")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newApuradoDivergenciaResponse(res))
}

// apurarTipo handles POST /v1/prazos/:id/apurar-tipo (V1, docs/design-motor-de-prazos-v1.md
// §"Fallback IA"): the human confirms or reclassifies the IA-inferred tipo de ato, flipping
// selo a_apurar → confiavel. A miss is ErrDeadlineNotFound → 404; a terminal prazo or one whose
// selo is already confiavel is ErrDeadlineNotApuravel/ErrDeadlineNotDivergent → 409.
func (h *Handler) apurarTipo(c *fiber.Ctx) error {
	var req ApurarTipoRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	res, err := h.writer.ApurarTipo(c.UserContext(), req.toApurarTipoCommand(p.TenantID, p.UserID, c.Params("id")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newApuradoTipoResponse(res))
}

// listTasksByProcesso handles GET /v1/processos/:id/tasks: one process's Tasks tab — its tasks
// ordered by due_date (soonest first, undated last), keyset paginated (?limit, ?cursor). The :id
// is the court_record id; tenant_id comes from the principal, so a foreign :id yields an empty
// page. The cursor's sort value is the coalesced due_date, so the first page starts at the min
// sentinel.
func (h *Handler) listTasksByProcesso(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastDue, lastID := minDate, zeroUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastDue, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.TasksByProcesso(c.UserContext(), TasksByProcessoQuery{
		TenantID: tenantID, CourtRecordID: c.Params("id"),
		LastDue: lastDue, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newTasksByProcessoPage(res, limit))
}

// listTasks handles GET /v1/tasks: the tenant's task agenda ("meus prazos") — every task ordered
// by due_date (soonest first, undated last), keyset paginated, optionally filtered by ?status (a
// closed set), ?assignee (a user id, or the sentinel "me" → the principal's own id), ?source (a
// closed set), ?intimation_id (a uuid, narrows to tasks of a specific intimação — mirrors the
// prazos route's same-name filter) and a due_date window ?from/?to. A bad
// status/date/assignee/intimation_id is a client error → 400; a param outside the route's
// allowlist is likewise 400.
func (h *Handler) listTasks(c *fiber.Ctx) error {
	if err := httpx.RejectUnknownParams(c, tasksListParams); err != nil {
		return httpx.WriteError(c, err)
	}
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	status := c.Query("status")
	if status != "" && !isKnownTaskStatus(status) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid status filter"))
	}
	assignee, err := resolveAssignee(c.Query("assignee"), p.UserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	source := c.Query("source")
	if source != "" && !isKnownTaskSource(source) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid source filter"))
	}
	intimationID := c.Query("intimation_id")
	if intimationID != "" {
		if _, err := uuid.Parse(intimationID); err != nil {
			return httpx.WriteError(c, apperr.NewInvalid("invalid intimation_id (want a uuid)"))
		}
	}
	from, err := validateOptionalDate(c.Query("from"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	to, err := validateOptionalDate(c.Query("to"))
	if err != nil {
		return httpx.WriteError(c, err)
	}

	lastDue, lastID := minDate, zeroUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastDue, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.Tasks(c.UserContext(), TasksQuery{
		TenantID: tenantID, Status: status, Assignee: assignee, Source: source,
		IntimationID: intimationID, From: from, To: to,
		LastDue: lastDue, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newTasksPage(res, limit))
}

// createTask handles POST /v1/tasks: create a manual task (§9). It validates the body, then
// persists the task (OPEN, MANUAL, created_by=principal) and emits task.created in one tx,
// returning 201 with the created task. tenant_id and created_by come from the verified principal,
// never the body. A bad body is a 400 with the {kind,message,details} envelope.
func (h *Handler) createTask(c *fiber.Ctx) error {
	var req CreateTaskRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	task, err := h.writer.CreateTask(c.UserContext(), req.toCommand(p.TenantID, p.UserID))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(newTaskViewFromEntity(task))
}

// updateTask handles PATCH /v1/tasks/:id: edit a task's fields (§9). It validates the partial
// body, then merges + updates the task in one tx, returning 200 with the saved task. tenant_id
// comes from the principal and the task id from the path (never the body). A miss is the use
// case's typed ErrTaskNotFound → 404; a bad body is a 400.
func (h *Handler) updateTask(c *fiber.Ctx) error {
	var req UpdateTaskRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	task, err := h.writer.UpdateTask(c.UserContext(), req.toUpdateCommand(p.TenantID, p.UserID, c.Params("id")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newTaskViewFromEntity(task))
}

// markTaskDone handles POST /v1/tasks/:id/done: concluir a tarefa (§9), OPEN→DONE stamping
// completed_at. No body — the task id comes from the path and tenant_id from the principal. A
// miss is ErrTaskNotFound → 404; a non-OPEN task is ErrTaskNotOpen → 409.
func (h *Handler) markTaskDone(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.MarkTaskDone(c.UserContext(), p.TenantID, p.UserID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newTaskTransitionResponse(res))
}

// dismissTask handles POST /v1/tasks/:id/dismiss: dispensar a tarefa (§9), OPEN→DISMISSED. Same
// path/tenant/guards as markTaskDone; completed_at is left NULL (dispensar is not a completion).
func (h *Handler) dismissTask(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.DismissTask(c.UserContext(), p.TenantID, p.UserID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newTaskTransitionResponse(res))
}

// getTask handles GET /v1/tasks/:id: the task detail read model — the task's fields, its ordered
// checklist, the {done, total} progress and the derived display_status. tenant_id comes from the
// principal; a miss (or a foreign tenant's id) is the repo's typed ErrTaskNotFound → 404.
func (h *Handler) getTask(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.TaskDetail(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// listTaskComments handles GET /v1/tasks/:id/comments: a task's discussion thread (oldest-first).
// tenant_id comes from the principal; a foreign/unknown task id is the read use case's typed
// ErrTaskNotFound → 404.
func (h *Handler) listTaskComments(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	comments, err := h.reader.TaskComments(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"data": comments})
}

// createTaskComment handles POST /v1/tasks/:id/comments: append one comment to a task's thread. It
// validates the body, then persists the comment (authored by the principal) + a COMMENTED activity
// row in one tx, returning 201 with the created comment. tenant_id/author come from the verified
// principal, the task id from the path (never the body). A foreign/unknown task id is the use
// case's typed ErrTaskNotFound → 404.
func (h *Handler) createTaskComment(c *fiber.Ctx) error {
	var req CreateTaskCommentRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	comment, err := h.writer.CreateTaskComment(c.UserContext(), req.toCommand(p.TenantID, p.UserID, c.Params("id")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(newTaskCommentResponse(comment))
}

// listTaskActivity handles GET /v1/tasks/:id/activity: a task's audit log (newest-first).
// tenant_id comes from the principal; a foreign/unknown task id is the read use case's typed
// ErrTaskNotFound → 404.
func (h *Handler) listTaskActivity(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	activity, err := h.reader.TaskActivity(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"data": activity})
}

// getPrazosSummary handles GET /v1/prazos/summary: the tenant's prazos KPI counts (a single
// aggregated object). tenant_id comes from the principal; the read is tenant-scoped.
func (h *Handler) getPrazosSummary(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.PrazosSummary(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// getTasksSummary handles GET /v1/tasks/summary: the tenant's tasks KPI counts (a single
// aggregated object, the display_status buckets). tenant_id comes from the principal.
func (h *Handler) getTasksSummary(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.TasksSummary(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// createTaskItem handles POST /v1/tasks/:id/items: append one checklist item to a task. It
// validates the body, then persists the item (appended last, done=false) in one tx, returning 201
// with the created item. tenant_id comes from the principal and the task id from the path (never
// the body). A foreign/unknown task id is the use case's typed ErrTaskItemNotFound → 404.
func (h *Handler) createTaskItem(c *fiber.Ctx) error {
	var req CreateTaskItemRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	item, err := h.writer.CreateTaskItem(c.UserContext(), req.toCommand(tenantID, c.Params("id")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(newTaskItemView(item))
}

// updateTaskItem handles PATCH /v1/tasks/:id/items/:itemId: toggle/rename one checklist item. It
// validates the partial body, then merges + updates the item in one tx (setting/clearing done_at
// per done), returning 200 with the saved item. tenant_id/task id/item id come from the principal
// + path. A miss is the use case's typed ErrTaskItemNotFound → 404.
func (h *Handler) updateTaskItem(c *fiber.Ctx) error {
	var req UpdateTaskItemRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	item, err := h.writer.UpdateTaskItem(c.UserContext(), req.toCommand(tenantID, c.Params("id"), c.Params("itemId")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newTaskItemView(item))
}

// deleteTaskItem handles DELETE /v1/tasks/:id/items/:itemId: remove one checklist item. No body —
// the ids come from the path and tenant_id from the principal. A miss is the use case's typed
// ErrTaskItemNotFound → 404; a successful delete is 204 No Content.
func (h *Handler) deleteTaskItem(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	if err := h.writer.DeleteTaskItem(c.UserContext(), tenantID, c.Params("id"), c.Params("itemId")); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// newTaskItemView renders a persisted *TaskItem (a create/patch write result) as the wire shape.
// done_at is null while the item is undone; it mirrors the read model's TaskItemView so a checklist
// item looks identical read or written.
func newTaskItemView(item *TaskItem) TaskItemView {
	return TaskItemView{
		ID:        item.ID,
		Title:     item.Title,
		Position:  item.Position,
		Done:      item.Done,
		DoneAt:    item.DoneAt,
		CreatedAt: item.CreatedAt,
	}
}

// isKnownStatus reports whether s is a member of the closed prazo status set (the DB
// CHECK enforces the same set). An unknown ?status is rejected at the edge rather than
// silently returning an empty page.
func isKnownStatus(s string) bool {
	switch Status(s) {
	case StatusPending, StatusOpen, StatusMet, StatusMissed, StatusCancelled, StatusNoDeadline:
		return true
	default:
		return false
	}
}

// isKnownTaskStatus reports whether s is a member of the closed task status set (the DB CHECK
// enforces the same set). An unknown ?status is rejected at the edge rather than silently
// returning an empty page.
func isKnownTaskStatus(s string) bool {
	switch TaskStatus(s) {
	case TaskStatusOpen, TaskStatusDone, TaskStatusDismissed:
		return true
	default:
		return false
	}
}

// resolveAssignee turns the ?assignee filter into the value the read model filters on: "" is no
// filter, the sentinel "me" resolves to the principal's own id (the "meus prazos" shortcut), and
// any other value must be a well-formed uuid (a client error → 400 otherwise). It returns the
// value verbatim (or the resolved principal id) so the repo re-parses it once.
func resolveAssignee(assignee, principalUserID string) (string, error) {
	switch assignee {
	case "":
		return "", nil
	case "me":
		return principalUserID, nil
	default:
		if _, err := uuid.Parse(assignee); err != nil {
			return "", apperr.NewInvalid("invalid assignee filter (want a user id or \"me\")")
		}
		return assignee, nil
	}
}

// validateOptionalDate accepts an empty filter ("" → open bound) or a wire date
// (2006-01-02); anything else is a client error → 400. It returns the value verbatim so
// the repo re-parses it once, keeping the query struct wire-shaped.
func validateOptionalDate(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		return "", apperr.NewInvalid("invalid date filter (want YYYY-MM-DD)")
	}
	return s, nil
}

// confirmResponse is the POST /prazos/confirm payload: the confirmed prazo. It is a
// purpose-built write DTO (not the read-model detail view — that is 5b's concern), so the F2
// screen can render the result without a follow-up read. It carries no tasks — those are
// created independently via POST /v1/tasks (the "Análise" section), not by the confirm.
type confirmResponse struct {
	Deadline confirmedDeadlineView `json:"deadline"`
}

// confirmedDeadlineView is the confirmed prazo in the response — the recomputed,
// human-approved fact with its auditable holidays_applied.
type confirmedDeadlineView struct {
	ID              string    `json:"id"`
	CourtRecordID   string    `json:"court_record_id"`
	IntimationID    string    `json:"intimation_id"`
	Kind            string    `json:"kind"`
	Days            int       `json:"days"`
	Counting        string    `json:"counting"`
	Doubled         bool      `json:"doubled"`
	DoubledReason   string    `json:"doubled_reason"`
	Status          string    `json:"status"`
	StartDate       time.Time `json:"start_date"`
	EndDate         time.Time `json:"end_date"`
	HolidaysApplied []string  `json:"holidays_applied"`
	ConfirmedBy     string    `json:"confirmed_by"`
	AnchorEvent     string    `json:"anchor_event"`
	ManualExtraDays int       `json:"manual_extra_days"`
	LegalCitation   string    `json:"legal_citation,omitempty"`
}

// newConfirmResponse maps the use case's ConfirmResult to the client-facing payload. The
// holidays audit and the wire dates are formatted here.
func newConfirmResponse(res ConfirmResult) confirmResponse {
	d := res.Deadline
	return confirmResponse{
		Deadline: confirmedDeadlineView{
			ID:              d.ID,
			CourtRecordID:   d.CourtRecordID,
			IntimationID:    d.IntimationID,
			Kind:            d.Kind,
			Days:            d.Days,
			Counting:        string(d.Counting),
			Doubled:         d.Doubled,
			DoubledReason:   d.DoubledReason,
			Status:          string(d.Status),
			StartDate:       d.StartDate,
			EndDate:         d.EndDate,
			HolidaysApplied: formatDates(d.HolidaysApplied),
			ConfirmedBy:     d.ConfirmedBy,
			AnchorEvent:     string(d.AnchorEvent),
			ManualExtraDays: d.ManualExtraDays,
			LegalCitation:   d.LegalCitation,
		},
	}
}

// adjustedDeadlineView is the PATCH /v1/prazos/:id payload: the recomputed prazo (the ajuste
// touches only the deadline, so there are no tasks). It carries the same auditable
// holidays_applied + wire dates as the confirm view; Status is echoed unchanged (the ajuste
// never flips the lifecycle).
type adjustedDeadlineView struct {
	ID              string    `json:"id"`
	CourtRecordID   string    `json:"court_record_id"`
	Kind            string    `json:"kind"`
	Days            int       `json:"days"`
	Counting        string    `json:"counting"`
	Doubled         bool      `json:"doubled"`
	DoubledReason   string    `json:"doubled_reason"`
	Status          string    `json:"status"`
	StartDate       time.Time `json:"start_date"`
	EndDate         time.Time `json:"end_date"`
	HolidaysApplied []string  `json:"holidays_applied"`
	AnchorEvent     string    `json:"anchor_event"`
	ManualExtraDays int       `json:"manual_extra_days"`
}

// newAdjustResponse maps the use case's AdjustedDeadline to the client-facing payload; the
// holidays audit is formatted here (always a non-nil slice, so an empty audit is []).
func newAdjustResponse(d AdjustedDeadline) adjustedDeadlineView {
	return adjustedDeadlineView{
		ID:              d.ID,
		CourtRecordID:   d.CourtRecordID,
		Kind:            d.Kind,
		Days:            d.Days,
		Counting:        string(d.Counting),
		Doubled:         d.Doubled,
		DoubledReason:   d.DoubledReason,
		Status:          string(d.Status),
		StartDate:       d.StartDate,
		EndDate:         d.EndDate,
		HolidaysApplied: formatDates(d.HolidaysApplied),
		AnchorEvent:     string(d.AnchorEvent),
		ManualExtraDays: d.ManualExtraDays,
	}
}

// previewResponse is the POST /v1/prazos/preview payload: the recomputed dates the confirmation
// panel renders live (never persisted). weekday is the pt-BR end-date weekday; days_left is
// calendar days from today; holidays_applied is the auditable skipped-days list.
type previewResponse struct {
	StartDate       time.Time `json:"start_date"`
	EndDate         time.Time `json:"end_date"`
	Weekday         string    `json:"weekday"`
	DaysLeft        int       `json:"days_left"`
	HolidaysApplied []string  `json:"holidays_applied"`
}

// newPreviewResponse maps the use case's PreviewResult to the client-facing payload; the holidays
// audit is formatted here (always a non-nil slice, so an empty audit is []).
func newPreviewResponse(r PreviewResult) previewResponse {
	return previewResponse{
		StartDate:       r.StartDate,
		EndDate:         r.EndDate,
		Weekday:         r.Weekday,
		DaysLeft:        r.DaysLeft,
		HolidaysApplied: formatDates(r.HolidaysApplied),
	}
}

// markResponse is the POST /v1/prazos/:id/met | .../missed payload: the transitioned prazo id
// and its new status (MET / MISSED) — the minimal fact the F2 screen needs to reflect the flip.
type markResponse struct {
	DeadlineID string `json:"deadline_id"`
	Status     string `json:"status"`
}

// newMarkResponse maps the use case's MarkedDeadline to the client-facing payload.
func newMarkResponse(d MarkedDeadline) markResponse {
	return markResponse{DeadlineID: d.ID, Status: string(d.Status)}
}

// apuradoDivergenciaResponse is the POST /v1/prazos/:id/apurar-divergencia payload: the (possibly
// recomputed) end_date, the flipped selo and the decisão recorded.
type apuradoDivergenciaResponse struct {
	DeadlineID string    `json:"deadline_id"`
	EndDate    time.Time `json:"end_date"`
	Selo       string    `json:"selo"`
	Decisao    string    `json:"decisao"`
}

// newApuradoDivergenciaResponse maps the use case's ApuradoDivergencia to the client-facing payload.
func newApuradoDivergenciaResponse(d ApuradoDivergencia) apuradoDivergenciaResponse {
	return apuradoDivergenciaResponse{
		DeadlineID: d.ID,
		EndDate:    d.EndDate,
		Selo:       string(d.Seal),
		Decisao:    string(d.Decisao),
	}
}

// apuradoTipoResponse is the POST /v1/prazos/:id/apurar-tipo payload: the confirmed/reclassified
// tipo de ato and the flipped selo.
type apuradoTipoResponse struct {
	DeadlineID string `json:"deadline_id"`
	Tipo       string `json:"tipo"`
	Selo       string `json:"selo"`
}

// newApuradoTipoResponse maps the use case's ApuradoTipo to the client-facing payload.
func newApuradoTipoResponse(t ApuradoTipo) apuradoTipoResponse {
	return apuradoTipoResponse{DeadlineID: t.ID, Tipo: t.Tipo, Selo: string(t.Seal)}
}

// taskTransitionResponse is the POST /v1/tasks/:id/done | .../dismiss payload: the transitioned
// task id and its new status (DONE / DISMISSED) — the minimal fact the FE needs to reflect the
// flip. It mirrors markResponse (the prazo transitions).
type taskTransitionResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// newTaskTransitionResponse maps the use case's TaskTransition to the client-facing payload.
func newTaskTransitionResponse(t TaskTransition) taskTransitionResponse {
	return taskTransitionResponse{TaskID: t.ID, Status: string(t.Status)}
}

// newTaskCommentResponse renders a persisted *TaskComment as the TaskCommentView wire shape — the
// same shape the GET .../comments list returns, so the created comment and the listed comments are
// byte-for-byte the same JSON. AuthorName is left "" (not resolved on the write path); the FE
// already knows the author (it is the current user) or refetches the thread.
func newTaskCommentResponse(c *TaskComment) TaskCommentView {
	return TaskCommentView{
		ID:           c.ID,
		AuthorUserID: c.AuthorUserID,
		Body:         c.Body,
		CreatedAt:    c.CreatedAt,
	}
}

// newTasksByProcessoPage wraps the Tasks-tab read model in the cursor envelope; the next cursor
// keys off the last row's (sort_due, id) — sort_due is the coalesced due_date, so an undated
// task's cursor keys off the '9999-12-31' sentinel and the scan resumes correctly. No filter on
// the tab, so the "X de Y" totals coincide.
func newTasksByProcessoPage(res TasksByProcessoResult, limit int) httpx.Page[TaskView] {
	items := res.Items
	if items == nil {
		items = []TaskView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.sortDue.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[TaskView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

// newTasksPage wraps the task agenda read model in the cursor envelope; the next cursor keys off
// the last row's (sort_due, id) and the totals carry "X de Y" (filtered vs tenant-wide).
func newTasksPage(res TasksResult, limit int) httpx.Page[TaskView] {
	items := res.Items
	if items == nil {
		items = []TaskView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.TotalCount, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.sortDue.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[TaskView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

// formatDates renders the holidays audit as wire dates (2006-01-02), always a non-nil
// slice so it serializes as [] not null.
func formatDates(days []time.Time) []string {
	out := make([]string, 0, len(days))
	for _, d := range days {
		out = append(out, d.Format(time.DateOnly))
	}
	return out
}

// newPrazosByProcessoPage wraps the Prazos-tab read model in the cursor envelope; the
// next cursor keys off the last row's (end_date, id). There is no filter on the tab, so
// the "X de Y" totals coincide (both the process's prazo count).
func newPrazosByProcessoPage(res PrazosByProcessoResult, limit int) httpx.Page[PrazoView] {
	items := res.Items
	if items == nil {
		items = []PrazoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.EndDate.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[PrazoView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

// newPrazosPage wraps the agenda read model in the cursor envelope; the next cursor keys
// off the last row's (end_date, id) and the totals carry "X de Y" (filtered vs
// tenant-wide).
func newPrazosPage(res PrazosResult, limit int) httpx.Page[AgendaPrazoView] {
	items := res.Items
	if items == nil {
		items = []AgendaPrazoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.TotalCount, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.EndDate.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[AgendaPrazoView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}
