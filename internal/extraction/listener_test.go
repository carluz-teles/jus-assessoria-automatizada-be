package extraction

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/internal/document"
	"github.com/jusassessoria/platform/lib/apperr"
)

// stubUC returns a configured error and records its call count, so the listener tests
// exercise only the transport's retry-decision mapping (not the use case).
type stubUC struct {
	err   error
	calls int
}

func (s *stubUC) OnDocumentUploaded(context.Context, DocumentUploaded) error {
	s.calls++
	return s.err
}

// TestListener_handleDocumentUploaded covers the listener's contract: a terminal domain error
// (KindInvalid/KindNotFound) is wrapped with asynq.SkipRetry so the task is archived, while an
// infra/unknown error stays retryable. The original error stays in the chain either way.
func TestListener_handleDocumentUploaded(t *testing.T) {
	task := asynq.NewTask(document.TypeDocumentUploaded, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "corrupt pdf is terminal", ucErr: apperr.NewInvalid("open pdf"), wantSkip: true},
		{name: "missing doc is terminal", ucErr: apperr.NewNotFound("gone"), wantSkip: true},
		{name: "infra error stays retryable", ucErr: apperr.NewInfra("api down", errors.New("boom")), wantSkip: false},
		{name: "unknown error stays retryable", ucErr: errors.New("opaque"), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubUC{err: tt.ucErr}
			l := NewListener(stub)

			err := l.handleDocumentUploaded(context.Background(), task)

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

// TestListener_handleDocumentUploaded_decodeFault proves a malformed payload is archived
// (SkipRetry) at decode, before the use case is ever invoked.
func TestListener_handleDocumentUploaded_decodeFault(t *testing.T) {
	stub := &stubUC{}
	l := NewListener(stub)

	err := l.handleDocumentUploaded(context.Background(), asynq.NewTask(document.TypeDocumentUploaded, []byte(`{`)))

	if stub.calls != 0 {
		t.Errorf("use case called on decode fault (calls = %d)", stub.calls)
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("decode fault not archived: %v", err)
	}
}

// TestListener_Register mounts the handler on the document.uploaded type — a smoke check that
// registration does not panic and the mux accepts the type.
func TestListener_Register(t *testing.T) {
	NewListener(&stubUC{}).Register(asynq.NewServeMux())
}
