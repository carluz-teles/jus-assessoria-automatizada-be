package deadline

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
)

// stubUC is the useCase port returning a configured error per entry point, so the listener
// tests exercise only the transport's retry-decision mapping (not the use case). Each
// handler records its own call count so a test can assert the right method was invoked.
type stubUC struct {
	observedErr    error
	observedCalls  int
	cancelledErr   error
	cancelledCalls int
}

func (s *stubUC) OnIntimationObserved(context.Context, IntimationObserved) error {
	s.observedCalls++
	return s.observedErr
}

func (s *stubUC) OnIntimationCancelled(context.Context, IntimationCancelled) error {
	s.cancelledCalls++
	return s.cancelledErr
}

// TestListener_handleIntimationObserved covers the listener's contract: a terminal
// domain error (KindInvalid/KindNotFound) is wrapped with asynq.SkipRetry so the task
// is archived, while an infra/unknown error stays retryable. The original error must
// remain in the chain either way (single handling rule — the listener classifies, it
// does not swallow).
func TestListener_handleIntimationObserved(t *testing.T) {
	// a well-formed payload so the failure under test comes from the use case, not decode.
	task := asynq.NewTask(TypeIntimationObserved, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "malformed anchor is terminal", ucErr: apperr.NewInvalid("bad date"), wantSkip: true},
		{name: "missing court record is terminal", ucErr: ErrCourtRecordNotFound, wantSkip: true},
		{name: "missing rule is terminal", ucErr: ErrRuleNotFound, wantSkip: true},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
		{name: "unknown error stays retryable", ucErr: errors.New("opaque"), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{observedErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleIntimationObserved(context.Background(), task)

			if stub.observedCalls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.observedCalls)
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

// TestListener_handleIntimationObserved_decodeFault proves a malformed payload is
// archived (SkipRetry) at decode, before the use case is ever invoked.
func TestListener_handleIntimationObserved_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleIntimationObserved(context.Background(), asynq.NewTask(TypeIntimationObserved, []byte(`{`)))

	if stub.observedCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.observedCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}

// TestListener_handleIntimationCancelled mirrors the observed contract for the revocation
// handler: an infra/unknown error stays retryable while the original stays in the chain
// (single handling rule). A successful revoke (including the no-op, which the use case
// returns as nil) acks. isTerminal is shared, so a terminal domain error would archive —
// but the revoke path never produces one (ErrDeadlineNotFound is a nil no-op upstream).
func TestListener_handleIntimationCancelled(t *testing.T) {
	task := asynq.NewTask(TypeIntimationCancelled, []byte(`{}`))

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
			stub := &stubUC{cancelledErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleIntimationCancelled(context.Background(), task)

			if stub.cancelledCalls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.cancelledCalls)
			}
			if stub.observedCalls != 0 {
				t.Errorf("observed handler invoked on a cancelled task (calls = %d)", stub.observedCalls)
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

// TestListener_handleIntimationCancelled_decodeFault proves a malformed cancelled payload
// is archived (SkipRetry) at decode, before the use case is ever invoked.
func TestListener_handleIntimationCancelled_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleIntimationCancelled(context.Background(), asynq.NewTask(TypeIntimationCancelled, []byte(`{`)))

	if stub.cancelledCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.cancelledCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}
