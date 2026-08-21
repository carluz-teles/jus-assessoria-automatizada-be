package acquisition

import (
	"context"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// analise_store.go is the analiseStore adapter — the write half of the intimation AI
// analysis (migration 0051). Like pgResumoStore it opens its own tenant-scoped tx via the
// unit of work (RLS barrier 2) instead of enrolling in a caller's tx. Unlike pgResumoStore
// the UPDATE has NO `IS NULL` guard: the analysis is re-executable ("Gerar novamente"
// OVERWRITES), so every call updates the row.

// pgAnaliseStore persists the intimation AI analysis over the unit of work (pool).
type pgAnaliseStore struct {
	uow database.UnitOfWork
}

var _ analiseStore = (*pgAnaliseStore)(nil)

// NewAnaliseStore returns the analiseStore over the unit of work. Each call overwrites the
// three ai_* columns of one intimation inside its own tenant-scoped tx.
func NewAnaliseStore(uow database.UnitOfWork) analiseStore { return &pgAnaliseStore{uow: uow} }

// SaveAnalise overwrites the intimation's ai_summary/ai_providencias/ai_analyzed_at.
// summary "" + providencias "[]" is the valid degraded write (analysed, IA unavailable).
func (s *pgAnaliseStore) SaveAnalise(ctx context.Context, tenantID, intimationID, summary string, providencias []byte) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	iid, err := uuid.Parse(intimationID)
	if err != nil {
		return apperr.NewInvalid("id de intimação inválido")
	}
	if len(providencias) == 0 {
		providencias = []byte("[]")
	}

	// The column is text NULL; an empty summary is stored as "" (not NULL) so ai_analyzed_at
	// being set is the sole "analysed" signal — degraded and rich analyses both stamp it.
	sum := summary

	return s.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if err := acquisitiondb.New(tx).SetIntimationAIAnalysis(ctx, acquisitiondb.SetIntimationAIAnalysisParams{
			AiSummary:      &sum,
			AiProvidencias: providencias,
			ID:             iid,
			TenantID:       tid,
		}); err != nil {
			return database.WrapInfra(err)
		}
		return nil
	})
}
