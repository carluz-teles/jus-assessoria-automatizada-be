package deadline

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
)

// stubOpenUC is the openUC port returning a configured error, so the listener test
// exercises only the transport's retry-decision mapping (not the use case).
type stubOpenUC struct {
	err   error
	calls int
}

func (s *stubOpenUC) OnIntimationObserved(context.Context, IntimationObserved) error {
	s.calls++
	return s.err
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
			stub := &stubOpenUC{err: tt.ucErr}
			l := NewListener(stub)

			err := l.handleIntimationObserved(context.Background(), task)

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

// TestListener_handleIntimationObserved_decodeFault proves a malformed payload is
// archived (SkipRetry) at decode, before the use case is ever invoked.
func TestListener_handleIntimationObserved_decodeFault(t *testing.T) {
	stub := &stubOpenUC{}
	l := NewListener(stub)

	err := l.handleIntimationObserved(context.Background(), asynq.NewTask(TypeIntimationObserved, []byte(`{`)))

	if stub.calls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.calls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}
