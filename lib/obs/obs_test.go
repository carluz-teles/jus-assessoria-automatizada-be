package obs_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/obs"
)

// newRecorder installs a global TracerProvider backed by an in-memory recorder
// and returns it. obs.Tracer reads the GLOBAL provider, so the global install is
// what makes obs.Start/Record observable. Tests here are not parallel: they
// share that single global.
func newRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return sr
}

func TestStart(t *testing.T) {
	sr := newRecorder(t)

	ctx, span := obs.Start(
		context.Background(),
		"acquisition.sync_requested process",
		trace.WithSpanKind(trace.SpanKindConsumer),
	)
	// Start must carry the span in the returned context so downstream code
	// (otelpgx, use cases) parents onto it.
	if trace.SpanFromContext(ctx) != span {
		t.Fatal("Start did not put the span in the returned context")
	}
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if got := ended[0].Name(); got != "acquisition.sync_requested process" {
		t.Errorf("span name = %q, want the event process name", got)
	}
	if got := ended[0].SpanKind(); got != trace.SpanKindConsumer {
		t.Errorf("span kind = %v, want Consumer", got)
	}
}

func TestRecord(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCode     codes.Code
		wantExcEvent bool
	}{
		{name: "success sets Ok", err: nil, wantCode: codes.Ok, wantExcEvent: false},
		{name: "failure sets Error and records the exception", err: errors.New("boom"), wantCode: codes.Error, wantExcEvent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := newRecorder(t)

			_, span := obs.Start(context.Background(), "op")
			obs.Record(span, tt.err)
			span.End()

			ended := sr.Ended()
			if len(ended) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(ended))
			}
			s := ended[0]

			if got := s.Status().Code; got != tt.wantCode {
				t.Errorf("status code = %v, want %v", got, tt.wantCode)
			}
			if tt.err != nil && s.Status().Description != tt.err.Error() {
				t.Errorf("status description = %q, want %q", s.Status().Description, tt.err.Error())
			}

			var gotExc bool
			for _, ev := range s.Events() {
				if ev.Name == "exception" {
					gotExc = true
				}
			}
			if gotExc != tt.wantExcEvent {
				t.Errorf("exception event present = %v, want %v", gotExc, tt.wantExcEvent)
			}
		})
	}
}
