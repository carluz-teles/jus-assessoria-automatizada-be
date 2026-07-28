package events

import (
	"context"
	"encoding/json"

	"github.com/jusassessoria/platform/lib/database"
)

// insertOutbox appends one row to the transactional outbox. Column order matches
// Publish's argument order; id and created_at default in the DB and published_at
// stays NULL until the relay drains it.
const insertOutbox = `INSERT INTO outbox
	(aggregate_type, aggregate_id, type, payload, idempotency_key, trace_context)
	VALUES ($1, $2, $3, $4, $5, $6)`

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
	)
	return database.WrapInfra(err)
}
