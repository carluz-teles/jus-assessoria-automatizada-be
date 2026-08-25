package draft

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

func TestIsFilingTerminal(t *testing.T) {
	if !isFilingTerminal(apperr.NewInvalid("consentimento")) {
		t.Fatal("invalid error must be terminal")
	}
	if !isFilingTerminal(apperr.NewNotFound("draft")) {
		t.Fatal("not-found error must be terminal")
	}
	if isFilingTerminal(apperr.NewInfra("rpa", errors.New("boom"))) {
		t.Fatal("infra error must be retryable (not terminal)")
	}
	if isFilingTerminal(nil) {
		t.Fatal("nil must not be terminal")
	}
}

func TestFilingEnqueuedEventRoundTrip(t *testing.T) {
	ev := FilingEnqueued{
		Base:            events.Base{EventID: "evt-1", Aggregate: "draft-1"},
		TenantID:        "tenant-1",
		DraftID:         "draft-1",
		FilingAttemptID: "attempt-1",
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	task := events.Encode(TypeFilingEnqueued, raw)
	got, err := events.Decode[FilingEnqueued](task)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FilingAttemptID != "attempt-1" || got.TenantID != "tenant-1" {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if got.Type() != TypeFilingEnqueued {
		t.Fatalf("type = %q, want %q", got.Type(), TypeFilingEnqueued)
	}
}
