package deadline

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/deadline/deadlinedb"
	"github.com/jusassessoria/platform/lib/database"
)

// read_repository.go is the pool-backed read adapter — the screen reads, off the
// transactional write path (which uses the stateless, tx-bound pgRepository). It holds
// its own deadlinedb bound to the pool; every query filters tenant_id explicitly
// (barrier 1) from the trusted principal, so a caller only ever sees its own prazos.
// The mapper here absorbs the driver types (pgtype.Date, the jsonb bytes, the
// interface{} sqlc infers for the confirmed expression) so the read models stay pure.

// pgReadRepository serves the read port off the connection pool. Reads are not part of
// the use case's write tx, so the repo owns its own Queries (bound once at construction).
type pgReadRepository struct {
	q *deadlinedb.Queries
}

var _ readRepo = (*pgReadRepository)(nil)

// NewReadRepository returns the read port over the pool. Share nothing with the write
// repo: the read side never enrolls in the write transaction.
func NewReadRepository(pool deadlinedb.DBTX) readRepo {
	return &pgReadRepository{q: deadlinedb.New(pool)}
}

// ListPrazosByProcesso reads one process's prazos (ascending keyset by end_date, soonest
// first) on the pool, filtered by tenant_id and court_record_id. The caller passes the
// min sentinel cursor for the first page.
func (r *pgReadRepository) ListPrazosByProcesso(ctx context.Context, q PrazosByProcessoQuery) ([]PrazoView, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return nil, err
	}
	crid, err := parseUUID(q.CourtRecordID)
	if err != nil {
		return nil, err
	}
	lastID, err := parseUUID(q.LastID)
	if err != nil {
		return nil, err
	}
	lastEnd, err := keysetDate(q.LastEnd)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListPrazosByProcesso(ctx, deadlinedb.ListPrazosByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
		LastEnd:       lastEnd,
		LastID:        lastID,
		PageLimit:     int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]PrazoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, PrazoView{
			ID:              row.ID.String(),
			Kind:            derefString(row.Kind),
			EndDate:         row.EndDate.Time,
			DaysLeft:        int(row.DaysLeft),
			Counting:        row.Counting,
			Doubled:         row.Doubled,
			DoubledReason:   derefString(row.DoubledReason),
			Status:          row.Status,
			HolidaysApplied: holidaysFromJSON(row.HolidaysApplied),
			IntimationID:    row.NotificationID.String(),
			Confirmed:       confirmedBool(row.Confirmed),
		})
	}
	return out, nil
}

// CountPrazosByProcesso returns the "X de Y" total for the Prazos tab, scoped by the
// same tenant + court_record as the list.
func (r *pgReadRepository) CountPrazosByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return 0, err
	}
	crid, err := parseUUID(courtRecordID)
	if err != nil {
		return 0, err
	}
	total, err := r.q.CountPrazosByProcesso(ctx, deadlinedb.CountPrazosByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

// ListPrazos reads the tenant's agenda (ascending keyset by end_date) on the pool, with
// the optional status/window filters applied. The process context (cnj/court) comes from
// the court_record join.
func (r *pgReadRepository) ListPrazos(ctx context.Context, q PrazosQuery) ([]AgendaPrazoView, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return nil, err
	}
	lastID, err := parseUUID(q.LastID)
	if err != nil {
		return nil, err
	}
	lastEnd, err := keysetDate(q.LastEnd)
	if err != nil {
		return nil, err
	}
	from, err := optionalFilterDate(q.From)
	if err != nil {
		return nil, err
	}
	to, err := optionalFilterDate(q.To)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListPrazos(ctx, deadlinedb.ListPrazosParams{
		TenantID:  tid,
		Status:    q.Status,
		Kind:      q.Kind,
		Court:     q.Court,
		FromDate:  from,
		ToDate:    to,
		LastEnd:   lastEnd,
		LastID:    lastID,
		PageLimit: int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]AgendaPrazoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, AgendaPrazoView{
			ID:              row.ID.String(),
			Kind:            derefString(row.Kind),
			EndDate:         row.EndDate.Time,
			DaysLeft:        int(row.DaysLeft),
			Counting:        row.Counting,
			Doubled:         row.Doubled,
			DoubledReason:   derefString(row.DoubledReason),
			Status:          row.Status,
			HolidaysApplied: holidaysFromJSON(row.HolidaysApplied),
			IntimationID:    row.NotificationID.String(),
			Confirmed:       confirmedBool(row.Confirmed),
			CourtRecordID:   row.CourtRecordID.String(),
			CNJNumber:       row.CnjNumber,
			Court:           row.Court,
		})
	}
	return out, nil
}

// ListPrazosByIntimacao reads the prazo of one intimação on the pool, filtered by
// tenant_id (barrier 1) and the 1:1 notification_id. It maps to the same AgendaPrazoView
// as ListPrazos (the process context comes from the court_record join); the result is 0
// or 1 row (notification_id is UNIQUE).
func (r *pgReadRepository) ListPrazosByIntimacao(ctx context.Context, tenantID, intimationID string) ([]AgendaPrazoView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	nid, err := parseUUID(intimationID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListPrazosByIntimacao(ctx, deadlinedb.ListPrazosByIntimacaoParams{
		TenantID:     tid,
		IntimationID: nid,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]AgendaPrazoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, AgendaPrazoView{
			ID:              row.ID.String(),
			Kind:            derefString(row.Kind),
			EndDate:         row.EndDate.Time,
			DaysLeft:        int(row.DaysLeft),
			Counting:        row.Counting,
			Doubled:         row.Doubled,
			DoubledReason:   derefString(row.DoubledReason),
			Status:          row.Status,
			HolidaysApplied: holidaysFromJSON(row.HolidaysApplied),
			IntimationID:    row.NotificationID.String(),
			Confirmed:       confirmedBool(row.Confirmed),
			CourtRecordID:   row.CourtRecordID.String(),
			CNJNumber:       row.CnjNumber,
			Court:           row.Court,
		})
	}
	return out, nil
}

// CountPrazos returns the agenda's "X de Y": the filtered count (Status/Kind/Court/
// window) and the tenant-wide count. When no filter is active the two coincide, so a
// single tenant COUNT fills both (mirrors the acquisition read model) — one query
// instead of two.
func (r *pgReadRepository) CountPrazos(ctx context.Context, q PrazosQuery) (int64, int64, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return 0, 0, err
	}

	total, err := r.q.CountPrazosByTenant(ctx, tid)
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}

	if q.Status == "" && q.Kind == "" && q.Court == "" && q.From == "" && q.To == "" {
		return total, total, nil
	}

	from, err := optionalFilterDate(q.From)
	if err != nil {
		return 0, 0, err
	}
	to, err := optionalFilterDate(q.To)
	if err != nil {
		return 0, 0, err
	}
	totalCount, err := r.q.CountPrazos(ctx, deadlinedb.CountPrazosParams{
		TenantID: tid,
		Status:   q.Status,
		Kind:     q.Kind,
		Court:    q.Court,
		FromDate: from,
		ToDate:   to,
	})
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}
	return totalCount, total, nil
}

// GetPrazo reads one prazo's audit detail on the pool, filtered by tenant_id. A miss —
// or a foreign tenant's row — maps to the typed ErrDeadlineNotFound (→ 404), never
// (nil, nil).
func (r *pgReadRepository) GetPrazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return PrazoDetailView{}, err
	}
	pid, err := parseUUID(id)
	if err != nil {
		return PrazoDetailView{}, err
	}

	row, err := r.q.GetPrazo(ctx, deadlinedb.GetPrazoParams{ID: pid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return PrazoDetailView{}, ErrDeadlineNotFound
	}
	if err != nil {
		return PrazoDetailView{}, database.WrapInfra(err)
	}

	view := PrazoDetailView{
		ID:                 row.ID.String(),
		CourtRecordID:      row.CourtRecordID.String(),
		Kind:               derefString(row.Kind),
		StartDate:          row.StartDate.Time,
		EndDate:            row.EndDate.Time,
		DaysLeft:           int(row.DaysLeft),
		Days:               int(row.Days),
		Counting:           row.Counting,
		Doubled:            row.Doubled,
		DoubledReason:      derefString(row.DoubledReason),
		Status:             row.Status,
		Source:             row.Source,
		HolidaysApplied:    holidaysFromJSON(row.HolidaysApplied),
		IntimationID:       row.NotificationID.String(),
		RulesVersion:       row.RulesVersion,
		Confirmed:          confirmedBool(row.Confirmed),
		AnchorEvent:        row.AnchorEvent,
		LegalCitation:      derefString(row.LegalCitation),
		ManualExtraDays:    int(row.ManualExtraDays),
		ConfirmedByID:      uuidText(row.ConfirmedBy),
		ConfirmedByName:    derefString(row.ConfirmedByName),
		ConfirmedAt:        timestampPtr(row.ConfirmedAt),
		Origem:             derefString(row.Origem),
		Selo:               derefString(row.Selo),
		ConfirmacaoExigida: derefBool(row.ConfirmacaoExigida),
		PrazoInterno:       row.PrazoInterno.Time,
	}

	// V1 memória de cálculo: calc_memory/cross_validation are LEFT JOINed, so a pré-V1 prazo (no
	// row ever written) comes back with every cm./cv. column NULL — row.CalcMemoryID.Valid /
	// row.Decisao being nil are the presence checks, degrading to nil rather than a zeroed view.
	if row.CalcMemoryID.Valid {
		view.CalcMemory = &CalcMemoryView{
			ID:                      row.CalcMemoryID.String(),
			PrazoBase:               derefString(row.PrazoBase),
			PrazoBaseFonte:          derefString(row.PrazoBaseFonte),
			TermoInicialRegra:       derefString(row.TermoInicialRegra),
			DiasUteis:               derefBool(row.DiasUteis),
			DobraMotivo:             derefString(row.DobraMotivo),
			TabelaLegalRef:          derefString(row.TabelaLegalRef),
			IATipoInferido:          derefString(row.IaTipoInferido),
			IAConfianca:             derefFloat64(row.IaConfianca),
			CalendarProviderVersion: derefString(row.CalendarProviderVersion),
		}
	}
	if row.Resultado != nil {
		view.CrossValidation = &CrossValidationView{
			DataDeclarada: row.DataDeclarada.Time,
			DataCalculada: row.DataCalculada.Time,
			DifDias:       derefInt32(row.DifDias),
			Resultado:     derefString(row.Resultado),
			CausaProvavel: derefString(row.CausaProvavel),
			Decisao:       derefString(row.Decisao),
			DecididoPor:   uuidText(row.DecididoPor),
		}
	}

	return view, nil
}

// ListAppliedHolidaysByCalcMemory reads the 1:N feriados snapshot for one calc_memory on the
// pool, filtered by tenant_id (barrier 1). No holidays applied yields an empty slice, never an
// error (a :many query never returns pgx.ErrNoRows).
func (r *pgReadRepository) ListAppliedHolidaysByCalcMemory(ctx context.Context, tenantID, calcMemoryID string) ([]AppliedHolidayView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	cmid, err := parseUUID(calcMemoryID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListAppliedHolidaysByCalcMemory(ctx, deadlinedb.ListAppliedHolidaysByCalcMemoryParams{
		CalcMemoryID: cmid,
		TenantID:     tid,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]AppliedHolidayView, 0, len(rows))
	for _, row := range rows {
		out = append(out, AppliedHolidayView{
			Data:    row.Data.Time,
			Nome:    derefString(row.Nome),
			Ambito:  derefString(row.Ambito),
			Comarca: derefString(row.Comarca),
		})
	}
	return out, nil
}

// GetPrazoSuggestContext reads the advisory case context for one prazo on the pool, filtered by
// tenant_id (barrier 1): the prazo's own signals plus the process's court_record and the origin
// intimação's type + truncated teor. A miss — or a foreign tenant's row — maps to the typed
// ErrDeadlineNotFound (→ 404), never (nil, nil). The mapper derefs the nullable columns
// (kind/class/subject/type) to "" so an absent signal drops out of the composed prompt.
func (r *pgReadRepository) GetPrazoSuggestContext(ctx context.Context, tenantID, id string) (PrazoSuggestContext, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return PrazoSuggestContext{}, err
	}
	pid, err := parseUUID(id)
	if err != nil {
		return PrazoSuggestContext{}, err
	}

	row, err := r.q.GetPrazoSuggestContext(ctx, deadlinedb.GetPrazoSuggestContextParams{ID: pid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return PrazoSuggestContext{}, ErrDeadlineNotFound
	}
	if err != nil {
		return PrazoSuggestContext{}, database.WrapInfra(err)
	}

	return PrazoSuggestContext{
		Kind:           derefString(row.Kind),
		Days:           int(row.Days),
		Counting:       row.Counting,
		IntimationID:   row.NotificationID.String(),
		Court:          row.Court,
		Degree:         row.Degree,
		Class:          derefString(row.Class),
		Subject:        derefString(row.Subject),
		IntimationType: derefString(row.IntimationType),
		IntimationText: row.IntimationText,

		AISummary:            derefString(row.AiSummary),
		AIRecommendation:     derefString(row.AiRecommendation),
		AISummaryGeneratedAt: timestampPtr(row.AiSummaryGeneratedAt),
	}, nil
}

// ListTasksByProcesso reads one process's tasks (ascending keyset by the coalesced due_date,
// soonest first / undated last) on the pool, filtered by tenant_id and court_record_id. The
// caller passes the min sentinel cursor for the first page.
func (r *pgReadRepository) ListTasksByProcesso(ctx context.Context, q TasksByProcessoQuery) ([]TaskView, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return nil, err
	}
	crid, err := parseUUID(q.CourtRecordID)
	if err != nil {
		return nil, err
	}
	lastID, err := parseUUID(q.LastID)
	if err != nil {
		return nil, err
	}
	lastDue, err := keysetDate(q.LastDue)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListTasksByProcesso(ctx, deadlinedb.ListTasksByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
		LastDue:       lastDue,
		LastID:        lastID,
		PageLimit:     int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]TaskView, 0, len(rows))
	for _, row := range rows {
		out = append(out, TaskView{
			ID:             row.ID.String(),
			Title:          row.Title,
			Description:    derefString(row.Description),
			Kind:           derefString(row.Kind),
			Priority:       derefString(row.Priority),
			DueDate:        datePtr(row.DueDate),
			Status:         row.Status,
			Source:         row.Source,
			AssigneeUserID: uuidText(row.AssigneeUserID),
			DeadlineID:     uuidText(row.DeadlineID),
			IntimationID:   uuidText(row.IntimationID),
			CourtRecordID:  uuidText(row.CourtRecordID),
			CompletedAt:    timestampPtr(row.CompletedAt),
			sortDue:        row.SortDue.Time,
			doneItems:      int(row.DoneItems),
		})
	}
	return out, nil
}

// CountTasksByProcesso returns the "X de Y" total for the Tasks tab, scoped by the same tenant +
// court_record as the list.
func (r *pgReadRepository) CountTasksByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return 0, err
	}
	crid, err := parseUUID(courtRecordID)
	if err != nil {
		return 0, err
	}
	total, err := r.q.CountTasksByProcesso(ctx, deadlinedb.CountTasksByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

// ListTasks reads the tenant's task agenda (ascending keyset by the coalesced due_date) on the
// pool, with the optional status/assignee/window filters applied.
func (r *pgReadRepository) ListTasks(ctx context.Context, q TasksQuery) ([]TaskView, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return nil, err
	}
	lastID, err := parseUUID(q.LastID)
	if err != nil {
		return nil, err
	}
	lastDue, err := keysetDate(q.LastDue)
	if err != nil {
		return nil, err
	}
	from, err := optionalFilterDate(q.From)
	if err != nil {
		return nil, err
	}
	to, err := optionalFilterDate(q.To)
	if err != nil {
		return nil, err
	}
	assignee, err := optionalFilterUUID(q.Assignee)
	if err != nil {
		return nil, err
	}
	intimationID, err := optionalFilterUUID(q.IntimationID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListTasks(ctx, deadlinedb.ListTasksParams{
		TenantID:     tid,
		Status:       q.Status,
		AssigneeID:   assignee,
		Source:       q.Source,
		IntimationID: intimationID,
		FromDate:     from,
		ToDate:       to,
		PipelineOnly: q.PipelineOnly,
		LastDue:      lastDue,
		LastID:       lastID,
		PageLimit:    int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]TaskView, 0, len(rows))
	for _, row := range rows {
		out = append(out, TaskView{
			ID:             row.ID.String(),
			Title:          row.Title,
			Description:    derefString(row.Description),
			Kind:           derefString(row.Kind),
			Priority:       derefString(row.Priority),
			DueDate:        datePtr(row.DueDate),
			Status:         row.Status,
			PipelineStage:  derivePipelineStage(row.DraftID.Valid, timestampPtr(row.SentToSigningAt), timestampPtr(row.FiledAt)),
			Source:         row.Source,
			AssigneeUserID: uuidText(row.AssigneeUserID),
			DeadlineID:     uuidText(row.DeadlineID),
			IntimationID:   uuidText(row.IntimationID),
			CourtRecordID:  uuidText(row.CourtRecordID),
			CompletedAt:    timestampPtr(row.CompletedAt),
			CNJNumber:      derefString(row.CnjNumber),
			Court:          derefString(row.Court),
			sortDue:        row.SortDue.Time,
			doneItems:      int(row.DoneItems),
		})
	}
	return out, nil
}

// CountTasks returns the task agenda's "X de Y": the filtered count (Status/Assignee/
// Source/window) and the tenant-wide count. When no filter is active the two coincide, so
// a single tenant COUNT fills both (mirrors CountPrazos) — one query instead of two.
func (r *pgReadRepository) CountTasks(ctx context.Context, q TasksQuery) (int64, int64, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return 0, 0, err
	}

	total, err := r.q.CountTasksByTenant(ctx, tid)
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}

	if !q.Filtered() {
		return total, total, nil
	}

	from, err := optionalFilterDate(q.From)
	if err != nil {
		return 0, 0, err
	}
	to, err := optionalFilterDate(q.To)
	if err != nil {
		return 0, 0, err
	}
	assignee, err := optionalFilterUUID(q.Assignee)
	if err != nil {
		return 0, 0, err
	}
	intimationID, err := optionalFilterUUID(q.IntimationID)
	if err != nil {
		return 0, 0, err
	}
	totalCount, err := r.q.CountTasks(ctx, deadlinedb.CountTasksParams{
		TenantID:     tid,
		Status:       q.Status,
		AssigneeID:   assignee,
		Source:       q.Source,
		IntimationID: intimationID,
		FromDate:     from,
		ToDate:       to,
	})
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}
	return totalCount, total, nil
}

// ListPrazoKinds reads the distinct kinds of the tenant's prazos (the agenda's ?kind
// options), ordered by name. Empty kinds are skipped in Go (a blank chip is never
// selectable).
func (r *pgReadRepository) ListPrazoKinds(ctx context.Context, tenantID string) ([]string, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListPrazoKinds(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if row != nil && *row != "" {
			out = append(out, *row)
		}
	}
	return out, nil
}

// ListPrazoCourts reads the distinct courts of the tenant's prazos' court records (the
// agenda's ?court options), ordered by name.
func (r *pgReadRepository) ListPrazoCourts(ctx context.Context, tenantID string) ([]string, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListPrazoCourts(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return rows, nil
}

// ListTaskAssignees reads the distinct responsáveis of the tenant's tasks (the agenda's
// ?assignee options), deduped by id and ordered by name. The LEFT JOIN app_user resolves a
// name when the id is a known user; an unknown id yields an empty name.
func (r *pgReadRepository) ListTaskAssignees(ctx context.Context, tenantID string) ([]AssigneeOption, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListTaskAssignees(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]AssigneeOption, 0, len(rows))
	for _, row := range rows {
		if !row.AssigneeUserID.Valid {
			continue
		}
		id := row.AssigneeUserID.Bytes
		out = append(out, AssigneeOption{Name: row.Name, ID: uuid.UUID(id).String()})
	}
	return out, nil
}

// GetTaskDetail reads one task's own fields for the detail view on the pool, filtered by
// tenant_id. A miss — or a foreign tenant's row — maps to the typed ErrTaskNotFound (→ 404),
// never (nil, nil). The checklist + progress are separate reads (a task with no items still
// resolves). display_status is derived in the use case, not here.
func (r *pgReadRepository) GetTaskDetail(ctx context.Context, tenantID, id string) (TaskDetailView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return TaskDetailView{}, err
	}
	taskID, err := parseUUID(id)
	if err != nil {
		return TaskDetailView{}, err
	}

	row, err := r.q.GetTaskDetail(ctx, deadlinedb.GetTaskDetailParams{ID: taskID, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskDetailView{}, ErrTaskNotFound
	}
	if err != nil {
		return TaskDetailView{}, database.WrapInfra(err)
	}

	return TaskDetailView{
		ID:             row.ID.String(),
		Title:          row.Title,
		Description:    derefString(row.Description),
		Kind:           derefString(row.Kind),
		Priority:       derefString(row.Priority),
		DueDate:        datePtr(row.DueDate),
		Status:         row.Status,
		Source:         row.Source,
		AssigneeUserID: uuidText(row.AssigneeUserID),
		DeadlineID:     uuidText(row.DeadlineID),
		IntimationID:   uuidText(row.IntimationID),
		CourtRecordID:  uuidText(row.CourtRecordID),
		CompletedAt:    timestampPtr(row.CompletedAt),
	}, nil
}

// ListTaskItems reads one task's ordered checklist on the pool, filtered by tenant_id and
// task_id. An itemless task yields an empty slice (never an error).
func (r *pgReadRepository) ListTaskItems(ctx context.Context, tenantID, taskID string) ([]TaskItemView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	tkid, err := parseUUID(taskID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListTaskItems(ctx, deadlinedb.ListTaskItemsParams{TaskID: tkid, TenantID: tid})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]TaskItemView, 0, len(rows))
	for _, row := range rows {
		out = append(out, TaskItemView{
			ID:        row.ID.String(),
			Title:     row.Title,
			Position:  int(row.Position),
			Done:      row.Done,
			DoneAt:    timestampPtr(row.DoneAt),
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return out, nil
}

// TaskItemProgress reads one task's {done, total} checklist tally on the pool, filtered by
// tenant_id and task_id. An itemless task yields {0, 0}.
func (r *pgReadRepository) TaskItemProgress(ctx context.Context, tenantID, taskID string) (TaskProgress, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return TaskProgress{}, err
	}
	tkid, err := parseUUID(taskID)
	if err != nil {
		return TaskProgress{}, err
	}

	row, err := r.q.GetTaskItemProgress(ctx, deadlinedb.GetTaskItemProgressParams{TaskID: tkid, TenantID: tid})
	if err != nil {
		return TaskProgress{}, database.WrapInfra(err)
	}
	return TaskProgress{Done: int(row.Done), Total: int(row.Total)}, nil
}

// ListTaskComments reads one task's discussion thread on the pool (oldest-first), filtered by
// tenant_id and task_id. The author is resolved to a name (LEFT JOIN app_user); an empty thread
// yields an empty slice (never an error).
func (r *pgReadRepository) ListTaskComments(ctx context.Context, tenantID, taskID string) ([]TaskCommentView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	tkid, err := parseUUID(taskID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListTaskComments(ctx, deadlinedb.ListTaskCommentsParams{TaskID: tkid, TenantID: tid})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]TaskCommentView, 0, len(rows))
	for _, row := range rows {
		out = append(out, TaskCommentView{
			ID:           row.ID.String(),
			AuthorUserID: row.AuthorUserID.String(),
			AuthorName:   row.AuthorName,
			Body:         row.Body,
			CreatedAt:    row.CreatedAt.Time,
		})
	}
	return out, nil
}

// ListTaskActivity reads one task's audit log on the pool (newest-first), filtered by tenant_id
// and task_id. The actor is resolved to a name (LEFT JOIN app_user); the nullable from/to are
// lifted to "" via derefString. An empty log yields an empty slice (never an error).
func (r *pgReadRepository) ListTaskActivity(ctx context.Context, tenantID, taskID string) ([]TaskActivityView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	tkid, err := parseUUID(taskID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListTaskActivity(ctx, deadlinedb.ListTaskActivityParams{TaskID: tkid, TenantID: tid})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]TaskActivityView, 0, len(rows))
	for _, row := range rows {
		out = append(out, TaskActivityView{
			ID:          row.ID.String(),
			ActorUserID: row.ActorUserID.String(),
			ActorName:   row.ActorName,
			EventType:   row.EventType,
			FromValue:   derefString(row.FromValue),
			ToValue:     derefString(row.ToValue),
			CreatedAt:   row.CreatedAt.Time,
		})
	}
	return out, nil
}

// PrazosSummary reads the tenant's prazos KPI counts on the pool (a single aggregated row),
// filtered by tenant_id. The buckets are computed in SQL (thresholds at PrazosSummary).
func (r *pgReadRepository) PrazosSummary(ctx context.Context, tenantID string) (PrazosSummary, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return PrazosSummary{}, err
	}
	row, err := r.q.GetPrazosSummary(ctx, tid)
	if err != nil {
		return PrazosSummary{}, database.WrapInfra(err)
	}
	return PrazosSummary{
		Total:     int(row.Total),
		Criticos:  int(row.Criticos),
		Vencendo:  int(row.Vencendo),
		Abertos:   int(row.Abertos),
		Futuros:   int(row.Futuros),
		Vencidos:  int(row.Vencidos),
		Cumpridos: int(row.Cumpridos),
	}, nil
}

// TasksSummary reads the tenant's tasks KPI counts on the pool (a single aggregated row),
// filtered by tenant_id. The buckets use the same display_status derivation as the read views,
// computed in SQL against CURRENT_DATE.
func (r *pgReadRepository) TasksSummary(ctx context.Context, tenantID string) (TasksSummary, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return TasksSummary{}, err
	}
	row, err := r.q.GetTasksSummary(ctx, tid)
	if err != nil {
		return TasksSummary{}, database.WrapInfra(err)
	}
	return TasksSummary{
		Abertas:    int(row.Abertas),
		EmExecucao: int(row.EmExecucao),
		Concluidas: int(row.Concluidas),
		Atrasadas:  int(row.Atrasadas),
	}, nil
}

// optionalFilterUUID parses an optional assignee filter: "" is no filter (a NULL pgtype.UUID the
// query reads as "any assignee"). A non-empty value is validated at the handler, so reaching
// here with a malformed one is an infra fault.
func optionalFilterUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	return pgUUID(s)
}

// holidaysFromJSON decodes the holidays_applied jsonb (an array of "2006-01-02" strings)
// into the read model's []string. The column is NOT NULL DEFAULT '[]', so it is always
// valid JSON; the slice is initialized so an empty audit serializes as [], never null.
func holidaysFromJSON(raw []byte) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	// A malformed audit blob is an infra fault, not client input; the read still returns
	// the row with an empty audit rather than failing the whole screen over one bad cell.
	_ = json.Unmarshal(raw, &out)
	return out
}

// confirmedBool coerces the interface{} sqlc infers for the (confirmed_by IS NOT NULL)
// expression: pgx scans the boolean into a Go bool, so the assertion holds; a nil/other
// value conservatively reads as false (unconfirmed).
func confirmedBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// keysetDate parses a keyset cursor's date (always present — the handler fills the min
// sentinel for the first page) into a pgtype.Date. A malformed value is an infra fault
// (the cursor is server-issued), wrapped so the edge treats it as 500. Named apart from
// domain.go's parseWireDate, which speaks the event anchor (a terminal KindInvalid).
func keysetDate(s string) (pgtype.Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return pgtype.Date{}, database.WrapInfra(err)
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}

// optionalFilterDate parses an optional filter date: "" is the open bound (a NULL
// pgtype.Date the query reads as "no bound"). A non-empty malformed value is validated
// away at the handler, so reaching here with one is an infra fault.
func optionalFilterDate(s string) (pgtype.Date, error) {
	if s == "" {
		return pgtype.Date{}, nil
	}
	return keysetDate(s)
}
