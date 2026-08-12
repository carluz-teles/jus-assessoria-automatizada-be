package events

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"go.opentelemetry.io/otel/trace"
)

// Publish inserts one row with the seven envelope columns in order, capturing a
// non-empty trace_context from the active span so the relay can replay the hop. A
// plain event does not opt into future delivery, so process_at is NULL.
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
		WithArgs("minuta", "agg-2", "minuta.revised", pgxmock.AnyArg(), "evt-2", "", (*time.Time)(nil)).
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
		WithArgs("minuta", "agg-3", "minuta.revised", pgxmock.AnyArg(), "evt-3", "", &at).
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
		WithArgs("minuta", "agg-4", "minuta.revised", pgxmock.AnyArg(), "evt-4", "", (*time.Time)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := NewOutbox().Publish(context.Background(), mock, ev); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
