package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/obs"
)

// newRecorder installs a global TracerProvider backed by an in-memory recorder so
// the consumer span obs.Start opens (via the global provider) is observable.
func newRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return sr
}

func TestObserve(t *testing.T) {
	tests := []struct {
		name       string
		handlerErr error
		wantCode   codes.Code
		wantLog    bool // a failure line is emitted only when the handler fails
	}{
		{name: "success opens the span and stays quiet", handlerErr: nil, wantCode: codes.Ok, wantLog: false},
		{name: "failure sets Error status and logs one line", handlerErr: errors.New("boom"), wantCode: codes.Error, wantLog: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := newRecorder(t)
			buf := &bytes.Buffer{}
			logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			task := asynq.NewTaskWithHeaders(
				"acquisition.sync_requested",
				[]byte("{}"),
				map[string]string{eventIDHeader: "evt-1", aggregateIDHeader: "agg-1"},
			)
			handler := Observe(logger)(asynq.HandlerFunc(
				func(context.Context, *asynq.Task) error { return tt.handlerErr },
			))

			err := handler.ProcessTask(context.Background(), task)
			if !errors.Is(err, tt.handlerErr) {
				t.Fatalf("ProcessTask err = %v, want %v (must return so asynq keeps retry semantics)", err, tt.handlerErr)
			}

			// Span: opened as a consumer, named "<type> process", status from outcome.
			ended := sr.Ended()
			if len(ended) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(ended))
			}
			span := ended[0]
			if span.SpanKind() != trace.SpanKindConsumer {
				t.Errorf("span kind = %v, want Consumer", span.SpanKind())
			}
			if span.Name() != "acquisition.sync_requested process" {
				t.Errorf("span name = %q", span.Name())
			}
			if span.Status().Code != tt.wantCode {
				t.Errorf("span status = %v, want %v", span.Status().Code, tt.wantCode)
			}

			// Log: exactly the failure line, only on failure; carries the keys to find it.
			logged := strings.Contains(buf.String(), `"event failed"`)
			if logged != tt.wantLog {
				t.Fatalf("failure log present = %v, want %v (buf: %s)", logged, tt.wantLog, buf.String())
			}
			if tt.wantLog {
				var rec map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
					t.Fatalf("log line is not valid JSON: %v", err)
				}
				if rec[obs.KeyEventType] != "acquisition.sync_requested" {
					t.Errorf("log %s = %v, want the event type", obs.KeyEventType, rec[obs.KeyEventType])
				}
				if rec[obs.KeyEventID] != "evt-1" {
					t.Errorf("log %s = %v, want evt-1", obs.KeyEventID, rec[obs.KeyEventID])
				}
			}
		})
	}
}

func TestOutcomeFor(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		retry    int
		maxRetry int
		want     string
	}{
		{name: "skip-retry archives", err: asynq.SkipRetry, retry: 0, maxRetry: 25, want: obs.OutcomeDropped},
		{name: "wrapped skip-retry archives", err: errors.Join(errors.New("decode"), asynq.SkipRetry), retry: 0, maxRetry: 25, want: obs.OutcomeDropped},
		{name: "last attempt exhausted is dropped", err: errors.New("boom"), retry: 25, maxRetry: 25, want: obs.OutcomeDropped},
		{name: "more attempts left is retryable", err: errors.New("boom"), retry: 3, maxRetry: 25, want: obs.OutcomeRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcomeFor(tt.err, tt.retry, tt.maxRetry); got != tt.want {
				t.Errorf("outcomeFor(%v, %d, %d) = %q, want %q", tt.err, tt.retry, tt.maxRetry, got, tt.want)
			}
		})
	}
}
