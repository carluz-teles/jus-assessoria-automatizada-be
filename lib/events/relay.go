package events

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/obs"
)

// selectUnpublished drains one batch of undelivered outbox rows in publication
// order. FOR UPDATE SKIP LOCKED lets several relay replicas share the table
// without publishing a row twice; the LIMIT bounds the work per Tick.
const selectUnpublished = `SELECT id, type, payload, idempotency_key, trace_context, aggregate_id, process_at
	FROM outbox
	WHERE published_at IS NULL
	ORDER BY id
	LIMIT 200
	FOR UPDATE SKIP LOCKED`

// markPublished stamps a row delivered. Issued only after its enqueue succeeds, in
// the same tx as the read, so a crash before it republishes the row next Tick.
const markPublished = `UPDATE outbox SET published_at = now() WHERE id = $1`

// Enqueuer is the slice of *asynq.Client the relay uses. The seam keeps Tick
// testable with a fake recorder — no Redis — and *asynq.Client satisfies it.
type Enqueuer interface {
	EnqueueContext(ctx context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// Relay is the only component that publishes to asynq. Tick drains the outbox in a
// single system-level transaction and marks each row published only after its
// enqueue succeeds (docs erd-backend §4c.2).
type Relay struct {
	uow database.UnitOfWork
	enq Enqueuer
}

// NewRelay wires the relay to its unit of work and enqueuer.
func NewRelay(uow database.UnitOfWork, enq Enqueuer) *Relay {
	return &Relay{uow: uow, enq: enq}
}

// pendingEvent is one outbox row read for publication. processAt is nil for the
// immediate rows (the default) and carries the ETA for a row that opted into future
// delivery; publish turns it into an asynq.ProcessAt option.
type pendingEvent struct {
	id             int64
	typ            string
	payload        []byte
	idempotencyKey string
	traceContext   string
	aggregateID    string
	processAt      *time.Time
}

// PublishedEvent is the identity of one event the relay drained this tick — the
// dimensions its logging needs (which events went out, under which trace), never
// the payload. The relay returns these so the loop can log a per-type summary and
// a per-event DEBUG line correlated to the event's own trace.
type PublishedEvent struct {
	Type         string // dotted event type (the asynq task type)
	ID           string // idempotency key = the event id
	AggregateID  string // the aggregate the event is about
	TraceContext string // W3C traceparent of the producing span
}

// Tick drains one batch of unpublished outbox rows into asynq and returns what it
// published. It runs in one transaction with an empty tenantID: the relay is
// system-level and reads across tenants (the outbox carries no tenant scope).
//
// For each row it enqueues the task, then marks the row published in the SAME tx.
// If an enqueue fails, Tick returns the error so the tx rolls back and nothing in
// this batch is marked — the rows are retried next Tick. That is the at-least-once
// contract: a crash between enqueue and mark republishes, and the consumer dedups
// (docs erd-backend §4c.2, §4c.3).
//
// A non-empty batch runs under a PRODUCER span so the per-row UPDATEs correlate
// and the outbox→publish drain is visible; idle ticks open no span (no one empty
// span per second).
func (r *Relay) Tick(ctx context.Context) (published []PublishedEvent, err error) {
	err = r.uow.Do(ctx, "", func(tx database.Tx) error {
		batch, err := readPending(ctx, tx)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		spanCtx, span := obs.Start(ctx, "outbox relay publish",
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(attribute.Int("batch.size", len(batch))),
		)
		defer span.End()

		for _, ev := range batch {
			if err := r.publish(spanCtx, tx, ev); err != nil {
				obs.Record(span, err)
				return err
			}
			published = append(published, PublishedEvent{
				Type:         ev.typ,
				ID:           ev.idempotencyKey,
				AggregateID:  ev.aggregateID,
				TraceContext: ev.traceContext,
			})
		}
		obs.Record(span, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return published, nil
}

// readPending loads the batch and closes the rows before any further query on tx
// runs — pgx forbids a second query while a Rows is open on the same connection, so
// the whole batch is materialized before the per-row UPDATEs begin.
func readPending(ctx context.Context, tx database.Tx) ([]pendingEvent, error) {
	rows, err := tx.Query(ctx, selectUnpublished)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	defer rows.Close()

	var batch []pendingEvent
	for rows.Next() {
		var ev pendingEvent
		if err := rows.Scan(
			&ev.id, &ev.typ, &ev.payload,
			&ev.idempotencyKey, &ev.traceContext, &ev.aggregateID, &ev.processAt,
		); err != nil {
			return nil, database.WrapInfra(err)
		}
		batch = append(batch, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, database.WrapInfra(err)
	}
	return batch, nil
}

// publish enqueues one event and marks its outbox row published. The traceparent
// travels as an asynq task header so the worker continues the producer's trace;
// TaskID gives asynq best-effort enqueue dedup, MaxRetry and Queue route by type.
//
// ErrTaskIDConflict is not a failure: asynq already holds a task with this id, so
// the event IS enqueued — mark it published so the row does not spin every Tick.
// Any other enqueue error propagates, rolling the batch back for a later retry.
func (r *Relay) publish(ctx context.Context, tx database.Tx, ev pendingEvent) error {
	// Carry the event identity on the task so the consumer middleware can attribute
	// its span/log without decoding the payload; traceparent joins the consumer span
	// to the producer's (empty when the producer ran with no active span).
	headers := map[string]string{
		eventIDHeader:     ev.idempotencyKey,
		aggregateIDHeader: ev.aggregateID,
	}
	if ev.traceContext != "" {
		headers[traceparentKey] = ev.traceContext
	}
	task := asynq.NewTaskWithHeaders(ev.typ, ev.payload, headers)

	opts := []asynq.Option{
		asynq.Queue(queueFor(ev.typ)),
		asynq.MaxRetry(maxRetryFor(ev.typ)),
	}
	if ev.idempotencyKey != "" {
		opts = append(opts, asynq.TaskID(ev.idempotencyKey))
	}
	// An opted-in ETA becomes an asynq.ProcessAt option, so the task lands SCHEDULED
	// and the consumer only sees it once the time arrives. A nil processAt (the common
	// case) leaves opts untouched — the task is pending immediately, exactly as before.
	if ev.processAt != nil {
		opts = append(opts, asynq.ProcessAt(*ev.processAt))
	}

	if _, err := r.enq.EnqueueContext(ctx, task, opts...); err != nil {
		if !errors.Is(err, asynq.ErrTaskIDConflict) {
			return database.WrapInfra(err)
		}
	}

	if _, err := tx.Exec(ctx, markPublished, ev.id); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// ExtractTrace returns a ctx carrying the producer's span context, read from the
// traceparent header the relay stamped on the task. The consumer middleware uses it to
// LINK its span back to the producer (each event is its own trace; docs erd-backend
// §4c.3). With no header it returns ctx unchanged.
func ExtractTrace(ctx context.Context, t *asynq.Task) context.Context {
	return CtxWithTraceContext(ctx, t.Headers()[traceparentKey])
}

// queueFor routes a dotted event type to a work queue by its domain prefix so a
// slow queue (e.g. a 10-minute OCR in "documents") never blocks another kind of
// work (docs erd-backend §4c.2).
func queueFor(typ string) string {
	// diario_requested (national bulk ingestion) gets its OWN queue, consumed by a
	// dedicated concurrency-1 server: the DJEN diário fetch is slow and rate-limited by
	// a GLOBAL cumulative cap, so running it serialized (one stream) rather than 3-way
	// on "ingestao" both keeps it under the cap (concurrency made the 429s WORSE) and
	// stops it from starving — or being starved by — the enrichment/sync work.
	if typ == "acquisition.diario_requested" {
		return "diario"
	}
	// sync_completed/sync_failed (the backfill completion counter) get their OWN light
	// queue, drained by a dedicated concurrency-2 server: the increment/finalize is
	// trivial, but on "ingestao" it queues behind thousands of slow court_record_observed
	// enrichment tasks (~110s each, no preemption), so the backfill_job only flips to
	// COMPLETED once the enrichment drains. asynq has no per-task priority within a queue
	// (weight is per-queue), so a separate queue is the only way to let the sync finish
	// independently of the eventual DATAJUD enrichment. Must match worker-ingestao's
	// syncStatusQueue and the dedicated server's Queues below.
	if typ == "acquisition.sync_completed" || typ == "acquisition.sync_failed" {
		return "sync_status"
	}
	// The deadline slice's async listener (intimation.observed/cancelled + the scheduled
	// reminder_check/missed_check self-messages) gets its OWN queue, drained by a dedicated
	// server on worker-ingestao: creating a prazo is fast (DB + outbox), but on "ingestao" it
	// queues behind the DATAJUD enrichment flood (thousands of ~110s court_record_observed
	// tasks, no per-task priority within a queue), so prazos come out starved (~16/min in
	// prod). A separate queue lets the prazo flow run independently — the same fix as
	// sync_status. intimation.observed/cancelled carry the "acquisition" prefix, so they must
	// be routed HERE (before the prefix switch sends "acquisition" to "ingestao"). String
	// literals, not the slice's consts: queueFor cannot import internal/deadline (import
	// cycle). Must match worker-ingestao's deadlineQueue and the dedicated server's Queues.
	if typ == "acquisition.intimation.observed" || typ == "acquisition.intimation.cancelled" ||
		typ == "deadline.reminder_check" || typ == "deadline.missed_check" {
		return "deadline"
	}
	// deadline.due_soon/missed are NOT consumed by the deadline server — they are consumed by
	// the NOTIFICATIONS listener, which runs on the main server (it serves "notifications").
	// Route them to that queue so the reminder/miss avisos are delivered, not to "deadline"
	// (whose server has no notifications handler) nor to "ingestao".
	if typ == "deadline.due_soon" || typ == "deadline.missed" {
		return "notifications"
	}
	switch prefix(typ) {
	case "ingestao", "acquisition":
		// The acquisition slice's events (integration_activated, sync_requested,
		// court_record_observed, …) are the ingestion work: worker-ingestao consumes
		// the "ingestao" queue, so route the acquisition domain there. Without this
		// they land in "default", which no worker consumes — the whole async
		// discovery/enrichment chain silently stalls.
		return "ingestao"
	case "deadline":
		// The deadline slice's CONSUMED events (reminder_check/missed_check → "deadline",
		// due_soon/missed → "notifications") are special-cased above. What reaches here is the
		// REST of the domain — deadline.opened/revoked/updated/met and task.* — which has NO
		// async consumer today. Keep it on "ingestao" (the current behavior: archived as
		// handler-not-found), NOT "default", so the rows don't pile up in a Redis queue no
		// worker drains.
		return "ingestao"
	case "documents":
		return "documents"
	case "ai":
		return "ai"
	case "notification":
		return "notifications"
	default:
		return "default"
	}
}

// maxRetryFor sets the retry budget per work kind: sync tolerates flaky courts and
// retries generously; AI costs money per attempt, so it retries little (docs
// erd-backend §4c.4).
func maxRetryFor(typ string) int {
	switch prefix(typ) {
	case "ingestao", "acquisition":
		return 25
	case "documents":
		return 10
	case "ai":
		return 3
	default:
		return 5
	}
}

// prefix returns the segment before the first dot of a dotted event type, or the
// whole string when it has no dot.
func prefix(typ string) string {
	if i := strings.IndexByte(typ, '.'); i >= 0 {
		return typ[:i]
	}
	return typ
}
