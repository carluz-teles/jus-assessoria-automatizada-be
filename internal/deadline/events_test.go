package deadline

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

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

// TestDocketEntryObserved_ContractRoundTrip is the reconcile counterpart of the observed/
// cancelled round-trip guards (memória parallel-producer-consumer-roundtrip): it MARSHALS the
// producer's acquisition.DocketEntryObserved and UNMARSHALS it into this slice's LOCAL decode
// struct, asserting the two fields the reconcile reads (TenantID, CourtRecordID) survive the
// wire. The producer carries more (DocketEntryID/Hash/SyncRunID) that this slice deliberately
// ignores — the reconcile re-reads docket_entry directly, keyed by the court_record — so the
// local shape is a strict subset. Without this test a rename of either shared field would drift
// silently and the reconcile would scope off zero values. It also pins the shared dotted id.
func TestDocketEntryObserved_ContractRoundTrip(t *testing.T) {
	if TypeDocketEntryObserved != acquisition.TypeDocketEntryObserved {
		t.Fatalf("consumed type %q != producer type %q", TypeDocketEntryObserved, acquisition.TypeDocketEntryObserved)
	}

	producer := acquisition.DocketEntryObserved{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:      uuid.NewString(),
		SyncRunID:     uuid.NewString(),
		CourtRecordID: uuid.NewString(),
		DocketEntryID: uuid.NewString(),
		Hash:          "abc123",
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got DocketEntryObserved
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if got.CourtRecordID != producer.CourtRecordID {
		t.Errorf("CourtRecordID = %q, want %q", got.CourtRecordID, producer.CourtRecordID)
	}
}

// TestDeadlineReminderCheck_ScheduledContract pins the D-N mark's scheduled contract: it
// implements events.ScheduledEvent, its ETA is start-of-day(end_date) − days_left calendar
// days, and its idempotency key is the stable per-mark id the relay uses as the asynq
// TaskID (schedule-once). The unexported processAt never leaks into the JSON payload.
func TestDeadlineReminderCheck_ScheduledContract(t *testing.T) {
	deadlineID := uuid.NewString()
	tenantID := uuid.NewString()
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		daysLeft int
		wantAt   time.Time
	}{
		{3, time.Date(2024, 1, 29, 0, 0, 0, 0, time.UTC)},
		{1, time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC)},
		{0, end},
	}
	for _, tt := range tests {
		ev := newDeadlineReminderCheck(tenantID, deadlineID, tt.daysLeft, end)

		var _ events.ScheduledEvent = ev // compile-time: opts into scheduling

		at, ok := ev.ProcessAt()
		if !ok || !at.Equal(tt.wantAt) {
			t.Errorf("days_left=%d ProcessAt = %v (ok=%v), want %v", tt.daysLeft, at, ok, tt.wantAt)
		}
		if ev.Type() != TypeDeadlineReminderCheck || ev.AggregateType() != aggregateTypeDeadline || ev.AggregateID() != deadlineID {
			t.Errorf("days_left=%d type/aggregate = %q/%q/%q", tt.daysLeft, ev.Type(), ev.AggregateType(), ev.AggregateID())
		}
		if want := "deadline-reminder:" + deadlineID + ":" + strconv.Itoa(tt.daysLeft); ev.IdempotencyKey() != want {
			t.Errorf("days_left=%d idempotency key = %q, want %q", tt.daysLeft, ev.IdempotencyKey(), want)
		}

		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, leaked := decoded["processAt"]; leaked {
			t.Errorf("processAt leaked into payload: %s", raw)
		}
		if decoded["tenant_id"] != tenantID || decoded["deadline_id"] != deadlineID {
			t.Errorf("payload tenant/deadline = %v/%v", decoded["tenant_id"], decoded["deadline_id"])
		}
	}
}

// TestDeadlineMissedCheck_ScheduledContract pins the carência mark: ScheduledEvent, ETA =
// start-of-day(end_date) + 1 day, stable "deadline-missed:{id}" key.
func TestDeadlineMissedCheck_ScheduledContract(t *testing.T) {
	deadlineID := uuid.NewString()
	tenantID := uuid.NewString()
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	ev := newDeadlineMissedCheck(tenantID, deadlineID, end)

	var _ events.ScheduledEvent = ev

	at, ok := ev.ProcessAt()
	if !ok || !at.Equal(time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ProcessAt = %v (ok=%v), want 2024-02-02", at, ok)
	}
	if ev.Type() != TypeDeadlineMissedCheck || ev.AggregateID() != deadlineID {
		t.Errorf("type/aggregate = %q/%q", ev.Type(), ev.AggregateID())
	}
	if want := "deadline-missed:" + deadlineID; ev.IdempotencyKey() != want {
		t.Errorf("idempotency key = %q, want %q", ev.IdempotencyKey(), want)
	}
}

// TestDeadlineImmediateEvents_NotScheduled proves the two lembrete/fact events deliver
// immediately: they do NOT implement events.ScheduledEvent, so the outbox leaves their
// process_at NULL. Both still carry the tenant + deadline id for the future consumer.
func TestDeadlineImmediateEvents_NotScheduled(t *testing.T) {
	deadlineID := uuid.NewString()
	tenantID := uuid.NewString()

	dueSoon := newDeadlineDueSoon(tenantID, deadlineID, 3)
	if _, ok := events.Event(dueSoon).(events.ScheduledEvent); ok {
		t.Error("due_soon must NOT implement ScheduledEvent (immediate delivery)")
	}
	if dueSoon.AggregateID() != deadlineID || dueSoon.TenantID != tenantID || dueSoon.DaysLeft != 3 {
		t.Errorf("due_soon aggregate/tenant/days = %q/%q/%d", dueSoon.AggregateID(), dueSoon.TenantID, dueSoon.DaysLeft)
	}
	if _, err := uuid.Parse(dueSoon.IdempotencyKey()); err != nil {
		t.Errorf("due_soon event id is not a uuid: %v", err)
	}

	missed := newDeadlineMissed(tenantID, deadlineID)
	if _, ok := events.Event(missed).(events.ScheduledEvent); ok {
		t.Error("missed must NOT implement ScheduledEvent (immediate delivery)")
	}
	if missed.AggregateID() != deadlineID || missed.TenantID != tenantID {
		t.Errorf("missed aggregate/tenant = %q/%q", missed.AggregateID(), missed.TenantID)
	}
	if _, err := uuid.Parse(missed.IdempotencyKey()); err != nil {
		t.Errorf("missed event id is not a uuid: %v", err)
	}
}
