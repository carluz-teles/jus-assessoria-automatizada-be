package notifications

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// processed_event consumer names for the in-app consumers (slice 1a). Dedup is
// per-consumer (docs §4c.3), so each consumed event type gets its own name —
// distinct from consumerNotifications (the email path) and from each other, so
// marking one event never blocks another consumer.
const (
	consumerBackfill        = "notifications.backfill"
	consumerDocket          = "notifications.docket"
	consumerDeadlineDueSoon = "notifications.deadline_due_soon"
	consumerDeadlineMissed  = "notifications.deadline_missed"
)

// txDeduper adapts lib/events' Dedup to the deduper port. events.Dedup binds its
// executor at construction, so this builds a fresh one bound to the caller's tx per
// call — that is what marks the event in the SAME transaction as the notification it
// guards (mark + write commit atomically; a crash never strands a marked-but-
// unwritten event). Stateless; the worker injects the zero value.
type txDeduper struct{}

// NewDedup returns the deduper the use case marks consumed events with. Stateless —
// the tx is supplied per call — but a constructor keeps slice wiring uniform.
func NewDedup() deduper { return txDeduper{} }

// SeenOrMark marks (consumer, eventID) within tx and reports whether it was already
// there (a replay). Delegates to events.Dedup bound to tx.
func (txDeduper) SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (bool, error) {
	return events.NewDedup(tx).SeenOrMark(ctx, consumer, eventID)
}
