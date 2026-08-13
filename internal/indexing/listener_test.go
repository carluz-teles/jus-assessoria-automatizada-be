package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

// listener_test.go covers the async surface: a decode fault is SkipRetry (archived), a terminal
// domain error is wrapped SkipRetry, an infra error stays retryable, and a happy delegate returns
// nil. The pipeline is a fake — the listener holds no persistence.

// fakePipeline records the event it was handed and returns a canned error.
type fakePipeline struct {
	got DocumentExtracted
	err error
}

func (p *fakePipeline) OnDocumentExtracted(_ context.Context, ev DocumentExtracted) error {
	p.got = ev
	return p.err
}

func taskFor(t *testing.T, ev DocumentExtracted) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return asynq.NewTask(TypeDocumentExtracted, payload)
}

func TestListener_HappyPath(t *testing.T) {
	t.Parallel()

	p := &fakePipeline{}
	l := NewListener(p)
	if err := l.handleDocumentExtracted(context.Background(), taskFor(t, extractedFixture())); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if p.got.DocumentID != testDoc {
		t.Errorf("pipeline got %q, want %q", p.got.DocumentID, testDoc)
	}
}

func TestListener_DecodeFaultIsSkipRetry(t *testing.T) {
	t.Parallel()

	l := NewListener(&fakePipeline{})
	bad := asynq.NewTask(TypeDocumentExtracted, []byte("not json"))
	err := l.handleDocumentExtracted(context.Background(), bad)
	if err == nil || !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("decode fault err = %v, want SkipRetry", err)
	}
}

func TestListener_TerminalErrorIsSkipRetry(t *testing.T) {
	t.Parallel()

	// A KindInvalid from the pipeline is terminal → wrapped SkipRetry.
	p := &fakePipeline{err: apperr.NewInvalid("bad text")}
	l := NewListener(p)
	err := l.handleDocumentExtracted(context.Background(), taskFor(t, extractedFixture()))
	if err == nil || !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("terminal err = %v, want SkipRetry", err)
	}
}

func TestListener_InfraErrorIsRetryable(t *testing.T) {
	t.Parallel()

	// A KindInfra stays retryable → NOT wrapped SkipRetry.
	p := &fakePipeline{err: apperr.NewInfra("voyage down", errors.New("boom"))}
	l := NewListener(p)
	err := l.handleDocumentExtracted(context.Background(), taskFor(t, extractedFixture()))
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, asynq.SkipRetry) {
		t.Errorf("infra error should be retryable, got SkipRetry")
	}
}

// TestListener_Register asserts the task type is mounted (a smoke check the mux wiring compiles
// and the const matches the pinned contract).
func TestListener_Register(t *testing.T) {
	t.Parallel()

	if TypeDocumentExtracted != "document.extracted" {
		t.Errorf("consumed type = %q, want document.extracted", TypeDocumentExtracted)
	}
	mux := asynq.NewServeMux()
	NewListener(&fakePipeline{}).Register(mux)
	// A handler is now registered for the type; asynq's mux has no public lookup, so just ensure
	// Register did not panic and the event contract types round-trip.
	var _ events.Event = DocumentReady{}
	var _ events.Event = DocumentFailed{}
}
