package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// spyNotifyUC records the decoded event and returns a preset error, so the test can
// assert the decode+dispatch wiring in isolation from the use case.
type spyNotifyUC struct {
	got  NotificationRequested
	call int
	err  error
}

func (s *spyNotifyUC) OnNotificationRequested(_ context.Context, ev NotificationRequested) error {
	s.call++
	s.got = ev
	return s.err
}

// A well-formed notification.requested task is decoded and dispatched to the use case
// with its payload intact.
func TestListener_HandleNotificationRequested_Dispatches(t *testing.T) {
	t.Parallel()

	ev := NotificationRequested{
		Base:            events.Base{EventID: "evt-7", Aggregate: "tenant-7"},
		TenantID:        "tenant-7",
		RecipientUserID: "user-7",
		Type:            "member_joined",
		Payload:         map[string]any{"org_name": "Escritório 7"},
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	spy := &spyNotifyUC{}
	l := NewListener(spy)
	if err := l.handleNotificationRequested(context.Background(), asynq.NewTask(TypeNotificationRequested, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if spy.call != 1 {
		t.Fatalf("use case called %d times, want 1", spy.call)
	}
	if spy.got.TenantID != "tenant-7" || spy.got.RecipientUserID != "user-7" || spy.got.Type != "member_joined" {
		t.Fatalf("dispatched event = %+v", spy.got)
	}
	if spy.got.EventID != "evt-7" {
		t.Fatalf("event id = %q, want evt-7", spy.got.EventID)
	}
}

// A malformed payload can never succeed on retry — the decode error wraps
// asynq.SkipRetry, so the task is archived, and the use case is never called.
func TestListener_HandleNotificationRequested_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyNotifyUC{}
	l := NewListener(spy)

	err := l.handleNotificationRequested(context.Background(), asynq.NewTask(TypeNotificationRequested, []byte("{not json")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.call != 0 {
		t.Fatalf("use case called %d times on a decode fault, want 0", spy.call)
	}
}

// A use-case error propagates unchanged (retryable infra stays retryable).
func TestListener_HandleNotificationRequested_UseCaseErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	spy := &spyNotifyUC{err: sentinel}
	l := NewListener(spy)

	payload, err := json.Marshal(NotificationRequested{Base: events.Base{EventID: "e"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := l.handleNotificationRequested(context.Background(), asynq.NewTask(TypeNotificationRequested, payload)); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the use-case error", err)
	}
}
