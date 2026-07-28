package events

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jusassessoria/platform/lib/database"
)

// markProcessed records that consumer has handled event_id. ON CONFLICT DO NOTHING
// makes the insert idempotent: a repeat leaves the row untouched and reports zero
// rows affected, which is how SeenOrMark tells a duplicate from a first sighting.
const markProcessed = `INSERT INTO processed_event (consumer, event_id)
	VALUES ($1, $2) ON CONFLICT DO NOTHING`

// execer is the sliver of a pgx pool Dedup needs: a single Exec. Depending on it
// rather than *pgxpool.Pool keeps SeenOrMark unit-testable with pgxmock, and lets
// the caller pass either the pool (dedup on its own) or a database.Tx (dedup in the
// same transaction as the effect, docs erd-backend §4c.3).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Dedup is the consumer-side idempotency guard. The relay is at-least-once by
// design, so every listener MUST check here before applying an effect — it is the
// counterpart that makes duplicate delivery safe (docs erd-backend §4c.3).
type Dedup struct {
	pool execer
}

// NewDedup returns a Dedup backed by pool. Inject *pgxpool.Pool in production, or a
// database.Tx to mark the event in the same transaction as the effect it guards.
func NewDedup(pool execer) *Dedup { return &Dedup{pool: pool} }

// SeenOrMark atomically records (consumer, eventID) and reports whether it was
// already there. seen=true means a prior delivery already handled this event for
// this consumer, so the listener should no-op and ack; seen=false means this call
// won the insert and the listener should proceed. The key is (consumer, eventID),
// not eventID alone: each consumer dedups independently, so one consumer marking an
// event never blocks another (docs erd-backend §4c.3).
func (d *Dedup) SeenOrMark(ctx context.Context, consumer, eventID string) (seen bool, err error) {
	tag, err := d.pool.Exec(ctx, markProcessed, consumer, eventID)
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return tag.RowsAffected() == 0, nil
}
