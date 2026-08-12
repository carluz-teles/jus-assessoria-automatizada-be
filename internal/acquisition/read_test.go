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
func (r *recordingReadRepo) CountIntimacoes(context.Context, string, string) (int64, int64, error) {
	return 0, 0, nil
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
