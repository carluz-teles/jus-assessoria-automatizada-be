package acquisition

import (
	"context"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// resumo_store.go is the resumoStore adapter — the write-through half of the AI process
// resume persistence (migration 0040). Like pgAISummaryStore, it WRITES on the READ path
// (GET /v1/processos/:id/resume), so it opens its own tenant-scoped tx via the unit of
// work (RLS barrier 2) instead of enrolling in a caller's tx: the summarizer runs on a
// GET and the persist is best-effort (log-not-fail), never blocking the response.
// `ai_resume IS NULL` in the UPDATE (queries/resumo.sql) makes the write idempotent at
// the DB layer too — a race between two first-opens is a safe no-op on the loser,
// never an error.

// pgResumoStore persists the AI resume over the unit of work (pool).
type pgResumoStore struct {
	uow database.UnitOfWork
}

var _ resumoStore = (*pgResumoStore)(nil)

// NewResumoStore returns the resumoStore over the unit of work. Each call writes (at
// most) one row inside its own tenant-scoped tx.
func NewResumoStore(uow database.UnitOfWork) resumoStore { return &pgResumoStore{uow: uow} }

// SaveResumo best-effort persists the AI resume the LLM answered with the FIRST time a
// court_record is summarized. A concurrent/redelivered call that loses the `ai_resume
// IS NULL` race simply updates 0 rows — not an error, since write-once is the point (no
// invalidation path exists).
func (s *pgResumoStore) SaveResumo(ctx context.Context, tenantID, courtRecordID string, rawJSON []byte) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	crid, err := uuid.Parse(courtRecordID)
	if err != nil {
		return apperr.NewInvalid("id de processo inválido")
	}

	return s.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if err := acquisitiondb.New(tx).SetCourtRecordAIResume(ctx, acquisitiondb.SetCourtRecordAIResumeParams{
			AiResume: rawJSON,
			ID:       crid,
			TenantID: tid,
		}); err != nil {
			return database.WrapInfra(err)
		}
		return nil
	})
}
