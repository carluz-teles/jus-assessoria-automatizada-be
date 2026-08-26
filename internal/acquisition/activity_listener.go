package acquisition

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// activity_listener.go is acquisition's consumer of draft's review.completed — the
// second (async) half of the process_activity_log feature (migration 0073; the first
// half, the SYNCHRONOUS intimation-analysis producer, lives in analise_store.go). It
// follows the same cross-slice contract shape as internal/notifications/events.go:
// only the dotted type id crosses the import boundary (TypeReviewCompleted below is a
// string literal, matching the relay's routing literal in lib/events/relay.go — NOT an
// alias to draft.TypeReviewCompleted, to keep this package import-free of internal/draft
// per the vertical-slice rule); the payload SHAPE is redefined LOCALLY
// (reviewCompletedPayload) so acquisition never imports draft's entity/repo.

// TypeReviewCompleted is the dotted id this listener consumes. Mirrors
// draft.TypeReviewCompleted's value exactly (see draft/events.go); kept as an
// independent string literal (not an import) to avoid a slice-to-slice import.
const TypeReviewCompleted = "review.completed"

// reviewCompletedPayload is the LOCAL decode shape of review.completed: a draft's
// Revisar call finished. TenantID scopes the write (barrier 1); DraftID resolves the
// owning court_record (via intimation — see queries/activity.sql); Status distinguishes
// a successful review (COMPLETED — the peça is considered "gerada" for the timeline)
// from a failure (FAILED — logged nowhere; review.go today only ever publishes
// COMPLETED, but a future FAILED path is handled defensively here too). Base carries
// the event id (consumer dedup) and the aggregate id.
type reviewCompletedPayload struct {
	events.Base
	TenantID string `json:"tenant_id"`
	DraftID  string `json:"draft_id"`
	ReviewID string `json:"review_id"`
	Status   string `json:"status"`
}

// consumerActivityReviewCompleted is this listener's identity in processed_event —
// distinct from every other acquisition consumer, so its dedup never collides.
const consumerActivityReviewCompleted = "acquisition.activity_review_completed"

// activityCourtRecordResolver is the narrow read port the activity use case needs to
// turn a draft id into the court_record it belongs to. Satisfied by pgRepository
// (ResolveCourtRecordIDForDraftIntimation); faked in unit tests.
type activityCourtRecordResolver interface {
	ResolveCourtRecordIDForDraftIntimation(ctx context.Context, tenantID, draftID string) (string, error)
}

// activityDeduper is the narrow processed_event dedup port, mirroring draft's
// generateDeduper / notifications' deduper — marks (consumer, eventID) inside the
// caller's tx.
type activityDeduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// txActivityDeduper adapts lib/events.Dedup to the activityDeduper port (same pattern
// as draft's txGenerateDeduper).
type txActivityDeduper struct{}

// NewActivityDeduper returns the activityDeduper the activity use case uses.
func NewActivityDeduper() activityDeduper { return txActivityDeduper{} }

func (txActivityDeduper) SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (bool, error) {
	return events.NewDedup(tx).SeenOrMark(ctx, consumer, eventID)
}

// activityLogWriter is the narrow write port wrapping insertProcessActivityLog
// (activity.go, Bloco A's shared helper) so ActivityUseCase stays unit-testable
// without a real Postgres tx. realActivityLogWriter's method body is a single call
// into the shared helper — REUSE, not a duplicate of the insert logic.
type activityLogWriter interface {
	InsertProcessActivityLog(
		ctx context.Context, tx database.Tx,
		tenantID, courtRecordID, eventType string, payload []byte,
	) error
}

// realActivityLogWriter is the production activityLogWriter — a stateless adapter
// over the package-private insertProcessActivityLog helper.
type realActivityLogWriter struct{}

// NewActivityLogWriter returns the activityLogWriter the activity use case uses in
// production (wraps the shared insertProcessActivityLog helper from activity.go).
func NewActivityLogWriter() activityLogWriter { return realActivityLogWriter{} }

func (realActivityLogWriter) InsertProcessActivityLog(
	ctx context.Context, tx database.Tx,
	tenantIDStr, courtRecordIDStr, eventType string, payload []byte,
) error {
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return database.WrapInfra(err)
	}
	courtRecordID, err := uuid.Parse(courtRecordIDStr)
	if err != nil {
		return database.WrapInfra(err)
	}
	return insertProcessActivityLog(ctx, tx, tenantID, courtRecordID, eventType, payload)
}

// ActivityUseCase turns a review.completed event into one process_activity_log row
// (event_type DRAFT_GENERATED). It depends on the resolver, dedup, writer and
// UnitOfWork interfaces, never a concrete implementation (docs §2.5).
type ActivityUseCase struct {
	resolver activityCourtRecordResolver
	dedup    activityDeduper
	writer   activityLogWriter
	uow      database.UnitOfWork
}

// NewActivityUseCase wires the activity use case to its resolver, dedup guard,
// log writer and unit of work.
func NewActivityUseCase(
	resolver activityCourtRecordResolver, dedup activityDeduper,
	writer activityLogWriter, uow database.UnitOfWork,
) *ActivityUseCase {
	return &ActivityUseCase{resolver: resolver, dedup: dedup, writer: writer, uow: uow}
}

// activityDraftGeneratedPayload is the process_activity_log payload for
// DRAFT_GENERATED — the draft and review ids, so the timeline can deep-link to them.
type activityDraftGeneratedPayload struct {
	DraftID  string `json:"draft_id"`
	ReviewID string `json:"review_id"`
}

// OnReviewCompleted handles one review.completed. A FAILED status (a peça was NOT
// successfully generated) is a no-op — DRAFT_GENERATED only ever fires for a
// successful review. Otherwise, in the event's tenant scope, it dedups FIRST (so the
// event is consumed exactly once), then resolves the owning court_record from the
// draft id and appends the DRAFT_GENERATED row. LOG-NOT-FAIL: an unresolvable
// court_record (a blank/processo draft with no intimation, or the draft was deleted)
// or a failed insert is WARNED and swallowed — this consumer never fails the asynq
// task for what is, at most, a missing timeline entry.
func (uc *ActivityUseCase) OnReviewCompleted(ctx context.Context, ev reviewCompletedPayload) error {
	if ev.Status != "" && ev.Status != "COMPLETED" {
		return nil // FAILED (or any non-success status) — nothing to log
	}

	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerActivityReviewCompleted, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		courtRecordID, err := uc.resolver.ResolveCourtRecordIDForDraftIntimation(ctx, ev.TenantID, ev.DraftID)
		if err != nil {
			slog.WarnContext(ctx, "acquisition: activity listener could not resolve court_record for draft",
				slog.String("draft_id", ev.DraftID), slog.Any("error", err))
			return nil // LOG-NOT-FAIL — the dedup mark above still commits
		}
		if courtRecordID == "" {
			slog.WarnContext(ctx, "acquisition: activity listener found no court_record for draft (blank/processo draft?)",
				slog.String("draft_id", ev.DraftID))
			return nil
		}

		payload, err := json.Marshal(activityDraftGeneratedPayload{DraftID: ev.DraftID, ReviewID: ev.ReviewID})
		if err != nil {
			payload = []byte("{}")
		}
		insertErr := uc.writer.InsertProcessActivityLog(
			ctx, tx, ev.TenantID, courtRecordID, ActivityEventDraftGenerated, payload,
		)
		if insertErr != nil {
			warnActivityLogFailed(ctx, ActivityEventDraftGenerated, insertErr)
		}
		return nil
	})
}

// ActivityListener is acquisition's asynq consumer for review.completed. It holds no
// transport state; ActivityUseCase owns persistence and the transaction boundary.
// Kept as a SEPARATE type from Listener (listener.go) — a distinct name avoids any
// confusion with that type's own Register (they mount on the SAME "notifications"
// queue's mux but are two independently-composable consumers).
type ActivityListener struct {
	uc *ActivityUseCase
}

// NewActivityListener wires the listener to the activity use case.
func NewActivityListener(uc *ActivityUseCase) *ActivityListener {
	return &ActivityListener{uc: uc}
}

// Register mounts the review.completed handler on the asynq mux. Called on the SAME
// mux as notifications.NewListener(...).Register(mux) (both drain the "notifications"
// queue) — see cmd/worker-ingestao/main.go.
func (l *ActivityListener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeReviewCompleted, l.handleReviewCompleted)
}

// handleReviewCompleted is the asynq.HandlerFunc for review.completed. A decode fault
// wraps asynq.SkipRetry (archived, not retried); the use case itself never returns an
// error for a resolvable-but-missing court_record (LOG-NOT-FAIL) — only a genuine
// infra fault (dedup/tx failure) stays retryable.
func (l *ActivityListener) handleReviewCompleted(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[reviewCompletedPayload](t)
	if err != nil {
		return err
	}
	return l.uc.OnReviewCompleted(ctx, ev)
}
