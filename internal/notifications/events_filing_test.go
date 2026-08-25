package notifications

import (
	"encoding/json"
	"testing"

	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/lib/events"
)

// TestFilingSucceededDecodesFromDraft guards the cross-slice contract: the producer
// (draft) marshals FilingSucceeded and this slice decodes it into its LOCAL shape.
// Drift in the shared JSON fields (tenant_id/draft_id/filing_attempt_id/filing_number)
// would break this test.
func TestFilingSucceededDecodesFromDraft(t *testing.T) {
	producer := draft.FilingSucceeded{
		Base:            events.Base{EventID: "evt-s", Aggregate: "draft-1"},
		TenantID:        "tenant-1",
		DraftID:         "draft-1",
		FilingAttemptID: "attempt-1",
		FilingNumber:    "1234567.89.2026.8.26.0101",
	}
	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}
	task := events.Encode(draft.TypeFilingSucceeded, raw)

	local, err := events.Decode[FilingSucceeded](task)
	if err != nil {
		t.Fatalf("decode local: %v", err)
	}
	if local.TenantID != "tenant-1" || local.DraftID != "draft-1" ||
		local.FilingAttemptID != "attempt-1" || local.FilingNumber != "1234567.89.2026.8.26.0101" {
		t.Fatalf("decoded filing.succeeded lost fields: %+v", local)
	}
	if local.EventID != "evt-s" {
		t.Fatalf("EventID = %q, want evt-s", local.EventID)
	}
}

func TestFilingFailedDecodesFromDraft(t *testing.T) {
	producer := draft.FilingFailed{
		Base:            events.Base{EventID: "evt-f", Aggregate: "draft-2"},
		TenantID:        "tenant-2",
		DraftID:         "draft-2",
		FilingAttemptID: "attempt-2",
		FailureReason:   "login recusado",
	}
	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}
	task := events.Encode(draft.TypeFilingFailed, raw)

	local, err := events.Decode[FilingFailed](task)
	if err != nil {
		t.Fatalf("decode local: %v", err)
	}
	if local.TenantID != "tenant-2" || local.FailureReason != "login recusado" {
		t.Fatalf("decoded filing.failed lost fields: %+v", local)
	}
}
