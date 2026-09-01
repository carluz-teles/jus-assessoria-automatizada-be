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
// analysis (migration 0051, now also publishing acquisition.intimation.analyzed per
// docs/erd-costura-providencia-tarefa-peca.md instead of persisting ai_providencias jsonb).
// Like pgResumoStore it opens its own tenant-scoped tx via the unit of work (RLS barrier 2)
// instead of enrolling in a caller's tx. Unlike pgResumoStore the UPDATE has NO `IS NULL`
// guard: the analysis is re-executable ("Gerar novamente" OVERWRITES), so every call
// updates the row.
//
// On a SUCCESSFUL (non-degraded) analysis it also appends one process_activity_log row
// (migration 0073) in the SAME tx as the UPDATE — see activity.go. The degraded write
// (logActivity=false) never logs: an "IA indisponível" write is not something to surface on
// the process timeline. The event publishes regardless of logActivity — even a degraded
// re-run must tell the actionitem listener to clear the previous run's still-pending
// suggestions (its guard aditivo, internal/actionitem/domain.go).

// pgAnaliseStore persists the intimation AI analysis over the unit of work (pool) and
// publishes acquisition.intimation.analyzed in the same tx.
type pgAnaliseStore struct {
	uow    database.UnitOfWork
	outbox publisher
}

var _ analiseStore = (*pgAnaliseStore)(nil)

// NewAnaliseStore returns the analiseStore over the unit of work + outbox. Each call
// overwrites the intimation's ai_summary/ai_analyzed_at and publishes the analysis event,
// inside its own tenant-scoped tx.
func NewAnaliseStore(uow database.UnitOfWork, outbox publisher) analiseStore {
	return &pgAnaliseStore{uow: uow, outbox: outbox}
}

// SaveAnalise overwrites the intimation's ai_summary/ai_act/ai_analyzed_at and publishes
// acquisition.intimation.analyzed carrying p.Providencias, in one tx. summary="" is the
// valid degraded write (analysed, IA unavailable) — the event still publishes (with
// whatever candidates are given, possibly none), so the actionitem listener's guard
// aditivo runs even on a degraded re-run. logActivity=true also appends a
// process_activity_log row (INTIMATION_ANALYSIS_COMPLETED) in the same tx — a failure to
// log is LOG-NOT-FAIL: it never rolls back the analysis, which already committed.
func (s *pgAnaliseStore) SaveAnalise(ctx context.Context, p SaveAnaliseParams) error {
	tid, err := uuid.Parse(p.TenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	iid, err := uuid.Parse(p.IntimationID)
	if err != nil {
		return apperr.NewInvalid("id de intimação inválido")
	}

	// The column is text NULL; an empty summary is stored as "" (not NULL) so ai_analyzed_at
	// being set is the sole "analysed" signal — degraded and rich analyses both stamp it.
	sum := p.Summary
	// ai_act: NULL quando não classificado (degradado/vazio), senão o ato — o read
	// model cai no class+subject quando NULL.
	var act *string
	if p.Ato != "" {
		act = &p.Ato
	}

	return s.uow.Do(ctx, p.TenantID, func(tx database.Tx) error {
		courtRecordID, err := acquisitiondb.New(tx).SetIntimationAIAnalysis(ctx, acquisitiondb.SetIntimationAIAnalysisParams{
			AiSummary: &sum,
			AiAct:     act,
			ID:        iid,
			TenantID:  tid,
		})
		if err != nil {
			return database.WrapInfra(err)
		}

		ev := newIntimationAnalyzed(p.TenantID, p.IntimationID, courtRecordID.String(), p.DeadlineID, p.Providencias)
		if err := s.outbox.Publish(ctx, tx, ev); err != nil {
			return err
		}

		if !p.LogActivity {
			return nil
		}
		payload, err := json.Marshal(activityIntimationAnalysisPayload{IntimationID: p.IntimationID})
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
