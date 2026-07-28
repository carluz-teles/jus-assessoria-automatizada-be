package events

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"go.opentelemetry.io/otel/trace"
)

// Publish inserts one row with the six envelope columns in order, capturing a
// non-empty trace_context from the active span so the relay can replay the hop.
func TestOutbox_Publish_InsertsRowWithTrace(t *testing.T) {
	mock := newMockPool(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext(t))

	ev := sampleEvent{Base: Base{EventID: "evt-1", Aggregate: "agg-1"}, Foo: "bar"}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs(
			"minuta",         // aggregate_type
			"agg-1",          // aggregate_id
			"minuta.revised", // type
			pgxmock.AnyArg(), // payload (JSON of ev)
			"evt-1",          // idempotency_key
			wantTraceparent,  // trace_context — non-empty, from the span
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
// begins a fresh trace at the consumer.
func TestOutbox_Publish_NoSpan_EmptyTrace(t *testing.T) {
	mock := newMockPool(t)
	ev := sampleEvent{Base: Base{EventID: "evt-2", Aggregate: "agg-2"}}

	mock.
		ExpectExec("INSERT INTO outbox").
		WithArgs("minuta", "agg-2", "minuta.revised", pgxmock.AnyArg(), "evt-2", "").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := NewOutbox().Publish(context.Background(), mock, ev); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
