package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// recordedEnqueue captures one EnqueueContext call, decoding the opaque options so
// tests can assert routing (Queue), retry budget (MaxRetry), dedup id (TaskID) and
// the optional schedule time (ProcessAt).
type recordedEnqueue struct {
	task         *asynq.Task
	queue        string
	maxRetry     int
	taskID       string
	hasTaskID    bool
	processAt    time.Time
	hasProcessAt bool
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
		case asynq.ProcessAtOpt:
			rec.processAt = o.Value().(time.Time)
			rec.hasProcessAt = true
		}
	}

	idx := len(f.calls)
	f.calls = append(f.calls, rec)
	if f.err != nil && idx == f.failAt {
		return nil, f.err
	}
	return &asynq.TaskInfo{ID: rec.taskID, Queue: rec.queue}, nil
}

// outboxColumns is the column order Tick scans, shared by the row builders.
var outboxColumns = []string{"id", "type", "payload", "idempotency_key", "trace_context", "aggregate_id", "process_at"}

// outboxRows builds a two-row result set in the column order Tick scans. Both rows
// have a NULL process_at (immediate delivery), the default path.
func outboxRows() *pgxmock.Rows {
	return pgxmock.NewRows(outboxColumns).
		AddRow(int64(1), "ingestao.movimento.observed", []byte(`{"a":1}`), "idem-1", wantTraceparent, "agg-1", nil).
		AddRow(int64(2), "ai.revisao.requested", []byte(`{"b":2}`), "idem-2", "", "agg-2", nil)
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
	// NULL process_at means no ProcessAt option — pending immediately, as before.
	if c0.hasProcessAt {
		t.Errorf("call[0] ProcessAt set = %v, want no ProcessAt for a NULL process_at row", c0.processAt)
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
	if c1.hasProcessAt {
		t.Errorf("call[1] ProcessAt set = %v, want no ProcessAt for a NULL process_at row", c1.processAt)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A row whose process_at is set (opted into future delivery) is enqueued with the
// asynq.ProcessAt option carrying exactly that time, so the task lands SCHEDULED.
func TestRelay_Tick_ScheduledRow_EnqueuesWithProcessAt(t *testing.T) {
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, type, payload").WillReturnRows(
		pgxmock.NewRows(outboxColumns).
			AddRow(int64(1), "ingestao.movimento.observed", []byte(`{"a":1}`), "idem-1", "", "agg-1", &at),
	)
	mock.ExpectExec("UPDATE outbox SET published_at").WithArgs(int64(1)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	fake := &fakeEnqueuer{failAt: -1}
	relay := NewRelay(database.NewUnitOfWork(mock), fake)

	if _, err := relay.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("enqueue calls = %d, want 1", len(fake.calls))
	}
	c0 := fake.calls[0]
	if !c0.hasProcessAt {
		t.Fatal("call[0] ProcessAt not set, want the row's process_at as an asynq.ProcessAt option")
	}
	if !c0.processAt.Equal(at) {
		t.Errorf("call[0] ProcessAt = %v, want %v", c0.processAt, at)
	}
	// The row is still drained this tick — the ETA lives in asynq, not the outbox.
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
		pgxmock.NewRows(outboxColumns).
			AddRow(int64(1), "ingestao.movimento.observed", []byte(`{"a":1}`), "idem-1", "", "agg-1", nil),
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
		{"acquisition.integration_activated", "ingestao"}, // regression: stays on "ingestao"
		{"acquisition.court_record_observed", "ingestao"}, // regression: enrichment stays on "ingestao"
		{"acquisition.sync_requested", "ingestao"},        // regression: the work event stays on "ingestao"
		{"acquisition.diario_requested", "diario"},        // dedicated serialized queue, not "ingestao"
		// The backfill completion counter gets its OWN light queue, drained by a separate
		// server, so sync_completed/failed finalize the job without waiting behind the
		// enrichment flood on "ingestao".
		{"acquisition.sync_completed", "sync_status"},
		{"acquisition.sync_failed", "sync_status"},
		// The prazo flow gets its OWN dedicated queue so creating a prazo is never starved by
		// the enrichment flood on "ingestao". The two intimation events carry the "acquisition"
		// prefix, so they must be special-cased ahead of the prefix switch.
		{"acquisition.intimation.observed", "deadline"},
		{"acquisition.intimation.cancelled", "deadline"},
		{"deadline.reminder_check", "deadline"},
		{"deadline.missed_check", "deadline"},
		// due_soon/missed are consumed by the NOTIFICATIONS listener (main server), not the
		// deadline server — route them to the queue that listener serves.
		{"deadline.due_soon", "notifications"},
		{"deadline.missed", "notifications"},
		// The rest of the deadline domain has no async consumer — stays on "ingestao"
		// (archived as handler-not-found), not "default".
		{"deadline.opened", "ingestao"},
		{"deadline.revoked", "ingestao"},
		{"deadline.task.created", "ingestao"},
		{"documents.file.extracted", "documents"},
		// The document slice's pipeline events (singular prefix) route to the same queue.
		{"document.uploaded", "documents"},
		{"document.extracted", "documents"},
		{"document.ready", "documents"},
		{"document.failed", "documents"},
		{"ai.revisao.requested", "ai"},
		{"notification.requested", "notifications"},
		// Trial provisioning (fatia 2): billing has no dedicated server, so it shares the
		// "notifications" queue with the main worker's mux. identity.tenant_provisioned
		// carries the "identity" prefix (unrouted otherwise); the billing.* pair carry
		// "billing" (also unrouted by the prefix switch below).
		{"identity.tenant_provisioned", "notifications"},
		{"billing.trial_ending_soon_check", "notifications"},
		{"billing.trial_ending_soon", "notifications"},
		// The rest of the billing domain has no async consumer today — stays on "default"
		// (an orphan, same as before this fatia).
		{"billing.subscription_activated", "default"},
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
		{"document.uploaded", 10},
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
