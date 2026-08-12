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
	reminderErr    error
	reminderCalls  int
	missedErr      error
	missedCalls    int
}

func (s *stubUC) OnIntimationObserved(context.Context, IntimationObserved) error {
	s.observedCalls++
	return s.observedErr
}

func (s *stubUC) OnIntimationCancelled(context.Context, IntimationCancelled) error {
	s.cancelledCalls++
	return s.cancelledErr
}

func (s *stubUC) OnReminderCheck(context.Context, DeadlineReminderCheck) error {
	s.reminderCalls++
	return s.reminderErr
}

func (s *stubUC) OnMissedCheck(context.Context, DeadlineMissedCheck) error {
	s.missedCalls++
	return s.missedErr
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

// TestListener_handleReminderCheck covers the fired-mark contract: a gone prazo
// (ErrDeadlineNotFound, KindNotFound) is terminal → SkipRetry; an infra error stays
// retryable; both keep the original error in the chain.
func TestListener_handleReminderCheck(t *testing.T) {
	task := asynq.NewTask(TypeDeadlineReminderCheck, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "gone prazo is terminal", ucErr: ErrDeadlineNotFound, wantSkip: true},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{reminderErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleReminderCheck(context.Background(), task)

			if stub.reminderCalls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.reminderCalls)
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

// TestListener_handleReminderCheck_decodeFault proves a malformed mark payload is archived
// at decode, before the use case runs.
func TestListener_handleReminderCheck_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleReminderCheck(context.Background(), asynq.NewTask(TypeDeadlineReminderCheck, []byte(`{`)))

	if stub.reminderCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.reminderCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}

// TestListener_handleMissedCheck covers the carência-mark contract: every non-OPEN case is
// an in-use-case nil no-op, so only an infra error surfaces (retryable), and it stays in
// the chain.
func TestListener_handleMissedCheck(t *testing.T) {
	task := asynq.NewTask(TypeDeadlineMissedCheck, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{missedErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleMissedCheck(context.Background(), task)

			if stub.missedCalls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.missedCalls)
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

// TestListener_handleMissedCheck_decodeFault proves a malformed carência payload is archived
// at decode, before the use case runs.
func TestListener_handleMissedCheck_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleMissedCheck(context.Background(), asynq.NewTask(TypeDeadlineMissedCheck, []byte(`{`)))

	if stub.missedCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.missedCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}
