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

// reviewCompletedEv returns a canonical reviewCompletedPayload for the tests below.
func reviewCompletedEv(status string) reviewCompletedPayload {
	return reviewCompletedPayload{
		Base:     events.Base{EventID: "event-rc-1", Aggregate: "draft-1"},
		TenantID: "tenant-1",
		DraftID:  "draft-1",
		ReviewID: "review-1",
		Status:   status,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestActivityUseCase_OnReviewCompleted_HappyPath verifies that a COMPLETED review
// resolves the court_record from the draft and inserts a DRAFT_GENERATED log row.
func TestActivityUseCase_OnReviewCompleted_HappyPath(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnReviewCompleted(context.Background(), reviewCompletedEv("COMPLETED")); err != nil {
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
	if payload.DraftID != "draft-1" || payload.ReviewID != "review-1" {
		t.Errorf("payload = %+v, want {draft-1 review-1}", payload)
	}
}

// TestActivityUseCase_OnReviewCompleted_Failed verifies that a FAILED review never
// dedups, resolves or writes — it is not a "peça gerada" fact.
func TestActivityUseCase_OnReviewCompleted_Failed(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnReviewCompleted(context.Background(), reviewCompletedEv("FAILED")); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if dedup.calls != 0 {
		t.Errorf("dedup.calls = %d, want 0 (FAILED short-circuits before the tx)", dedup.calls)
	}
	if resolver.calls != 0 {
		t.Errorf("resolver.calls = %d, want 0", resolver.calls)
	}
	if writer.calls != 0 {
		t.Errorf("writer.calls = %d, want 0", writer.calls)
	}
}

// TestActivityUseCase_OnReviewCompleted_Dedup verifies that a replayed event_id
// (already marked seen) is a no-op — the resolver and writer are never called.
func TestActivityUseCase_OnReviewCompleted_Dedup(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{seen: true}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnReviewCompleted(context.Background(), reviewCompletedEv("COMPLETED")); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if resolver.calls != 0 {
		t.Errorf("resolver.calls = %d, want 0 (dedup replay short-circuits)", resolver.calls)
	}
	if writer.calls != 0 {
		t.Errorf("writer.calls = %d, want 0", writer.calls)
	}
}

// TestActivityUseCase_OnReviewCompleted_UnresolvableDraft verifies LOG-NOT-FAIL: when
// the resolver finds no court_record for the draft (a deleted draft, or a blank/processo
// draft with no intimation), the use case returns nil without writing a log row.
func TestActivityUseCase_OnReviewCompleted_UnresolvableDraft(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: ""} // not found
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnReviewCompleted(context.Background(), reviewCompletedEv("COMPLETED")); err != nil {
		t.Fatalf("want nil err (LOG-NOT-FAIL), got %v", err)
	}
	if writer.calls != 0 {
		t.Errorf("writer.calls = %d, want 0 (nothing to log)", writer.calls)
	}
}

// TestActivityUseCase_OnReviewCompleted_ResolverError verifies LOG-NOT-FAIL when the
// resolver itself errors (e.g. an infra fault) — swallowed, not retried.
func TestActivityUseCase_OnReviewCompleted_ResolverError(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("boom")}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnReviewCompleted(context.Background(), reviewCompletedEv("COMPLETED")); err != nil {
		t.Fatalf("want nil err (LOG-NOT-FAIL), got %v", err)
	}
	if writer.calls != 0 {
		t.Errorf("writer.calls = %d, want 0", writer.calls)
	}
}

// TestActivityUseCase_OnReviewCompleted_WriteFails verifies LOG-NOT-FAIL when the
// insert itself fails — the use case still returns nil (never rolls back the tx over
// a best-effort timeline row).
func TestActivityUseCase_OnReviewCompleted_WriteFails(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{err: errors.New("insert failed")}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)

	if err := uc.OnReviewCompleted(context.Background(), reviewCompletedEv("COMPLETED")); err != nil {
		t.Fatalf("want nil err (LOG-NOT-FAIL), got %v", err)
	}
	if writer.calls != 1 {
		t.Errorf("writer.calls = %d, want 1 (attempted, then swallowed)", writer.calls)
	}
}

// TestActivityUseCase_OnReviewCompleted_TenantScoped verifies the uow.Do call is
// scoped to the event's tenant (barrier 1/2 — RLS is set per this tenant).
func TestActivityUseCase_OnReviewCompleted_TenantScoped(t *testing.T) {
	resolver := &fakeResolver{courtRecordID: "cr-1"}
	dedup := &fakeActivityDedup{}
	writer := &fakeActivityWriter{}
	uow := &fakeUoW{}

	uc := NewActivityUseCase(resolver, dedup, writer, uow)
	if err := uc.OnReviewCompleted(context.Background(), reviewCompletedEv("COMPLETED")); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if uow.tenantID != "tenant-1" {
		t.Errorf("uow.tenantID = %q, want tenant-1", uow.tenantID)
	}
}

// TestReviewCompleted_ContractRoundTrip is the producer∥consumer contract guard
// (memória parallel-producer-consumer-roundtrip): it MARSHALS the producer's
// draft.ReviewCompleted and UNMARSHALS it into this slice's LOCAL decode struct
// (reviewCompletedPayload), asserting every field this listener reads survives the
// wire, and pins the shared dotted id.
func TestReviewCompleted_ContractRoundTrip(t *testing.T) {
	if TypeReviewCompleted != draft.TypeReviewCompleted {
		t.Fatalf("consumed type %q != producer type %q", TypeReviewCompleted, draft.TypeReviewCompleted)
	}

	producer := draft.ReviewCompleted{
		Base:     events.Base{EventID: uuid.NewString(), Aggregate: "draft-x"},
		DraftID:  "draft-x",
		ReviewID: "review-x",
		TenantID: "tenant-x",
		Status:   "COMPLETED",
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got reviewCompletedPayload
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
	if got.ReviewID != producer.ReviewID {
		t.Errorf("ReviewID = %q, want %q", got.ReviewID, producer.ReviewID)
	}
	if got.Status != producer.Status {
		t.Errorf("Status = %q, want %q", got.Status, producer.Status)
	}
}
