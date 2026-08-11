package events

import (
	"context"
	"errors"
	"strings"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/obs"
)

// selectUnpublished drains one batch of undelivered outbox rows in publication
// order. FOR UPDATE SKIP LOCKED lets several relay replicas share the table
// without publishing a row twice; the LIMIT bounds the work per Tick.
const selectUnpublished = `SELECT id, type, payload, idempotency_key, trace_context, aggregate_id
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

// pendingEvent is one outbox row read for publication.
type pendingEvent struct {
	id             int64
	typ            string
	payload        []byte
	idempotencyKey string
	traceContext   string
	aggregateID    string
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
			&ev.idempotencyKey, &ev.traceContext, &ev.aggregateID,
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

// ExtractTrace returns a ctx continuing the producer's trace, read from the
// traceparent header the relay stamped on the task. Consumers call it first so
// their spans join the originating trace (docs erd-backend §4c.3). With no header
// it returns ctx unchanged.
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
	switch prefix(typ) {
	case "ingestao", "acquisition":
		// The acquisition slice's events (integration_activated, sync_requested,
		// court_record_observed, …) are the ingestion work: worker-ingestao consumes
		// the "ingestao" queue, so route the acquisition domain there. Without this
		// they land in "default", which no worker consumes — the whole async
		// discovery/enrichment chain silently stalls.
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
