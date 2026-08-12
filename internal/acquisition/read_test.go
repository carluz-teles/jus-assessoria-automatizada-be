package acquisition

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jusassessoria/platform/lib/httpx"
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

// --- handler HTTP tests (recording reader) ----------------------------------

// recordingReader implements the handler's reader port, capturing the ProcessosQuery
// and returning a canned result — lets the HTTP tests assert query-param wiring and
// the envelope shape without a database.
type recordingReader struct {
	res      ProcessosResult
	gotQuery ProcessosQuery
}

func (r *recordingReader) Processos(_ context.Context, q ProcessosQuery) (ProcessosResult, error) {
	r.gotQuery = q
	return r.res, nil
}
func (r *recordingReader) Intimacoes(context.Context, IntimacoesQuery) (IntimacoesResult, error) {
	return IntimacoesResult{}, nil
}
func (r *recordingReader) ImportStatus(context.Context, string) (ImportStatusView, error) {
	return ImportStatusView{}, nil
}
func (r *recordingReader) Reconciliations(context.Context, string) (ReconciliationsView, error) {
	return ReconciliationsView{}, nil
}
func (r *recordingReader) ReconciliationDetail(context.Context, string, string) (ReconciliationDetailView, error) {
	return ReconciliationDetailView{}, nil
}
func (r *recordingReader) SyncRunItems(context.Context, string, string) (SyncRunItemsView, error) {
	return SyncRunItemsView{}, nil
}

// GET /v1/processos forwards ?search and the decoded ?cursor to the read port, and the
// tenant comes from the principal (never the query).
func TestHandler_ListProcessos_ForwardsSearchAndCursor(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	cursor := httpx.EncodeCursor(httpx.Cursor{
		LastSortValue: "0000123-45.2023.8.26.0001",
		LastID:        "018f0000-0000-7000-8000-000000000abc",
	})
	status, _ := do(t, app, http.MethodGet,
		"/v1/processos?search=petrobras&limit=25&cursor="+cursor, "", "jwt")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotQuery.Search != "petrobras" {
		t.Errorf("Search = %q, want petrobras", rd.gotQuery.Search)
	}
	if rd.gotQuery.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotQuery.TenantID)
	}
	if rd.gotQuery.LastCNJ != "0000123-45.2023.8.26.0001" {
		t.Errorf("LastCNJ = %q, want the decoded cursor sort value", rd.gotQuery.LastCNJ)
	}
	if rd.gotQuery.Limit != 25 {
		t.Errorf("Limit = %d, want 25", rd.gotQuery.Limit)
	}
}

// ?limit above the ceiling is clamped to MaxLimit (100) — the handler never asks the
// repo for an unbounded page.
func TestHandler_ListProcessos_ClampsLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos?limit=500", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotQuery.Limit != 100 {
		t.Errorf("Limit = %d, want 100 (clamped)", rd.gotQuery.Limit)
	}
}

// A malformed ?cursor is a client error → 400, not a 500.
func TestHandler_ListProcessos_BadCursor_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/processos?cursor=not-a-cursor", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
}

// The response envelope carries the "X de Y" totals from the read result.
func TestHandler_ListProcessos_EnvelopeHasTotals(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{res: ProcessosResult{
		Items:      []ProcessoView{{ID: "a", CNJNumber: "0001"}},
		TotalCount: 32,
		Total:      1247,
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos?search=petro", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"total_count":32`, `"total":1247`} {
		if !strings.Contains(body, want) {
			t.Errorf("envelope missing %s\ngot: %s", want, body)
		}
	}
}
