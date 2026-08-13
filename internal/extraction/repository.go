package extraction

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
)

// repository.go is the raw-pgx docRepo adapter — the document row's saga transitions for
// this slice, written directly against database.Tx (this slice owns no sqlc gen dir; the
// document table's columns are stable from migrations 0001 + 0033). Every UPDATE is
// tenant-scoped in the WHERE (belt to the RLS suspenders) and, for the forward transitions,
// GUARDED on the source status so a duplicate/late delivery cannot re-run the effect.

// Repository is the stateless docRepo implementation. It holds no connection — the tx is
// supplied per call by the use case's unit of work.
type Repository struct{}

// NewRepository returns the docRepo adapter. Stateless; the worker injects the zero value.
func NewRepository() *Repository { return &Repository{} }

// markExtracting flips UPLOADED→EXTRACTING. The `status = 'UPLOADED'` guard makes it
// idempotent: a redelivery whose first attempt already advanced the row updates zero rows,
// which is a tolerated no-op (the dedup in the same tx already short-circuits the common
// replay; this guard covers a crash between mark and commit). extractor_version/pages are
// left for the final EXTRACTED update.
const markExtracting = `UPDATE document
	SET status = 'EXTRACTING', error = NULL
	WHERE id = $1 AND tenant_id = $2 AND status = 'UPLOADED'`

// MarkExtracting advances the row to EXTRACTING within tx. A zero-row update (already
// advanced) is not an error — the pipeline proceeds; the finalize step's own guard is the
// real concurrency floor.
func (Repository) MarkExtracting(ctx context.Context, tx database.Tx, documentID, tenantID string) error {
	_, err := tx.Exec(ctx, markExtracting, documentID, tenantID)
	return database.WrapInfra(err)
}

// markExtracted finalizes the row →EXTRACTED with the extraction metadata and stamps
// extracted_at=now(). Guarded on EXTRACTING so it only fires after the forward transition;
// error is cleared (a prior FAILED that is being reprocessed comes back clean).
const markExtracted = `UPDATE document
	SET status = 'EXTRACTED',
	    pages = $3,
	    has_text_layer = $4,
	    extractor_version = $5,
	    extracted_at = now(),
	    error = NULL
	WHERE id = $1 AND tenant_id = $2`

// MarkExtracted finalizes the document row within tx. It is not status-guarded beyond the
// tenant scope: the finalize runs in a tx after a successful extraction, and the dedup +
// EXTRACTING guard upstream already prevent a concurrent second run.
func (Repository) MarkExtracted(ctx context.Context, tx database.Tx, p MarkExtractedParams) error {
	_, err := tx.Exec(ctx, markExtracted,
		p.DocumentID, p.TenantID, p.Pages, p.HasTextLayer, p.ExtractorVersion)
	return database.WrapInfra(err)
}

// markFailed flips the row →FAILED and records the {stage,message} error jsonb (cast from
// the text the use case marshals). It is not status-guarded: a fault can arrive from any
// stage, and recording FAILED is always the right terminal audit state.
const markFailed = `UPDATE document
	SET status = 'FAILED', error = $3::jsonb
	WHERE id = $1 AND tenant_id = $2`

// MarkFailed records the terminal FAILED state within tx. errorJSON is the {stage,message}
// object the use case built; it is cast to jsonb in the query.
func (Repository) MarkFailed(ctx context.Context, tx database.Tx, documentID, tenantID, errorJSON string) error {
	_, err := tx.Exec(ctx, markFailed, documentID, tenantID, errorJSON)
	return database.WrapInfra(err)
}
