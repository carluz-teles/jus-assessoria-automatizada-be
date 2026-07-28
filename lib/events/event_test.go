package events

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TestMain installs the W3C TraceContext propagator once for the whole package —
// the same propagator lib/telemetry.Setup installs in production — so the trace
// hop (Inject/Extract) works without booting telemetry.
func TestMain(m *testing.M) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}

// sampleEvent is a stand-in domain event used across the package's tests. It
// embeds Base for the id/aggregate fields and pins the two constant methods.
type sampleEvent struct {
	Base
	Foo string `json:"foo"`
}

func (sampleEvent) AggregateType() string { return "minuta" }
func (sampleEvent) Type() string          { return "minuta.revised" }

// Compile-time proof sampleEvent (and thus the Base embedding) satisfies Event.
var _ Event = sampleEvent{}

// newMockPool builds a pgxmock pool shared by the DB-less tests in this package.
// It satisfies both database.Tx (Exec/Query/QueryRow) and the Dedup execer seam.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	t.Cleanup(mock.Close)

	return mock
}

// spanContext is a deterministic sampled SpanContext whose traceparent is known,
// so tests can assert the exact serialized value.
func spanContext(t *testing.T) trace.SpanContext {
	t.Helper()

	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
}

// wantTraceparent is the traceparent produced by spanContext under the W3C format.
const wantTraceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

func TestBase_AccessorsAndSerialization(t *testing.T) {
	ev := sampleEvent{Base: Base{EventID: "evt-1", Aggregate: "agg-1"}, Foo: "bar"}

	if got := ev.AggregateID(); got != "agg-1" {
		t.Errorf("AggregateID() = %q, want %q", got, "agg-1")
	}
	if got := ev.IdempotencyKey(); got != "evt-1" {
		t.Errorf("IdempotencyKey() = %q, want %q", got, "evt-1")
	}
	if got := ev.AggregateType(); got != "minuta" {
		t.Errorf("AggregateType() = %q, want %q", got, "minuta")
	}
	if got := ev.Type(); got != "minuta.revised" {
		t.Errorf("Type() = %q, want %q", got, "minuta.revised")
	}

	// The id/aggregate fields must survive the JSON round-trip so the consumer can
	// read the idempotency key back off the decoded event.
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back sampleEvent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.IdempotencyKey() != "evt-1" || back.AggregateID() != "agg-1" || back.Foo != "bar" {
		t.Errorf("round-trip lost data: %+v", back)
	}
}
