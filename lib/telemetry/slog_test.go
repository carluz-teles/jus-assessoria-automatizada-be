package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/telemetry"
)

// Deterministic ids so assertions can compare against exact strings.
const (
	wantTraceID = "0102030405060708090a0b0c0d0e0f10"
	wantSpanID  = "0102030405060708"
)

// ctxWithSpan returns a context carrying a valid, sampled SpanContext built
// from the deterministic ids above.
func ctxWithSpan(t *testing.T) context.Context {
	t.Helper()

	traceID, err := trace.TraceIDFromHex(wantTraceID)
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := trace.SpanIDFromHex(wantSpanID)
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// findString walks a decoded JSON object (including nested groups) and returns
// the first string value found under key, so tests stay robust to grouping.
func findString(m map[string]any, key string) (string, bool) {
	for k, v := range m {
		switch val := v.(type) {
		case string:
			if k == key {
				return val, true
			}
		case map[string]any:
			if s, ok := findString(val, key); ok {
				return s, true
			}
		}
	}
	return "", false
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	m := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal log line %q: %v", buf.String(), err)
	}
	return m
}

func TestNewLogger_TraceCorrelation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		withSpan  bool
		wantTrace bool
	}{
		{name: "valid span injects ids", withSpan: true, wantTrace: true},
		{name: "no span omits ids", withSpan: false, wantTrace: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			buf := &bytes.Buffer{}
			logger := telemetry.NewLogger(buf, slog.LevelInfo)

			ctx := context.Background()
			if tt.withSpan {
				ctx = ctxWithSpan(t)
			}
			logger.InfoContext(ctx, "hello")

			m := decode(t, buf)

			gotTrace, hasTrace := findString(m, "trace_id")
			gotSpan, hasSpan := findString(m, "span_id")

			if hasTrace != tt.wantTrace || hasSpan != tt.wantTrace {
				t.Fatalf("presence: trace_id=%v span_id=%v, want both %v", hasTrace, hasSpan, tt.wantTrace)
			}
			if !tt.wantTrace {
				return
			}
			if gotTrace != wantTraceID {
				t.Errorf("trace_id = %q, want %q", gotTrace, wantTraceID)
			}
			if gotSpan != wantSpanID {
				t.Errorf("span_id = %q, want %q", gotSpan, wantSpanID)
			}
		})
	}
}

// TestTraceHandler_WithAttrs verifies attrs added via WithAttrs coexist with
// injected trace ids on the derived logger.
func TestTraceHandler_WithAttrs(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := telemetry.NewLogger(buf, slog.LevelInfo).With("component", "repo")

	logger.InfoContext(ctxWithSpan(t), "hello")

	m := decode(t, buf)

	if got, ok := findString(m, "trace_id"); !ok || got != wantTraceID {
		t.Errorf("trace_id = %q (present=%v), want %q", got, ok, wantTraceID)
	}
	if got, ok := findString(m, "component"); !ok || got != "repo" {
		t.Errorf("component = %q (present=%v), want %q", got, ok, "repo")
	}
}

// TestTraceHandler_WithGroup verifies trace ids still propagate through a
// logger derived with WithGroup (the ids nest under the group, which findString
// resolves recursively).
func TestTraceHandler_WithGroup(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	logger := telemetry.NewLogger(buf, slog.LevelInfo).WithGroup("request")

	logger.InfoContext(ctxWithSpan(t), "hello")

	m := decode(t, buf)

	if got, ok := findString(m, "trace_id"); !ok || got != wantTraceID {
		t.Errorf("trace_id = %q (present=%v), want %q", got, ok, wantTraceID)
	}
	if got, ok := findString(m, "span_id"); !ok || got != wantSpanID {
		t.Errorf("span_id = %q (present=%v), want %q", got, ok, wantSpanID)
	}
}
