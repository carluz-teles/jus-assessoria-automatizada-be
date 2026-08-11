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

func (m *mockRepo) GetReconciliationTotals(_ context.Context, _ string) (ReconciliationTotals, error) {
	return ReconciliationTotals{}, nil
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

func (m *mockRepo) UpsertDocketEntries(_ context.Context, _ database.Tx, _ []DocketEntryParams) ([]DocketEntry, error) {
	return nil, nil
}

func (m *mockRepo) UpsertIntimations(_ context.Context, _ database.Tx, _ []IntimationParams) (int, error) {
	return 0, nil
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

func (m *mockRepo) UpsertGradedCourtRecord(_ context.Context, _ database.Tx, _ GradedRecordParams) (*CourtRecord, error) {
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

func (m *mockRepo) ListIntimacoes(_ context.Context, _ IntimacoesQuery) ([]IntimacaoView, error) {
	return nil, nil
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

const testTenant = "tenant-1"

// --- tests -------------------------------------------------------------------

// AC1 (unit): two sources → two upserts and two events, in one unit of work.
func TestUseCase_ActivateIntegration_TwoSources(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}}
	outbox := &fakeOutbox{}
	uow := &fakeUoW{}
	uc := NewUseCase(repo, outbox, uow)

	got, err := uc.ActivateIntegration(context.Background(), testTenant,
		[]string{SourceDJEN, SourceDATAJUD}, Scope{OAB: []string{"SP123456"}})
	if err != nil {
		t.Fatalf("ActivateIntegration() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("activated = %d, want 2", len(got))
	}
	if len(repo.upsertedFor) != 2 {
		t.Fatalf("upserts = %d, want 2", len(repo.upsertedFor))
	}
	if outbox.calls != 2 {
		t.Fatalf("publishes = %d, want 2", outbox.calls)
	}
	if uow.calls != 1 {
		t.Fatalf("unit of work runs = %d, want 1 (all sources in one tx)", uow.calls)
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

// AC1 (unit): if a publish fails, the error propagates so the unit of work rolls
// back — no partial success. The second publish never happens.
func TestUseCase_ActivateIntegration_PublishFailAborts(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{existing: map[string]*Integration{}}
	sentinel := errors.New("outbox down")
	outbox := &fakeOutbox{failAt: 1, err: sentinel}
	uc := NewUseCase(repo, outbox, &fakeUoW{})

	_, err := uc.ActivateIntegration(context.Background(), testTenant,
		[]string{SourceDJEN, SourceDATAJUD}, Scope{OAB: []string{"SP123456"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ActivateIntegration() error = %v, want %v", err, sentinel)
	}
	if outbox.calls != 1 {
		t.Fatalf("publishes = %d, want 1 (aborted after the first failure)", outbox.calls)
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
				[]string{SourceDJEN}, newScope); err != nil {
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
