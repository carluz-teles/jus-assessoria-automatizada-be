//go:build integration

// court integration test — proves internal/court's FetchAutosBatch/FetchAutosItem
// repository layer (court_fetch_state, court_connection.session_ref, sync_run —
// migration 0079 + the sqlc queries in internal/court/queries/court.sql) against a
// real Postgres, with a FAKE CourtProvider standing in for the real eproc portal
// (the ERD's own "Portão A" convention — real adapter needs a live tribunal, not
// CI). Session persistence, batch marking, and the sync_run audit trail are the
// hand-written SQL most likely to hide a real bug that a repo-mocked unit test
// can't catch.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/court"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/vault"
)

// fakeEprocProvider stands in for a real tribunal — Connect always succeeds,
// FetchAutos answers with a canned AutosResult/Session per call, scripted by the
// test the same way internal/court's own unit tests script fakeProvider.
type fakeEprocProvider struct {
	fetchResults []court.AutosResult
	fetchErrs    []error
	session      court.Session
	calls        int
}

func (p *fakeEprocProvider) Connect(context.Context, *court.CourtConnection, string) error {
	return nil
}

func (p *fakeEprocProvider) FetchAutos(_ context.Context, _ *court.CourtConnection, _ string, _ court.Session, _, _ string, _ time.Time) (court.AutosResult, court.Session, error) {
	idx := p.calls
	p.calls++
	var result court.AutosResult
	if idx < len(p.fetchResults) {
		result = p.fetchResults[idx]
	}
	var err error
	if idx < len(p.fetchErrs) {
		err = p.fetchErrs[idx]
	}
	return result, p.session, err
}

func newCourtUseCase(t *testing.T, pool *pgxpool.Pool) *court.UseCase {
	t.Helper()
	kek, err := vault.GenerateKEK()
	if err != nil {
		t.Fatalf("generate test kek: %v", err)
	}
	v, err := vault.New(kek)
	if err != nil {
		t.Fatalf("init test vault: %v", err)
	}
	return court.NewUseCase(court.NewRepository(), database.NewUnitOfWork(pool), v, events.NewOutbox())
}

// seedCourtConnection inserts a CONNECTED court_connection directly (PASSWORD
// method with no real credential — this test exercises FetchAutosBatch's
// repository layer, not the login flow, so no real certificate row is needed).
func seedCourtConnection(t *testing.T, pool *pgxpool.Pool, tenantID, appUserID string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, pool,
		`INSERT INTO court_connection (id, tenant_id, app_user_id, court, system, authentication_method, status)
		 VALUES ($1, $2, $3, 'TJSP', 'EPROC', 'PASSWORD', 'CONNECTED')`,
		id, tenantID, appUserID,
	)
	return id
}

func seedFetchStateDue(t *testing.T, pool *pgxpool.Pool, tenantID, connectionID, courtRecordID, cnjNumber string, observedAt time.Time) {
	t.Helper()
	mustExec(t, pool,
		`INSERT INTO court_fetch_state (tenant_id, court_connection_id, court_record_id, cnj_number, observed_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		tenantID, connectionID, courtRecordID, cnjNumber, observedAt,
	)
}

func TestCourt_FetchAutosBatch_MarksFetched_PersistsSession_RecordsSyncRun(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-court-batch", 1)
	appUserID := firstAppUserID(t, pool, tenantID)
	connID := seedCourtConnection(t, pool, tenantID, appUserID)

	rec1, rec2 := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	seedFetchStateDue(t, pool, tenantID, connID, rec1, "0000001-11.2026.8.26.0100", now)
	seedFetchStateDue(t, pool, tenantID, connID, rec2, "0000002-22.2026.8.26.0100", now)

	uc := newCourtUseCase(t, pool)
	provider := &fakeEprocProvider{
		fetchResults: []court.AutosResult{{}, {}},
		session:      court.Session(`[{"host":"https://eproc1g.tjsp.jus.br","cookies":[{"Name":"s","Value":"v"}]}]`),
	}
	uc.RegisterProvider("EPROC", provider)

	result, err := uc.FetchAutosBatch(ctx, tenantID, connID)
	if err != nil {
		t.Fatalf("FetchAutosBatch: %v", err)
	}
	if result.HasMore {
		t.Error("HasMore = true, want false (only 2 due records, well under the batch size)")
	}
	if len(result.RetryItems) != 0 {
		t.Errorf("RetryItems = %d, want 0", len(result.RetryItems))
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2", provider.calls)
	}

	var pending int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM court_fetch_state WHERE court_connection_id = $1 AND last_fetched_at IS NULL`,
		connID).Scan(&pending); err != nil {
		t.Fatalf("count pending fetch state: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending court_fetch_state rows = %d, want 0 (both should be marked fetched)", pending)
	}

	var sessionRef, status string
	if err := pool.QueryRow(ctx,
		`SELECT session_ref, status FROM court_connection WHERE id = $1`, connID,
	).Scan(&sessionRef, &status); err != nil {
		t.Fatalf("query court_connection: %v", err)
	}
	if sessionRef == "" {
		t.Error("session_ref not persisted after a successful batch")
	}
	if status != string(court.StatusConnected) {
		t.Errorf("status = %q, want CONNECTED (a clean batch must not touch it)", status)
	}

	var runStatus, connectorID string
	if err := pool.QueryRow(ctx,
		`SELECT status, connector_id FROM sync_run WHERE tenant_id = $1 ORDER BY started_at DESC LIMIT 1`,
		tenantID,
	).Scan(&runStatus, &connectorID); err != nil {
		t.Fatalf("query sync_run: %v", err)
	}
	if runStatus != "OK" {
		t.Errorf("sync_run status = %q, want OK", runStatus)
	}
	if connectorID != "EPROC" {
		t.Errorf("sync_run connector_id = %q, want EPROC", connectorID)
	}
}

func TestCourt_FetchAutosBatch_TransientFailure_LeavesItemDue(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-court-retry", 1)
	appUserID := firstAppUserID(t, pool, tenantID)
	connID := seedCourtConnection(t, pool, tenantID, appUserID)

	rec1 := uuid.NewString()
	seedFetchStateDue(t, pool, tenantID, connID, rec1, "0000003-33.2026.8.26.0100", time.Now().UTC())

	uc := newCourtUseCase(t, pool)
	provider := &fakeEprocProvider{
		fetchErrs: []error{context.DeadlineExceeded},
		session:   court.Session(`[]`),
	}
	uc.RegisterProvider("EPROC", provider)

	result, err := uc.FetchAutosBatch(ctx, tenantID, connID)
	if err != nil {
		t.Fatalf("FetchAutosBatch: %v", err)
	}
	if len(result.RetryItems) != 1 {
		t.Fatalf("RetryItems = %d, want 1", len(result.RetryItems))
	}

	var lastFetchedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_fetched_at FROM court_fetch_state WHERE court_connection_id = $1 AND court_record_id = $2`,
		connID, rec1,
	).Scan(&lastFetchedAt); err != nil {
		t.Fatalf("query court_fetch_state: %v", err)
	}
	if lastFetchedAt != nil {
		t.Error("last_fetched_at set on a transient failure — the item must stay due")
	}
}

func TestCourt_FetchAutosItem_MarksSingleRecordFetched(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-court-item", 1)
	appUserID := firstAppUserID(t, pool, tenantID)
	connID := seedCourtConnection(t, pool, tenantID, appUserID)

	rec1 := uuid.NewString()
	seedFetchStateDue(t, pool, tenantID, connID, rec1, "0000004-44.2026.8.26.0100", time.Now().UTC())

	uc := newCourtUseCase(t, pool)
	provider := &fakeEprocProvider{
		fetchResults: []court.AutosResult{{}},
		session:      court.Session(`[]`),
	}
	uc.RegisterProvider("EPROC", provider)

	if err := uc.FetchAutosItem(ctx, tenantID, connID, rec1, "0000004-44.2026.8.26.0100"); err != nil {
		t.Fatalf("FetchAutosItem: %v", err)
	}

	var lastFetchedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_fetched_at FROM court_fetch_state WHERE court_connection_id = $1 AND court_record_id = $2`,
		connID, rec1,
	).Scan(&lastFetchedAt); err != nil {
		t.Fatalf("query court_fetch_state: %v", err)
	}
	if lastFetchedAt == nil {
		t.Error("last_fetched_at not set after a successful FetchAutosItem")
	}
}
