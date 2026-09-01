package actionitem

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
)

// stubUC is the useCase port fake returning a configured error, recording call count.
type stubUC struct {
	err   error
	calls int

	taskCreatedErr   error
	taskCreatedCalls int
}

func (s *stubUC) OnIntimationAnalyzed(context.Context, IntimationAnalyzed) error {
	s.calls++
	return s.err
}

func (s *stubUC) OnTaskCreated(context.Context, TaskCreated) error {
	s.taskCreatedCalls++
	return s.taskCreatedErr
}

// TestListener_handleIntimationAnalyzed covers the listener's contract: a terminal domain
// error (KindInvalid — a bug in this slice's own candidate construction) is wrapped with
// asynq.SkipRetry so the task is archived, while an infra/unknown error stays retryable.
// The original error must remain in the chain either way (single handling rule).
func TestListener_handleIntimationAnalyzed(t *testing.T) {
	task := asynq.NewTask(TypeIntimationAnalyzed, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "invalid candidate is terminal", ucErr: apperr.NewInvalid("bad candidate"), wantSkip: true},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
		{name: "not-found error stays retryable (not classified terminal here)", ucErr: apperr.NewNotFound("gone"), wantSkip: false},
		{name: "unknown error stays retryable", ucErr: errors.New("opaque"), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{err: tt.ucErr}
			l := NewListener(stub)

			err := l.handleIntimationAnalyzed(context.Background(), task)

			if stub.calls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.calls)
			}
			if tt.ucErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.ucErr) {
				t.Errorf("original error dropped from chain: %v", err)
			}
			if got := errors.Is(err, asynq.SkipRetry); got != tt.wantSkip {
				t.Errorf("SkipRetry = %v, want %v (err = %v)", got, tt.wantSkip, err)
			}
		})
	}
}

// TestListener_handleIntimationAnalyzed_decodeFault proves a malformed payload is archived
// (SkipRetry) at decode, before the use case is ever invoked.
func TestListener_handleIntimationAnalyzed_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleIntimationAnalyzed(context.Background(), asynq.NewTask(TypeIntimationAnalyzed, []byte(`{`)))

	if stub.calls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.calls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}

// TestListener_handleTaskCreated covers the fatia 3 reverse-pointer handler's contract: an
// infra/unknown error stays retryable (the domain never surfaces ErrActionItemNotFound past
// OnTaskCreated — it is absorbed as a no-op there), and the original error stays in the chain.
func TestListener_handleTaskCreated(t *testing.T) {
	task := asynq.NewTask(TypeTaskCreated, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
		{name: "unknown error stays retryable", ucErr: errors.New("opaque"), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{taskCreatedErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleTaskCreated(context.Background(), task)

			if stub.taskCreatedCalls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.taskCreatedCalls)
			}
			if tt.ucErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.ucErr) {
				t.Errorf("original error dropped from chain: %v", err)
			}
			if got := errors.Is(err, asynq.SkipRetry); got != tt.wantSkip {
				t.Errorf("SkipRetry = %v, want %v (err = %v)", got, tt.wantSkip, err)
			}
		})
	}
}

// TestListener_handleTaskCreated_decodeFault proves a malformed payload is archived (SkipRetry)
// at decode, before the use case is ever invoked.
func TestListener_handleTaskCreated_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleTaskCreated(context.Background(), asynq.NewTask(TypeTaskCreated, []byte(`{`)))

	if stub.taskCreatedCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.taskCreatedCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}
