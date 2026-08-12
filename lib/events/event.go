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

import "time"

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

// ScheduledEvent is the OPTIONAL half of Event: an event that OPTS INTO future
// delivery implements it, returning the wall-clock time it should be delivered.
// The relay reads process_at off the outbox row and enqueues the asynq task with
// asynq.ProcessAt(t), so the task starts SCHEDULED and the consumer only sees it
// once t arrives — the ETA lives in asynq, never in a polling loop (docs
// erd-backend §4c; repo directive: ETA work is a scheduled task, not polling).
//
// The bool is the opt-out escape hatch: ProcessAt() returning (_, false) — or a
// zero time — means "deliver now", identical to an event that does not implement
// this interface at all. The MAJORITY of events do NOT implement ScheduledEvent
// and stay immediate; the outbox type-asserts for it, so adding it to one event
// changes nothing for the rest (retrocompatible).
type ScheduledEvent interface {
	Event
	ProcessAt() (time.Time, bool)
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
