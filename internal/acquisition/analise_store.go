package acquisition

import (
	"context"
	"encoding/json"

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
//
// On a SUCCESSFUL (non-degraded) analysis it also appends one process_activity_log row
// (migration 0073) in the SAME tx as the UPDATE — see activity.go. The degraded write
// (logActivity=false) never logs: an "IA indisponível" write is not something to surface on
// the process timeline.

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
// logActivity=true also appends a process_activity_log row (INTIMATION_ANALYSIS_COMPLETED)
// in the same tx — a failure to log is LOG-NOT-FAIL: it never rolls back the analysis, which
// already committed the columns above.
func (s *pgAnaliseStore) SaveAnalise(ctx context.Context, tenantID, intimationID, summary, ato string, providencias []byte, logActivity bool) error {
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
	// ai_act: NULL quando não classificado (degradado/vazio), senão o ato — o read
	// model cai no class+subject quando NULL.
	var act *string
	if ato != "" {
		act = &ato
	}

	return s.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		courtRecordID, err := acquisitiondb.New(tx).SetIntimationAIAnalysis(ctx, acquisitiondb.SetIntimationAIAnalysisParams{
			AiSummary:      &sum,
			AiProvidencias: providencias,
			AiAct:          act,
			ID:             iid,
			TenantID:       tid,
		})
		if err != nil {
			return database.WrapInfra(err)
		}

		if !logActivity {
			return nil
		}
		payload, err := json.Marshal(activityIntimationAnalysisPayload{IntimationID: intimationID})
		if err != nil {
			payload = []byte("{}")
		}
		if err := insertProcessActivityLog(ctx, tx, tid, courtRecordID, ActivityEventIntimationAnalysisCompleted, payload); err != nil {
			warnActivityLogFailed(ctx, ActivityEventIntimationAnalysisCompleted, err)
		}
		return nil
	})
}

// activityIntimationAnalysisPayload is the process_activity_log payload for
// INTIMATION_ANALYSIS_COMPLETED — just the intimation id, so the timeline can deep-link to it.
type activityIntimationAnalysisPayload struct {
	IntimationID string `json:"intimation_id"`
}
