package draft

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the draft slice's async consumer surface for Fatia 3. It handles
// draft.generation_requested messages from the "ai" queue, delegating to the
// GenerateUseCase. The pattern is identical to internal/deadline/listener.go.

// generateUseCase is the narrow port the Listener delegates to.
type generateUseCase interface {
	OnGenerationRequested(ctx context.Context, ev GenerationRequested) error
}

// Listener is the draft slice's asynq consumer for the AI generation pipeline.
type Listener struct {
	uc generateUseCase
}

// NewListener wires the listener to the generation use case.
func NewListener(uc generateUseCase) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the draft.generation_requested handler on the asynq mux.
// Called by cmd/worker-ai composition (one line per Register call, per the convention).
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeGenerationRequested, l.handleGenerationRequested)
}

// handleGenerationRequested decodes the payload and delegates to the use case,
// mapping the outcome to asynq's retry decision exactly as the deadline listener does.
// A decode error is already SkipRetry (events.Decode wraps it). A terminal use-case
// error (obsolete saga, generator nil, parse failure) is wrapped with SkipRetry.
// Transient errors (infra, unavailable) are retryable.
func (l *Listener) handleGenerationRequested(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[GenerationRequested](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnGenerationRequested(ctx, ev); err != nil {
		if isGenerationTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// isGenerationTerminal reports whether the use-case error can never succeed on retry.
// Invalid (obsolete saga, nil generator, parse failure) are terminal; infra/unavailable
// stay retryable.
func isGenerationTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindNotFound
}
