package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"go.opentelemetry.io/otel/trace"
)

// Publish inserts one row with the eight envelope columns in order, capturing a
// non-empty trace_context from the active span so the relay can replay the hop. A
// plain event does not opt into future delivery, so process_at is NULL. sampleEvent's
// type ("minuta.revised") is not in priorityFor's P0 list, so priority is 1
// (background) — the fail-safe default.
func TestOutbox_Publish_InsertsRowWithTrace(t *testing.T) {
	mock := newMockPool(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext(t))

	ev := sampleEvent{Base: Base{EventID: "evt-1", Aggregate: "agg-1"}, Foo: "bar"}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs(
			"minuta",          // aggregate_type
			"agg-1",           // aggregate_id
			"minuta.revised",  // type
			pgxmock.AnyArg(),  // payload (JSON of ev)
			"evt-1",           // idempotency_key
			wantTraceparent,   // trace_context — non-empty, from the span
			(*time.Time)(nil), // process_at — NULL, immediate delivery
			int16(1),          // priority — priorityFor("minuta.revised") = 1 (background)
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := NewOutbox().Publish(ctx, mock, ev); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Without an active span the trace_context column is written empty; the hop then
// begins a fresh trace at the consumer. process_at stays NULL (immediate).
func TestOutbox_Publish_NoSpan_EmptyTrace(t *testing.T) {
	mock := newMockPool(t)
	ev := sampleEvent{Base: Base{EventID: "evt-2", Aggregate: "agg-2"}}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs("minuta", "agg-2", "minuta.revised", pgxmock.AnyArg(), "evt-2", "", (*time.Time)(nil), int16(1)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := NewOutbox().Publish(context.Background(), mock, ev); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An event that implements ScheduledEvent and asks for a future time has that time
// written to process_at — the opt-in path the relay turns into asynq.ProcessAt.
func TestOutbox_Publish_ScheduledEvent_WritesProcessAt(t *testing.T) {
	mock := newMockPool(t)
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	ev := scheduledSampleEvent{
		sampleEvent: sampleEvent{Base: Base{EventID: "evt-3", Aggregate: "agg-3"}},
		at:          at,
		ok:          true,
	}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs("minuta", "agg-3", "minuta.revised", pgxmock.AnyArg(), "evt-3", "", &at, int16(1)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := NewOutbox().Publish(context.Background(), mock, ev); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A ScheduledEvent that returns ok=false opts back out: process_at is NULL, identical
// to an event that never implemented the interface (immediate delivery).
func TestOutbox_Publish_ScheduledEvent_OptOut_WritesNull(t *testing.T) {
	mock := newMockPool(t)
	ev := scheduledSampleEvent{
		sampleEvent: sampleEvent{Base: Base{EventID: "evt-4", Aggregate: "agg-4"}},
		ok:          false,
	}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs("minuta", "agg-4", "minuta.revised", pgxmock.AnyArg(), "evt-4", "", (*time.Time)(nil), int16(1)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := NewOutbox().Publish(context.Background(), mock, ev); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// p0SampleEvent is a sampleEvent stand-in whose Type() is one of priorityFor's exact
// P0 (interactive) types, so Publish/PublishBatch write priority=0 for it — the
// write-path counterpart to the P1 default exercised by sampleEvent above.
type p0SampleEvent struct {
	Base
	Foo string `json:"foo"`
}

func (p0SampleEvent) AggregateType() string { return "notification" }
func (p0SampleEvent) Type() string          { return "notification.requested" }

var _ Event = p0SampleEvent{}

// A P0 (interactive) event type writes priority=0, proving the write path calls
// priorityFor with the event's own Type() rather than hardcoding the P1 default.
func TestOutbox_Publish_P0Event_WritesPriorityZero(t *testing.T) {
	mock := newMockPool(t)
	ev := p0SampleEvent{Base: Base{EventID: "evt-5", Aggregate: "agg-5"}}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs("notification", "agg-5", "notification.requested", pgxmock.AnyArg(), "evt-5", "", (*time.Time)(nil), int16(0)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := NewOutbox().Publish(context.Background(), mock, ev); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// batchPriorities is a pgxmock.Argument that decodes the jsonb_to_recordset payload
// PublishBatch sends and asserts each row's priority, in order — the batch path's
// counterpart to the plain-args assertions above (the single $1::jsonb arg can't be
// asserted with WithArgs' exact-value matching, since it embeds the marshaled
// payload bytes for each event).
type batchPriorities struct {
	want []int16
}

func (b batchPriorities) Match(v interface{}) bool {
	raw, ok := v.([]byte)
	if !ok {
		return false
	}
	var rows []outboxRowJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return false
	}
	if len(rows) != len(b.want) {
		return false
	}
	for i, row := range rows {
		if row.Priority != b.want[i] {
			return false
		}
	}
	return true
}

// PublishBatch writes priority PER ROW (not one batch-wide value): a P0 event and a
// P1 event in the same batch each land with their own priorityFor(ev.Type()) result.
func TestOutbox_PublishBatch_WritesPriorityPerRow(t *testing.T) {
	mock := newMockPool(t)
	evs := []Event{
		p0SampleEvent{Base: Base{EventID: "evt-6", Aggregate: "agg-6"}},           // P0
		sampleEvent{Base: Base{EventID: "evt-7", Aggregate: "agg-7"}, Foo: "bar"}, // P1
	}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs(batchPriorities{want: []int16{0, 1}}).
		WillReturnResult(pgxmock.NewResult("INSERT", 2))

	if err := NewOutbox().PublishBatch(context.Background(), mock, evs); err != nil {
		t.Fatalf("PublishBatch() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
