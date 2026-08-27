package acquisition

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/database"
)

// activity.go is the shared write half of process_activity_log (migration 0073) — the
// process cockpit's "Atividade" timeline. Every producer (this slice's intimation analysis,
// and later the draft-generation listener) calls insertProcessActivityLog INSIDE ITS OWN tx,
// right after the write it documents, so the log never diverges from the mutation it records.
//
// LOG-NOT-FAIL: insertProcessActivityLog itself never fails the caller's tx — callers invoke
// it from inside their uow.Do closure and, on error, log-and-continue rather than returning
// the error (the log row is best-effort; the mutation it documents already committed the
// moment this call runs, in the sense that a failed log insert must not roll it back).

// ActivityEventIntimationAnalysisCompleted fires when an intimation's AI analysis finishes
// with a real (non-degraded) result — see analise_store.go.
const ActivityEventIntimationAnalysisCompleted = "INTIMATION_ANALYSIS_COMPLETED"

// ActivityEventDraftGenerated fires when a peça draft is generated for a process. Reserved
// for the future draft-generation listener (Bloco B) — the CHECK constraint already allows
// it (migration 0073) so that producer needs no new migration.
const ActivityEventDraftGenerated = "DRAFT_GENERATED"

// insertProcessActivityLog appends one row to process_activity_log inside tx. Callers own the
// LOG-NOT-FAIL contract: on error here, log-and-continue instead of propagating (a failed
// activity-log insert must never roll back the write it documents).
func insertProcessActivityLog(ctx context.Context, tx database.Tx, tenantID, courtRecordID uuid.UUID, eventType string, payload []byte) error {
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if err := acquisitiondb.New(tx).InsertProcessActivityLog(ctx, acquisitiondb.InsertProcessActivityLogParams{
		TenantID:      tenantID,
		CourtRecordID: courtRecordID,
		EventType:     eventType,
		Payload:       payload,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// warnActivityLogFailed is the shared log line for a LOG-NOT-FAIL insertProcessActivityLog
// failure, so every producer logs it the same way.
func warnActivityLogFailed(ctx context.Context, eventType string, err error) {
	slog.WarnContext(ctx, "acquisition: insert process activity log failed",
		slog.String("event_type", eventType), slog.Any("error", err))
}
