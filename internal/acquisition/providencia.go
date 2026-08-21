package acquisition

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// providencia.go owns the aprovar/descartar actions on an AI-suggested providência (the
// buttons on the "Analisar esta intimação" card). Both flip ONE providência's status inside
// its ai_providencias jsonb array by index:
//
//   - Aprovar  → APPROVED. The REAL task is NOT created here: acquisition never imports the
//     deadline slice's entity. The FE orchestrates — it first calls POST /v1/tasks (deadline)
//     with the providência's title/description/assignee/due_date/intimation_id, then calls this
//     endpoint to mark the providência APPROVED. This keeps the two slices decoupled (no cross-
//     slice import, no new event) while reusing the existing task-creation endpoint.
//   - Descartar → DISCARDED.
//
// Both are idempotent (re-approving an APPROVED item, or a status flip on an already-terminal
// item, is a no-op success — the FE may retry) and run in one tenant-scoped tx (RLS barrier 2)
// over the unit of work, with a FOR UPDATE read-modify-write so concurrent flips don't clobber
// each other's sibling items.

// ProvidenciaActionUseCase flips a providência's status (APPROVED/DISCARDED) by index.
type ProvidenciaActionUseCase struct {
	uow database.UnitOfWork
}

// NewProvidenciaActionUseCase wires the aprovar/descartar use case over the unit of work.
func NewProvidenciaActionUseCase(uow database.UnitOfWork) *ProvidenciaActionUseCase {
	return &ProvidenciaActionUseCase{uow: uow}
}

// Aprovar marks the providência at idx APPROVED and records the id of the real task the FE
// already created (POST /v1/tasks) so the approved row can link back to it. A blank taskID
// leaves the reference nil (idempotent re-approve without a fresh task).
func (uc *ProvidenciaActionUseCase) Aprovar(ctx context.Context, tenantID, intimationID string, idx int, taskID string) (IntimacaoAnaliseView, error) {
	return uc.setStatus(ctx, tenantID, intimationID, idx, ProvidenciaStatusApproved, taskID)
}

// Descartar marks the providência at idx DISCARDED.
func (uc *ProvidenciaActionUseCase) Descartar(ctx context.Context, tenantID, intimationID string, idx int) (IntimacaoAnaliseView, error) {
	return uc.setStatus(ctx, tenantID, intimationID, idx, ProvidenciaStatusDiscarded, "")
}

// setStatus loads the ai_providencias jsonb (FOR UPDATE), flips the item at idx to the target
// status, and writes it back — all in one tenant-scoped tx. A non-uuid id or an out-of-range
// idx is a typed 400; a missing/foreign intimation (or one never analysed) is a typed 404.
// Idempotent: if the item is already in the target status the write still runs (a no-op), so a
// retry after a network blip is safe. Returns the refreshed analysis view so the FE reidrates.
func (uc *ProvidenciaActionUseCase) setStatus(ctx context.Context, tenantID, intimationID string, idx int, status, taskID string) (IntimacaoAnaliseView, error) {
	if idx < 0 {
		return IntimacaoAnaliseView{}, apperr.NewInvalid("índice de providência inválido")
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return IntimacaoAnaliseView{}, apperr.NewInvalid("tenant id inválido")
	}
	iid, err := uuid.Parse(intimationID)
	if err != nil {
		return IntimacaoAnaliseView{}, apperr.NewInvalid("id de intimação inválido")
	}

	var view IntimacaoAnaliseView
	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		q := acquisitiondb.New(tx)
		row, err := q.GetIntimationProvidenciasForUpdate(ctx, acquisitiondb.GetIntimationProvidenciasForUpdateParams{ID: iid, TenantID: tid})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIntimationNotFound
		}
		if err != nil {
			return database.WrapInfra(err)
		}
		// A never-analysed intimation has no providências to act on.
		if !row.AiAnalyzedAt.Valid {
			return ErrIntimationNotFound
		}

		prov := decodeProvidencias(row.AiProvidencias)
		if idx >= len(prov) {
			return apperr.NewInvalid("índice de providência fora do intervalo")
		}
		prov[idx].Status = status
		if taskID != "" {
			prov[idx].TaskID = &taskID
		}

		raw, err := json.Marshal(prov)
		if err != nil {
			return database.WrapInfra(err)
		}
		if err := q.SetIntimationProvidencias(ctx, acquisitiondb.SetIntimationProvidenciasParams{
			AiProvidencias: raw,
			ID:             iid,
			TenantID:       tid,
		}); err != nil {
			return database.WrapInfra(err)
		}

		view = IntimacaoAnaliseView{
			Summary:      "", // action responses carry only the refreshed providências
			Providencias: prov,
			AnalyzedAt:   row.AiAnalyzedAt.Time,
		}
		return nil
	})
	if err != nil {
		return IntimacaoAnaliseView{}, err
	}
	return view, nil
}
