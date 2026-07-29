package acquisition

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// Backfill onboarding constants. The horizon is how far back the first sync
// reaches (one year); the window is how many days each sync slice covers. The
// slice count is ceil(horizon/window) = 53 for 365/7 — the last slice is the
// short remainder (365 = 52*7 + 1).
const (
	BackfillHorizonDays = 365
	BackfillWindowDays  = 7
)

// Backfill job lifecycle values written by this slice. RUNNING is the normal
// landing state; COMPLETED is only used for a degenerate zero-slice horizon
// (nothing to sync). PARTIAL and the counters are the sync slice's concern.
const (
	BackfillStatusRunning   = "RUNNING"
	BackfillStatusCompleted = "COMPLETED"
)

// consumerBackfill is this listener's identity in processed_event. Dedup is
// per-consumer, so marking an event here never blocks another consumer.
const consumerBackfill = "acquisition.backfill"

// dateLayout is the wire format for the window bounds in the sync_requested
// payload — bare dates, matching the backfill_job.window_from/to date columns.
const dateLayout = "2006-01-02"

// calculateSlices returns how many window-sized slices cover a horizon: the
// ceiling of horizonDays/windowDays. A non-positive horizon or window means
// nothing to slice and returns 0 (the caller then creates a COMPLETED job).
func calculateSlices(horizonDays, windowDays int) int {
	if horizonDays <= 0 || windowDays <= 0 {
		return 0
	}
	return (horizonDays + windowDays - 1) / windowDays
}

// syncWindow is one [from, to) date slice of the backfill horizon. The bounds
// are date-only (UTC midnight); consecutive windows share an edge
// (window[i].To == window[i+1].From) so together they tile [from, to) with no
// gap or overlap.
type syncWindow struct {
	From time.Time
	To   time.Time
}

// buildSyncWindows tiles [from, to) into consecutive windowDays-wide slices. The
// final slice is clamped to `to`, so it may be shorter than windowDays (a
// 365-day span in 7-day windows yields 53 slices, the last one day long). An
// empty or inverted range, or a non-positive window, yields no windows.
func buildSyncWindows(from, to time.Time, windowDays int) []syncWindow {
	windows := []syncWindow{}
	if windowDays <= 0 {
		return windows
	}
	for cur := from; cur.Before(to); {
		next := cur.AddDate(0, 0, windowDays)
		if next.After(to) {
			next = to
		}
		windows = append(windows, syncWindow{From: cur, To: next})
		cur = next
	}
	return windows
}

// backfillRepo is the narrow persistence port the backfill use case needs — the
// first-activation guard and the job insert. *pgRepository satisfies it (it also
// satisfies the wider Repository); the use case depends on this minimal port so
// its unit test mocks only these two methods.
type backfillRepo interface {
	BackfillJobExistsByIntegration(ctx context.Context, tx database.Tx, integrationID string) (bool, error)
	InsertBackfillJob(ctx context.Context, tx database.Tx, params BackfillJobParams) (id string, err error)
}

// BackfillJobParams is the insert payload for a backfill_job. The dates are
// date-only; the repository absorbs the conversion to pgtype.Date.
type BackfillJobParams struct {
	TenantID      string
	IntegrationID string
	WindowFrom    time.Time
	WindowTo      time.Time
	TotalSlices   int
	Status        string
}

// BackfillUseCase reacts to integration_activated by opening the onboarding
// backfill: on the first activation of an integration it creates one
// backfill_job and emits N sync_requested events, all in one transaction. It
// depends on the narrow backfillRepo port, the outbox publisher and the unit of
// work — never the concrete pg implementation.
//
// horizonDays/windowDays and now are seams: they default to the package
// constants and time.Now, and the white-box test overrides them to exercise the
// zero-slice edge deterministically.
type BackfillUseCase struct {
	repo        backfillRepo
	outbox      publisher
	uow         database.UnitOfWork
	horizonDays int
	windowDays  int
	now         func() time.Time
}

// backfillOption tunes a BackfillUseCase at construction. The options are
// unexported: production callers take the defaults, only same-package tests
// override the clock or horizon.
type backfillOption func(*BackfillUseCase)

// NewBackfillUseCase wires the backfill use case with production defaults (a
// one-year horizon in weekly windows, the wall clock).
func NewBackfillUseCase(repo backfillRepo, outbox publisher, uow database.UnitOfWork, opts ...backfillOption) *BackfillUseCase {
	uc := &BackfillUseCase{
		repo:        repo,
		outbox:      outbox,
		uow:         uow,
		horizonDays: BackfillHorizonDays,
		windowDays:  BackfillWindowDays,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// OnIntegrationActivated is the backfill use case invoked by the listener. In a
// single unit of work it dedups the event (processed_event, committed with the
// effect), guards against a re-activation (a job already exists for this
// integration), and otherwise creates the job and emits the sync slices. A seen
// event or an existing job is a no-op that still commits the dedup mark, so the
// task acks and never re-dispatches.
//
// tenantID comes from the event payload (a trusted producer inside the same
// system, no Clerk token on the worker) and scopes the transaction's RLS.
func (uc *BackfillUseCase) OnIntegrationActivated(ctx context.Context, ev IntegrationActivated) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := events.NewDedup(tx).SeenOrMark(ctx, consumerBackfill, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		exists, err := uc.repo.BackfillJobExistsByIntegration(ctx, tx, ev.IntegrationID)
		if err != nil {
			return err
		}
		if exists {
			// Only the first activation backfills; the dedup mark is committed so
			// this event is not reprocessed.
			return nil
		}

		return uc.createBackfill(ctx, tx, ev)
	})
}

// createBackfill inserts the backfill_job and publishes one sync_requested event
// per window, all within the caller's tx. A zero-slice horizon lands a COMPLETED
// job and emits nothing.
func (uc *BackfillUseCase) createBackfill(ctx context.Context, tx database.Tx, ev IntegrationActivated) error {
	to := truncateToDate(uc.now())
	from := to.AddDate(0, 0, -uc.horizonDays)
	windows := buildSyncWindows(from, to, uc.windowDays)
	totalSlices := calculateSlices(uc.horizonDays, uc.windowDays)

	status := BackfillStatusRunning
	if totalSlices == 0 {
		status = BackfillStatusCompleted
	}

	jobID, err := uc.repo.InsertBackfillJob(ctx, tx, BackfillJobParams{
		TenantID:      ev.TenantID,
		IntegrationID: ev.IntegrationID,
		WindowFrom:    from,
		WindowTo:      to,
		TotalSlices:   totalSlices,
		Status:        status,
	})
	if err != nil {
		return err
	}

	for i, w := range windows {
		req := newSyncRequested(jobID, ev.TenantID, ev.IntegrationID, i, w)
		if err := uc.outbox.Publish(ctx, tx, req); err != nil {
			return err
		}
	}
	return nil
}

// newSyncRequested builds one sync slice event, minting a fresh v7 event id
// (time-ordered) as the per-slice idempotency key. The window bounds are
// serialized as bare dates to match the schema's date columns.
func newSyncRequested(jobID, tenantID, integrationID string, sliceIndex int, w syncWindow) SyncRequested {
	return SyncRequested{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: jobID},
		BackfillJobID: jobID,
		TenantID:      tenantID,
		IntegrationID: integrationID,
		SliceIndex:    sliceIndex,
		WindowFrom:    w.From.Format(dateLayout),
		WindowTo:      w.To.Format(dateLayout),
	}
}

// truncateToDate drops the clock time, keeping the UTC calendar date at
// midnight — the granularity the window bounds and the date columns work at.
func truncateToDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
