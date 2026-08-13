package document

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
)

// discardLogger is a slog.Logger that drops everything — used when the test asserts behaviour
// (nil-return, no panic) rather than log content.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// task builds an asynq task for a terminal event type with the given payload marshalled to JSON.
func task(t *testing.T, typ string, payload any) *asynq.Task {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return asynq.NewTask(typ, body)
}

// TestHandleReady_AcksAndLogs: a well-formed document.ready is logged (message + fields),
// returns nil (ACK — terminal, no retry), and the metric calls do not panic.
func TestHandleReady_AcksAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	obs, err := NewLifecycleObserver(logger)
	if err != nil {
		t.Fatalf("NewLifecycleObserver: %v", err)
	}

	tk := task(t, typeDocumentReady, map[string]any{
		"event_id":        "ev-1",
		"aggregate_id":    "doc-1",
		"document_id":     "doc-1",
		"tenant_id":       "tenant-1",
		"chunk_count":     42,
		"embedding_model": "voyage-3.5-lite",
	})

	if err := obs.handleReady(context.Background(), tk); err != nil {
		t.Fatalf("handleReady returned %v, want nil (ack)", err)
	}

	line := buf.String()
	for _, want := range []string{"document ready", "doc-1", "tenant-1", "chunk_count=42", "voyage-3.5-lite"} {
		if !strings.Contains(line, want) {
			t.Errorf("log missing %q; got: %s", want, line)
		}
	}
}

// TestHandleFailed_AcksAndLogs: a well-formed document.failed is logged at error level with the
// stage + error, and returns nil — a failure NOTICE must not be retried (the original bug).
func TestHandleFailed_AcksAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	obs, err := NewLifecycleObserver(logger)
	if err != nil {
		t.Fatalf("NewLifecycleObserver: %v", err)
	}

	tk := task(t, typeDocumentFailed, map[string]any{
		"event_id":     "ev-2",
		"aggregate_id": "doc-2",
		"document_id":  "doc-2",
		"tenant_id":    "tenant-2",
		"stage":        "indexing",
		"error":        "voyage timeout",
	})

	if err := obs.handleFailed(context.Background(), tk); err != nil {
		t.Fatalf("handleFailed returned %v, want nil (ack)", err)
	}

	line := buf.String()
	for _, want := range []string{"document pipeline failed", "doc-2", "indexing", "voyage timeout"} {
		if !strings.Contains(line, want) {
			t.Errorf("log missing %q; got: %s", want, line)
		}
	}
}

// TestHandlers_AckOnDecodeFault: a malformed terminal payload can never parse on retry, so both
// handlers ACK (return nil) instead of re-spamming the queue.
func TestHandlers_AckOnDecodeFault(t *testing.T) {
	obs, err := NewLifecycleObserver(discardLogger())
	if err != nil {
		t.Fatalf("NewLifecycleObserver: %v", err)
	}

	bad := asynq.NewTask(typeDocumentReady, []byte("{not json"))
	if err := obs.handleReady(context.Background(), bad); err != nil {
		t.Errorf("handleReady on bad payload = %v, want nil (ack)", err)
	}

	badFailed := asynq.NewTask(typeDocumentFailed, []byte("{not json"))
	if err := obs.handleFailed(context.Background(), badFailed); err != nil {
		t.Errorf("handleFailed on bad payload = %v, want nil (ack)", err)
	}
}

// TestRegister_MountsBothTerminalTypes: Register wires handlers for BOTH document.ready and
// document.failed — the fix for "handler not found". A ServeMux with the observer's handlers
// mounted must dispatch both types without the not-found error.
func TestRegister_MountsBothTerminalTypes(t *testing.T) {
	obs, err := NewLifecycleObserver(discardLogger())
	if err != nil {
		t.Fatalf("NewLifecycleObserver: %v", err)
	}

	mux := asynq.NewServeMux()
	obs.Register(mux)

	for _, typ := range []string{typeDocumentReady, typeDocumentFailed} {
		tk := task(t, typ, map[string]any{"document_id": "d", "tenant_id": "t"})
		if err := mux.ProcessTask(context.Background(), tk); err != nil {
			t.Errorf("mux.ProcessTask(%s) = %v, want nil (handler mounted + acks)", typ, err)
		}
	}
}
