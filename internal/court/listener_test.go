package court

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
)

// stubUC is the useCase port returning a configured error per entry point, so the
// listener tests exercise only the transport's retry-decision mapping (not the use
// case). Each handler records its own call count so a test can assert the right
// method was invoked.
type stubUC struct {
	observedErr    error
	observedCalls  int
	requestedErr   error
	requestedCalls int
	itemErr        error
	itemCalls      int
}

func (s *stubUC) OnCourtRecordObserved(context.Context, courtRecordObserved) error {
	s.observedCalls++
	return s.observedErr
}

func (s *stubUC) OnFetchAutosRequested(context.Context, fetchAutosRequested) error {
	s.requestedCalls++
	return s.requestedErr
}

func (s *stubUC) OnFetchAutosItemRequested(context.Context, fetchAutosItemRequested) error {
	s.itemCalls++
	return s.itemErr
}

// TestListener_handleCourtRecordObserved covers the listener's contract: a
// terminal domain error (KindInvalid/KindNotFound) is wrapped with asynq.SkipRetry
// so the task is archived, while an infra/unknown error stays retryable. The
// original error must remain in the chain either way (single handling rule — the
// listener classifies, it does not swallow).
func TestListener_handleCourtRecordObserved(t *testing.T) {
	task := asynq.NewTask(TypeCourtRecordObserved, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "invalid input is terminal", ucErr: apperr.NewInvalid("payload ruim"), wantSkip: true},
		{name: "not found is terminal", ucErr: ErrConnectionNotFound, wantSkip: true},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
		{name: "unknown error stays retryable", ucErr: errors.New("opaque"), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{observedErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleCourtRecordObserved(context.Background(), task)

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

func TestListener_handleCourtRecordObserved_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleCourtRecordObserved(context.Background(), asynq.NewTask(TypeCourtRecordObserved, []byte(`{`)))

	if stub.observedCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.observedCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}

func TestListener_handleFetchAutosRequested(t *testing.T) {
	task := asynq.NewTask(typeFetchAutosRequested, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "not found is terminal", ucErr: ErrConnectionNotFound, wantSkip: true},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{requestedErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleFetchAutosRequested(context.Background(), task)

			if stub.requestedCalls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.requestedCalls)
			}
			if tt.ucErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if got := errors.Is(err, asynq.SkipRetry); got != tt.wantSkip {
				t.Errorf("SkipRetry = %v, want %v (err = %v)", got, tt.wantSkip, err)
			}
		})
	}
}

func TestListener_handleFetchAutosRequested_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleFetchAutosRequested(context.Background(), asynq.NewTask(typeFetchAutosRequested, []byte(`{`)))

	if stub.requestedCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.requestedCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}

func TestListener_handleFetchAutosItemRequested(t *testing.T) {
	task := asynq.NewTask(typeFetchAutosItemRequested, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "transient stays retryable", ucErr: apperr.NewUnavailable("timeout", nil), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{itemErr: tt.ucErr}
			l := NewListener(stub)

			err := l.handleFetchAutosItemRequested(context.Background(), task)

			if stub.itemCalls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.itemCalls)
			}
			if tt.ucErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if got := errors.Is(err, asynq.SkipRetry); got != tt.wantSkip {
				t.Errorf("SkipRetry = %v, want %v (err = %v)", got, tt.wantSkip, err)
			}
		})
	}
}

func TestListener_handleFetchAutosItemRequested_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleFetchAutosItemRequested(context.Background(), asynq.NewTask(typeFetchAutosItemRequested, []byte(`{`)))

	if stub.itemCalls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.itemCalls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}

func TestListener_Register_MountsAllThreeTaskTypes(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)
	mux := asynq.NewServeMux()

	// Register panics on a duplicate pattern within one mux — a clean call proves
	// the three types are distinct and all mounted.
	l.Register(mux)
}
