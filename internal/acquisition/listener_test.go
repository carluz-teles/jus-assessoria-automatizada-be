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

// spySyncUC is the sync-side counterpart: it records the decoded sync_requested
// event and returns a preset error.
type spySyncUC struct {
	got  SyncRequested
	call int
	err  error
}

func (s *spySyncUC) OnSyncRequested(_ context.Context, ev SyncRequested) error {
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
	l := NewListener(spy, nil)
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
	l := NewListener(spy, nil)
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

// U1: a well-formed sync_requested task is decoded and dispatched to the sync use
// case with its payload intact.
func TestListener_HandleSyncRequested_Dispatches(t *testing.T) {
	t.Parallel()

	ev := SyncRequested{
		Base:          events.Base{EventID: "evt-7", Aggregate: "job-7"},
		BackfillJobID: "job-7",
		TenantID:      "tenant-7",
		IntegrationID: "integ-7",
		SliceIndex:    3,
		WindowFrom:    "2024-01-01",
		WindowTo:      "2024-01-08",
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	spy := &spySyncUC{}
	l := NewListener(nil, spy)
	task := asynq.NewTask(TypeSyncRequested, payload)

	if err := l.handleSyncRequested(context.Background(), task); err != nil {
		t.Fatalf("handleSyncRequested() error = %v", err)
	}
	if spy.call != 1 {
		t.Fatalf("use case calls = %d, want 1", spy.call)
	}
	if spy.got.EventID != "evt-7" || spy.got.IntegrationID != "integ-7" || spy.got.SliceIndex != 3 {
		t.Fatalf("dispatched event = %+v, payload lost", spy.got)
	}
}

// U2: a malformed sync_requested payload wraps asynq.SkipRetry and never reaches
// the use case.
func TestListener_HandleSyncRequested_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spySyncUC{}
	l := NewListener(nil, spy)
	task := asynq.NewTask(TypeSyncRequested, []byte("{not json"))

	err := l.handleSyncRequested(context.Background(), task)
	if err == nil {
		t.Fatal("handleSyncRequested() error = nil, want decode failure")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("error = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.call != 0 {
		t.Fatalf("use case reached %d times on a bad payload, want 0", spy.call)
	}
}
