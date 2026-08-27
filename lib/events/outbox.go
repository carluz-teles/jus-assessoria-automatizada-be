package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// insertOutbox appends one row to the transactional outbox. Column order matches
// Publish's argument order; id and created_at default in the DB and published_at
// stays NULL until the relay drains it. process_at is NULL unless the event opts
// into future delivery (see scheduledAt). priority is priorityFor(ev.Type()) — the
// relay's drain order (migration 0075), written once here at the event's birth.
const insertOutbox = `INSERT INTO outbox
	(aggregate_type, aggregate_id, type, payload, idempotency_key, trace_context, process_at, priority)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// Outbox is the producer half of the pipeline. It is stateless: Publish writes
// through the caller's transaction, so the same tx that persists the domain entity
// persists the event — they commit together or not at all (transactional outbox,
// docs erd-backend §4b.2, §4c.1). It never talks to asynq; the relay does.
type Outbox struct{}

// NewOutbox returns an Outbox. It holds no state — the transaction is supplied per
// call — but the constructor keeps slice wiring uniform.
func NewOutbox() *Outbox { return &Outbox{} }

// Publish inserts ev into the outbox within tx. The event is serialized to JSON
// for the payload column, and the active trace is captured now, at the event's
// birth, so the relay can replay it onto the async hop (docs erd-backend §4c.1).
// Both the marshal fault and the insert failure surface as typed infra errors.
//
// It does not open or commit tx: the use case owns the transaction boundary and
// this call only participates — that is what makes the entity+outbox write atomic.
func (o *Outbox) Publish(ctx context.Context, tx database.Tx, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return database.WrapInfra(err)
	}

	_, err = tx.Exec(ctx, insertOutbox,
		ev.AggregateType(),
		ev.AggregateID(),
		ev.Type(),
		payload,
		ev.IdempotencyKey(),
		TraceContextFromCtx(ctx),
		scheduledAt(ev),
		priorityFor(ev.Type()),
	)
	return database.WrapInfra(err)
}

// scheduledAt is the internal type-assert that reads an event's optional ETA. If ev
// implements ScheduledEvent and asks for a future delivery, it returns a pointer to
// that time (written to process_at); otherwise it returns nil, so process_at is SQL
// NULL and the event is delivered immediately — the default for the vast majority
// of events, which do not implement ScheduledEvent. Keeping the assertion here (not
// in Publish's signature) is what makes the primitive retrocompatible.
func scheduledAt(ev Event) *time.Time {
	s, ok := ev.(ScheduledEvent)
	if !ok {
		return nil
	}
	t, ok := s.ProcessAt()
	if !ok || t.IsZero() {
		return nil
	}
	return &t
}

// insertOutboxBatch appends MANY rows in one round-trip from a jsonb array — the set-
// based counterpart of insertOutbox. bigserial ids are assigned in array order, so the
// events keep their relative publication order. priority is carried per row (same
// priorityFor(ev.Type()) rule as insertOutbox), not a single batch-wide value — a mixed
// batch (rare, but not precluded) still drains each event at its own priority.
const insertOutboxBatch = `INSERT INTO outbox
	(aggregate_type, aggregate_id, type, payload, idempotency_key, trace_context, process_at, priority)
	SELECT r.aggregate_type, r.aggregate_id, r.type, r.payload, r.idempotency_key, r.trace_context, r.process_at, r.priority
	FROM jsonb_to_recordset($1::jsonb) AS r(
		aggregate_type text, aggregate_id uuid, type text, payload jsonb,
		idempotency_key text, trace_context text, process_at timestamptz, priority smallint
	)`

// outboxRowJSON is one row of the PublishBatch payload. payload is the event's own JSON,
// embedded as-is; trace_context is captured once for the whole batch. ProcessAt is the
// per-event optional ETA: a nil pointer marshals to JSON null, which jsonb_to_recordset
// reads back as a SQL NULL process_at (immediate delivery); a set time marshals as
// RFC3339, which timestamptz parses (future delivery). Priority is priorityFor(ev.Type())
// per event — the relay's drain order (migration 0075).
type outboxRowJSON struct {
	AggregateType  string          `json:"aggregate_type"`
	AggregateID    string          `json:"aggregate_id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	TraceContext   string          `json:"trace_context"`
	ProcessAt      *time.Time      `json:"process_at"`
	Priority       int16           `json:"priority"`
}

// PublishBatch inserts MANY events into the outbox in ONE round-trip, within tx. It is
// the loop-free path for a producer that emits hundreds of events under a lock (a sync
// window's court_record_observed fan-out): the per-event Publish loop held the advisory
// lock for as many round-trips as events; this holds it for one. Order is preserved.
func (o *Outbox) PublishBatch(ctx context.Context, tx database.Tx, evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	trace := TraceContextFromCtx(ctx)
	rows := make([]outboxRowJSON, len(evs))
	for i, ev := range evs {
		payload, err := json.Marshal(ev)
		if err != nil {
			return database.WrapInfra(err)
		}
		rows[i] = outboxRowJSON{
			AggregateType:  ev.AggregateType(),
			AggregateID:    ev.AggregateID(),
			Type:           ev.Type(),
			Payload:        payload,
			IdempotencyKey: ev.IdempotencyKey(),
			TraceContext:   trace,
			ProcessAt:      scheduledAt(ev),
			Priority:       priorityFor(ev.Type()),
		}
	}
	batch, err := json.Marshal(rows)
	if err != nil {
		return database.WrapInfra(err)
	}
	if _, err := tx.Exec(ctx, insertOutboxBatch, batch); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}
