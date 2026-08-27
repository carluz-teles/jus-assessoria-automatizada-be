package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- test doubles ------------------------------------------------------------

// fakeResolver answers ResolveCourtRecordIDForDraftIntimation with a canned id/error.
type fakeResolver struct {
	courtRecordID string
	err           error
	gotTenantID   string
	gotDraftID    string
	calls         int
}

func (f *fakeResolver) ResolveCourtRecordIDForDraftIntimation(
	_ context.Context, tenantID, draftID string,
) (string, error) {
	f.calls++
	f.gotTenantID, f.gotDraftID = tenantID, draftID
	return f.courtRecordID, f.err
}

// fakeActivityDedup controls seen/mark behaviour for the activity listener's tests.
type fakeActivityDedup struct {
	seen  bool
	err   error
	calls int
}

func (f *fakeActivityDedup) SeenOrMark(_ context.Context, _ database.Tx, _, _ string) (bool, error) {
	f.calls++
	return f.seen, f.err
}

// fakeActivityWriter records InsertProcessActivityLog calls.
type fakeActivityWriter struct {
	err          error
	calls        int
	gotTenantID  string
	gotCourtID   string
	gotEventType string
	gotPayload   []byte
}

func (f *fakeActivityWriter) InsertProcessActivityLog(
	_ context.Context, _ database.Tx,
	tenantID, courtRecordID, eventType string, payload []byte,
) error {
	f.calls++
	f.gotTenantID, f.gotCourtID, f.gotEventType, f.gotPayload = tenantID, courtRecordID, eventType, payload
	return f.err
}

// draftGeneratedEv returns a canonical draftGeneratedPayload for the tests below.
func draftGeneratedEv(courtRecordID string) draftGeneratedPayload {
	return draftGeneratedPayload{
		Base:          events.Base{EventID: "event-dg-1", Aggregate: "draft-1"},
		TenantID:      "tenant-1",
		DraftID:       "draft-1",
		CourtRecordID: courtRecordID,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestActivityUseCase_OnDraftGenerated_HappyPath verifies that when the event carries
// no CourtRecordID, the use case falls back to the resolver and inserts a
// DRAFT_GENERATED log row.
func TestActivityUseCase_OnDraftGenerated_HappyPath(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnDraftGenerated(context.Background(), draftGeneratedEv("")); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	if resolver.calls != 1 {
		t.Errorf("resolver.calls = %d, want 1", resolver.calls)
	}
	if resolver.gotTenantID != "tenant-1" || resolver.gotDraftID != "draft-1" {
		t.Errorf("resolver got (%q, %q), want (tenant-1, draft-1)", resolver.gotTenantID, resolver.gotDraftID)
	}
	if writer.calls != 1 {
		t.Fatalf("writer.calls = %d, want 1", writer.calls)
	}
	if writer.gotTenantID != "tenant-1" {
		t.Errorf("writer tenantID = %q, want tenant-1", writer.gotTenantID)
	}
	if writer.gotCourtID != "cr-1" {
		t.Errorf("writer courtRecordID = %q, want cr-1", writer.gotCourtID)
	}
	if writer.gotEventType != ActivityEventDraftGenerated {
		t.Errorf("writer eventType = %q, want %q", writer.gotEventType, ActivityEventDraftGenerated)
	}
	var payload activityDraftGeneratedPayload
	if err := json.Unmarshal(writer.gotPayload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.DraftID != "draft-1" {
		t.Errorf("payload = %+v, want {draft-1}", payload)
	}
}

// TestActivityUseCase_OnDraftGenerated_CourtRecordIDFromEvent verifies that when the
// event already carries CourtRecordID (the common case — resolved by GenerateUseCase
// from the already-loaded intimation), the resolver is never called.
func TestActivityUseCase_OnDraftGenerated_CourtRecordIDFromEvent(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-should-not-be-used"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnDraftGenerated(context.Background(), draftGeneratedEv("cr-from-event")); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	if resolver.calls != 0 {
		t.Errorf("resolver.calls = %d, want 0 (event already carried CourtRecordID)", resolver.calls)
	}
	if writer.gotCourtID != "cr-from-event" {
		t.Errorf("writer courtRecordID = %q, want cr-from-event", writer.gotCourtID)
	}
}

// TestActivityUseCase_OnDraftGenerated_Dedup verifies that a replayed event_id
// (already marked seen) is a no-op — the resolver and writer are never called.
func TestActivityUseCase_OnDraftGenerated_Dedup(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{seen: true}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnDraftGenerated(context.Background(), draftGeneratedEv("")); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if resolver.calls != 0 {
		t.Errorf("resolver.calls = %d, want 0 (dedup replay short-circuits)", resolver.calls)
	}
	if writer.calls != 0 {
		t.Errorf("writer.calls = %d, want 0", writer.calls)
	}
}

// TestActivityUseCase_OnDraftGenerated_UnresolvableDraft verifies LOG-NOT-FAIL: when
// the resolver finds no court_record for the draft (a deleted draft, or a blank/processo
// draft with no intimation), the use case returns nil without writing a log row.
func TestActivityUseCase_OnDraftGenerated_UnresolvableDraft(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: ""} // not found
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnDraftGenerated(context.Background(), draftGeneratedEv("")); err != nil {
		t.Fatalf("want nil err (LOG-NOT-FAIL), got %v", err)
	}
	if writer.calls != 0 {
		t.Errorf("writer.calls = %d, want 0 (nothing to log)", writer.calls)
	}
}

// TestActivityUseCase_OnDraftGenerated_ResolverError verifies LOG-NOT-FAIL when the
// resolver itself errors (e.g. an infra fault) — swallowed, not retried.
func TestActivityUseCase_OnDraftGenerated_ResolverError(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("boom")}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnDraftGenerated(context.Background(), draftGeneratedEv("")); err != nil {
		t.Fatalf("want nil err (LOG-NOT-FAIL), got %v", err)
	}
	if writer.calls != 0 {
		t.Errorf("writer.calls = %d, want 0", writer.calls)
	}
}

// TestActivityUseCase_OnDraftGenerated_WriteFails verifies LOG-NOT-FAIL when the
// insert itself fails — the use case still returns nil (never rolls back the tx over
// a best-effort timeline row).
func TestActivityUseCase_OnDraftGenerated_WriteFails(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{err: errors.New("insert failed")}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnDraftGenerated(context.Background(), draftGeneratedEv("")); err != nil {
		t.Fatalf("want nil err (LOG-NOT-FAIL), got %v", err)
	}
	if writer.calls != 1 {
		t.Errorf("writer.calls = %d, want 1 (attempted, then swallowed)", writer.calls)
	}
}

// TestActivityUseCase_OnDraftGenerated_TenantScoped verifies the uow.Do call is
// scoped to the event's tenant (barrier 1/2 — RLS is set per this tenant).
func TestActivityUseCase_OnDraftGenerated_TenantScoped(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)
	if err := uc.OnDraftGenerated(context.Background(), draftGeneratedEv("")); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if uow.tenantID != "tenant-1" {
		t.Errorf("uow.tenantID = %q, want tenant-1", uow.tenantID)
	}
}

// TestDraftGenerated_ContractRoundTrip is the producer∥consumer contract guard
// (memória parallel-producer-consumer-roundtrip): it MARSHALS the producer's
// draft.DraftGenerated and UNMARSHALS it into this slice's LOCAL decode struct
// (draftGeneratedPayload), asserting every field this listener reads survives the
// wire, and pins the shared dotted id.
func TestDraftGenerated_ContractRoundTrip(t *testing.T) {
	if TypeDraftGenerated != draft.TypeDraftGenerated {
		t.Fatalf("consumed type %q != producer type %q", TypeDraftGenerated, draft.TypeDraftGenerated)
	}

	producer := draft.DraftGenerated{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: "draft-x"},
		DraftID:       "draft-x",
		TenantID:      "tenant-x",
		CourtRecordID: "cr-x",
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got draftGeneratedPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if got.DraftID != producer.DraftID {
		t.Errorf("DraftID = %q, want %q", got.DraftID, producer.DraftID)
	}
	if got.CourtRecordID != producer.CourtRecordID {
		t.Errorf("CourtRecordID = %q, want %q", got.CourtRecordID, producer.CourtRecordID)
	}
}
