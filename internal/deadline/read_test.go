package deadline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// --- read use-case unit tests (recording readRepo) --------------------------

// recordingReadRepo implements readRepo, capturing the queries the ReadUseCase
// forwards and returning canned rows/totals. Detail lookups echo a fixed view or a
// canned error so the passthrough is observable.
type recordingReadRepo struct {
	byProcRows      []PrazoView
	byProcTotal     int64
	lastByProcQuery PrazosByProcessoQuery

	agendaRows       []AgendaPrazoView
	agendaTotalCount int64
	agendaTotal      int64
	lastAgendaQuery  PrazosQuery
	lastAgendaCountQ PrazosQuery

	byIntimacaoRows []AgendaPrazoView
	lastByIntimTID  string
	lastByIntimID   string

	detailView PrazoDetailView
	detailErr  error
	lastGetTID string
	lastGetID  string

	suggestContext PrazoSuggestContext
	suggestErr     error

	tasksByProcRows  []TaskView
	tasksByProcTotal int64
	lastTasksByProcQ TasksByProcessoQuery
	tasksAgendaRows  []TaskView
	tasksTotalCount  int64
	tasksTotal       int64
	lastTasksQuery   TasksQuery
	lastTasksCountQ  TasksQuery

	taskDetailView TaskDetailView
	taskDetailErr  error
	lastDetailTID  string
	lastDetailID   string
	taskItems      []TaskItemView
	itemsErr       error
	taskProgress   TaskProgress
	progressErr    error
	prazosSummary  PrazosSummary
	tasksSummary   TasksSummary
	lastSummaryTID string
	// Filter-option reads — canned distinct values the use case turns into chips.
	prazoKinds    []string
	prazoCourts   []string
	taskAssignees []AssigneeOption
}

func (r *recordingReadRepo) ListPrazosByProcesso(_ context.Context, q PrazosByProcessoQuery) ([]PrazoView, error) {
	r.lastByProcQuery = q
	return r.byProcRows, nil
}

func (r *recordingReadRepo) CountPrazosByProcesso(context.Context, string, string) (int64, error) {
	return r.byProcTotal, nil
}

func (r *recordingReadRepo) ListPrazos(_ context.Context, q PrazosQuery) ([]AgendaPrazoView, error) {
	r.lastAgendaQuery = q
	return r.agendaRows, nil
}

func (r *recordingReadRepo) ListPrazosByIntimacao(_ context.Context, tenantID, intimationID string) ([]AgendaPrazoView, error) {
	r.lastByIntimTID, r.lastByIntimID = tenantID, intimationID
	return r.byIntimacaoRows, nil
}

func (r *recordingReadRepo) CountPrazos(_ context.Context, q PrazosQuery) (int64, int64, error) {
	r.lastAgendaCountQ = q
	return r.agendaTotalCount, r.agendaTotal, nil
}

func (r *recordingReadRepo) GetPrazo(_ context.Context, tenantID, id string) (PrazoDetailView, error) {
	r.lastGetTID, r.lastGetID = tenantID, id
	return r.detailView, r.detailErr
}

func (r *recordingReadRepo) GetPrazoSuggestContext(_ context.Context, tenantID, id string) (PrazoSuggestContext, error) {
	r.lastGetTID, r.lastGetID = tenantID, id
	return r.suggestContext, r.suggestErr
}

func (r *recordingReadRepo) ListTasksByProcesso(_ context.Context, q TasksByProcessoQuery) ([]TaskView, error) {
	r.lastTasksByProcQ = q
	return r.tasksByProcRows, nil
}

func (r *recordingReadRepo) CountTasksByProcesso(context.Context, string, string) (int64, error) {
	return r.tasksByProcTotal, nil
}

func (r *recordingReadRepo) ListTasks(_ context.Context, q TasksQuery) ([]TaskView, error) {
	r.lastTasksQuery = q
	return r.tasksAgendaRows, nil
}

func (r *recordingReadRepo) CountTasks(_ context.Context, q TasksQuery) (int64, int64, error) {
	r.lastTasksCountQ = q
	return r.tasksTotalCount, r.tasksTotal, nil
}

func (r *recordingReadRepo) GetTaskDetail(_ context.Context, tenantID, id string) (TaskDetailView, error) {
	r.lastDetailTID, r.lastDetailID = tenantID, id
	return r.taskDetailView, r.taskDetailErr
}

func (r *recordingReadRepo) ListTaskItems(_ context.Context, _, _ string) ([]TaskItemView, error) {
	return r.taskItems, r.itemsErr
}

func (r *recordingReadRepo) TaskItemProgress(_ context.Context, _, _ string) (TaskProgress, error) {
	return r.taskProgress, r.progressErr
}

func (r *recordingReadRepo) PrazosSummary(_ context.Context, tenantID string) (PrazosSummary, error) {
	r.lastSummaryTID = tenantID
	return r.prazosSummary, nil
}

func (r *recordingReadRepo) TasksSummary(_ context.Context, tenantID string) (TasksSummary, error) {
	r.lastSummaryTID = tenantID
	return r.tasksSummary, nil
}

func (r *recordingReadRepo) ListPrazoKinds(context.Context, string) ([]string, error) {
	return r.prazoKinds, nil
}

func (r *recordingReadRepo) ListPrazoCourts(context.Context, string) ([]string, error) {
	return r.prazoCourts, nil
}

func (r *recordingReadRepo) ListTaskAssignees(context.Context, string) ([]AssigneeOption, error) {
	return r.taskAssignees, nil
}

// The prazos agenda envelope's filter options come from the distinct-value reads: kind
// and court are label==value options (the free-text filters), each key omitted when the
// read yields nothing.
func TestReadUseCase_Prazos_AssemblesFilters(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		prazoKinds:  []string{"Aguardando resposta", "Manifestação"},
		prazoCourts: []string{"TJSP"},
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Prazos(context.Background(), PrazosQuery{TenantID: "t-1", Limit: 20})
	if err != nil {
		t.Fatalf("Prazos: %v", err)
	}

	kinds := res.Filters["kind"]
	if len(kinds) != 2 || kinds[0].Label != "Aguardando resposta" || kinds[0].Value != "Aguardando resposta" {
		t.Errorf("kind options = %+v, want label==value", kinds)
	}
	courts := res.Filters["court"]
	if len(courts) != 1 || courts[0].Label != "TJSP" || courts[0].Value != "TJSP" {
		t.Errorf("court options = %+v, want label==value TJSP", courts)
	}
	if _, ok := res.Filters["status"]; ok {
		t.Error("status key present — the agenda does not render a status chip row")
	}
}

// The task agenda envelope's filter options: source is the closed enum set (canonical
// order) and assignee is label==name/value==id. An assignee without an id is never
// selectable; a key with no options is omitted.
func TestReadUseCase_Tasks_AssemblesFilters(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		taskAssignees: []AssigneeOption{{Name: "Ana", ID: "u-1"}, {Name: "Sem Id", ID: ""}},
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Tasks(context.Background(), TasksQuery{TenantID: "t-1", Limit: 20})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}

	source := res.Filters["source"]
	wantSource := []string{string(SourceAI), string(SourceRule), string(SourceManual)}
	if len(source) != len(wantSource) {
		t.Fatalf("source options = %+v, want %v", source, wantSource)
	}
	for i, want := range wantSource {
		if source[i].Label != want || source[i].Value != want {
			t.Errorf("source[%d] = %+v, want label==value %q", i, source[i], want)
		}
	}
	assignees := res.Filters["assignee"]
	if len(assignees) != 1 || assignees[0].Label != "Ana" || assignees[0].Value != "u-1" {
		t.Errorf("assignee options = %+v, want only the id-bearing Ana", assignees)
	}
}

// The agenda queries are "filtered" once any filter is set — the counter then needs the
// filtered COUNT.
func TestAgendaQueries_Filtered(t *testing.T) {
	t.Parallel()

	for name, q := range map[string]interface{ Filtered() bool }{
		"prazos none":    PrazosQuery{},
		"prazos kind":    PrazosQuery{Kind: "Aguardando"},
		"prazos court":   PrazosQuery{Court: "TJSP"},
		"tasks none":     TasksQuery{},
		"tasks source":   TasksQuery{Source: "AI"},
		"tasks assignee": TasksQuery{Assignee: "u-1"},
		"tasks window":   TasksQuery{From: "2025-01-01"},
		"prazos window":  PrazosQuery{To: "2025-12-31"},
		"prazos status":  PrazosQuery{Status: "OPEN"},
		"tasks status":   TasksQuery{Status: "OPEN"},
	} {
		name, q := name, q
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A query struct with a filter must report Filtered; the all-empty zero
			// value must not. Both directions are asserted explicitly so a wrong
			// implementation fails loudly instead of passing the "return" skip.
			got := q.Filtered()
			want := !strings.Contains(name, "none")
			if got != want {
				t.Errorf("%s: Filtered() = %v, want %v", name, got, want)
			}
		})
	}
}

// PrazosByProcesso forwards the process (court_record) and keyset cursor to the repo,
// over-fetches limit+1 to detect the next page, trims the extra row, and wires the total.
func TestReadUseCase_PrazosByProcesso_OverfetchesAndWiresTotal(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		byProcRows:  []PrazoView{{ID: "a"}, {ID: "b"}, {ID: "c"}}, // 3 rows for limit 2 → hasMore
		byProcTotal: 12,
	}
	uc := NewReadUseCase(repo)

	res, err := uc.PrazosByProcesso(context.Background(), PrazosByProcessoQuery{
		TenantID: "t-1", CourtRecordID: "cr-9", Limit: 2,
	})
	if err != nil {
		t.Fatalf("PrazosByProcesso: %v", err)
	}
	if repo.lastByProcQuery.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch of limit+1)", repo.lastByProcQuery.Limit)
	}
	if repo.lastByProcQuery.CourtRecordID != "cr-9" {
		t.Errorf("CourtRecordID = %q, want cr-9 (forwarded)", repo.lastByProcQuery.CourtRecordID)
	}
	if !res.HasMore {
		t.Error("HasMore = false, want true")
	}
	if len(res.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2 (extra row trimmed)", len(res.Items))
	}
	if res.Total != 12 {
		t.Errorf("Total = %d, want 12", res.Total)
	}
}

// With fewer rows than the limit there is no next page and every row is returned — the
// process-with-few (or no) prazos case.
func TestReadUseCase_PrazosByProcesso_NoNextPage(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{byProcRows: []PrazoView{{ID: "a"}}}
	uc := NewReadUseCase(repo)

	res, err := uc.PrazosByProcesso(context.Background(), PrazosByProcessoQuery{TenantID: "t-1", CourtRecordID: "cr-1", Limit: 20})
	if err != nil {
		t.Fatalf("PrazosByProcesso: %v", err)
	}
	if res.HasMore {
		t.Error("HasMore = true, want false")
	}
	if len(res.Items) != 1 {
		t.Errorf("len(Items) = %d, want 1", len(res.Items))
	}
}

// Prazos forwards the status/window filters to both the list and the count, over-fetches
// limit+1, trims, and wires the "X de Y" totals verbatim.
func TestReadUseCase_Prazos_ForwardsFiltersAndWiresTotals(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		agendaRows:       []AgendaPrazoView{{ID: "a"}, {ID: "b"}, {ID: "c"}}, // 3 for limit 2 → hasMore
		agendaTotalCount: 7,
		agendaTotal:      41,
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Prazos(context.Background(), PrazosQuery{
		TenantID: "t-1", Status: "PENDING", From: "2024-03-01", To: "2024-03-31", Limit: 2,
	})
	if err != nil {
		t.Fatalf("Prazos: %v", err)
	}
	if repo.lastAgendaQuery.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch of limit+1)", repo.lastAgendaQuery.Limit)
	}
	if repo.lastAgendaQuery.Status != "PENDING" || repo.lastAgendaQuery.From != "2024-03-01" || repo.lastAgendaQuery.To != "2024-03-31" {
		t.Errorf("filters not forwarded to list: %+v", repo.lastAgendaQuery)
	}
	if repo.lastAgendaCountQ.Status != "PENDING" {
		t.Errorf("filters not forwarded to count: %+v", repo.lastAgendaCountQ)
	}
	if !res.HasMore {
		t.Error("HasMore = false, want true")
	}
	if len(res.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2 (extra row trimmed)", len(res.Items))
	}
	if res.TotalCount != 7 || res.Total != 41 {
		t.Errorf("totals = (%d, %d), want (7, 41)", res.TotalCount, res.Total)
	}
}

// PrazosByIntimacao forwards the tenant + intimação id to the repo and wraps the single
// derived prazo in the agenda result shape: HasMore false and both totals = the item
// count (the 1:1 lookup is one row, no pagination).
func TestReadUseCase_PrazosByIntimacao_WrapsSingleRow(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{byIntimacaoRows: []AgendaPrazoView{{ID: "d-1", IntimationID: "i-9"}}}
	uc := NewReadUseCase(repo)

	res, err := uc.PrazosByIntimacao(context.Background(), "t-1", "i-9")
	if err != nil {
		t.Fatalf("PrazosByIntimacao: %v", err)
	}
	if repo.lastByIntimTID != "t-1" || repo.lastByIntimID != "i-9" {
		t.Errorf("forwarded (tenant, intimation) = (%q, %q), want (t-1, i-9)", repo.lastByIntimTID, repo.lastByIntimID)
	}
	if res.HasMore {
		t.Error("HasMore = true, want false (1:1 lookup, never paginated)")
	}
	if len(res.Items) != 1 || res.TotalCount != 1 || res.Total != 1 {
		t.Errorf("result = items %d / totals %d,%d, want 1/1,1", len(res.Items), res.TotalCount, res.Total)
	}
}

// An intimação with no derived prazo yields an empty page with zero totals — not an error
// (the F2 screen renders "sem prazo" from an empty data array).
func TestReadUseCase_PrazosByIntimacao_NoPrazo_EmptyResult(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{byIntimacaoRows: nil}
	uc := NewReadUseCase(repo)

	res, err := uc.PrazosByIntimacao(context.Background(), "t-1", "i-empty")
	if err != nil {
		t.Fatalf("PrazosByIntimacao: %v", err)
	}
	if len(res.Items) != 0 || res.TotalCount != 0 || res.Total != 0 || res.HasMore {
		t.Errorf("result = items %d / totals %d,%d / hasMore %v, want 0/0,0/false",
			len(res.Items), res.TotalCount, res.Total, res.HasMore)
	}
}

// Prazo passes the tenant + id straight to the repo and returns its view; a repo
// not-found flows back verbatim (the edge maps it to 404).
func TestReadUseCase_Prazo_Passthrough(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{detailView: PrazoDetailView{ID: "d-1", Status: "PENDING"}}
	uc := NewReadUseCase(repo)

	view, err := uc.Prazo(context.Background(), "t-1", "d-1")
	if err != nil {
		t.Fatalf("Prazo: %v", err)
	}
	if repo.lastGetTID != "t-1" || repo.lastGetID != "d-1" {
		t.Errorf("forwarded (tenant, id) = (%q, %q), want (t-1, d-1)", repo.lastGetTID, repo.lastGetID)
	}
	if view.ID != "d-1" {
		t.Errorf("view.ID = %q, want d-1", view.ID)
	}

	repo.detailErr = ErrDeadlineNotFound
	if _, err := uc.Prazo(context.Background(), "t-1", "missing"); !errors.Is(err, ErrDeadlineNotFound) {
		t.Errorf("err = %v, want ErrDeadlineNotFound", err)
	}
}

// TasksByProcesso forwards the process (court_record) + keyset, over-fetches limit+1, trims the
// extra row, and wires the total — the task-tab mirror of PrazosByProcesso.
func TestReadUseCase_TasksByProcesso_OverfetchesAndWiresTotal(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		tasksByProcRows:  []TaskView{{ID: "a"}, {ID: "b"}, {ID: "c"}}, // 3 for limit 2 → hasMore
		tasksByProcTotal: 9,
	}
	uc := NewReadUseCase(repo)

	res, err := uc.TasksByProcesso(context.Background(), TasksByProcessoQuery{
		TenantID: "t-1", CourtRecordID: "cr-9", Limit: 2,
	})
	if err != nil {
		t.Fatalf("TasksByProcesso: %v", err)
	}
	if repo.lastTasksByProcQ.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch of limit+1)", repo.lastTasksByProcQ.Limit)
	}
	if repo.lastTasksByProcQ.CourtRecordID != "cr-9" {
		t.Errorf("CourtRecordID = %q, want cr-9 (forwarded)", repo.lastTasksByProcQ.CourtRecordID)
	}
	if !res.HasMore || len(res.Items) != 2 || res.Total != 9 {
		t.Errorf("result = hasMore %v / items %d / total %d, want true/2/9", res.HasMore, len(res.Items), res.Total)
	}
}

// Tasks forwards the status/assignee/window filters to both the list and the count, over-fetches
// limit+1, trims, and wires the "X de Y" totals verbatim.
func TestReadUseCase_Tasks_ForwardsFiltersAndWiresTotals(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		tasksAgendaRows: []TaskView{{ID: "a"}, {ID: "b"}, {ID: "c"}}, // 3 for limit 2 → hasMore
		tasksTotalCount: 4,
		tasksTotal:      30,
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Tasks(context.Background(), TasksQuery{
		TenantID: "t-1", Status: "OPEN", Assignee: "u-7", From: "2024-03-01", To: "2024-03-31", Limit: 2,
	})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if repo.lastTasksQuery.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch of limit+1)", repo.lastTasksQuery.Limit)
	}
	if repo.lastTasksQuery.Status != "OPEN" || repo.lastTasksQuery.Assignee != "u-7" ||
		repo.lastTasksQuery.From != "2024-03-01" || repo.lastTasksQuery.To != "2024-03-31" {
		t.Errorf("filters not forwarded to list: %+v", repo.lastTasksQuery)
	}
	if repo.lastTasksCountQ.Status != "OPEN" || repo.lastTasksCountQ.Assignee != "u-7" {
		t.Errorf("filters not forwarded to count: %+v", repo.lastTasksCountQ)
	}
	if !res.HasMore || len(res.Items) != 2 || res.TotalCount != 4 || res.Total != 30 {
		t.Errorf("result = hasMore %v / items %d / totals %d,%d, want true/2/4,30",
			res.HasMore, len(res.Items), res.TotalCount, res.Total)
	}
}

// --- TaskDetail + display_status + summaries --------------------------------

// TestReadUseCase_TaskDetail_AssemblesItemsProgressAndDisplayStatus proves the detail read model
// folds the task's fields, its ordered checklist and the {done,total} progress together, and derives
// display_status from (status, progress, due_date) against the pinned clock.
func TestReadUseCase_TaskDetail_AssemblesItemsProgressAndDisplayStatus(t *testing.T) {
	t.Parallel()

	today := time.Date(2024, 3, 20, 12, 0, 0, 0, time.UTC)
	due := today.AddDate(0, 0, 5) // future → not overdue
	repo := &recordingReadRepo{
		taskDetailView: TaskDetailView{ID: "t-1", Title: "Contestar", Status: "OPEN", DueDate: &due},
		taskItems: []TaskItemView{
			{ID: "i-1", Title: "Ler", Position: 0, Done: true},
			{ID: "i-2", Title: "Redigir", Position: 1, Done: false},
		},
		taskProgress: TaskProgress{Done: 1, Total: 2},
	}
	uc := NewReadUseCase(repo, WithReadClock(func() time.Time { return today }))

	view, err := uc.TaskDetail(context.Background(), "tenant-9", "t-1")
	if err != nil {
		t.Fatalf("TaskDetail: %v", err)
	}
	if repo.lastDetailTID != "tenant-9" || repo.lastDetailID != "t-1" {
		t.Errorf("forwarded (tenant,id) = (%q,%q), want (tenant-9,t-1)", repo.lastDetailTID, repo.lastDetailID)
	}
	if len(view.Items) != 2 || view.Items[0].ID != "i-1" {
		t.Errorf("items = %+v, want the 2 checklist rows ordered", view.Items)
	}
	if view.Progress.Done != 1 || view.Progress.Total != 2 {
		t.Errorf("progress = %+v, want {1,2}", view.Progress)
	}
	// OPEN, not overdue, one item done → Em execução.
	if view.DisplayStatus != string(DisplayEmExecucao) {
		t.Errorf("display_status = %q, want %q", view.DisplayStatus, DisplayEmExecucao)
	}
}

// TestReadUseCase_TaskDetail_ItemlessTaskEmptySlice proves an itemless task resolves with an empty
// (non-nil) checklist, {0,0} progress and an Aberta display_status.
func TestReadUseCase_TaskDetail_ItemlessTaskEmptySlice(t *testing.T) {
	t.Parallel()

	today := time.Date(2024, 3, 20, 12, 0, 0, 0, time.UTC)
	repo := &recordingReadRepo{
		taskDetailView: TaskDetailView{ID: "t-1", Status: "OPEN"},
		taskItems:      nil,
		taskProgress:   TaskProgress{},
	}
	uc := NewReadUseCase(repo, WithReadClock(func() time.Time { return today }))

	view, err := uc.TaskDetail(context.Background(), "tenant-9", "t-1")
	if err != nil {
		t.Fatalf("TaskDetail: %v", err)
	}
	if view.Items == nil || len(view.Items) != 0 {
		t.Errorf("items = %v, want an empty non-nil slice", view.Items)
	}
	if view.DisplayStatus != string(DisplayAberta) {
		t.Errorf("display_status = %q, want %q", view.DisplayStatus, DisplayAberta)
	}
}

// TestReadUseCase_TaskDetail_NotFound proves a miss on the task row is the repo's typed
// ErrTaskNotFound (→ 404): the items/progress reads never run.
func TestReadUseCase_TaskDetail_NotFound(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{taskDetailErr: ErrTaskNotFound}
	uc := NewReadUseCase(repo)

	_, err := uc.TaskDetail(context.Background(), "tenant-9", "missing")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

// TestReadUseCase_Tasks_DecoratesDisplayStatus proves the LIST read decorates each row's
// display_status from its status + done-item count + due_date (the additive field the new tabs read).
func TestReadUseCase_Tasks_DecoratesDisplayStatus(t *testing.T) {
	t.Parallel()

	today := time.Date(2024, 3, 20, 12, 0, 0, 0, time.UTC)
	overdue := today.AddDate(0, 0, -2)
	future := today.AddDate(0, 0, 2)

	rows := []TaskView{
		{ID: "done", Status: "DONE"},
		{ID: "atrasada", Status: "OPEN", DueDate: &overdue},
		{ID: "exec", Status: "OPEN", DueDate: &future, doneItems: 2},
		{ID: "aberta", Status: "OPEN", DueDate: &future, doneItems: 0},
	}
	repo := &recordingReadRepo{tasksAgendaRows: rows, tasksTotalCount: 4, tasksTotal: 4}
	uc := NewReadUseCase(repo, WithReadClock(func() time.Time { return today }))

	res, err := uc.Tasks(context.Background(), TasksQuery{TenantID: "t-1", Limit: 10})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	want := map[string]string{
		"done":     string(DisplayConcluida),
		"atrasada": string(DisplayAtrasada),
		"exec":     string(DisplayEmExecucao),
		"aberta":   string(DisplayAberta),
	}
	for _, row := range res.Items {
		if row.DisplayStatus != want[row.ID] {
			t.Errorf("row %q display_status = %q, want %q", row.ID, row.DisplayStatus, want[row.ID])
		}
	}
}

// TestReadUseCase_Summaries_PassThroughTenant proves both summaries forward the tenant and return
// the repo's aggregated counts unchanged (single-object read models).
func TestReadUseCase_Summaries_PassThroughTenant(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		prazosSummary: PrazosSummary{Total: 10, Criticos: 2, Vencendo: 3, Abertos: 6, Futuros: 1, Vencidos: 2, Cumpridos: 2},
		tasksSummary:  TasksSummary{Abertas: 4, EmExecucao: 3, Concluidas: 5, Atrasadas: 1},
	}
	uc := NewReadUseCase(repo)

	ps, err := uc.PrazosSummary(context.Background(), "tenant-9")
	if err != nil {
		t.Fatalf("PrazosSummary: %v", err)
	}
	if repo.lastSummaryTID != "tenant-9" || ps.Total != 10 || ps.Criticos != 2 || ps.Cumpridos != 2 {
		t.Errorf("prazos summary = %+v (tid %q), want the canned counts for tenant-9", ps, repo.lastSummaryTID)
	}

	ts, err := uc.TasksSummary(context.Background(), "tenant-9")
	if err != nil {
		t.Fatalf("TasksSummary: %v", err)
	}
	if ts.Abertas != 4 || ts.EmExecucao != 3 || ts.Concluidas != 5 || ts.Atrasadas != 1 {
		t.Errorf("tasks summary = %+v, want the canned counts", ts)
	}
}
