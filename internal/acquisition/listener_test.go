package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// spyListenerUC records the events the listener decoded and hands back a preset
// error, so the test can assert the decode+dispatch wiring in isolation. It
// covers all three backfill consumers (activated, sync_completed, sync_failed).
type spyListenerUC struct {
	got           IntegrationActivated
	call          int
	completed     SyncCompleted
	completeCall  int
	failed        SyncFailed
	failCall      int
	reenabled     WatchedOABReenabled
	reenabledCall int
	err           error
}

func (s *spyListenerUC) OnIntegrationActivated(_ context.Context, ev IntegrationActivated) error {
	s.call++
	s.got = ev
	return s.err
}

func (s *spyListenerUC) OnWatchedOABReenabled(_ context.Context, ev WatchedOABReenabled) error {
	s.reenabledCall++
	s.reenabled = ev
	return s.err
}

func (s *spyListenerUC) OnSyncCompleted(_ context.Context, ev SyncCompleted) error {
	s.completeCall++
	s.completed = ev
	return s.err
}

func (s *spyListenerUC) OnSyncFailed(_ context.Context, ev SyncFailed) error {
	s.failCall++
	s.failed = ev
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

// spyIngestionUC records the decoded diario_requested event and returns a preset error.
type spyIngestionUC struct {
	got  DiarioRequested
	call int
	err  error
}

func (s *spyIngestionUC) OnDiarioRequested(_ context.Context, ev DiarioRequested) error {
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
	l := NewListener(spy, nil, nil, nil, nil)
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
	l := NewListener(spy, nil, nil, nil, nil)
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
	l := NewListener(nil, spy, nil, nil, nil)
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
	l := NewListener(nil, spy, nil, nil, nil)
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

// D1: a well-formed sync_completed task is decoded and dispatched to the backfill
// completion counter with its payload intact.
func TestListener_HandleSyncCompleted_Dispatches(t *testing.T) {
	t.Parallel()

	ev := SyncCompleted{
		Base:          events.Base{EventID: "evt-c1", Aggregate: "run-1"},
		TenantID:      "tenant-1",
		SyncRunID:     "run-1",
		IntegrationID: "integ-1",
		BackfillJobID: "job-1",
		SliceIndex:    5,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	spy := &spyListenerUC{}
	l := NewListener(spy, nil, nil, nil, nil)
	task := asynq.NewTask(TypeSyncCompleted, payload)

	if err := l.handleSyncCompleted(context.Background(), task); err != nil {
		t.Fatalf("handleSyncCompleted() error = %v", err)
	}
	if spy.completeCall != 1 {
		t.Fatalf("use case calls = %d, want 1", spy.completeCall)
	}
	if spy.completed.BackfillJobID != "job-1" || spy.completed.SliceIndex != 5 || spy.completed.TenantID != "tenant-1" {
		t.Fatalf("dispatched event = %+v, payload lost", spy.completed)
	}
}

// D1 (failed side): a well-formed sync_failed task is decoded and dispatched.
func TestListener_HandleSyncFailed_Dispatches(t *testing.T) {
	t.Parallel()

	ev := SyncFailed{
		Base:          events.Base{EventID: "evt-f1", Aggregate: "run-1"},
		TenantID:      "tenant-1",
		SyncRunID:     "run-1",
		IntegrationID: "integ-1",
		BackfillJobID: "job-1",
		SliceIndex:    5,
		Reason:        "fetch timeout",
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	spy := &spyListenerUC{}
	l := NewListener(spy, nil, nil, nil, nil)
	task := asynq.NewTask(TypeSyncFailed, payload)

	if err := l.handleSyncFailed(context.Background(), task); err != nil {
		t.Fatalf("handleSyncFailed() error = %v", err)
	}
	if spy.failCall != 1 {
		t.Fatalf("use case calls = %d, want 1", spy.failCall)
	}
	if spy.failed.BackfillJobID != "job-1" || spy.failed.SliceIndex != 5 {
		t.Fatalf("dispatched event = %+v, payload lost", spy.failed)
	}
}

// A well-formed watched_oab_reenabled task is decoded and dispatched to the
// backfill use case's catch-up path with its payload intact.
func TestListener_HandleWatchedOABReenabled_Dispatches(t *testing.T) {
	t.Parallel()

	ev := WatchedOABReenabled{
		Base:          events.Base{EventID: "evt-reenable-1"},
		TenantID:      "tenant-1",
		IntegrationID: "integ-1",
		Source:        SourceDJEN,
		OABKey:        "347019|SP",
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	spy := &spyListenerUC{}
	l := NewListener(spy, nil, nil, nil, nil)
	task := asynq.NewTask(TypeWatchedOABReenabled, payload)

	if err := l.handleWatchedOABReenabled(context.Background(), task); err != nil {
		t.Fatalf("handleWatchedOABReenabled() error = %v", err)
	}
	if spy.reenabledCall != 1 {
		t.Fatalf("use case calls = %d, want 1", spy.reenabledCall)
	}
	if spy.reenabled.OABKey != "347019|SP" || spy.reenabled.TenantID != "tenant-1" {
		t.Fatalf("dispatched event = %+v, payload lost", spy.reenabled)
	}
}

// A malformed watched_oab_reenabled payload wraps asynq.SkipRetry and never
// reaches the use case.
func TestListener_HandleWatchedOABReenabled_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyListenerUC{}
	l := NewListener(spy, nil, nil, nil, nil)
	task := asynq.NewTask(TypeWatchedOABReenabled, []byte("{not json"))

	err := l.handleWatchedOABReenabled(context.Background(), task)
	if err == nil {
		t.Fatal("handleWatchedOABReenabled() error = nil, want decode failure")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("error = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.reenabledCall != 0 {
		t.Fatalf("use case reached %d times on a bad payload, want 0", spy.reenabledCall)
	}
}

// D2: a malformed sync_completed payload wraps asynq.SkipRetry and never reaches
// the use case.
func TestListener_HandleSyncCompleted_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyListenerUC{}
	l := NewListener(spy, nil, nil, nil, nil)
	task := asynq.NewTask(TypeSyncCompleted, []byte("{not json"))

	err := l.handleSyncCompleted(context.Background(), task)
	if err == nil {
		t.Fatal("handleSyncCompleted() error = nil, want decode failure")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("error = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.completeCall != 0 {
		t.Fatalf("use case reached %d times on a bad payload, want 0", spy.completeCall)
	}
}

// D2 (failed side): a malformed sync_failed payload wraps asynq.SkipRetry.
func TestListener_HandleSyncFailed_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyListenerUC{}
	l := NewListener(spy, nil, nil, nil, nil)
	task := asynq.NewTask(TypeSyncFailed, []byte("{not json"))

	err := l.handleSyncFailed(context.Background(), task)
	if err == nil {
		t.Fatal("handleSyncFailed() error = nil, want decode failure")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("error = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.failCall != 0 {
		t.Fatalf("use case reached %d times on a bad payload, want 0", spy.failCall)
	}
}

// I1: a well-formed diario_requested task is decoded and dispatched to the ingestion
// use case with its payload intact.
func TestListener_HandleDiarioRequested_Dispatches(t *testing.T) {
	t.Parallel()

	ev := DiarioRequested{
		Base:     events.Base{EventID: "evt-d1", Aggregate: "TJSP"},
		Tribunal: "TJSP",
		Day:      "2025-08-08",
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	spy := &spyIngestionUC{}
	l := NewListener(nil, nil, nil, nil, spy)
	task := asynq.NewTask(TypeDiarioRequested, payload)

	if err := l.handleDiarioRequested(context.Background(), task); err != nil {
		t.Fatalf("handleDiarioRequested() error = %v", err)
	}
	if spy.call != 1 {
		t.Fatalf("use case calls = %d, want 1", spy.call)
	}
	if spy.got.Tribunal != "TJSP" || spy.got.Day != "2025-08-08" {
		t.Fatalf("dispatched event = %+v, payload lost", spy.got)
	}
}

// I2: a malformed diario_requested payload wraps asynq.SkipRetry and never reaches the
// use case.
func TestListener_HandleDiarioRequested_BadPayloadSkipsRetry(t *testing.T) {
	t.Parallel()

	spy := &spyIngestionUC{}
	l := NewListener(nil, nil, nil, nil, spy)
	task := asynq.NewTask(TypeDiarioRequested, []byte("{not json"))

	err := l.handleDiarioRequested(context.Background(), task)
	if err == nil {
		t.Fatal("handleDiarioRequested() error = nil, want decode failure")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("error = %v, want it to wrap asynq.SkipRetry", err)
	}
	if spy.call != 0 {
		t.Fatalf("use case reached %d times on a bad payload, want 0", spy.call)
	}
}

// I3: diario_requested is NOT on the main Register mux (it lives on the dedicated
// concurrency-1 server), but IS mounted by RegisterIngestion. This keeps the national
// bulk fetch serialized and isolated from the shared "ingestao" queue.
func TestListener_DiarioRequestedOnlyViaRegisterIngestion(t *testing.T) {
	t.Parallel()

	task := asynq.NewTask(TypeDiarioRequested, nil)

	// asynq's ServeMux.Handler returns an empty pattern (and a NotFoundHandler) when no
	// route matches; the registered pattern otherwise.
	mainMux := asynq.NewServeMux()
	NewListener(&spyListenerUC{}, &spySyncUC{}, nil, nil, &spyIngestionUC{}).Register(mainMux)
	if _, pattern := mainMux.Handler(task); pattern != "" {
		t.Errorf("diario_requested on the main mux (pattern %q), want it only on the dedicated server", pattern)
	}

	diarioMux := asynq.NewServeMux()
	NewListener(nil, nil, nil, nil, &spyIngestionUC{}).RegisterIngestion(diarioMux)
	if _, pattern := diarioMux.Handler(task); pattern != TypeDiarioRequested {
		t.Errorf("RegisterIngestion pattern = %q, want %q", pattern, TypeDiarioRequested)
	}
}
