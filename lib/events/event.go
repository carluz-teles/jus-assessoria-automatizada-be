// Package events is the transactional-outbox event pipeline plumbing shared by
// every slice: the producer (Outbox.Publish writes the outbox row inside the
// caller's transaction), the codec (Encode/Decode between a domain event and an
// asynq task), the consumer idempotency guard (Dedup.SeenOrMark against
// processed_event), the relay (Relay.Tick drains the outbox into asynq), and the
// distributed-trace hop that stitches producer and consumer into one trace
// (docs erd-backend §4c).
//
// It is infrastructure, not domain: slices depend on it, it depends on no slice.
// It reuses lib/database (Tx, UnitOfWork, WrapInfra) for persistence and, through
// WrapInfra, lib/apperr for typed errors — and it never imports Fiber or net/http.
package events

// Event is a domain fact a slice publishes. AggregateType and AggregateID place
// the fact on its aggregate (aggregate_id orders the stream); Type is the dotted
// id the relay routes and the consumer matches on (e.g. "ingestao.movimento.observed");
// IdempotencyKey is the consumer-side dedup key (see Dedup) and doubles as the
// asynq enqueue-dedup TaskID at the relay.
//
// An event is serialized to the outbox payload with encoding/json, so a concrete
// event's exported fields must be JSON-tagged to survive to the consumer.
type Event interface {
	AggregateType() string
	AggregateID() string
	Type() string
	IdempotencyKey() string
}

// Base is an embeddable helper carrying the two per-instance fields every event
// needs: the aggregate id it belongs to and its idempotency key (which also is the
// event id the consumer dedups on). Concrete events embed Base and add Type() and
// AggregateType() (constant per event) plus their own payload fields.
//
// Base's fields are JSON-tagged so they survive the round-trip through the outbox
// payload — the consumer decodes the concrete event and still reads its
// idempotency key back out via IdempotencyKey().
type Base struct {
	EventID   string `json:"event_id"`
	Aggregate string `json:"aggregate_id"`
}

// AggregateID returns the id of the aggregate this event belongs to.
func (b Base) AggregateID() string { return b.Aggregate }

// IdempotencyKey returns the event id used for consumer dedup and enqueue dedup.
func (b Base) IdempotencyKey() string { return b.EventID }
