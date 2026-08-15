package billing

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface (fatia 2, first asynq consumer this
// slice ever needed — until now billing was webhook-only, driven by
// WebhookHandler off /webhooks/stripe). It owns its task-type registration
// (Register); the worker only composes. Per task: decode with the shared codec (a
// decode fault is SkipRetry) and delegate to the use case. Trace continuation and
// the consumer span are handled once by the events.Observe middleware, not here —
// same shape as deadline/notifications' listeners.

// useCase is the port the listener delegates to — the two async entry points this
// slice drives: starting a tenant's trial (identity.tenant_provisioned) and
// re-checking it at its scheduled ETA (billing.trial_ending_soon_check).
type useCase interface {
	OnTenantProvisioned(ctx context.Context, ev TenantProvisioned) error
	OnTrialEndingSoonCheck(ctx context.Context, ev TrialEndingSoonCheck) error
}

// Listener is the billing slice's asynq consumer. It holds no transport state;
// the use case owns persistence and the transaction boundary.
type Listener struct {
	uc useCase
}

// NewListener wires the listener to the billing use case.
func NewListener(uc useCase) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog
// of Handler.RegisterV1. Both types route to the "notifications" queue (see
// lib/events' queueFor): billing has no dedicated server yet, and the work is
// light/low-volume (one check per subscription), so it shares the main worker's
// mux rather than standing up a new dedicated asynq server for it.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeTenantProvisioned, l.handleTenantProvisioned)
	mux.HandleFunc(TypeTrialEndingSoonCheck, l.handleTrialEndingSoonCheck)
}

// handleTenantProvisioned is the asynq.HandlerFunc for identity.tenant_provisioned.
// It decodes the payload into the LOCAL shape and hands off to the use case, then
// maps the outcome to asynq's retry decision. A decode error is returned as-is (it
// already wraps asynq.SkipRetry, so a malformed task is archived, not retried); a
// terminal domain error (e.g. ErrNoDefaultTrialPolicy, ErrPlanNotFound — catalog
// misconfigurations a retry cannot heal) is wrapped with SkipRetry here; every
// other error (infra/unavailable) stays retryable.
func (l *Listener) handleTenantProvisioned(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[TenantProvisioned](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnTenantProvisioned(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// handleTrialEndingSoonCheck is the asynq.HandlerFunc for the slice's own
// billing.trial_ending_soon_check: the trial's scheduled mark fired at its ETA. It
// decodes the LOCAL scheduled shape and delegates to the re-check use case, mapping
// the outcome to asynq's retry decision the same way. Every obsolete-state case
// (converted, canceled, window missed) is an in-use-case no-op that returns nil.
func (l *Listener) handleTrialEndingSoonCheck(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[TrialEndingSoonCheck](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnTrialEndingSoonCheck(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// isTerminal reports whether a use-case error can never succeed on redelivery, so
// the listener archives the task (asynq.SkipRetry) instead of burning the retry
// budget — mirroring internal/deadline/listener.go's identical classification:
//   - KindInvalid — a malformed input a retry re-parses unchanged.
//   - KindNotFound — a missing trial_policy default or Starter plan: a broken
//     catalog seed, not a race any redelivery heals on its own.
//
// Everything else (KindInfra, KindUnavailable) stays retryable.
func isTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindNotFound
}
