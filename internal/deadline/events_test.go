package deadline

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/events"
)

// TestIntimationObserved_ContractRoundTrip is the producer∥consumer contract guard
// (memória parallel-producer-consumer-roundtrip): it MARSHALS the producer's
// acquisition.IntimationObserved and UNMARSHALS it into this slice's LOCAL decode struct,
// asserting every field the derivation reads survives the wire. This slice deliberately
// does NOT import the producer's struct (only the type const), so without this test a
// field rename on either side would drift silently and the prazo would derive from zero
// values. It also pins the shared dotted id.
func TestIntimationObserved_ContractRoundTrip(t *testing.T) {
	if TypeIntimationObserved != acquisition.TypeIntimationObserved {
		t.Fatalf("consumed type %q != producer type %q", TypeIntimationObserved, acquisition.TypeIntimationObserved)
	}

	producer := acquisition.IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:        uuid.NewString(),
		IntimationID:    uuid.NewString(),
		CourtRecordID:   uuid.NewString(),
		CaseID:          uuid.NewString(),
		IntimationType:  "CITACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2024-01-16",
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got IntimationObserved
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	// Base (the dedup key) and every derivation field must round-trip.
	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if got.IntimationID != producer.IntimationID {
		t.Errorf("IntimationID = %q, want %q", got.IntimationID, producer.IntimationID)
	}
	if got.CourtRecordID != producer.CourtRecordID {
		t.Errorf("CourtRecordID = %q, want %q", got.CourtRecordID, producer.CourtRecordID)
	}
	if got.CaseID != producer.CaseID {
		t.Errorf("CaseID = %q, want %q", got.CaseID, producer.CaseID)
	}
	// The producer's Go field is IntimationType; the local one is Type. Both carry the
	// SAME json tag "type" — this asserts that bridge holds.
	if got.Type != producer.IntimationType {
		t.Errorf("Type = %q, want %q (json tag \"type\")", got.Type, producer.IntimationType)
	}
	if got.Court != producer.Court {
		t.Errorf("Court = %q, want %q", got.Court, producer.Court)
	}
	if got.UF != producer.UF {
		t.Errorf("UF = %q, want %q", got.UF, producer.UF)
	}
	if got.DeadlineStartAt != producer.DeadlineStartAt {
		t.Errorf("DeadlineStartAt = %q, want %q", got.DeadlineStartAt, producer.DeadlineStartAt)
	}
}

// TestIntimationCancelled_ContractRoundTrip is the revocation counterpart of the observed
// round-trip guard (memória parallel-producer-consumer-roundtrip): it MARSHALS the
// producer's acquisition.IntimationCancelled and UNMARSHALS it into this slice's LOCAL
// decode struct, asserting every field the revocation reads survives the wire. This slice
// imports only the type const, so without this test a field rename on either side would
// drift silently and the revoke would key off zero values. It also pins the shared id.
func TestIntimationCancelled_ContractRoundTrip(t *testing.T) {
	if TypeIntimationCancelled != acquisition.TypeIntimationCancelled {
		t.Fatalf("consumed type %q != producer type %q", TypeIntimationCancelled, acquisition.TypeIntimationCancelled)
	}

	producer := acquisition.IntimationCancelled{
		Base:         events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:     uuid.NewString(),
		IntimationID: uuid.NewString(),
		Reason:       "retificada pelo tribunal",
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got IntimationCancelled
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if got.IntimationID != producer.IntimationID {
		t.Errorf("IntimationID = %q, want %q", got.IntimationID, producer.IntimationID)
	}
	if got.Reason != producer.Reason {
		t.Errorf("Reason = %q, want %q", got.Reason, producer.Reason)
	}
}
