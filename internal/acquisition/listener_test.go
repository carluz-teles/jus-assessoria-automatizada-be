package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// spyListenerUC records the event the listener decoded and hands back a preset
// error, so the test can assert the decode+dispatch wiring in isolation.
type spyListenerUC struct {
	got  IntegrationActivated
	call int
	err  error
}

func (s *spyListenerUC) OnIntegrationActivated(_ context.Context, ev IntegrationActivated) error {
	s.call++
	s.got = ev
	return s.err
}

// A well-formed integration_activated task is decoded and dispatched to the use
// case with its payload intact.
func TestListener_HandleIntegrationActivated_Dispatches(t *testing.T) {
	t.Parallel()

	ev := IntegrationActivated{
		Base:          events.Base{EventID: "evt-42", Aggregate: "integ-9"},
		IntegrationID: "integ-9",
		TenantID:      "tenant-9",
		Source:        SourceDJEN,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	spy := &spyListenerUC{}
	l := NewListener(spy)
	task := asynq.NewTask(TypeIntegrationActivated, payload)

	if err := l.handleIntegrationActivated(context.Background(), task); err != nil {
		t.Fatalf("handleIntegrationActivated() error = %v", err)
	}
	if spy.call != 1 {
		t.Fatalf("use case calls = %d, want 1", spy.call)
	}
	if spy.got.EventID != "evt-42" || spy.got.IntegrationID != "integ-9" || spy.got.TenantID != "tenant-9" {
		t.Fatalf("dispatched event = %+v, payload lost", spy.got)
	}
}

// A malformed payload never becomes valid on retry: Decode wraps
// asynq.SkipRetry, and the use case is never reached.
func TestListener_HandleIntegrationActivated_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyListenerUC{}
	l := NewListener(spy)
	task := asynq.NewTask(TypeIntegrationActivated, []byte("{not json"))

	err := l.handleIntegrationActivated(context.Background(), task)
	if err == nil {
		t.Fatal("handleIntegrationActivated() error = nil, want decode failure")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("error = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.call != 0 {
		t.Fatalf("use case reached %d times on a bad payload, want 0", spy.call)
	}
}
