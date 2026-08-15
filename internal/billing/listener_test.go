package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

// spyUseCase records the decoded event and returns a preset error, so the tests
// can assert the listener's decode+dispatch+retry-classification wiring in
// isolation from the use case's own business logic (covered in domain_test.go).
type spyUseCase struct {
	provisioned  TenantProvisioned
	provisionedN int
	provisionErr error
	check        TrialEndingSoonCheck
	checkN       int
	checkErr     error
}

func (s *spyUseCase) OnTenantProvisioned(_ context.Context, ev TenantProvisioned) error {
	s.provisionedN++
	s.provisioned = ev
	return s.provisionErr
}

func (s *spyUseCase) OnTrialEndingSoonCheck(_ context.Context, ev TrialEndingSoonCheck) error {
	s.checkN++
	s.check = ev
	return s.checkErr
}

// A well-formed identity.tenant_provisioned task is decoded and dispatched with
// its payload intact.
func TestListener_HandleTenantProvisioned_Dispatches(t *testing.T) {
	t.Parallel()

	ev := TenantProvisioned{Base: events.Base{EventID: "tenant-provisioned:tenant-1"}, TenantID: "tenant-1"}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	spy := &spyUseCase{}
	l := NewListener(spy)
	if err := l.handleTenantProvisioned(context.Background(), asynq.NewTask(TypeTenantProvisioned, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if spy.provisionedN != 1 {
		t.Fatalf("use case called %d times, want 1", spy.provisionedN)
	}
	if spy.provisioned.TenantID != "tenant-1" || spy.provisioned.EventID != ev.EventID {
		t.Fatalf("dispatched event = %+v", spy.provisioned)
	}
}

// A malformed payload can never succeed on retry — the decode error wraps
// asynq.SkipRetry (archived), and the use case is never called.
func TestListener_HandleTenantProvisioned_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyUseCase{}
	l := NewListener(spy)

	err := l.handleTenantProvisioned(context.Background(), asynq.NewTask(TypeTenantProvisioned, []byte("{bad")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.provisionedN != 0 {
		t.Fatalf("use case called %d times on a decode fault, want 0", spy.provisionedN)
	}
}

// A terminal domain error (no default trial policy — a catalog misconfiguration a
// retry cannot heal) wraps asynq.SkipRetry so the task archives instead of
// burning the retry budget.
func TestListener_HandleTenantProvisioned_TerminalErrorSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyUseCase{provisionErr: ErrNoDefaultTrialPolicy}
	l := NewListener(spy)

	payload, err := json.Marshal(TenantProvisioned{Base: events.Base{EventID: "e"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = l.handleTenantProvisioned(context.Background(), asynq.NewTask(TypeTenantProvisioned, payload))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if !errors.Is(err, ErrNoDefaultTrialPolicy) {
		t.Fatalf("err = %v, want it to still wrap ErrNoDefaultTrialPolicy", err)
	}
}

// A retryable (infra) use-case error propagates unchanged.
func TestListener_HandleTenantProvisioned_InfraErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := apperr.NewInfra("db down", errors.New("boom"))
	spy := &spyUseCase{provisionErr: sentinel}
	l := NewListener(spy)

	payload, err := json.Marshal(TenantProvisioned{Base: events.Base{EventID: "e"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = l.handleTenantProvisioned(context.Background(), asynq.NewTask(TypeTenantProvisioned, payload))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the use-case error unwrapped (retryable)", err)
	}
	if errors.Is(err, asynq.SkipRetry) {
		t.Fatal("an infra error must stay retryable, got asynq.SkipRetry")
	}
}

// A well-formed trial_ending_soon_check task is decoded and dispatched with its
// payload intact.
func TestListener_HandleTrialEndingSoonCheck_Dispatches(t *testing.T) {
	t.Parallel()

	ev := TrialEndingSoonCheck{
		Base: events.Base{EventID: "trial-ending-soon:sub-1"}, TenantID: "tenant-1", SubscriptionID: "sub-1",
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	spy := &spyUseCase{}
	l := NewListener(spy)
	if err := l.handleTrialEndingSoonCheck(context.Background(), asynq.NewTask(TypeTrialEndingSoonCheck, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if spy.checkN != 1 {
		t.Fatalf("use case called %d times, want 1", spy.checkN)
	}
	if spy.check.TenantID != "tenant-1" || spy.check.SubscriptionID != "sub-1" {
		t.Fatalf("dispatched event = %+v", spy.check)
	}
}

// A malformed trial_ending_soon_check payload wraps asynq.SkipRetry (archived),
// and the use case is never called.
func TestListener_HandleTrialEndingSoonCheck_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyUseCase{}
	l := NewListener(spy)

	err := l.handleTrialEndingSoonCheck(context.Background(), asynq.NewTask(TypeTrialEndingSoonCheck, []byte("{bad")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.checkN != 0 {
		t.Fatalf("use case called %d times on a decode fault, want 0", spy.checkN)
	}
}
