package acquisition

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// --- test doubles ------------------------------------------------------------

type stubMatchUoW struct{ tenants []string }

func (u *stubMatchUoW) Do(_ context.Context, tenantID string, fn func(database.Tx) error) error {
	u.tenants = append(u.tenants, tenantID)
	return fn(nil)
}
func (u *stubMatchUoW) DoSystem(_ context.Context, fn func(database.Tx) error) error {
	return fn(nil)
}

type fakeMatchParser struct {
	result    ParsedResult
	lastItems int
}

func (*fakeMatchParser) CanParse(RawPayload) bool { return true }
func (p *fakeMatchParser) Parse(_ context.Context, raw RawPayload) (ParsedResult, error) {
	var pl djenPayload
	_ = json.Unmarshal(raw.Body, &pl)
	p.lastItems = len(pl.Items)
	return p.result, nil
}

type stubMatchRepo struct {
	matches       []PublicationMatch
	findCalls     int
	upsertCalls   int
	lockTenants   []string
	captureParams []DailyCaptureParams
}

func (r *stubMatchRepo) MatchPublicationsByDay(context.Context, database.Tx, time.Time) ([]PublicationMatch, error) {
	return r.matches, nil
}
func (r *stubMatchRepo) MatchPublicationsForTenantSince(_ context.Context, _ database.Tx, _ string, _ time.Time) ([]PublicationMatch, error) {
	return r.matches, nil
}
func (r *stubMatchRepo) AcquireTenantWriteLock(_ context.Context, _ database.Tx, tenantID string) error {
	r.lockTenants = append(r.lockTenants, tenantID)
	return nil
}
func (r *stubMatchRepo) BatchUpsertCourtRecords(_ context.Context, _ database.Tx, _ string, _ int, params []FindOrCreateCourtRecordParams) ([]CourtRecordOutcome, int, error) {
	r.findCalls++ // one batch call per tenant window (the test's records are 1-per-tenant)
	outcomes := make([]CourtRecordOutcome, len(params))
	for i, p := range params {
		outcomes[i] = CourtRecordOutcome{Record: &CourtRecord{ID: "rec-" + p.CNJNumber, CaseID: "case-" + p.CNJNumber, CNJNumber: p.CNJNumber, Degree: p.Degree}}
	}
	return outcomes, len(params), nil
}
func (r *stubMatchRepo) UpsertIntimations(_ context.Context, _ database.Tx, params []IntimationParams) ([]IntimationChange, []IntimationChange, error) {
	r.upsertCalls++
	// The national match path discards the cancelled slice, but the NEW slice count is
	// the capture's intimations_new — return one "new" change per param so the audit row
	// under test carries an honest tally.
	newRows := make([]IntimationChange, len(params))
	return newRows, nil, nil
}

func (r *stubMatchRepo) WriteDailyCaptureRun(_ context.Context, _ database.Tx, p DailyCaptureParams) error {
	r.captureParams = append(r.captureParams, p)
	return nil
}

// --- tests -------------------------------------------------------------------

// TestGroupMatches buckets flat match rows by tenant, dedups the payload a tenant
// matched via two of its OABs, and keeps both keys.
func TestGroupMatches(t *testing.T) {
	t.Parallel()

	p := json.RawMessage(`{"hash":"P"}`)
	q := json.RawMessage(`{"hash":"Q"}`)
	got := groupMatches([]PublicationMatch{
		{TenantID: "A", OABKey: "1|SP", Payload: p},
		{TenantID: "A", OABKey: "2|MG", Payload: p}, // same publication, second OAB → deduped
		{TenantID: "B", OABKey: "9|RJ", Payload: q},
	})

	if len(got) != 2 {
		t.Fatalf("tenants = %d, want 2", len(got))
	}
	if a := got["A"]; len(a.items) != 1 || len(a.keys) != 2 || !a.keys["1|SP"] || !a.keys["2|MG"] {
		t.Errorf("A = {items:%d keys:%v}, want 1 item + {1|SP,2|MG}", len(a.items), a.keys)
	}
	if b := got["B"]; len(b.items) != 1 || len(b.keys) != 1 || !b.keys["9|RJ"] {
		t.Errorf("B = {items:%d keys:%v}, want 1 item + {9|RJ}", len(b.items), b.keys)
	}
}

// TestMatchUseCase_MatchDay drives the orchestration: two matched tenants, each of
// whose payloads parse to one record + one intimação, so each gets its write tx
// (advisory lock + FindOrCreate + UpsertIntimations) exactly once.
func TestMatchUseCase_MatchDay(t *testing.T) {
	t.Parallel()

	repo := &stubMatchRepo{matches: []PublicationMatch{
		{TenantID: "A", OABKey: "1|SP", Payload: json.RawMessage(`{"hash":"P"}`)},
		{TenantID: "B", OABKey: "9|RJ", Payload: json.RawMessage(`{"hash":"Q"}`)},
	}}
	parser := &fakeMatchParser{result: ParsedResult{
		CourtRecords: []ParsedCourtRecord{{CNJNumber: "1", Degree: DegreeUnknown}},
		Intimations:  []ParsedIntimation{{CNJNumber: "1", Degree: DegreeUnknown, Hash: "i1"}},
	}}
	uow := &stubMatchUoW{}
	uc := NewMatchUseCase(repo, uow, parser)

	day := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	if err := uc.MatchDay(context.Background(), day); err != nil {
		t.Fatalf("MatchDay: %v", err)
	}

	if repo.findCalls != 2 {
		t.Errorf("FindOrCreateCourtRecord calls = %d, want 2 (one per tenant)", repo.findCalls)
	}
	if repo.upsertCalls != 2 {
		t.Errorf("UpsertIntimations calls = %d, want 2", repo.upsertCalls)
	}
	slices.Sort(repo.lockTenants)
	if !slices.Equal(repo.lockTenants, []string{"A", "B"}) {
		t.Errorf("locked tenants = %v, want [A B]", repo.lockTenants)
	}
	slices.Sort(uow.tenants)
	if !slices.Equal(uow.tenants, []string{"A", "B"}) {
		t.Errorf("write txs for tenants = %v, want [A B]", uow.tenants)
	}
	// Each tenant's fan-out writes exactly one DAILY_CAPTURE audit row (DJEN /
	// DAILY_CAPTURE), stamped on the processed day, with the tenant's effect: the stub
	// returns len(params) new records and one "new" intimation per param.
	if len(repo.captureParams) != 2 {
		t.Fatalf("DailyCaptureRun writes = %d, want 2", len(repo.captureParams))
	}
	for _, p := range repo.captureParams {
		if p.CourtRecordsNew != 1 || p.CourtRecordsUpdated != 0 || p.IntimationsNew != 1 {
			t.Errorf("capture tally = {new:%d updated:%d intim:%d}, want {1 0 1}",
				p.CourtRecordsNew, p.CourtRecordsUpdated, p.IntimationsNew)
		}
		if !p.Window.Equal(day) {
			t.Errorf("capture window = %v, want the processed day %v", p.Window, day)
		}
		if p.FinishedAt.Before(p.StartedAt) {
			t.Errorf("capture finished_at %v before started_at %v", p.FinishedAt, p.StartedAt)
		}
	}
}

// TestMatchUseCase_MatchTenantSince catches a single tenant up from the store: its
// matched payloads parse to a record + intimação and get exactly one write tx.
func TestMatchUseCase_MatchTenantSince(t *testing.T) {
	t.Parallel()

	repo := &stubMatchRepo{matches: []PublicationMatch{
		{TenantID: "A", OABKey: "1|SP", Payload: json.RawMessage(`{"hash":"P"}`)},
	}}
	parser := &fakeMatchParser{result: ParsedResult{
		CourtRecords: []ParsedCourtRecord{{CNJNumber: "1", Degree: DegreeUnknown}},
		Intimations:  []ParsedIntimation{{CNJNumber: "1", Degree: DegreeUnknown, Hash: "i1"}},
	}}
	uow := &stubMatchUoW{}
	uc := NewMatchUseCase(repo, uow, parser)

	if err := uc.MatchTenantSince(context.Background(), "A", time.Now()); err != nil {
		t.Fatalf("MatchTenantSince: %v", err)
	}
	if repo.findCalls != 1 || repo.upsertCalls != 1 {
		t.Errorf("writes = {find:%d upsert:%d}, want {1 1}", repo.findCalls, repo.upsertCalls)
	}
	if len(uow.tenants) != 1 || uow.tenants[0] != "A" {
		t.Errorf("write tx tenants = %v, want [A]", uow.tenants)
	}
}
