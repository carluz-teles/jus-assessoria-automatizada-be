package deadline

import (
	"context"

	"github.com/jusassessoria/platform/internal/deadline/deadlinedb"
	"github.com/jusassessoria/platform/lib/database"
)

// ai_summary_store.go is the aiSummaryStore adapter — the write-through half of the "O que
// aconteceu"/"O que fazer" persistence (migration 0036). Like pgSuggestionStore, it WRITES
// on the READ path (GET /v1/prazos/:id/suggested-tasks), so it opens its own tenant-scoped
// tx via the unit of work (RLS barrier 2) instead of enrolling in a caller's tx: the
// suggester runs on a GET and the persist is best-effort (log-not-fail), never blocking the
// F2 pre-fill. `ai_summary IS NULL` in the UPDATE (queries/deadline.sql) makes the write
// idempotent at the DB layer too — a race between two first-opens is a safe no-op on the
// loser, never an error.

// pgAISummaryStore persists the AI summary/recommendation over the unit of work (pool).
type pgAISummaryStore struct {
	uow database.UnitOfWork
}

var _ aiSummaryStore = (*pgAISummaryStore)(nil)

// NewAISummaryStore returns the aiSummaryStore over the unit of work. Each call writes (at
// most) one row inside its own tenant-scoped tx.
func NewAISummaryStore(uow database.UnitOfWork) aiSummaryStore { return &pgAISummaryStore{uow: uow} }

// SaveAISummary best-effort persists the summary/recommendation the LLM answered with the
// FIRST time a prazo is summarized. A concurrent/redelivered call that loses the `ai_summary
// IS NULL` race simply updates 0 rows — not an error, since write-once is the point (no
// invalidation path exists).
func (s *pgAISummaryStore) SaveAISummary(ctx context.Context, tenantID, deadlineID, summary, recommendation string) error {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	deadline, err := parseUUID(deadlineID)
	if err != nil {
		return err
	}

	return s.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		_, err := deadlinedb.New(tx).SetDeadlineAISummary(ctx, deadlinedb.SetDeadlineAISummaryParams{
			AiSummary:        textToNull(summary),
			AiRecommendation: textToNull(recommendation),
			ID:               deadline,
			TenantID:         tenant,
		})
		if err != nil {
			return database.WrapInfra(err)
		}
		return nil
	})
}
