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

// IntimacoesSummary passes the repo's triagem-bucketed counts through verbatim.
func TestReadUseCase_IntimacoesSummary_PassesThrough(t *testing.T) {
	t.Parallel()

	repo := &recordingReadRepo{intiSummary: IntimacoesSummaryView{
		Total: 20, Pendentes: 12, EmAnalise: 0, Resolvidas: 6, Ignoradas: 2, Criticas: 0,
	}}
	uc := NewReadUseCase(repo)

	got, err := uc.IntimacoesSummary(context.Background(), "tenant-9")
	if err != nil {
		t.Fatalf("IntimacoesSummary: %v", err)
	}
	if got != (IntimacoesSummaryView{Total: 20, Pendentes: 12, Resolvidas: 6, Ignoradas: 2}) {
		t.Errorf("summary = %+v, want the repo's counts verbatim", got)
	}
}
