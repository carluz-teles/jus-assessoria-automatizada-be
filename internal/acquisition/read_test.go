package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// --- use-case unit tests (recording readRepo) -------------------------------

// pgDate builds a pgtype.Date for test assertions (year/month/day in UTC).
func pgDate(year int, month time.Month, day int) pgtype.Date {
	return pgtype.Date{Time: time.Date(year, month, day, 0, 0, 0, 0, time.UTC), Valid: true}
}

// pgDateInvalid returns an invalid pgtype.Date, simulating a LEFT JOIN miss.
func pgDateInvalid() pgtype.Date {
	return pgtype.Date{}
}

// recordingReadRepo implements readRepo, capturing the queries the ReadUseCase
// forwards and returning canned rows/totals. Only the processos/intimacoes paths
// are exercised; the rest are no-ops to satisfy the interface.
type recordingReadRepo struct {
	procRows       []ProcessoView
	procTotalCount int64
	procTotal      int64
	lastProcQuery  ProcessosQuery
	procSearchSeen string
	// Filter-option reads — canned distinct values the use case turns into chips.
	procCourts    []string
	procDegrees   []string
	procAssignees []AssigneeOption
	intiCourts    []string
	// GetProcesso deep-link — capture the forwarded (tenant, id) and return a canned view.
	procOneRes    ProcessoView
	gotProcOneTID string
	gotProcOneID  string
	andRows       []AndamentoView
	andTotal      int64
	lastAndQuery  AndamentosQuery
	intiRows      []IntimacaoView
	intiTotal     int64
	lastIntiQuery IntimacoesByProcessoQuery
	// Partes deep-read — capture the forwarded (tenant, court_record) and return canned rows.
	partesRows    []PartyRow
	gotPartesTID  string
	gotPartesCRID string
	// Summary reads — canned views the ReadUseCase forwards verbatim.
	procSummary ProcessosSummaryView
	intiSummary IntimacoesSummaryView
	// Captures reads — canned rows/summary/detail + the derivation counts, and the
	// captured (tenant, id) forwarded to the repo.
	captureRows      []CaptureRunRow
	captureSummary   CaptureSummaryRow
	captureOne       CaptureRunRow
	captureOneErr    error
	deadlinesBetween int
	tasksBetween     int
	deadlinesToday   int
	oabCount         int
	gotCapturesTID   string
	gotCaptureTID    string
	gotCaptureID     string
}

func (r *recordingReadRepo) ListProcessos(_ context.Context, q ProcessosQuery) ([]ProcessoView, error) {
	r.lastProcQuery = q
	return r.procRows, nil
}

func (r *recordingReadRepo) GetProcesso(_ context.Context, tenantID, id string) (ProcessoView, error) {
	r.gotProcOneTID, r.gotProcOneID = tenantID, id
	return r.procOneRes, nil
}

func (r *recordingReadRepo) CountProcessos(_ context.Context, q ProcessosQuery) (int64, int64, error) {
	r.lastProcQuery = q
	r.procSearchSeen = q.Search
	return r.procTotalCount, r.procTotal, nil
}

func (r *recordingReadRepo) ListIntimacoes(context.Context, IntimacoesQuery) ([]IntimacaoView, error) {
	return nil, nil
}
func (r *recordingReadRepo) GetIntimacao(context.Context, string, string) (IntimacaoDetailView, error) {
	return IntimacaoDetailView{}, nil
}
func (r *recordingReadRepo) CountIntimacoes(context.Context, IntimacoesQuery) (int64, int64, error) {
	return 0, 0, nil
}
func (r *recordingReadRepo) BucketIntimacoes(context.Context, IntimacoesQuery) (IntimacaoBucketsView, error) {
	return IntimacaoBucketsView{}, nil
}
func (r *recordingReadRepo) ListProcessoCourts(context.Context, string) ([]string, error) {
	return r.procCourts, nil
}
func (r *recordingReadRepo) ListProcessoDegrees(context.Context, string) ([]string, error) {
	return r.procDegrees, nil
}
func (r *recordingReadRepo) ListProcessoAssignees(context.Context, string) ([]AssigneeOption, error) {
	return r.procAssignees, nil
}
func (r *recordingReadRepo) ListIntimacaoCourts(context.Context, string) ([]string, error) {
	return r.intiCourts, nil
}
func (r *recordingReadRepo) SummarizeProcessos(context.Context, string) (ProcessosSummaryView, error) {
	return r.procSummary, nil
}
func (r *recordingReadRepo) SummarizeIntimacoes(context.Context, string) (IntimacoesSummaryView, error) {
	return r.intiSummary, nil
}
func (r *recordingReadRepo) ListAndamentosByProcesso(_ context.Context, q AndamentosQuery) ([]AndamentoView, error) {
	r.lastAndQuery = q
	return r.andRows, nil
}
func (r *recordingReadRepo) ListPartesByProcesso(_ context.Context, tenantID, courtRecordID string) ([]PartyRow, error) {
	r.gotPartesTID, r.gotPartesCRID = tenantID, courtRecordID
	return r.partesRows, nil
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
func (r *recordingReadRepo) GetResumoContext(context.Context, string, string) (ProcessoResumoCtx, error) {
	return ProcessoResumoCtx{}, nil
}

func (r *recordingReadRepo) GetIntimacaoAnaliseContext(context.Context, string, string) (IntimacaoAnaliseCtx, error) {
	return IntimacaoAnaliseCtx{}, nil
}

// Captures reads — canned values the Captures/CaptureDetail use case folds together.
// Count* return per-field canned counts; the capture rows/summary/detail are canned.
func (r *recordingReadRepo) ListCaptureRuns(_ context.Context, tenantID string, _ int) ([]CaptureRunRow, error) {
	r.gotCapturesTID = tenantID
	return r.captureRows, nil
}
func (r *recordingReadRepo) GetCaptureRun(_ context.Context, tenantID, id string) (CaptureRunRow, error) {
	r.gotCaptureTID, r.gotCaptureID = tenantID, id
	return r.captureOne, r.captureOneErr
}
func (r *recordingReadRepo) GetCaptureSummary(context.Context, string) (CaptureSummaryRow, error) {
	return r.captureSummary, nil
}
func (r *recordingReadRepo) CountDeadlinesCreatedBetween(context.Context, string, time.Time, time.Time) (int, error) {
	return r.deadlinesBetween, nil
}
func (r *recordingReadRepo) CountTasksCreatedBetween(context.Context, string, time.Time, time.Time) (int, error) {
	return r.tasksBetween, nil
}
func (r *recordingReadRepo) CountDeadlinesCreatedToday(context.Context, string) (int, error) {
	return r.deadlinesToday, nil
}
func (r *recordingReadRepo) CountWatchedOABsForDJEN(context.Context, string) (int, error) {
	return r.oabCount, nil
}
func (r *recordingReadRepo) ListWatchedOABsWithName(context.Context, string) ([]WatchedOABView, error) {
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

// The processes envelope's filter options are assembled from the distinct-value reads:
// court/degree are label==value strings, lifecycle is the closed enum set (canonical
// order), assignee is label==name/value==id. A key with no options is omitted; an
// assignee without an id is never selectable.
func TestReadUseCase_Processos_AssemblesFilters(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{
		procCourts:    []string{"TJSP", "TRT3"},
		procDegrees:   []string{"PRIMEIRO_GRAU"},
		procAssignees: []AssigneeOption{{Name: "Ana", ID: "u-1"}, {Name: "Sem Id", ID: ""}},
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Processos(context.Background(), ProcessosQuery{TenantID: "t-1", Limit: 20})
	if err != nil {
		t.Fatalf("Processos: %v", err)
	}

	courts := res.Filters["court"]
	if len(courts) != 2 || courts[0].Label != "TJSP" || courts[0].Value != "TJSP" || courts[1].Value != "TRT3" {
		t.Errorf("court options = %+v, want label==value TJSP,TRT3", courts)
	}
	degrees := res.Filters["degree"]
	if len(degrees) != 1 || degrees[0].Label != "PRIMEIRO_GRAU" || degrees[0].Value != "PRIMEIRO_GRAU" {
		t.Errorf("degree options = %+v", degrees)
	}
	lifecycle := res.Filters["lifecycle"]
	wantLifecycle := []string{LifecycleActive, LifecycleSuspended, LifecycleArchived, LifecycleSuperseded}
	if len(lifecycle) != len(wantLifecycle) {
		t.Fatalf("lifecycle options = %+v, want %v", lifecycle, wantLifecycle)
	}
	for i, want := range wantLifecycle {
		if lifecycle[i].Label != want || lifecycle[i].Value != want {
			t.Errorf("lifecycle[%d] = %+v, want label==value %q", i, lifecycle[i], want)
		}
	}
	assignees := res.Filters["assignee"]
	if len(assignees) != 1 || assignees[0].Label != "Ana" || assignees[0].Value != "u-1" {
		t.Errorf("assignee options = %+v, want only the id-bearing Ana", assignees)
	}
}

// A ProcessosQuery is "filtered" when any of search/court/lifecycle/degree/assignee is
// set — the counter then needs the filtered COUNT; all-empty is the default context.
func TestProcessosQuery_Filtered(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		q    ProcessosQuery
		want bool
	}{
		{"none", ProcessosQuery{}, false},
		{"search", ProcessosQuery{Search: "x"}, true},
		{"court", ProcessosQuery{Court: "TJSP"}, true},
		{"lifecycle", ProcessosQuery{Lifecycle: LifecycleArchived}, true},
		{"degree", ProcessosQuery{Degree: "PRIMEIRO_GRAU"}, true},
		{"assignee", ProcessosQuery{Assignee: "u-1"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.q.Filtered(); got != tc.want {
				t.Errorf("Filtered() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Processo (the deep-link) is a plain delegation: it forwards (tenant, id) verbatim to
// the repo and returns the canned ProcessoView — including claim_value — untouched (no
// pagination policy).
func TestReadUseCase_Processo_ForwardsAndReturnsView(t *testing.T) {
	t.Parallel()

	claim := "150000.00"
	repo := &recordingReadRepo{procOneRes: ProcessoView{
		ID:         "cr-7",
		CNJNumber:  "0001111-22.2024.8.26.0100",
		ClaimValue: &claim,
	}}
	uc := NewReadUseCase(repo)

	view, err := uc.Processo(context.Background(), "tenant-9", "cr-7")
	if err != nil {
		t.Fatalf("Processo: %v", err)
	}
	if repo.gotProcOneTID != "tenant-9" || repo.gotProcOneID != "cr-7" {
		t.Errorf("forwarded (tenant, id) = (%q, %q), want (tenant-9, cr-7)", repo.gotProcOneTID, repo.gotProcOneID)
	}
	if view.ID != "cr-7" || view.CNJNumber != "0001111-22.2024.8.26.0100" {
		t.Errorf("view not returned verbatim: %+v", view)
	}
	if view.ClaimValue == nil || *view.ClaimValue != "150000.00" {
		t.Errorf("claim_value = %v, want 150000.00", view.ClaimValue)
	}
}

// --- next_deadline (Entrega 1) -------------------------------------------------

// Processo (the deep-link) propagates the NextDeadline block verbatim from the repo
// when the process has an OPEN/PENDING prazo — the use case is a thin delegation with
// no transformation of the next_deadline field.
func TestReadUseCase_Processo_NextDeadline_Present(t *testing.T) {
	t.Parallel()

	endDate := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	repo := &recordingReadRepo{procOneRes: ProcessoView{
		ID:           "cr-99",
		NextDeadline: &NextDeadlineView{EndDate: endDate, DaysLeft: 12, Kind: "CONTESTACAO"},
	}}
	uc := NewReadUseCase(repo)

	view, err := uc.Processo(context.Background(), "t-1", "cr-99")
	if err != nil {
		t.Fatalf("Processo: %v", err)
	}
	if view.NextDeadline == nil {
		t.Fatal("NextDeadline should be non-nil when process has an open prazo")
	}
	if view.NextDeadline.Kind != "CONTESTACAO" {
		t.Errorf("Kind = %q, want CONTESTACAO", view.NextDeadline.Kind)
	}
	if view.NextDeadline.DaysLeft != 12 {
		t.Errorf("DaysLeft = %d, want 12", view.NextDeadline.DaysLeft)
	}
	if !view.NextDeadline.EndDate.Equal(endDate) {
		t.Errorf("EndDate = %v, want %v", view.NextDeadline.EndDate, endDate)
	}
}

// Processo propagates NextDeadline as nil when the process has no open/pending prazo.
func TestReadUseCase_Processo_NextDeadline_Nil(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{procOneRes: ProcessoView{ID: "cr-1", NextDeadline: nil}}
	uc := NewReadUseCase(repo)

	view, err := uc.Processo(context.Background(), "t-1", "cr-1")
	if err != nil {
		t.Fatalf("Processo: %v", err)
	}
	if view.NextDeadline != nil {
		t.Errorf("NextDeadline should be nil when no prazo exists, got %+v", view.NextDeadline)
	}
}

// Processos (the list) includes the NextDeadline block from each row as returned by
// the repo — the use case applies the over-fetch/trim policy without touching the field.
func TestReadUseCase_Processos_NextDeadline_CarriedThrough(t *testing.T) {
	t.Parallel()

	endDate := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	repo := &recordingReadRepo{
		procRows: []ProcessoView{
			{ID: "cr-1", NextDeadline: &NextDeadlineView{EndDate: endDate, DaysLeft: 3, Kind: "RECURSO"}},
			{ID: "cr-2", NextDeadline: nil},
		},
	}
	uc := NewReadUseCase(repo)

	res, err := uc.Processos(context.Background(), ProcessosQuery{TenantID: "t-1", Limit: 10})
	if err != nil {
		t.Fatalf("Processos: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(res.Items))
	}
	if res.Items[0].NextDeadline == nil {
		t.Fatal("Items[0].NextDeadline should be non-nil")
	}
	if res.Items[0].NextDeadline.Kind != "RECURSO" {
		t.Errorf("Items[0].NextDeadline.Kind = %q, want RECURSO", res.Items[0].NextDeadline.Kind)
	}
	if res.Items[0].NextDeadline.DaysLeft != 3 {
		t.Errorf("Items[0].NextDeadline.DaysLeft = %d, want 3", res.Items[0].NextDeadline.DaysLeft)
	}
	if res.Items[1].NextDeadline != nil {
		t.Errorf("Items[1].NextDeadline should be nil (no prazo), got %+v", res.Items[1].NextDeadline)
	}
}

// mapNextDeadline — unit tests for the repository helper (white-box: lives in the
// same package). The CASE WHEN expression sqlc emits returns interface{} from pgx;
// these tests prove the int64/int32 type-switch handles both scan targets.
func TestMapNextDeadline_WithPrazo(t *testing.T) {
	t.Parallel()

	endDate := pgDate(2026, 9, 15)
	kind := "DEFESA"
	v := mapNextDeadline(endDate, int64(7), &kind)

	if v == nil {
		t.Fatal("expected non-nil NextDeadlineView")
	}
	if v.DaysLeft != 7 {
		t.Errorf("DaysLeft = %d, want 7", v.DaysLeft)
	}
	if v.Kind != "DEFESA" {
		t.Errorf("Kind = %q, want DEFESA", v.Kind)
	}
	want := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	if !v.EndDate.Equal(want) {
		t.Errorf("EndDate = %v, want %v", v.EndDate, want)
	}
}

func TestMapNextDeadline_NilWhenNoRow(t *testing.T) {
	t.Parallel()

	// An invalid pgtype.Date means the LEFT JOIN LATERAL returned no row.
	v := mapNextDeadline(pgDateInvalid(), nil, nil)
	if v != nil {
		t.Errorf("mapNextDeadline with invalid date should return nil, got %+v", v)
	}
}

func TestMapNextDeadline_Int32DaysLeft(t *testing.T) {
	t.Parallel()

	endDate := pgDate(2026, 10, 1)
	kind := "RECURSO"
	v := mapNextDeadline(endDate, int32(14), &kind)
	if v == nil {
		t.Fatal("expected non-nil NextDeadlineView")
	}
	if v.DaysLeft != 14 {
		t.Errorf("DaysLeft = %d, want 14 (int32 branch)", v.DaysLeft)
	}
}

// Partes forwards (tenant, court_record) to the repo and buckets the flat rows by role
// into the AUTOR/RÉU/TERCEIROS lists the cockpit renders — with each party's advogados.
func TestReadUseCase_Partes_BucketsByRole(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{partesRows: []PartyRow{
		{Role: PartyRolePlaintiff, PartyView: PartyView{
			Name:     "AUTOR",
			Counsels: []PartyCounselView{{Name: "ADV", OAB: "111", UF: "SP"}},
		}},
		{Role: PartyRoleDefendant, PartyView: PartyView{Name: "REU", Counsels: []PartyCounselView{}}},
		{Role: PartyRoleThirdParty, PartyView: PartyView{Name: "TERCEIRO", Counsels: []PartyCounselView{}}},
	}}
	uc := NewReadUseCase(repo)

	view, err := uc.Partes(context.Background(), "tenant-3", "cr-9")
	if err != nil {
		t.Fatalf("Partes: %v", err)
	}
	if repo.gotPartesTID != "tenant-3" || repo.gotPartesCRID != "cr-9" {
		t.Errorf("forwarded (tenant, court_record) = (%q, %q), want (tenant-3, cr-9)", repo.gotPartesTID, repo.gotPartesCRID)
	}
	if len(view.Autor) != 1 || view.Autor[0].Name != "AUTOR" {
		t.Errorf("autor bucket = %+v, want one AUTOR", view.Autor)
	}
	if len(view.Autor) == 1 && (len(view.Autor[0].Counsels) != 1 || view.Autor[0].Counsels[0].OAB != "111") {
		t.Errorf("autor counsels = %+v, want one 111/SP", view.Autor[0].Counsels)
	}
	if len(view.Reu) != 1 || view.Reu[0].Name != "REU" {
		t.Errorf("réu bucket = %+v, want one REU", view.Reu)
	}
	if len(view.Terceiros) != 1 || view.Terceiros[0].Name != "TERCEIRO" {
		t.Errorf("terceiros bucket = %+v, want one TERCEIRO", view.Terceiros)
	}
}

// An empty read (a foreign or partyless process) yields three initialized empty lists,
// never nil — so the JSON payload is three arrays, not nulls.
func TestReadUseCase_Partes_EmptyIsThreeArrays(t *testing.T) {
	t.Parallel()

	uc := NewReadUseCase(&recordingReadRepo{partesRows: nil})
	view, err := uc.Partes(context.Background(), "tenant-3", "cr-x")
	if err != nil {
		t.Fatalf("Partes: %v", err)
	}
	if view.Autor == nil || view.Reu == nil || view.Terceiros == nil {
		t.Errorf("empty buckets must be initialized: %+v", view)
	}
	if len(view.Autor)+len(view.Reu)+len(view.Terceiros) != 0 {
		t.Errorf("expected all buckets empty, got %+v", view)
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

// The responsável fields ride the ProcessoView through the read use case: when the
// repo's deep-link row carries an assigned user, the view surfaces both id and name (the
// FE renders the header from a single read). The mapping pgtype.UUID/*string → *string
// lives in the repo (a DB concern); here we prove the view field flows through untouched.
func TestReadUseCase_Processo_CarriesResponsible(t *testing.T) {
	t.Parallel()

	userID := "018f0000-0000-7000-8000-0000000000aa"
	userName := "Dra. Ana"
	repo := &recordingReadRepo{procOneRes: ProcessoView{
		ID:               "cr-7",
		AssignedUserID:   &userID,
		AssignedUserName: &userName,
	}}
	uc := NewReadUseCase(repo)

	view, err := uc.Processo(context.Background(), "tenant-9", "cr-7")
	if err != nil {
		t.Fatalf("Processo: %v", err)
	}
	if view.AssignedUserID == nil || *view.AssignedUserID != userID {
		t.Errorf("assigned_user_id = %v, want %q", view.AssignedUserID, userID)
	}
	if view.AssignedUserName == nil || *view.AssignedUserName != userName {
		t.Errorf("assigned_user_name = %v, want %q", view.AssignedUserName, userName)
	}
}

// An unassigned process surfaces nil responsável fields (JSON null), not a zero string.
func TestReadUseCase_Processo_UnassignedIsNil(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{procOneRes: ProcessoView{ID: "cr-7"}}
	uc := NewReadUseCase(repo)

	view, err := uc.Processo(context.Background(), "tenant-9", "cr-7")
	if err != nil {
		t.Fatalf("Processo: %v", err)
	}
	if view.AssignedUserID != nil || view.AssignedUserName != nil {
		t.Errorf("unassigned view carries responsável: id=%v name=%v", view.AssignedUserID, view.AssignedUserName)
	}
}

// --- summaries ---------------------------------------------------------------

// ProcessosSummary passes the repo's bucketed counts through verbatim (the read use case
// adds no policy — it is a straight delegate).
func TestReadUseCase_ProcessosSummary_PassesThrough(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{procSummary: ProcessosSummaryView{
		Total: 42, EmAndamento: 30, Suspensos: 5, Arquivados: 7, Baixados: 0,
	}}
	uc := NewReadUseCase(repo)

	got, err := uc.ProcessosSummary(context.Background(), "tenant-9")
	if err != nil {
		t.Fatalf("ProcessosSummary: %v", err)
	}
	if got != (ProcessosSummaryView{Total: 42, EmAndamento: 30, Suspensos: 5, Arquivados: 7, Baixados: 0}) {
		t.Errorf("summary = %+v, want the repo's counts verbatim", got)
	}
}

// WatchedOABs passes the repo rows through to the caller. A nil Name (no counsel
// yet) must be preserved as nil in the result; a non-empty name passes through.
func TestReadUseCase_WatchedOABs_PassesThrough(t *testing.T) {
	t.Parallel()

	nameStr := "LUAN GOMES"
	rows := []WatchedOABView{
		{OAB: "SP347019", Name: &nameStr},
		{OAB: "SP100001", Name: nil},
	}
	called := false
	uc := NewReadUseCase(&watchedOABsRecordingRepo{
		recordingReadRepo: &recordingReadRepo{},
		oabRows:           rows,
		onCall:            func() { called = true },
	})

	got, err := uc.WatchedOABs(context.Background(), "tenant-oabs")
	if err != nil {
		t.Fatalf("WatchedOABs: %v", err)
	}
	if !called {
		t.Error("repo.ListWatchedOABsWithName was not called")
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].OAB != "SP347019" || got[0].Name == nil || *got[0].Name != "LUAN GOMES" {
		t.Errorf("got[0] = %+v, want {OAB:SP347019 Name:&LUAN GOMES}", got[0])
	}
	if got[1].OAB != "SP100001" || got[1].Name != nil {
		t.Errorf("got[1] = %+v, want {OAB:SP100001 Name:nil}", got[1])
	}
}

// watchedOABsRecordingRepo embeds recordingReadRepo and overrides only
// ListWatchedOABsWithName so the WatchedOABs use-case test can verify the repo
// was called and the result passes through unchanged.
type watchedOABsRecordingRepo struct {
	*recordingReadRepo
	oabRows []WatchedOABView
	onCall  func()
}

func (r *watchedOABsRecordingRepo) ListWatchedOABsWithName(_ context.Context, _ string) ([]WatchedOABView, error) {
	if r.onCall != nil {
		r.onCall()
	}
	return r.oabRows, nil
}

// IntimacoesSummary passes the repo's triagem-bucketed counts through verbatim,
// including the new urgência KPIs (em_atraso, vencem_hoje, nao_confirmado).
func TestReadUseCase_IntimacoesSummary_PassesThrough(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{intiSummary: IntimacoesSummaryView{
		Total: 20, Pendentes: 12, EmAnalise: 0, Resolvidas: 6, Ignoradas: 2, Criticas: 0,
		EmAtraso: 3, VencemHoje: 1, NaoConfirmado: 5,
	}}
	uc := NewReadUseCase(repo)

	got, err := uc.IntimacoesSummary(context.Background(), "tenant-9")
	if err != nil {
		t.Fatalf("IntimacoesSummary: %v", err)
	}
	want := IntimacoesSummaryView{
		Total: 20, Pendentes: 12, Resolvidas: 6, Ignoradas: 2,
		EmAtraso: 3, VencemHoje: 1, NaoConfirmado: 5,
	}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

// IntimacoesSummary passes the urgência KPIs as zeroes when there are no prazos —
// the summary is still valid (the counts are just 0).
func TestReadUseCase_IntimacoesSummary_ZeroUrgenciaWhenNoDeadlines(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{intiSummary: IntimacoesSummaryView{
		Total: 5, Pendentes: 5,
	}}
	uc := NewReadUseCase(repo)

	got, err := uc.IntimacoesSummary(context.Background(), "tenant-10")
	if err != nil {
		t.Fatalf("IntimacoesSummary: %v", err)
	}
	if got.EmAtraso != 0 || got.VencemHoje != 0 || got.NaoConfirmado != 0 {
		t.Errorf("urgência KPIs should be 0 when no deadlines: em_atraso=%d vencem_hoje=%d nao_confirmado=%d",
			got.EmAtraso, got.VencemHoje, got.NaoConfirmado)
	}
}

// Intimacoes use case forwards Urgencia to the repo query when set.
func TestReadUseCase_Intimacoes_ForwardsUrgencia(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		urgencia string
	}{
		{name: "atraso", urgencia: UrgenciaAtraso},
		{name: "hoje", urgencia: UrgenciaHoje},
		{name: "semana", urgencia: UrgenciaSemana},
		{name: "mais_adiante", urgencia: UrgenciaMaisAdiante},
		{name: "nao_confirmado", urgencia: UrgenciaNaoConfirmado},
		{name: "empty passes through", urgencia: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got IntimacoesQuery
			repo := &recordingIntimacoesRepo{onList: func(q IntimacoesQuery) { got = q }}
			uc := NewReadUseCase(repo)

			_, err := uc.Intimacoes(context.Background(), IntimacoesQuery{
				TenantID: "t-1", LastMadeAvailable: "9999-12-31", LastID: maxUUID, Limit: 10,
				Urgencia: tt.urgencia,
			})
			if err != nil {
				t.Fatalf("Intimacoes: %v", err)
			}
			if got.Urgencia != tt.urgencia {
				t.Errorf("forwarded urgencia = %q, want %q", got.Urgencia, tt.urgencia)
			}
		})
	}
}

// recordingIntimacoesRepo extends recordingReadRepo and captures the forwarded
// IntimacoesQuery so Urgencia-forwarding tests can inspect it.
type recordingIntimacoesRepo struct {
	*recordingReadRepo
	onList func(IntimacoesQuery)
}

func newRecordingIntimacoesRepo(onList func(IntimacoesQuery)) *recordingIntimacoesRepo {
	return &recordingIntimacoesRepo{
		recordingReadRepo: &recordingReadRepo{},
		onList:            onList,
	}
}

func (r *recordingIntimacoesRepo) ListIntimacoes(_ context.Context, q IntimacoesQuery) ([]IntimacaoView, error) {
	if r.onList != nil {
		r.onList(q)
	}
	return nil, nil
}

func (r *recordingIntimacoesRepo) CountIntimacoes(context.Context, IntimacoesQuery) (int64, int64, error) {
	return 0, 0, nil
}

func (r *recordingIntimacoesRepo) ListIntimacaoCourts(context.Context, string) ([]string, error) {
	return nil, nil
}

// IntimacaoView carries a nil Prazo when the intimation has no derived deadline.
func TestIntimacaoView_NilPrazoWhenNoDeadline(t *testing.T) {
	t.Parallel()

	view := IntimacaoView{ID: "abc", Prazo: nil}
	if view.Prazo != nil {
		t.Errorf("Prazo should be nil when no deadline derived, got %+v", view.Prazo)
	}
}

// IntimacaoView carries a populated Prazo when a deadline exists.
func TestIntimacaoView_PrazoPresent(t *testing.T) {
	t.Parallel()

	deadlineID := "018f0000-0000-7000-8000-000000000001"
	endDate := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	view := IntimacaoView{
		ID: "abc",
		Prazo: &IntimacaoPrazoView{
			DeadlineID: deadlineID,
			EndDate:    endDate,
			DaysLeft:   5,
			Status:     "OPEN",
			Confirmed:  false,
		},
	}
	if view.Prazo == nil {
		t.Fatal("Prazo should be non-nil when deadline exists")
	}
	if view.Prazo.DeadlineID != deadlineID {
		t.Errorf("DeadlineID = %q, want %q", view.Prazo.DeadlineID, deadlineID)
	}
	if view.Prazo.DaysLeft != 5 {
		t.Errorf("DaysLeft = %d, want 5", view.Prazo.DaysLeft)
	}
	if view.Prazo.Status != "OPEN" {
		t.Errorf("Status = %q, want OPEN", view.Prazo.Status)
	}
}

// --- bucket counts (Entrega 3) -----------------------------------------------

// bucketsRecordingRepo extends recordingIntimacoesRepo and captures the BucketIntimacoes
// call so bucket-forwarding tests can verify the query and the returned counts.
type bucketsRecordingRepo struct {
	*recordingIntimacoesRepo
	buckets     IntimacaoBucketsView
	bucketQuery IntimacoesQuery
}

func newBucketsRepo(buckets IntimacaoBucketsView) *bucketsRecordingRepo {
	return &bucketsRecordingRepo{
		recordingIntimacoesRepo: newRecordingIntimacoesRepo(nil),
		buckets:                 buckets,
	}
}

func (r *bucketsRecordingRepo) BucketIntimacoes(_ context.Context, q IntimacoesQuery) (IntimacaoBucketsView, error) {
	r.bucketQuery = q
	return r.buckets, nil
}

// Intimacoes use case forwards the non-urgência filters to BucketIntimacoes
// and carries the returned counts in IntimacoesResult.Buckets.
func TestReadUseCase_Intimacoes_BucketCountsForwarded(t *testing.T) {
	t.Parallel()

	want := IntimacaoBucketsView{
		Atraso:      3,
		Hoje:        1,
		EstaSemana:  5,
		MaisAdiante: 12,
		SemPrazo:    7,
	}
	repo := newBucketsRepo(want)
	uc := NewReadUseCase(repo)

	res, err := uc.Intimacoes(context.Background(), IntimacoesQuery{
		TenantID:          "t-1",
		LastMadeAvailable: "9999-12-31",
		LastID:            maxUUID,
		Limit:             10,
		Type:              IntimationTypeIntimacao,
		UserStatus:        IntimationUserStatusPending,
		Court:             "TJSP",
		Search:            "cnj",
		Urgencia:          UrgenciaAtraso, // urgência filter must NOT be forwarded to buckets
	})
	if err != nil {
		t.Fatalf("Intimacoes: %v", err)
	}
	if res.Buckets != want {
		t.Errorf("Buckets = %+v, want %+v", res.Buckets, want)
	}
	// Non-urgência filters must be forwarded; urgência must NOT be forwarded to bucket query.
	if repo.bucketQuery.Type != IntimationTypeIntimacao {
		t.Errorf("bucket query Type = %q, want %q", repo.bucketQuery.Type, IntimationTypeIntimacao)
	}
	if repo.bucketQuery.UserStatus != IntimationUserStatusPending {
		t.Errorf("bucket query UserStatus = %q, want %q", repo.bucketQuery.UserStatus, IntimationUserStatusPending)
	}
	if repo.bucketQuery.Court != "TJSP" {
		t.Errorf("bucket query Court = %q, want %q", repo.bucketQuery.Court, "TJSP")
	}
	if repo.bucketQuery.Search != "cnj" {
		t.Errorf("bucket query Search = %q, want %q", repo.bucketQuery.Search, "cnj")
	}
}

// Intimacoes result carries zero buckets when no intimations have active deadlines.
func TestReadUseCase_Intimacoes_BucketCountsZeroWhenNoDeadlines(t *testing.T) {
	t.Parallel()

	repo := newBucketsRepo(IntimacaoBucketsView{})
	uc := NewReadUseCase(repo)

	res, err := uc.Intimacoes(context.Background(), IntimacoesQuery{
		TenantID: "t-1", LastMadeAvailable: "9999-12-31", LastID: maxUUID, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Intimacoes: %v", err)
	}
	zero := IntimacaoBucketsView{}
	if res.Buckets != zero {
		t.Errorf("Buckets should be zero when no deadlines, got %+v", res.Buckets)
	}
}

// The urgência filter options include mais_adiante (the new bucket) alongside the
// existing ones — proved via the assembled Filters block.
func TestReadUseCase_Intimacoes_UrgenciaFilterIncludesMaisAdiante(t *testing.T) {
	t.Parallel()

	repo := newBucketsRepo(IntimacaoBucketsView{})
	uc := NewReadUseCase(repo)

	res, err := uc.Intimacoes(context.Background(), IntimacoesQuery{
		TenantID: "t-1", LastMadeAvailable: "9999-12-31", LastID: maxUUID, Limit: 10,
	})
	if err != nil {
		t.Fatalf("Intimacoes: %v", err)
	}
	opts := res.Filters["urgencia"]
	want := []string{UrgenciaAtraso, UrgenciaHoje, UrgenciaSemana, UrgenciaMaisAdiante, UrgenciaNaoConfirmado}
	if len(opts) != len(want) {
		t.Fatalf("urgencia filter options = %d, want %d: %+v", len(opts), len(want), opts)
	}
	for i, w := range want {
		if opts[i].Value != w {
			t.Errorf("urgencia filter[%d].value = %q, want %q", i, opts[i].Value, w)
		}
	}
}
