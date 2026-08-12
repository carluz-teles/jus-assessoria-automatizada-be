package deadline

import (
	"context"
	"errors"
	"testing"
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

	detailView PrazoDetailView
	detailErr  error
	lastGetTID string
	lastGetID  string
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

func (r *recordingReadRepo) CountPrazos(_ context.Context, q PrazosQuery) (int64, int64, error) {
	r.lastAgendaCountQ = q
	return r.agendaTotalCount, r.agendaTotal, nil
}

func (r *recordingReadRepo) GetPrazo(_ context.Context, tenantID, id string) (PrazoDetailView, error) {
	r.lastGetTID, r.lastGetID = tenantID, id
	return r.detailView, r.detailErr
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
