package notifications

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/lib/events"
)

// TestDeadlineDueSoon_ContractRoundTrip is the producer∥consumer contract guard (memória
// parallel-producer-consumer-roundtrip): it MARSHALS the producer's deadline.DeadlineDueSoon
// and UNMARSHALS it into this slice's LOCAL decode struct, asserting every field the aviso
// reads survives the wire. This slice deliberately imports only the type CONST (not the
// producer's struct), so without this test a field rename on either side would drift silently
// and the aviso would render from zero values. It also pins the shared dotted id.
func TestDeadlineDueSoon_ContractRoundTrip(t *testing.T) {
	t.Parallel()

	if TypeDeadlineDueSoon != deadline.TypeDeadlineDueSoon {
		t.Fatalf("consumed type %q != producer type %q", TypeDeadlineDueSoon, deadline.TypeDeadlineDueSoon)
	}

	producer := deadline.DeadlineDueSoon{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:   uuid.NewString(),
		DeadlineID: uuid.NewString(),
		DaysLeft:   3,
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got DeadlineDueSoon
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if got.DeadlineID != producer.DeadlineID {
		t.Errorf("DeadlineID = %q, want %q", got.DeadlineID, producer.DeadlineID)
	}
	if got.DaysLeft != producer.DaysLeft {
		t.Errorf("DaysLeft = %d, want %d", got.DaysLeft, producer.DaysLeft)
	}
}

// TestDeadlineMissed_ContractRoundTrip is the missed-fact counterpart of the round-trip guard:
// it MARSHALS deadline.DeadlineMissed and UNMARSHALS it into this slice's LOCAL decode struct,
// asserting every field survives the wire, and pins the shared dotted id.
func TestDeadlineMissed_ContractRoundTrip(t *testing.T) {
	t.Parallel()

	if TypeDeadlineMissed != deadline.TypeDeadlineMissed {
		t.Fatalf("consumed type %q != producer type %q", TypeDeadlineMissed, deadline.TypeDeadlineMissed)
	}

	producer := deadline.DeadlineMissed{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:   uuid.NewString(),
		DeadlineID: uuid.NewString(),
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got DeadlineMissed
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if got.DeadlineID != producer.DeadlineID {
		t.Errorf("DeadlineID = %q, want %q", got.DeadlineID, producer.DeadlineID)
	}
}

// TestTrialEndingSoon_ContractRoundTrip is the billing counterpart of the round-trip guard
// (fatia 2): it MARSHALS billing.TrialEndingSoon and UNMARSHALS it into this slice's LOCAL
// decode struct, asserting every field the aviso reads survives the wire, and pins the
// shared dotted id.
func TestTrialEndingSoon_ContractRoundTrip(t *testing.T) {
	t.Parallel()

	if TypeTrialEndingSoon != billing.TypeTrialEndingSoon {
		t.Fatalf("consumed type %q != producer type %q", TypeTrialEndingSoon, billing.TypeTrialEndingSoon)
	}

	producer := billing.TrialEndingSoon{
		Base:        events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:    uuid.NewString(),
		TrialEndsAt: time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC),
		DaysLeft:    2,
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got TrialEndingSoon
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if !got.TrialEndsAt.Equal(producer.TrialEndsAt) {
		t.Errorf("TrialEndsAt = %v, want %v", got.TrialEndsAt, producer.TrialEndsAt)
	}
	if got.DaysLeft != producer.DaysLeft {
		t.Errorf("DaysLeft = %d, want %d", got.DaysLeft, producer.DaysLeft)
	}
}
