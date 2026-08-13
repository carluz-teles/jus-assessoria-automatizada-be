package acquisition

import (
	"context"
	"testing"
)

// --- use-case unit tests (recording readRepo) -------------------------------

// recordingReadRepo implements readRepo, capturing the queries the ReadUseCase
// forwards and returning canned rows/totals. Only the processos/intimacoes paths
// are exercised; the rest are no-ops to satisfy the interface.
type recordingReadRepo struct {
	procRows       []ProcessoView
	procTotalCount int64
	procTotal      int64
	lastProcQuery  ProcessosQuery
	procSearchSeen string
	andRows        []AndamentoView
	andTotal       int64
	lastAndQuery   AndamentosQuery
	intiRows       []IntimacaoView
	intiTotal      int64
	lastIntiQuery  IntimacoesByProcessoQuery
}

func (r *recordingReadRepo) ListProcessos(_ context.Context, q ProcessosQuery) ([]ProcessoView, error) {
	r.lastProcQuery = q
	return r.procRows, nil
}

func (r *recordingReadRepo) CountProcessos(_ context.Context, _, search string) (int64, int64, error) {
	r.procSearchSeen = search
	return r.procTotalCount, r.procTotal, nil
}

func (r *recordingReadRepo) ListIntimacoes(context.Context, IntimacoesQuery) ([]IntimacaoView, error) {
	return nil, nil
}
func (r *recordingReadRepo) GetIntimacao(context.Context, string, string) (IntimacaoView, error) {
	return IntimacaoView{}, nil
}
func (r *recordingReadRepo) CountIntimacoes(context.Context, string, string) (int64, int64, error) {
	return 0, 0, nil
}
func (r *recordingReadRepo) ListAndamentosByProcesso(_ context.Context, q AndamentosQuery) ([]AndamentoView, error) {
	r.lastAndQuery = q
	return r.andRows, nil
}
func (r *recordingReadRepo) CountAndamentosByProcesso(context.Context, string, string) (int64, error) {
	return r.andTotal, nil
}
func (r *recordingReadRepo) ListIntimacoesByProcesso(_ context.Context, q IntimacoesByProcessoQuery) ([]IntimacaoView, error) {
	r.lastIntiQuery = q
	return r.intiRows, nil
}
func (r *recordingReadRepo) CountIntimacoesByProcesso(context.Context, string, string) (int64, error) {
	return r.intiTotal, nil
}
func (r *recordingReadRepo) GetImportStatus(context.Context, string) (ImportStatusView, error) {
	return ImportStatusView{}, nil
}
func (r *recordingReadRepo) GetReconciliationTotals(context.Context, string) (ReconciliationTotals, error) {
	return ReconciliationTotals{}, nil
}
func (r *recordingReadRepo) ListReconciliations(context.Context, string, int) ([]ReconciliationView, error) {
	return nil, nil
}
func (r *recordingReadRepo) GetReconciliation(context.Context, string, string) (ReconciliationView, error) {
	return ReconciliationView{}, nil
}
func (r *recordingReadRepo) ListSyncRunsByJob(context.Context, string, string) ([]ReconciliationRunView, error) {
	return nil, nil
}
func (r *recordingReadRepo) ListProcessosBySyncRun(context.Context, string, string) ([]ProcessoLineView, error) {
	return nil, nil
}
func (r *recordingReadRepo) ListIntimacoesBySyncRun(context.Context, string, string) ([]IntimacaoLineView, error) {
	return nil, nil
}

// The use case forwards ?search to the repo and over-fetches one row (limit+1) to
// detect the next page without a COUNT — the extra row is trimmed off the result.
func TestReadUseCase_Processos_ForwardsSearchAndOverfetches(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		procRows: []ProcessoView{{ID: "a"}, {ID: "b"}, {ID: "c"}}, // 3 rows for limit 2 → hasMore
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Processos(context.Background(), ProcessosQuery{
		TenantID: "t-1", Limit: 2, Search: "petrobras",
	})
	if err != nil {
		t.Fatalf("Processos: %v", err)
	}
	if repo.lastProcQuery.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch of limit+1)", repo.lastProcQuery.Limit)
	}
	if repo.lastProcQuery.Search != "petrobras" || repo.procSearchSeen != "petrobras" {
		t.Errorf("search not forwarded: list=%q count=%q", repo.lastProcQuery.Search, repo.procSearchSeen)
	}
	if !res.HasMore {
		t.Error("HasMore = false, want true")
	}
	if len(res.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2 (extra row trimmed)", len(res.Items))
	}
}

// The "X de Y" totals from the repo flow verbatim into the result: TotalCount is the
// filtered context, Total the tenant-wide count.
func TestReadUseCase_Processos_WiresTotals(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{procTotalCount: 32, procTotal: 1247}
	uc := NewReadUseCase(repo)

	res, err := uc.Processos(context.Background(), ProcessosQuery{TenantID: "t-1", Limit: 20, Search: "x"})
	if err != nil {
		t.Fatalf("Processos: %v", err)
	}
	if res.TotalCount != 32 || res.Total != 1247 {
		t.Errorf("totals = (%d, %d), want (32, 1247)", res.TotalCount, res.Total)
	}
}

// Andamentos forwards the process (court_record) and keyset cursor to the repo, over-
// fetches limit+1 to detect the next page, trims the extra row, and wires the total.
func TestReadUseCase_Andamentos_OverfetchesAndWiresTotal(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		andRows:  []AndamentoView{{ID: "a"}, {ID: "b"}, {ID: "c"}}, // 3 rows for limit 2 → hasMore
		andTotal: 87,
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Andamentos(context.Background(), AndamentosQuery{
		TenantID: "t-1", CourtRecordID: "cr-9", Limit: 2,
	})
	if err != nil {
		t.Fatalf("Andamentos: %v", err)
	}
	if repo.lastAndQuery.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch of limit+1)", repo.lastAndQuery.Limit)
	}
	if repo.lastAndQuery.CourtRecordID != "cr-9" {
		t.Errorf("CourtRecordID = %q, want cr-9 (forwarded)", repo.lastAndQuery.CourtRecordID)
	}
	if !res.HasMore {
		t.Error("HasMore = false, want true")
	}
	if len(res.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2 (extra row trimmed)", len(res.Items))
	}
	if res.Total != 87 {
		t.Errorf("Total = %d, want 87", res.Total)
	}
}

// With no over-fetch (fewer rows than the limit) there is no next page and every row
// is returned — the process-with-few (or no) andamentos case.
func TestReadUseCase_Andamentos_NoNextPage(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{andRows: []AndamentoView{{ID: "a"}}}
	uc := NewReadUseCase(repo)

	res, err := uc.Andamentos(context.Background(), AndamentosQuery{TenantID: "t-1", CourtRecordID: "cr-1", Limit: 20})
	if err != nil {
		t.Fatalf("Andamentos: %v", err)
	}
	if res.HasMore {
		t.Error("HasMore = true, want false")
	}
	if len(res.Items) != 1 {
		t.Errorf("len(Items) = %d, want 1", len(res.Items))
	}
}

// IntimacoesByProcesso forwards the process (court_record) and the descending keyset
// cursor's "after" (made_available_at) to the repo, over-fetches limit+1 to detect the
// next page, trims the extra row, and wires the total.
func TestReadUseCase_IntimacoesByProcesso_OverfetchesAndWiresTotal(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		intiRows:  []IntimacaoView{{ID: "a"}, {ID: "b"}, {ID: "c"}}, // 3 rows for limit 2 → hasMore
		intiTotal: 41,
	}
	uc := NewReadUseCase(repo)

	res, err := uc.IntimacoesByProcesso(context.Background(), IntimacoesByProcessoQuery{
		TenantID: "t-1", CourtRecordID: "cr-9", LastMadeAvailable: "2024-03-01", Limit: 2,
	})
	if err != nil {
		t.Fatalf("IntimacoesByProcesso: %v", err)
	}
	if repo.lastIntiQuery.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch of limit+1)", repo.lastIntiQuery.Limit)
	}
	if repo.lastIntiQuery.CourtRecordID != "cr-9" {
		t.Errorf("CourtRecordID = %q, want cr-9 (forwarded)", repo.lastIntiQuery.CourtRecordID)
	}
	if repo.lastIntiQuery.LastMadeAvailable != "2024-03-01" {
		t.Errorf("LastMadeAvailable = %q, want 2024-03-01 (cursor after forwarded)", repo.lastIntiQuery.LastMadeAvailable)
	}
	if !res.HasMore {
		t.Error("HasMore = false, want true")
	}
	if len(res.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2 (extra row trimmed)", len(res.Items))
	}
	if res.Total != 41 {
		t.Errorf("Total = %d, want 41", res.Total)
	}
}

// With no over-fetch (fewer rows than the limit) there is no next page and every row is
// returned — the process-with-few (or no) intimations case.
func TestReadUseCase_IntimacoesByProcesso_NoNextPage(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{intiRows: []IntimacaoView{{ID: "a"}}}
	uc := NewReadUseCase(repo)

	res, err := uc.IntimacoesByProcesso(context.Background(), IntimacoesByProcessoQuery{TenantID: "t-1", CourtRecordID: "cr-1", Limit: 20})
	if err != nil {
		t.Fatalf("IntimacoesByProcesso: %v", err)
	}
	if res.HasMore {
		t.Error("HasMore = true, want false")
	}
	if len(res.Items) != 1 {
		t.Errorf("len(Items) = %d, want 1", len(res.Items))
	}
}
