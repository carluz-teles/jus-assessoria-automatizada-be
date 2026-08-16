package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

// spyInAppUC records the decoded events and returns a preset error, so the two in-app
// handlers can be asserted in isolation from the use case.
type spyInAppUC struct {
	backfill       BackfillFinished
	docket         DocketEntryObserved
	dueSoon        DeadlineDueSoon
	missed         DeadlineMissed
	trialEndSoon   TrialEndingSoon
	paymentFail    PaymentFailed
	backfillN      int
	docketN        int
	dueSoonN       int
	missedN        int
	trialEndN      int
	paymentFailN   int
	backfillErr    error
	docketErr      error
	dueSoonErr     error
	missedErr      error
	trialEndErr    error
	paymentFailErr error
}

func (s *spyInAppUC) OnBackfillFinished(_ context.Context, ev BackfillFinished) error {
	s.backfillN++
	s.backfill = ev
	return s.backfillErr
}

func (s *spyInAppUC) OnDocketEntryObserved(_ context.Context, ev DocketEntryObserved) error {
	s.docketN++
	s.docket = ev
	return s.docketErr
}

func (s *spyInAppUC) OnDeadlineDueSoon(_ context.Context, ev DeadlineDueSoon) error {
	s.dueSoonN++
	s.dueSoon = ev
	return s.dueSoonErr
}

func (s *spyInAppUC) OnDeadlineMissed(_ context.Context, ev DeadlineMissed) error {
	s.missedN++
	s.missed = ev
	return s.missedErr
}

func (s *spyInAppUC) OnTrialEndingSoon(_ context.Context, ev TrialEndingSoon) error {
	s.trialEndN++
	s.trialEndSoon = ev
	return s.trialEndErr
}

func (s *spyInAppUC) OnPaymentFailed(_ context.Context, ev PaymentFailed) error {
	s.paymentFailN++
	s.paymentFail = ev
	return s.paymentFailErr
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
	l := NewListener(spy, &spyInAppUC{})
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
	l := NewListener(spy, &spyInAppUC{})

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
	l := NewListener(spy, &spyInAppUC{})

	payload, err := json.Marshal(NotificationRequested{Base: events.Base{EventID: "e"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := l.handleNotificationRequested(context.Background(), asynq.NewTask(TypeNotificationRequested, payload)); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the use-case error", err)
	}
}

// A well-formed backfill_finished task is decoded and dispatched to the in-app use case
// with its tally intact.
func TestListener_HandleBackfillFinished_Dispatches(t *testing.T) {
	t.Parallel()

	ev := BackfillFinished{
		Base:          events.Base{EventID: "evt-bf", Aggregate: "job-1"},
		TenantID:      "tenant-1",
		BackfillJobID: "job-1",
		Status:        "PARTIAL",
		SlicesError:   2,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)
	if err := l.handleBackfillFinished(context.Background(), asynq.NewTask(TypeBackfillFinished, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if inApp.backfillN != 1 {
		t.Fatalf("use case called %d times, want 1", inApp.backfillN)
	}
	if inApp.backfill.TenantID != "tenant-1" || inApp.backfill.Status != "PARTIAL" || inApp.backfill.SlicesError != 2 {
		t.Fatalf("dispatched event = %+v", inApp.backfill)
	}
}

// A malformed backfill_finished payload wraps asynq.SkipRetry (archived), and the use
// case is never called.
func TestListener_HandleBackfillFinished_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)

	err := l.handleBackfillFinished(context.Background(), asynq.NewTask(TypeBackfillFinished, []byte("{bad")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if inApp.backfillN != 0 {
		t.Fatalf("use case called %d times on a decode fault, want 0", inApp.backfillN)
	}
}

// A well-formed docket_entry_observed task is decoded and dispatched to the in-app use case.
func TestListener_HandleDocketEntryObserved_Dispatches(t *testing.T) {
	t.Parallel()

	ev := DocketEntryObserved{
		Base:          events.Base{EventID: "evt-dk", Aggregate: "entry-1"},
		TenantID:      "tenant-2",
		DocketEntryID: "entry-1",
		CourtRecordID: "record-1",
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)
	if err := l.handleDocketEntryObserved(context.Background(), asynq.NewTask(TypeDocketEntryObserved, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if inApp.docketN != 1 {
		t.Fatalf("use case called %d times, want 1", inApp.docketN)
	}
	if inApp.docket.TenantID != "tenant-2" || inApp.docket.DocketEntryID != "entry-1" {
		t.Fatalf("dispatched event = %+v", inApp.docket)
	}
}

// A use-case error from the docket handler propagates unchanged (retryable infra stays retryable).
func TestListener_HandleDocketEntryObserved_UseCaseErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	inApp := &spyInAppUC{docketErr: sentinel}
	l := NewListener(&spyNotifyUC{}, inApp)

	payload, err := json.Marshal(DocketEntryObserved{Base: events.Base{EventID: "e"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := l.handleDocketEntryObserved(context.Background(), asynq.NewTask(TypeDocketEntryObserved, payload)); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the use-case error", err)
	}
}

// A well-formed deadline.due_soon task is decoded and dispatched to the in-app use case with
// its fields intact.
func TestListener_HandleDeadlineDueSoon_Dispatches(t *testing.T) {
	t.Parallel()

	ev := DeadlineDueSoon{
		Base:       events.Base{EventID: "evt-ds", Aggregate: "deadline-1"},
		TenantID:   "tenant-3",
		DeadlineID: "deadline-1",
		DaysLeft:   3,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)
	if err := l.handleDeadlineDueSoon(context.Background(), asynq.NewTask(TypeDeadlineDueSoon, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if inApp.dueSoonN != 1 {
		t.Fatalf("use case called %d times, want 1", inApp.dueSoonN)
	}
	if inApp.dueSoon.TenantID != "tenant-3" || inApp.dueSoon.DeadlineID != "deadline-1" || inApp.dueSoon.DaysLeft != 3 {
		t.Fatalf("dispatched event = %+v", inApp.dueSoon)
	}
}

// A malformed deadline.due_soon payload wraps asynq.SkipRetry (archived), and the use case is
// never called.
func TestListener_HandleDeadlineDueSoon_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)

	err := l.handleDeadlineDueSoon(context.Background(), asynq.NewTask(TypeDeadlineDueSoon, []byte("{bad")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if inApp.dueSoonN != 0 {
		t.Fatalf("use case called %d times on a decode fault, want 0", inApp.dueSoonN)
	}
}

// A well-formed deadline.missed task is decoded and dispatched to the in-app use case.
func TestListener_HandleDeadlineMissed_Dispatches(t *testing.T) {
	t.Parallel()

	ev := DeadlineMissed{
		Base:       events.Base{EventID: "evt-ms", Aggregate: "deadline-2"},
		TenantID:   "tenant-4",
		DeadlineID: "deadline-2",
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)
	if err := l.handleDeadlineMissed(context.Background(), asynq.NewTask(TypeDeadlineMissed, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if inApp.missedN != 1 {
		t.Fatalf("use case called %d times, want 1", inApp.missedN)
	}
	if inApp.missed.TenantID != "tenant-4" || inApp.missed.DeadlineID != "deadline-2" {
		t.Fatalf("dispatched event = %+v", inApp.missed)
	}
}

// A use-case error from the missed handler propagates unchanged (retryable infra stays retryable).
func TestListener_HandleDeadlineMissed_UseCaseErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	inApp := &spyInAppUC{missedErr: sentinel}
	l := NewListener(&spyNotifyUC{}, inApp)

	payload, err := json.Marshal(DeadlineMissed{Base: events.Base{EventID: "e"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := l.handleDeadlineMissed(context.Background(), asynq.NewTask(TypeDeadlineMissed, payload)); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the use-case error", err)
	}
}

// A well-formed billing.trial_ending_soon task is decoded and dispatched to the
// in-app use case with its fields intact.
func TestListener_HandleTrialEndingSoon_Dispatches(t *testing.T) {
	t.Parallel()

	trialEndsAt := time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC)
	ev := TrialEndingSoon{
		Base:        events.Base{EventID: "evt-tr", Aggregate: "tenant-5"},
		TenantID:    "tenant-5",
		TrialEndsAt: trialEndsAt,
		DaysLeft:    2,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)
	if err := l.handleTrialEndingSoon(context.Background(), asynq.NewTask(TypeTrialEndingSoon, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if inApp.trialEndN != 1 {
		t.Fatalf("use case called %d times, want 1", inApp.trialEndN)
	}
	if inApp.trialEndSoon.TenantID != "tenant-5" || inApp.trialEndSoon.DaysLeft != 2 || !inApp.trialEndSoon.TrialEndsAt.Equal(trialEndsAt) {
		t.Fatalf("dispatched event = %+v", inApp.trialEndSoon)
	}
}

// A malformed billing.trial_ending_soon payload wraps asynq.SkipRetry (archived), and the
// use case is never called.
func TestListener_HandleTrialEndingSoon_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)

	err := l.handleTrialEndingSoon(context.Background(), asynq.NewTask(TypeTrialEndingSoon, []byte("{bad")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if inApp.trialEndN != 0 {
		t.Fatalf("use case called %d times on a decode fault, want 0", inApp.trialEndN)
	}
}

func TestListener_HandlePaymentFailed_Dispatches(t *testing.T) {
	t.Parallel()

	ev := PaymentFailed{
		Base:      events.Base{EventID: "evt-pf", Aggregate: "tenant-5"},
		TenantID:  "tenant-5",
		InvoiceID: "invoice-1",
		AmountDue: 15090,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)
	if err := l.handlePaymentFailed(context.Background(), asynq.NewTask(TypePaymentFailed, payload)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if inApp.paymentFailN != 1 {
		t.Fatalf("use case called %d times, want 1", inApp.paymentFailN)
	}
	if inApp.paymentFail.TenantID != "tenant-5" || inApp.paymentFail.InvoiceID != "invoice-1" || inApp.paymentFail.AmountDue != 15090 {
		t.Fatalf("dispatched event = %+v", inApp.paymentFail)
	}
}

// A malformed billing.payment_failed payload wraps asynq.SkipRetry (archived), and the
// use case is never called.
func TestListener_HandlePaymentFailed_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	inApp := &spyInAppUC{}
	l := NewListener(&spyNotifyUC{}, inApp)

	err := l.handlePaymentFailed(context.Background(), asynq.NewTask(TypePaymentFailed, []byte("{bad")))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want it to wrap asynq.SkipRetry", err)
	}
	if inApp.paymentFailN != 0 {
		t.Fatalf("use case called %d times on a decode fault, want 0", inApp.paymentFailN)
	}
}
