package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- test doubles ------------------------------------------------------------

// fakeUoW runs fn with a nil Tx (the mock repo ignores the handle) and returns
// its error verbatim, mirroring UnitOfWork.Do's contract that a domain error
// propagates unwrapped. Rollback semantics against a real DB are covered by the
// integration test.
type fakeUoW struct {
	tenantID string // captured for assertions
	calls    int
}

func (f *fakeUoW) Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error {
	f.calls++
	f.tenantID = tenantID
	return fn(nil)
}

func (f *fakeUoW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	f.calls++
	return fn(nil)
}

// mockRepo records upserts and answers GetBySource from a preconfigured map.
type mockRepo struct {
	existing    map[string]*Integration // by source; absent → ErrIntegrationNotFound
	upsertedFor []string                // sources upserted, in order
	listResp    []*Integration
	// totals backs GetReconciliationTotals — the entitlement gate's active-count
	// read. Defaults to the zero value (0 active records).
	totals ReconciliationTotals

	// Responsável write path (AssignResponsible). resolveCaseID is the case the
	// record→case hop returns; resolveErr forces the not-found branch. memberOK backs
	// the membership guard (false → not-a-member). The rest capture what was written.
	resolveCaseID   string
	resolveErr      error
	memberOK        bool
	assignedCaseID  string
	assignedUser    *string
	memberCheckedID string

	// Triagem write path (SetIntimationUserStatus): capture what was written and an
	// optional canned error to drive the not-found branch.
	setUserStatusErr    error
	gotUserStatusIntiID string
	gotUserStatusValue  string
}

func (m *mockRepo) GetBySource(_ context.Context, _ database.Tx, _, source string) (*Integration, error) {
	if e, ok := m.existing[source]; ok {
		return e, nil
	}
	return nil, ErrIntegrationNotFound
}

func (m *mockRepo) Upsert(_ context.Context, _ database.Tx, tenantID, source string, scope Scope) (*Integration, error) {
	m.upsertedFor = append(m.upsertedFor, source)
	return &Integration{
		ID:       "id-" + source,
		TenantID: tenantID,
		Source:   source,
		Scope:    scope,
		Status:   StatusActive,
	}, nil
}

func (m *mockRepo) List(_ context.Context, _ string) ([]*Integration, error) {
	return m.listResp, nil
}

func (m *mockRepo) GetImportStatus(_ context.Context, _ string) (ImportStatusView, error) {
	return ImportStatusView{}, nil
}

func (m *mockRepo) ListReconciliations(_ context.Context, _ string, _ int) ([]ReconciliationView, error) {
	return nil, nil
}

func (m *mockRepo) GetReconciliation(_ context.Context, _, _ string) (ReconciliationView, error) {
	return ReconciliationView{}, nil
}

func (m *mockRepo) ListSyncRunsByJob(_ context.Context, _, _ string) ([]ReconciliationRunView, error) {
	return nil, nil
}

func (m *mockRepo) ListProcessosBySyncRun(_ context.Context, _, _ string) ([]ProcessoLineView, error) {
	return nil, nil
}

func (m *mockRepo) ListIntimacoesBySyncRun(_ context.Context, _, _ string) ([]IntimacaoLineView, error) {
	return nil, nil
}

// Captures reads — no-ops; the activation use case under test never calls them.
func (m *mockRepo) ListCaptureRuns(_ context.Context, _ string, _ int) ([]CaptureRunRow, error) {
	return nil, nil
}
func (m *mockRepo) GetCaptureRun(_ context.Context, _, _ string) (CaptureRunRow, error) {
	return CaptureRunRow{}, nil
}
func (m *mockRepo) GetCaptureSummary(_ context.Context, _ string) (CaptureSummaryRow, error) {
	return CaptureSummaryRow{}, nil
}
func (m *mockRepo) CountDeadlinesCreatedBetween(_ context.Context, _ string, _, _ time.Time) (int, error) {
	return 0, nil
}
func (m *mockRepo) CountTasksCreatedBetween(_ context.Context, _ string, _, _ time.Time) (int, error) {
	return 0, nil
}
func (m *mockRepo) CountDeadlinesCreatedToday(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (m *mockRepo) CountWatchedOABsForDJEN(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (m *mockRepo) ListWatchedOABsWithName(_ context.Context, _ string) ([]WatchedOABView, error) {
	return nil, nil
}

// Capture writes — no-ops; the activation use case under test never calls them.
func (m *mockRepo) WriteDailyCaptureRun(_ context.Context, _ database.Tx, _ DailyCaptureParams) error {
	return nil
}
func (m *mockRepo) IncrementImportEnrichmentRun(_ context.Context, _ database.Tx, _, _ string, _ time.Time) error {
	return nil
}
func (m *mockRepo) IncrementImportEnrichmentRunBy(_ context.Context, _ database.Tx, _, _ string, _, _ int, _ time.Time) error {
	return nil
}
func (m *mockRepo) SelectDueForEnrichment(_ context.Context, _ database.Tx, _, _ string, _ int) ([]DueRecord, error) {
	return nil, nil
}
func (m *mockRepo) CountRemainingDueForJob(_ context.Context, _ database.Tx, _, _ string) (int, error) {
	return 0, nil
}
func (m *mockRepo) MarkEnrichmentAttempted(_ context.Context, _ database.Tx, _ string, _ []string, _ time.Time) error {
	return nil
}
func (m *mockRepo) GetImportEnrichmentCounter(_ context.Context, _ database.Tx, _, _ string) (int, int, bool, error) {
	return 0, 0, false, nil
}
func (m *mockRepo) CountImportDiscoveredRecords(_ context.Context, _ database.Tx, _, _ string) (int, error) {
	return 0, nil
}
func (m *mockRepo) CloseImportEnrichmentRun(_ context.Context, _ database.Tx, _ CloseEnrichmentRunParams) (int, error) {
	return 0, nil
}
func (m *mockRepo) InsertEmptyImportEnrichmentRun(_ context.Context, _ database.Tx, _ CloseEnrichmentRunParams) error {
	return nil
}

func (m *mockRepo) GetReconciliationTotals(_ context.Context, _ string) (ReconciliationTotals, error) {
	return m.totals, nil
}

// The backfill methods satisfy the widened Repository interface; the activation
// use case under test here never calls them (they are exercised by the backfill
// use case's own stub in backfill_test.go).
func (m *mockRepo) BackfillJobExistsByIntegration(_ context.Context, _ database.Tx, _ string) (bool, error) {
	return false, nil
}

func (m *mockRepo) InsertBackfillJob(_ context.Context, _ database.Tx, _ BackfillJobParams) (string, error) {
	return "", nil
}

func (m *mockRepo) IncrementBackfillSlicesOK(_ context.Context, _ database.Tx, _, _ string) (BackfillCounters, error) {
	return BackfillCounters{}, nil
}

func (m *mockRepo) IncrementBackfillSlicesError(_ context.Context, _ database.Tx, _, _ string) (BackfillCounters, error) {
	return BackfillCounters{}, nil
}

func (m *mockRepo) FinalizeBackfillJob(_ context.Context, _ database.Tx, _, _, _ string) error {
	return nil
}

// The sync methods satisfy the widened Repository interface; the activation use
// case under test here never calls them (they are exercised by the sync use
// case's own narrow stub in sync_test.go).
func (m *mockRepo) InsertSyncRun(_ context.Context, _ database.Tx, _ SyncRunParams) (string, error) {
	return "", nil
}

func (m *mockRepo) FindSyncRunByEventID(_ context.Context, _ database.Tx, _ string) (*SyncRun, error) {
	return nil, nil
}

func (m *mockRepo) UpdateSyncRun(_ context.Context, _ database.Tx, _ SyncRunOutcome) (bool, error) {
	return true, nil
}

func (m *mockRepo) AcquireTenantWriteLock(_ context.Context, _ database.Tx, _ string) error {
	return nil
}

func (m *mockRepo) FindOrCreateCourtRecord(_ context.Context, _ database.Tx, _ FindOrCreateCourtRecordParams) (*CourtRecord, bool, error) {
	return nil, false, nil
}

func (m *mockRepo) BatchUpsertCourtRecords(_ context.Context, _ database.Tx, _ string, _ int, params []FindOrCreateCourtRecordParams) ([]CourtRecordOutcome, int, error) {
	return make([]CourtRecordOutcome, len(params)), 0, nil
}

func (m *mockRepo) UpsertDocketEntries(_ context.Context, _ database.Tx, _ []DocketEntryParams) ([]DocketEntry, error) {
	return nil, nil
}

func (m *mockRepo) UpsertIntimations(_ context.Context, _ database.Tx, _ []IntimationParams) ([]IntimationChange, []IntimationChange, error) {
	return nil, nil, nil
}

func (m *mockRepo) UpsertParties(_ context.Context, _ database.Tx, _ []PartyParams) error {
	return nil
}

func (m *mockRepo) ListPartesByProcesso(_ context.Context, _, _ string) ([]PartyRow, error) {
	return nil, nil
}

func (m *mockRepo) InsertPublications(_ context.Context, _ database.Tx, _ []PublicationParams) (int, error) {
	return 0, nil
}

func (m *mockRepo) ReplaceWatchedOABs(_ context.Context, _ database.Tx, _, _ string, _ []string) error {
	return nil
}

func (m *mockRepo) MatchPublicationsByDay(_ context.Context, _ database.Tx, _ time.Time) ([]PublicationMatch, error) {
	return nil, nil
}

func (m *mockRepo) MatchPublicationsForTenantSince(_ context.Context, _ database.Tx, _ string, _ time.Time) ([]PublicationMatch, error) {
	return nil, nil
}

func (m *mockRepo) GetCourtRecordByKey(_ context.Context, _ database.Tx, _, _, _ string) (*CourtRecord, bool, error) {
	return nil, false, nil
}

func (m *mockRepo) UpdateCourtRecordGrade(_ context.Context, _ database.Tx, _ GradeParams) (*CourtRecord, error) {
	return nil, nil
}

func (m *mockRepo) RepointIntimations(_ context.Context, _ database.Tx, _, _, _ string) (int, error) {
	return 0, nil
}

func (m *mockRepo) SupersedeCourtRecord(_ context.Context, _ database.Tx, _, _ string) error {
	return nil
}

func (m *mockRepo) DueCourtRecordsForResync(_ context.Context, _ database.Tx, _ int) ([]DueRecord, error) {
	return nil, nil
}

func (m *mockRepo) ClaimCourtRecordResync(_ context.Context, _ database.Tx, _ string, _ time.Time) error {
	return nil
}

func (m *mockRepo) ListProcessos(_ context.Context, _ ProcessosQuery) ([]ProcessoView, error) {
	return nil, nil
}

func (m *mockRepo) GetProcesso(_ context.Context, _, _ string) (ProcessoView, error) {
	return ProcessoView{}, nil
}

func (m *mockRepo) ResolveCaseIDByCourtRecord(_ context.Context, _ database.Tx, _, _ string) (string, error) {
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	return m.resolveCaseID, nil
}

func (m *mockRepo) AppUserInTenant(_ context.Context, _ database.Tx, _, appUserID string) (bool, error) {
	m.memberCheckedID = appUserID
	return m.memberOK, nil
}

func (m *mockRepo) AssignCaseResponsible(_ context.Context, _ database.Tx, _, caseID string, assignedUserID *string) error {
	m.assignedCaseID = caseID
	m.assignedUser = assignedUserID
	return nil
}

func (m *mockRepo) ListIntimacoes(_ context.Context, _ IntimacoesQuery) ([]IntimacaoView, error) {
	return nil, nil
}

func (m *mockRepo) GetIntimacao(_ context.Context, _, _ string) (IntimacaoDetailView, error) {
	return IntimacaoDetailView{}, nil
}

func (m *mockRepo) SetIntimationUserStatus(_ context.Context, _ database.Tx, _, intimationID, userStatus string) error {
	m.gotUserStatusIntiID = intimationID
	m.gotUserStatusValue = userStatus
	return m.setUserStatusErr
}

func (m *mockRepo) AssignIntimacaoResponsaveis(_ context.Context, _ database.Tx, _, _ string, _, _ *string) error {
	return nil
}

func (m *mockRepo) CountProcessos(_ context.Context, _ ProcessosQuery) (int64, int64, error) {
	return 0, 0, nil
}

func (m *mockRepo) CountIntimacoes(_ context.Context, _ IntimacoesQuery) (int64, int64, error) {
	return 0, 0, nil
}

func (m *mockRepo) BucketIntimacoes(_ context.Context, _ IntimacoesQuery) (IntimacaoBucketsView, error) {
	return IntimacaoBucketsView{}, nil
}

func (m *mockRepo) ListProcessoCourts(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockRepo) ListProcessoDegrees(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockRepo) ListProcessoAssignees(_ context.Context, _ string) ([]AssigneeOption, error) {
	return nil, nil
}

func (m *mockRepo) ListIntimacaoCourts(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockRepo) SummarizeProcessos(_ context.Context, _ string) (ProcessosSummaryView, error) {
	return ProcessosSummaryView{}, nil
}

func (m *mockRepo) SummarizeIntimacoes(_ context.Context, _ string) (IntimacoesSummaryView, error) {
	return IntimacoesSummaryView{}, nil
}

func (m *mockRepo) ListAndamentosByProcesso(_ context.Context, _ AndamentosQuery) ([]AndamentoView, error) {
	return nil, nil
}

func (m *mockRepo) CountAndamentosByProcesso(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

func (m *mockRepo) ListIntimacoesByProcesso(_ context.Context, _ IntimacoesByProcessoQuery) ([]IntimacaoView, error) {
	return nil, nil
}

func (m *mockRepo) CountIntimacoesByProcesso(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

func (m *mockRepo) GetResumoContext(_ context.Context, _, _ string) (ProcessoResumoCtx, error) {
	return ProcessoResumoCtx{}, nil
}

func (m *mockRepo) GetIntimacaoAnaliseContext(_ context.Context, _, _ string) (IntimacaoAnaliseCtx, error) {
	return IntimacaoAnaliseCtx{}, nil
}

// fakeOutbox records published events and can be told to fail one call to
// exercise the abort path.
type fakeOutbox struct {
	published []events.Event
	failAt    int // 1-based call index that returns err; 0 = never
	calls     int
	err       error
}

func (f *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	f.calls++
	if f.failAt != 0 && f.calls == f.failAt {
		return f.err
	}
	f.published = append(f.published, ev)
	return nil
}

// PublishBatch behaves as N Publishes for the fake, so call counting, the failAt abort
// and the recorded-events assertions stay identical whether the code batches or not.
func (f *fakeOutbox) PublishBatch(ctx context.Context, tx database.Tx, evs []events.Event) error {
	for _, ev := range evs {
		if err := f.Publish(ctx, tx, ev); err != nil {
			return err
		}
	}
	return nil
}

const testTenant = "tenant-1"

// --- tests -------------------------------------------------------------------

// AC1 (unit): activation upserts the integration row and publishes the event, in
// one unit of work.
func TestUseCase_ActivateIntegration(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}}
	outbox := &fakeOutbox{}
	uow := &fakeUoW{}
	uc := NewUseCase(repo, outbox, uow)

	got, err := uc.ActivateIntegration(context.Background(), testTenant,
		Scope{OAB: []string{"SP123456"}})
	if err != nil {
		t.Fatalf("ActivateIntegration() error = %v", err)
	}

	if got == nil {
		t.Fatalf("activated = nil, want a non-nil integration")
	}
	if len(repo.upsertedFor) != 1 {
		t.Fatalf("upserts = %d, want 1", len(repo.upsertedFor))
	}
	if outbox.calls != 1 {
		t.Fatalf("publishes = %d, want 1", outbox.calls)
	}
	if uow.calls != 1 {
		t.Fatalf("unit of work runs = %d, want 1", uow.calls)
	}
	if uow.tenantID != testTenant {
		t.Fatalf("uow tenantID = %q, want %q", uow.tenantID, testTenant)
	}

	// The event payload carries the identifiers a consumer needs.
	ev, ok := outbox.published[0].(IntegrationActivated)
	if !ok {
		t.Fatalf("published[0] type = %T, want IntegrationActivated", outbox.published[0])
	}
	if ev.TenantID != testTenant || ev.Source != SourceDJEN || ev.IntegrationID != "id-"+SourceDJEN {
		t.Fatalf("event payload = %+v, unexpected", ev)
	}
	if ev.Type() != TypeIntegrationActivated {
		t.Fatalf("event type = %q, want %q", ev.Type(), TypeIntegrationActivated)
	}
	if ev.AggregateID() != "id-"+SourceDJEN {
		t.Fatalf("aggregate id = %q, want the integration id", ev.AggregateID())
	}
}

// AC1 (unit): if the publish fails, the error propagates so the unit of work
// rolls back — no partial success.
func TestUseCase_ActivateIntegration_PublishFailAborts(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}}
	sentinel := errors.New("outbox down")
	outbox := &fakeOutbox{failAt: 1, err: sentinel}
	uc := NewUseCase(repo, outbox, &fakeUoW{})

	_, err := uc.ActivateIntegration(context.Background(), testTenant,
		Scope{OAB: []string{"SP123456"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ActivateIntegration() error = %v, want %v", err, sentinel)
	}
	if outbox.calls != 1 {
		t.Fatalf("publishes = %d, want 1", outbox.calls)
	}
}

// AC5: the event fires on a new source and on a scope change, but NOT on an
// identical, already-active re-post.
func TestUseCase_ActivateIntegration_EventOnlyWhenChanged(t *testing.T) {
	t.Parallel()

	newScope := Scope{OAB: []string{"SP123456"}}

	tests := []struct {
		name        string
		existing    *Integration // nil → first activation
		wantPublish int
	}{
		{
			name:        "first activation emits",
			existing:    nil,
			wantPublish: 1,
		},
		{
			name:        "identical active re-post does not emit",
			existing:    &Integration{Source: SourceDJEN, Status: StatusActive, Scope: Scope{OAB: []string{"SP123456"}}},
			wantPublish: 0,
		},
		{
			name:        "scope change emits",
			existing:    &Integration{Source: SourceDJEN, Status: StatusActive, Scope: Scope{OAB: []string{"RJ999"}}},
			wantPublish: 1,
		},
		{
			name:        "reactivation of a disabled source emits",
			existing:    &Integration{Source: SourceDJEN, Status: StatusDisabled, Scope: Scope{OAB: []string{"SP123456"}}},
			wantPublish: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			existing := map[string]*Integration{}
			if tt.existing != nil {
				existing[SourceDJEN] = tt.existing
			}
			repo := &mockRepo{existing: existing}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, outbox, &fakeUoW{})

			if _, err := uc.ActivateIntegration(context.Background(), testTenant,
				newScope); err != nil {
				t.Fatalf("ActivateIntegration() error = %v", err)
			}
			// The row is always upserted, regardless of whether an event fires.
			if len(repo.upsertedFor) != 1 {
				t.Fatalf("upserts = %d, want 1", len(repo.upsertedFor))
			}
			if outbox.calls != tt.wantPublish {
				t.Fatalf("publishes = %d, want %d", outbox.calls, tt.wantPublish)
			}
		})
	}
}

// --- entitlement gating tests (fatia 5, edge gate) ---------------------------

// A tenant with no subscription resolves to limit 0 upstream (the adapter's
// fail-closed contract, mirrored here by the stub): activation is blocked before
// any upsert or publish, and the backfill-triggering event never fires.
func TestUseCase_ActivateIntegration_NoSubscription_Blocked(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}, totals: ReconciliationTotals{CourtRecords: 0}}
	outbox := &fakeOutbox{}
	uow := &fakeUoW{}
	checker := &stubEntitlementChecker{limit: 0}
	uc := NewUseCase(repo, outbox, uow, WithActivationEntitlementChecker(checker))

	_, err := uc.ActivateIntegration(context.Background(), testTenant,
		Scope{OAB: []string{"SP123456"}})
	if !errors.Is(err, ErrActivationBlocked) {
		t.Fatalf("ActivateIntegration() error = %v, want ErrActivationBlocked", err)
	}
	if len(repo.upsertedFor) != 0 {
		t.Fatalf("upserts = %d, want 0 (blocked before touching the repo)", len(repo.upsertedFor))
	}
	if outbox.calls != 0 {
		t.Fatalf("publishes = %d, want 0 (no backfill-triggering event)", outbox.calls)
	}
	if uow.calls != 0 {
		t.Fatalf("unit of work runs = %d, want 0 (blocked before opening a tx)", uow.calls)
	}
}

// A tenant within its plan's ceiling activates normally — the gate is
// transparent, and the checker is consulted scoped to the event's tenant.
func TestUseCase_ActivateIntegration_WithinLimit_Proceeds(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}, totals: ReconciliationTotals{CourtRecords: 3}}
	outbox := &fakeOutbox{}
	uow := &fakeUoW{}
	checker := &stubEntitlementChecker{limit: 10}
	uc := NewUseCase(repo, outbox, uow, WithActivationEntitlementChecker(checker))

	got, err := uc.ActivateIntegration(context.Background(), testTenant,
		Scope{OAB: []string{"SP123456"}})
	if err != nil {
		t.Fatalf("ActivateIntegration() error = %v", err)
	}
	if got == nil {
		t.Fatalf("activated = nil, want a non-nil integration")
	}
	if len(repo.upsertedFor) != 1 {
		t.Fatalf("upserts = %d, want 1", len(repo.upsertedFor))
	}
	if outbox.calls != 1 {
		t.Fatalf("publishes = %d, want 1", outbox.calls)
	}
	if checker.calls != 1 || checker.lastTenant != testTenant {
		t.Fatalf("checker = {calls:%d tenant:%q}, want {1 %q}", checker.calls, checker.lastTenant, testTenant)
	}
}

// A tenant already AT its ceiling (active count == limit) is blocked, same as a
// tenant over it — the gate does not require crossing the line, just reaching it.
func TestUseCase_ActivateIntegration_AtLimit_Blocked(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}, totals: ReconciliationTotals{CourtRecords: 5}}
	outbox := &fakeOutbox{}
	uow := &fakeUoW{}
	checker := &stubEntitlementChecker{limit: 5}
	uc := NewUseCase(repo, outbox, uow, WithActivationEntitlementChecker(checker))

	_, err := uc.ActivateIntegration(context.Background(), testTenant,
		Scope{OAB: []string{"SP123456"}})
	if !errors.Is(err, ErrActivationBlocked) {
		t.Fatalf("ActivateIntegration() error = %v, want ErrActivationBlocked", err)
	}
	if len(repo.upsertedFor) != 0 {
		t.Fatalf("upserts = %d, want 0 (blocked before touching the repo)", len(repo.upsertedFor))
	}
	if outbox.calls != 0 {
		t.Fatalf("publishes = %d, want 0 (no backfill-triggering event)", outbox.calls)
	}
}

// Without an injected checker (the default unlimitedEntitlement), activation is
// never gated — mirrors every pre-existing test in this file that constructs
// NewUseCase with no options.
func TestUseCase_ActivateIntegration_NoCheckerInjected_Unlimited(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}, totals: ReconciliationTotals{CourtRecords: 1_000_000}}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, outbox, &fakeUoW{})

	if _, err := uc.ActivateIntegration(context.Background(), testTenant,
		Scope{OAB: []string{"SP123456"}}); err != nil {
		t.Fatalf("ActivateIntegration() error = %v, want nil (no checker injected → unlimited)", err)
	}
}

// ListIntegrations is a thin read-model passthrough — no tx, no event.
func TestUseCase_ListIntegrations(t *testing.T) {
	t.Parallel()

	want := []*Integration{{ID: "a", Source: SourceDJEN}, {ID: "b", Source: SourceDATAJUD}}
	repo := &mockRepo{listResp: want}
	uow := &fakeUoW{}
	uc := NewUseCase(repo, &fakeOutbox{}, uow)

	got, err := uc.ListIntegrations(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("ListIntegrations() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list len = %d, want 2", len(got))
	}
	if uow.calls != 0 {
		t.Fatalf("unit of work runs = %d, want 0 (a read needs no tx)", uow.calls)
	}
}

// --- AssignResponsible (responsável do processo) -----------------------------

// Assigning a responsável: the use case hops court_record → court_case (one tx),
// guards the target user is a member, and writes the assignment with that case id.
func TestUseCase_AssignResponsible_Assigns(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{resolveCaseID: "case-42", memberOK: true}
	uow := &fakeUoW{}
	uc := NewUseCase(repo, &fakeOutbox{}, uow)

	user := "user-7"
	if err := uc.AssignResponsible(context.Background(), testTenant, "cr-1", &user); err != nil {
		t.Fatalf("AssignResponsible() error = %v", err)
	}

	if uow.calls != 1 || uow.tenantID != testTenant {
		t.Fatalf("uow calls=%d tenant=%q, want 1 run scoped to %q", uow.calls, uow.tenantID, testTenant)
	}
	if repo.memberCheckedID != user {
		t.Fatalf("membership guard checked %q, want %q", repo.memberCheckedID, user)
	}
	if repo.assignedCaseID != "case-42" {
		t.Fatalf("assigned on case %q, want the resolved case-42", repo.assignedCaseID)
	}
	if repo.assignedUser == nil || *repo.assignedUser != user {
		t.Fatalf("assigned user = %v, want %q", repo.assignedUser, user)
	}
}

// Desatribuir: a nil user_id clears the responsável and SKIPS the membership guard
// (there is no user to validate).
func TestUseCase_AssignResponsible_Unassigns(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{resolveCaseID: "case-42", memberOK: false}
	uow := &fakeUoW{}
	uc := NewUseCase(repo, &fakeOutbox{}, uow)

	if err := uc.AssignResponsible(context.Background(), testTenant, "cr-1", nil); err != nil {
		t.Fatalf("AssignResponsible(nil) error = %v", err)
	}

	if repo.memberCheckedID != "" {
		t.Fatalf("membership guard ran for a desatribuir (checked %q), want skipped", repo.memberCheckedID)
	}
	if repo.assignedCaseID != "case-42" || repo.assignedUser != nil {
		t.Fatalf("expected NULL assignment on case-42, got case=%q user=%v", repo.assignedCaseID, repo.assignedUser)
	}
}

// A user_id from outside the escritório fails the membership guard → the typed
// ErrResponsibleNotMember, and nothing is written.
func TestUseCase_AssignResponsible_ForeignUser_Errors(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{resolveCaseID: "case-42", memberOK: false}
	uc := NewUseCase(repo, &fakeOutbox{}, &fakeUoW{})

	user := "user-outsider"
	err := uc.AssignResponsible(context.Background(), testTenant, "cr-1", &user)
	if !errors.Is(err, ErrResponsibleNotMember) {
		t.Fatalf("error = %v, want ErrResponsibleNotMember", err)
	}
	if repo.assignedCaseID != "" {
		t.Fatalf("assignment written despite a foreign user (case %q)", repo.assignedCaseID)
	}
}

// An unknown/foreign court_record :id (the record→case hop finds nothing) → the typed
// ErrProcessoNotFound, and nothing is written.
func TestUseCase_AssignResponsible_UnknownRecord_NotFound(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{resolveErr: ErrProcessoNotFound, memberOK: true}
	uc := NewUseCase(repo, &fakeOutbox{}, &fakeUoW{})

	user := "user-7"
	err := uc.AssignResponsible(context.Background(), testTenant, "cr-missing", &user)
	if !errors.Is(err, ErrProcessoNotFound) {
		t.Fatalf("error = %v, want ErrProcessoNotFound", err)
	}
	if repo.memberCheckedID != "" || repo.assignedCaseID != "" {
		t.Fatalf("guard/assign ran despite an unresolved record (checked=%q case=%q)", repo.memberCheckedID, repo.assignedCaseID)
	}
}

// --- triagem: resolve / ignore / reopen --------------------------------------

// The three triagem verbs map to the three target user_status values, and each runs in
// exactly one tx scoped to the principal's tenant. Table-driven: one row per verb.
func TestUseCase_TriageIntimacao_SetsUserStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		act        func(uc *UseCase, ctx context.Context) error
		wantStatus string
	}{
		{
			name:       "resolve → RESOLVED",
			act:        func(uc *UseCase, ctx context.Context) error { return uc.ResolveIntimacao(ctx, testTenant, "inti-1") },
			wantStatus: IntimationUserStatusResolved,
		},
		{
			name:       "ignore → IGNORED",
			act:        func(uc *UseCase, ctx context.Context) error { return uc.IgnoreIntimacao(ctx, testTenant, "inti-1") },
			wantStatus: IntimationUserStatusIgnored,
		},
		{
			name:       "reopen → PENDING",
			act:        func(uc *UseCase, ctx context.Context) error { return uc.ReopenIntimacao(ctx, testTenant, "inti-1") },
			wantStatus: IntimationUserStatusPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := &mockRepo{}
			uow := &fakeUoW{}
			uc := NewUseCase(repo, &fakeOutbox{}, uow)

			if err := tt.act(uc, context.Background()); err != nil {
				t.Fatalf("triage action error = %v", err)
			}
			if uow.calls != 1 || uow.tenantID != testTenant {
				t.Fatalf("uow calls=%d tenant=%q, want 1 run scoped to %q", uow.calls, uow.tenantID, testTenant)
			}
			if repo.gotUserStatusIntiID != "inti-1" {
				t.Fatalf("wrote to intimation %q, want inti-1", repo.gotUserStatusIntiID)
			}
			if repo.gotUserStatusValue != tt.wantStatus {
				t.Fatalf("user_status = %q, want %q", repo.gotUserStatusValue, tt.wantStatus)
			}
		})
	}
}

// A miss/foreign id surfaces as the repo's typed ErrIntimationNotFound, propagated
// verbatim out of the tx (the use case adds no handling of its own).
func TestUseCase_TriageIntimacao_NotFound(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{setUserStatusErr: ErrIntimationNotFound}
	uc := NewUseCase(repo, &fakeOutbox{}, &fakeUoW{})

	err := uc.ResolveIntimacao(context.Background(), testTenant, "inti-missing")
	if !errors.Is(err, ErrIntimationNotFound) {
		t.Fatalf("error = %v, want ErrIntimationNotFound", err)
	}
}
