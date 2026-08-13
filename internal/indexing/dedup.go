package indexing

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// dedup.go adapts lib/events' Dedup to the deduper port (ports.go). events.Dedup binds its
// executor at construction, so this builds a fresh one bound to the caller's tx per call — that
// marks the event in the SAME transaction as the chunks it guards (mark + write commit
// atomically; a crash never strands a marked-but-unwritten index). Stateless; the worker injects
// the zero value. Mirrors internal/deadline's txDeduper.
type txDeduper struct{}

// NewDedup returns the deduper the use case marks consumed events with. Stateless — the tx is
// supplied per call — but a constructor keeps slice wiring uniform.
func NewDedup() deduper { return txDeduper{} }

// SeenOrMark marks (consumer, eventID) within tx and reports whether it was already there (a
// replay). Delegates to events.Dedup bound to tx.
func (txDeduper) SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (bool, error) {
	return events.NewDedup(tx).SeenOrMark(ctx, consumer, eventID)
}
