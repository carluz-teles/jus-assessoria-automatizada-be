package events

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// recordedEnqueue captures one EnqueueContext call, decoding the opaque options so
// tests can assert routing (Queue), retry budget (MaxRetry) and dedup id (TaskID).
type recordedEnqueue struct {
	task      *asynq.Task
	queue     string
	maxRetry  int
	taskID    string
	hasTaskID bool
}

// fakeEnqueuer is a Redis-free Enqueuer: it records every call and can be told to
// fail a specific call index to exercise the rollback path.
type fakeEnqueuer struct {
	calls  []recordedEnqueue
	failAt int // call index that returns err; -1 to never fail
	err    error
}

func (f *fakeEnqueuer) EnqueueContext(_ context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	rec := recordedEnqueue{task: task, maxRetry: -1}
	for _, o := range opts {
		switch o.Type() {
		case asynq.QueueOpt:
			rec.queue = o.Value().(string)
		case asynq.MaxRetryOpt:
			rec.maxRetry = o.Value().(int)
		case asynq.TaskIDOpt:
			rec.taskID = o.Value().(string)
			rec.hasTaskID = true
		}
	}

	idx := len(f.calls)
	f.calls = append(f.calls, rec)
	if f.err != nil && idx == f.failAt {
		return nil, f.err
	}
	return &asynq.TaskInfo{ID: rec.taskID, Queue: rec.queue}, nil
}

// outboxRows builds a two-row result set in the column order Tick scans.
func outboxRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "type", "payload", "idempotency_key", "trace_context", "aggregate_id"}).
		AddRow(int64(1), "ingestao.movimento.observed", []byte(`{"a":1}`), "idem-1", wantTraceparent, "agg-1").
		AddRow(int64(2), "ai.revisao.requested", []byte(`{"b":2}`), "idem-2", "", "agg-2")
}

// Happy path: two rows drain in order — each enqueued with the right queue/retry/id
// and trace header, then marked published — and the tx commits with published=2.
func TestRelay_Tick_PublishesBatch(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, type, payload").WillReturnRows(outboxRows())
	mock.ExpectExec("UPDATE outbox SET published_at").WithArgs(int64(1)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("UPDATE outbox SET published_at").WithArgs(int64(2)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	fake := &fakeEnqueuer{failAt: -1}
	relay := NewRelay(database.NewUnitOfWork(mock), fake)

	published, err := relay.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(published) != 2 {
		t.Errorf("published = %d, want 2", len(published))
	}
	if len(fake.calls) != 2 {
		t.Fatalf("enqueue calls = %d, want 2", len(fake.calls))
	}

	// Row 1 → ingestao queue, generous retry, its idempotency key as TaskID, and the
	// producer's traceparent carried as a header.
	c0 := fake.calls[0]
	if c0.queue != "ingestao" || c0.maxRetry != 25 {
		t.Errorf("call[0] queue/maxRetry = %q/%d, want ingestao/25", c0.queue, c0.maxRetry)
	}
	if !c0.hasTaskID || c0.taskID != "idem-1" {
		t.Errorf("call[0] taskID = %q (set=%v), want idem-1", c0.taskID, c0.hasTaskID)
	}
	if got := c0.task.Headers()[traceparentKey]; got != wantTraceparent {
		t.Errorf("call[0] traceparent header = %q, want %q", got, wantTraceparent)
	}
	// Event identity travels as headers so the consumer middleware attributes its
	// span/log without decoding the payload.
	if got := c0.task.Headers()[eventIDHeader]; got != "idem-1" {
		t.Errorf("call[0] %s header = %q, want idem-1", eventIDHeader, got)
	}
	if got := c0.task.Headers()[aggregateIDHeader]; got != "agg-1" {
		t.Errorf("call[0] %s header = %q, want agg-1", aggregateIDHeader, got)
	}
	if c0.task.Type() != "ingestao.movimento.observed" {
		t.Errorf("call[0] type = %q", c0.task.Type())
	}

	// Row 2 → ai queue, small retry; empty trace_context means no trace header, but
	// the event-identity headers are still present.
	c1 := fake.calls[1]
	if c1.queue != "ai" || c1.maxRetry != 3 {
		t.Errorf("call[1] queue/maxRetry = %q/%d, want ai/3", c1.queue, c1.maxRetry)
	}
	if got := c1.task.Headers()[traceparentKey]; got != "" {
		t.Errorf("call[1] traceparent header = %q, want empty", got)
	}
	if got := c1.task.Headers()[aggregateIDHeader]; got != "agg-2" {
		t.Errorf("call[1] %s header = %q, want agg-2", aggregateIDHeader, got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// If an enqueue fails, Tick returns the error, the tx rolls back, and no row is
// marked published — the batch is retried next Tick (at-least-once).
func TestRelay_Tick_EnqueueError_RollsBack(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, type, payload").WillReturnRows(outboxRows())
	// No ExpectExec: the very first enqueue fails before any UPDATE is issued.
	mock.ExpectRollback()

	fake := &fakeEnqueuer{failAt: 0, err: errors.New("redis unreachable")}
	relay := NewRelay(database.NewUnitOfWork(mock), fake)

	published, err := relay.Tick(context.Background())
	if err == nil {
		t.Fatal("Tick() error = nil, want error")
	}
	if len(published) != 0 {
		t.Errorf("published = %d, want 0", len(published))
	}
	if len(fake.calls) != 1 {
		t.Errorf("enqueue calls = %d, want 1 (stopped after failure)", len(fake.calls))
	}
	if ae, ok := apperr.From(err); !ok || ae.Kind != apperr.KindInfra {
		t.Errorf("Tick() error = %v, want INFRA_ERROR AppError", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An ErrTaskIDConflict means asynq already holds the task — the event is enqueued,
// so the row is still marked published and the tx commits.
func TestRelay_Tick_TaskIDConflict_MarksPublished(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, type, payload").WillReturnRows(
		pgxmock.NewRows([]string{"id", "type", "payload", "idempotency_key", "trace_context", "aggregate_id"}).
			AddRow(int64(1), "ingestao.movimento.observed", []byte(`{"a":1}`), "idem-1", "", "agg-1"),
	)
	mock.ExpectExec("UPDATE outbox SET published_at").WithArgs(int64(1)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	fake := &fakeEnqueuer{failAt: 0, err: asynq.ErrTaskIDConflict}
	relay := NewRelay(database.NewUnitOfWork(mock), fake)

	published, err := relay.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(published) != 1 {
		t.Errorf("published = %d, want 1", len(published))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestQueueFor(t *testing.T) {
	tests := []struct {
		typ  string
		want string
	}{
		{"ingestao.movimento.observed", "ingestao"},
		{"acquisition.integration_activated", "ingestao"},
		{"acquisition.court_record_observed", "ingestao"},
		{"documents.file.extracted", "documents"},
		{"ai.revisao.requested", "ai"},
		{"notification.requested", "notifications"},
		{"minuta.revised", "default"},
		{"nodot", "default"},
		{"", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			if got := queueFor(tt.typ); got != tt.want {
				t.Errorf("queueFor(%q) = %q, want %q", tt.typ, got, tt.want)
			}
		})
	}
}

func TestMaxRetryFor(t *testing.T) {
	tests := []struct {
		typ  string
		want int
	}{
		{"ingestao.movimento.observed", 25},
		{"acquisition.sync_requested", 25},
		{"documents.file.extracted", 10},
		{"ai.revisao.requested", 3},
		{"minuta.revised", 5},
		{"nodot", 5},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			if got := maxRetryFor(tt.typ); got != tt.want {
				t.Errorf("maxRetryFor(%q) = %d, want %d", tt.typ, got, tt.want)
			}
		})
	}
}
