package acquisition

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/obs"
)

// Backfill onboarding constants. The horizon is how far back the first sync
// reaches; the window is how many days each sync slice covers. The slice count is
// ceil(horizon/window) = 5 for 30/7 — the last slice is the short remainder
// (30 = 4*7 + 2). The horizon was cut 365→30 for the lean-ingestion policy: the
// onboarding backfill only needs the recent, live-deadline window, so the burst
// drops from 53 slices to 5.
const (
	BackfillHorizonDays = 30
	BackfillWindowDays  = 7
)

// Backfill job lifecycle values written by this slice. RUNNING is the landing
// state; a job finalizes COMPLETED (every slice succeeded — also the degenerate
// zero-slice horizon) or PARTIAL (at least one slice failed).
const (
	BackfillStatusRunning   = "RUNNING"
	BackfillStatusCompleted = "COMPLETED"
	BackfillStatusPartial   = "PARTIAL"
)

// consumerBackfill is the integration_activated listener's identity in
// processed_event. Dedup is per-consumer, so marking an event here never blocks
// another consumer.
const consumerBackfill = "acquisition.backfill"

// consumerBackfillCounter is the completion counter's identity in
// processed_event — a DISTINCT consumer from consumerBackfill so the counter's
// dedup of a sync_completed/sync_failed never collides with any other consumer.
const consumerBackfillCounter = "acquisition.backfill_counter"

// consumerWatchedOABReenabled is the watched_oab_reenabled listener's identity in
// processed_event — a DISTINCT consumer from consumerBackfill (which dedups
// integration_activated), so the two event families' dedup spaces never collide.
const consumerWatchedOABReenabled = "acquisition.watched_oab_reenabled"

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

// backfillRepo is the narrow persistence port the backfill use case needs.
// AddOrEnableWatchedOAB is the per-OAB entry point that replaced the old whole-
// integration first-activation guard: it tells the caller, per watched OAB, whether
// it is brand-new (needsHistory) or a real re-enable (needsCatchUp) — see
// watched_oab.sql. InsertBackfillJob opens the delta backfill for the newly-watched
// OABs; ClearWatchedOABCatchUp resolves a completed catch-up. The remaining three
// advance and close the job (the completion counter). *pgRepository satisfies it
// (it also satisfies the wider Repository); the use case depends on this minimal
// port so its unit test mocks only these methods.
type backfillRepo interface {
	AddOrEnableWatchedOAB(ctx context.Context, tx database.Tx, tenantID, integrationID, oabKey string) (row WatchedOAB, needsHistory, needsCatchUp bool, err error)
	InsertBackfillJob(ctx context.Context, tx database.Tx, params BackfillJobParams) (id string, err error)
	ClearWatchedOABCatchUp(ctx context.Context, tx database.Tx, tenantID, integrationID, oabKey string, dispatchedSince time.Time) error

	// The completion-counter path: each closing slice atomically bumps one counter
	// and reads back the job's tallies (a row lock serializes concurrent closes);
	// the finalizing slice flips RUNNING → COMPLETED/PARTIAL. A miss (wrong tenant
	// or gone) surfaces as ErrBackfillJobNotFound.
	IncrementBackfillSlicesOK(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (BackfillCounters, error)
	IncrementBackfillSlicesError(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (BackfillCounters, error)
	FinalizeBackfillJob(ctx context.Context, tx database.Tx, tenantID, backfillJobID, status string) error
}

// BackfillCounters is a backfill_job's tally and status as read back by an
// atomic slice-counter increment (the UPDATE ... RETURNING). SlicesOK+SlicesError
// reaching TotalSlices while Status is still RUNNING means this increment closed
// the last slice — the signal to finalize.
type BackfillCounters struct {
	TotalSlices int
	SlicesOK    int
	SlicesError int
	Status      string
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
	// history is the onboarding cutover: when set (INGESTION_ENABLED), a fresh
	// activation catches the tenant up from the stored national firehose instead of
	// firing the per-OAB backfill. nil keeps the legacy per-OAB path.
	history     historyMatcher
	historyDays int
}

// historyMatcher catches one tenant up from the already-stored firehose (the
// MatchUseCase). The backfill use case depends on this narrow port so the cutover is
// a swapped dependency, not a rewrite.
type historyMatcher interface {
	MatchTenantSince(ctx context.Context, tenantID string, since time.Time) error
}

// backfillOption tunes a BackfillUseCase at construction. Same-package tests
// override the clock and horizon; production overrides the window size via the
// exported WithBackfillWindowDays (wired from BACKFILL_WINDOW_DAYS).
type backfillOption func(*BackfillUseCase)

// NewBackfillUseCase wires the backfill use case with production defaults (a
// 30-day horizon in weekly windows, the wall clock).
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

// WithBackfillWindowDays overrides how many days each backfill sync slice covers.
// Production wires this from BACKFILL_WINDOW_DAYS so the window can be widened
// without a rebuild; a non-positive value keeps the BackfillWindowDays default.
// Wider windows tile the horizon into FEWER slices, so the backfill
// issues fewer DJEN round-trips — the dominant cost is the per-request latency of
// the residential-proxy egress, not the item volume, so halving the request count
// nearly halves the wall clock. The ceiling is DJEN's ~10000-count-per-query cap:
// a window must not hold more than that for a single OAB (30 days keeps even a
// high-volume OAB well under it).
func WithBackfillWindowDays(days int) backfillOption {
	return func(uc *BackfillUseCase) {
		if days > 0 {
			uc.windowDays = days
		}
	}
}

// WithHistoryMatcher turns on the onboarding cutover: a fresh activation catches the
// tenant up from the stored firehose over the last `days` days (the bootstrap window)
// instead of firing the per-OAB backfill. Wired from the worker when INGESTION_ENABLED.
func WithHistoryMatcher(m historyMatcher, days int) backfillOption {
	return func(uc *BackfillUseCase) {
		uc.history = m
		uc.historyDays = days
	}
}

// OnIntegrationActivated is the backfill use case invoked by the listener. In a
// single unit of work it dedups the event (processed_event, committed with the
// effect), then upserts EVERY OAB in the scope via AddOrEnableWatchedOAB — the
// per-OAB replacement for the old whole-integration first-activation guard, which
// is what silently swallowed an OAB added to an already-active integration (the bug
// this fixes: a delta backfill now fires for that OAB, not the whole scope again and
// not nothing). Brand-new OABs (needsHistory) get the full onboarding backfill,
// scoped to just them; OABs that were disabled and are back in the scope
// (needsCatchUp — e.g. a full-scope re-POST after a Termos toggle-off) get a
// catch-up sync scoped to their downtime, same as ToggleWatchedOAB/
// OnWatchedOABReenabled. A seen event or an unchanged scope is a no-op that still
// commits the dedup mark, so the task acks and never re-dispatches.
//
// tenantID comes from the event payload (a trusted producer inside the same
// system, no Clerk token on the worker) and scopes the transaction's RLS.
func (uc *BackfillUseCase) OnIntegrationActivated(ctx context.Context, ev IntegrationActivated) error {
	// History cutover (uc.history != nil): no per-OAB DJEN calls — the stored firehose
	// already has the history, and the daily match covers forward. postCommitSince
	// aggregates the earliest "since" any new/re-enabled OAB needs the post-commit
	// MatchTenantSince to cover; nil means nothing new to catch up.
	var postCommitSince *time.Time

	if err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := events.NewDedup(tx).SeenOrMark(ctx, consumerBackfill, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		var newOABKeys []string
		var catchUps []WatchedOAB
		for _, oabKey := range watchedOABKeys(ev.Scope) {
			row, needsHistory, needsCatchUp, err := uc.repo.AddOrEnableWatchedOAB(ctx, tx, ev.TenantID, ev.IntegrationID, oabKey)
			if err != nil {
				return err
			}
			switch {
			case needsHistory:
				newOABKeys = append(newOABKeys, oabKey)
			case needsCatchUp:
				catchUps = append(catchUps, row)
			}
		}

		if len(newOABKeys) == 0 && len(catchUps) == 0 {
			// Every OAB in the scope was already watched and enabled — an idempotent
			// re-post. The dedup mark still commits, so this event is not reprocessed.
			return nil
		}

		if uc.history != nil {
			if len(newOABKeys) > 0 {
				since := uc.now().AddDate(0, 0, -uc.historyDays)
				postCommitSince = earlierTime(postCommitSince, &since)
			}
			for _, row := range catchUps {
				postCommitSince = earlierTime(postCommitSince, row.CatchUpSince)
			}
			return nil
		}

		// Legacy path: fire the actual per-OAB work inside the tx.
		if len(newOABKeys) > 0 {
			scope := Scope{OAB: canonicalOABs(newOABKeys)}
			if err := uc.createBackfillForScope(ctx, tx, ev.TenantID, ev.IntegrationID, ev.Source, scope); err != nil {
				return err
			}
		}
		for _, row := range catchUps {
			if row.CatchUpSince == nil {
				// Defensive: needsCatchUp implies a disabled_at existed to derive
				// catch_up_since from (see UpsertWatchedOAB). Should never happen.
				continue
			}
			req := newCatchUpSyncRequested(ev.TenantID, ev.IntegrationID, ev.Source, row.OABKey, *row.CatchUpSince, uc.now())
			if err := uc.outbox.Publish(ctx, tx, req); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Catch the tenant up AFTER the watched_oab commit (the match reads system-wide
	// and writes in its own tenant tx, so the just-committed OABs are visible). The
	// write is idempotent; note it is gated by the activation dedup, so a transient
	// failure here is best-effort — the daily match still covers everything forward.
	if postCommitSince != nil {
		return uc.history.MatchTenantSince(ctx, ev.TenantID, *postCommitSince)
	}
	return nil
}

// OnWatchedOABReenabled reacts to the Termos re-enable toggle (ToggleWatchedOAB): it
// dedups the event under its OWN consumer (consumerWatchedOABReenabled — distinct
// from consumerBackfill's integration_activated dedup space), then either catches the
// tenant up from the stored firehose since ev.CatchUpSince (history cutover) or fires
// one standalone catch-up sync scoped to just that OAB's downtime (legacy path). It
// does NOT clear catch_up_since itself — that only happens when the resulting
// sync_completed lands (OnSyncCompleted), so a failed catch-up leaves the gap marked
// for the next toggle to retry.
func (uc *BackfillUseCase) OnWatchedOABReenabled(ctx context.Context, ev WatchedOABReenabled) error {
	needsMatch := false
	if err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := events.NewDedup(tx).SeenOrMark(ctx, consumerWatchedOABReenabled, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}
		if uc.history != nil {
			needsMatch = true
			return nil
		}
		req := newCatchUpSyncRequested(ev.TenantID, ev.IntegrationID, ev.Source, ev.OABKey, ev.CatchUpSince, uc.now())
		return uc.outbox.Publish(ctx, tx, req)
	}); err != nil {
		return err
	}
	if needsMatch {
		return uc.history.MatchTenantSince(ctx, ev.TenantID, ev.CatchUpSince)
	}
	return nil
}

// createBackfillForScope inserts the backfill_job and publishes one sync_requested
// event per window, all within the caller's tx, scoped to the given delta (not
// necessarily the integration's whole current scope — see OnIntegrationActivated,
// which passes only the newly-watched OABs). A zero-slice horizon lands a COMPLETED
// job and emits nothing.
func (uc *BackfillUseCase) createBackfillForScope(ctx context.Context, tx database.Tx, tenantID, integrationID, source string, scope Scope) error {
	to := truncateToDate(uc.now())
	from := to.AddDate(0, 0, -uc.horizonDays)
	windows := buildSyncWindows(from, to, uc.windowDays)
	totalSlices := calculateSlices(uc.horizonDays, uc.windowDays)

	status := BackfillStatusRunning
	if totalSlices == 0 {
		status = BackfillStatusCompleted
	}

	jobID, err := uc.repo.InsertBackfillJob(ctx, tx, BackfillJobParams{
		TenantID:      tenantID,
		IntegrationID: integrationID,
		WindowFrom:    from,
		WindowTo:      to,
		TotalSlices:   totalSlices,
		Status:        status,
	})
	if err != nil {
		return err
	}

	// Emit newest window first (recent-first). With the worker fetching many slices
	// in parallel, the most recent windows — the ones carrying live deadlines — land
	// in the first batch, so the lawyer sees current intimações within minutes while
	// the older tail fills in behind. slice_index stays chronological (i); only the
	// emission (and therefore processing) order is reversed.
	for i := len(windows) - 1; i >= 0; i-- {
		req := newSyncRequested(jobID, tenantID, integrationID, source, scope, i, windows[i])
		if err := uc.outbox.Publish(ctx, tx, req); err != nil {
			return err
		}
	}

	// Milestone: this is what fans the activation into N sync slices (the burst of
	// sync_requested events) — logging total_slices here explains that volume.
	slog.InfoContext(ctx, "acquisition: backfill started",
		obs.KeyTenantID, tenantID,
		"integration_id", integrationID,
		"source", source,
		"backfill_job_id", jobID,
		"total_slices", totalSlices,
		"window_from", from.Format(dateLayout),
		"window_to", to.Format(dateLayout),
		"oab_count", len(scope.OAB),
	)
	return nil
}

// newCatchUpSyncRequested builds a standalone, one-shot sync slice for a single
// re-enabled OAB, scoped to its downtime window [since, today] — NOT the full
// onboarding horizon. BackfillJobID is empty (there is no backfill_job counter to
// advance — see onSliceClosed's early return); CatchUpOABKey/CatchUpSince are carried
// through so the closing sync_completed can compare-and-clear the OAB's pending
// catch_up_since. The aggregate id reuses watchedOABAggregate (the same
// (tenant|integration|oab_key) v5 derivation WatchedOABReenabled uses) since there is
// no backfill_job id to hang it on.
func newCatchUpSyncRequested(tenantID, integrationID, source, oabKey string, since, now time.Time) SyncRequested {
	from := truncateToDate(since)
	to := truncateToDate(now)
	if to.Before(from) {
		to = from // same-day disable/enable: a zero-width, single-day window
	}
	return SyncRequested{
		Base:          events.Base{EventID: newEventID(), Aggregate: watchedOABAggregate(tenantID, integrationID, oabKey)},
		TenantID:      tenantID,
		IntegrationID: integrationID,
		Source:        source,
		SliceIndex:    0,
		WindowFrom:    from.Format(dateLayout),
		WindowTo:      to.Format(dateLayout),
		Scope:         Scope{OAB: []string{canonicalOAB(oabKey)}},
		CatchUpOABKey: oabKey,
		CatchUpSince:  since.Format(time.RFC3339),
	}
}

// earlierTime returns whichever of a, b is earlier, treating nil as "no opinion" (the
// other value wins; two nils yield nil). Used to aggregate the single post-commit
// MatchTenantSince "since" across possibly several new/re-enabled OABs in one
// integration_activated — the match is idempotent, so using the most inclusive
// (earliest) since is always safe.
func earlierTime(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if b.Before(*a) {
		return b
	}
	return a
}

// newSyncRequested builds one sync slice event, minting a fresh v7 event id
// (time-ordered) as the per-slice idempotency key. Source and scope are carried
// through from the activation so the sync consumer resolves the right connector
// and its OAB discovery scope. The window bounds are serialized as bare dates to
// match the schema's date columns.
func newSyncRequested(jobID, tenantID, integrationID, source string, scope Scope, sliceIndex int, w syncWindow) SyncRequested {
	return SyncRequested{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: jobID},
		BackfillJobID: jobID,
		TenantID:      tenantID,
		IntegrationID: integrationID,
		Source:        source,
		SliceIndex:    sliceIndex,
		WindowFrom:    w.From.Format(dateLayout),
		WindowTo:      w.To.Format(dateLayout),
		Scope:         scope,
	}
}

// sliceClose is the tenant-scoped identity of one closing slice, factored out of
// the two events that report it: sync_completed (ok=true) and sync_failed
// (ok=false). It carries what the counter needs — the event to dedup, the job to
// advance, and the integration to name in the finished event — plus the catch-up
// identity (empty for a normal backfill/scheduled slice) so a re-enable catch-up
// sync's closing event can compare-and-clear its watched_oab.catch_up_since.
type sliceClose struct {
	eventID       string
	tenantID      string
	backfillJobID string
	integrationID string
	ok            bool
	catchUpOABKey string
	catchUpSince  string
}

// OnSyncCompleted advances a backfill's success counter when one of its slices
// finishes OK. It is the "last one turns off the light" counter: the increment
// that closes the final slice finalizes the job and emits backfill_finished.
func (uc *BackfillUseCase) OnSyncCompleted(ctx context.Context, ev SyncCompleted) error {
	return uc.onSliceClosed(ctx, sliceClose{
		eventID:       ev.EventID,
		tenantID:      ev.TenantID,
		backfillJobID: ev.BackfillJobID,
		integrationID: ev.IntegrationID,
		ok:            true,
		catchUpOABKey: ev.CatchUpOABKey,
		catchUpSince:  ev.CatchUpSince,
	})
}

// OnSyncFailed advances a backfill's error counter when one of its slices fails.
// Same finalize path as OnSyncCompleted; a failed slice still counts toward the
// total, and a job with any failed slice finalizes PARTIAL.
func (uc *BackfillUseCase) OnSyncFailed(ctx context.Context, ev SyncFailed) error {
	return uc.onSliceClosed(ctx, sliceClose{
		eventID:       ev.EventID,
		tenantID:      ev.TenantID,
		backfillJobID: ev.BackfillJobID,
		integrationID: ev.IntegrationID,
		ok:            false,
		catchUpOABKey: ev.CatchUpOABKey,
		catchUpSince:  ev.CatchUpSince,
	})
}

// onSliceClosed is the shared counter + catch-up-clear path. A standalone sync with
// neither a backfill job nor a catch-up identity (e.g. a scheduled re-sync) has
// nothing to advance or clear, so it acks before opening any transaction. Otherwise,
// in one unit of work scoped to the event's tenant: dedup the event, then (when a
// backfill job is attached) increment its slice counter and finalize if it was the
// last, and (when this closed a re-enable catch-up sync, only on success) clear the
// OAB's pending catch_up_since. A seen event or a vanished job
// (ErrBackfillJobNotFound) is a no-op for the counter half but does not block the
// catch-up-clear half; the dedup mark commits either way, so the task acks and is
// not redelivered.
func (uc *BackfillUseCase) onSliceClosed(ctx context.Context, c sliceClose) error {
	if c.backfillJobID == "" && c.catchUpOABKey == "" {
		return nil
	}
	return uc.uow.Do(ctx, c.tenantID, func(tx database.Tx) error {
		seen, err := events.NewDedup(tx).SeenOrMark(ctx, consumerBackfillCounter, c.eventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		if c.backfillJobID != "" {
			counters, err := uc.incrementSlice(ctx, tx, c)
			switch {
			case errors.Is(err, ErrBackfillJobNotFound):
				// The job is not visible under this tenant or is gone. Nothing to
				// advance; fall through (the dedup mark still commits).
			case err != nil:
				return err
			default:
				if err := uc.finalizeIfComplete(ctx, tx, c, counters); err != nil {
					return err
				}
			}
		}

		// Only a SUCCESSFUL catch-up sync clears the gap — a failed one deliberately
		// leaves catch_up_since set so the next toggle retries it (see
		// OnWatchedOABReenabled's doc comment).
		if c.ok && c.catchUpOABKey != "" {
			if err := uc.clearCatchUp(ctx, tx, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// clearCatchUp compare-and-clears the resolved catch-up gap. A malformed
// catch_up_since (should never happen — always written by newCatchUpSyncRequested/
// WatchedOABReenabled as RFC3339) is logged and skipped rather than failing the
// whole sync-close, since retrying cannot fix a producer bug.
func (uc *BackfillUseCase) clearCatchUp(ctx context.Context, tx database.Tx, c sliceClose) error {
	since, err := time.Parse(time.RFC3339, c.catchUpSince)
	if err != nil {
		slog.ErrorContext(ctx, "acquisition: malformed catch_up_since on sync_completed, skipping clear",
			obs.KeyTenantID, c.tenantID,
			"integration_id", c.integrationID,
			"oab_key", c.catchUpOABKey,
			"error", err,
		)
		return nil
	}
	return uc.repo.ClearWatchedOABCatchUp(ctx, tx, c.tenantID, c.integrationID, c.catchUpOABKey, since)
}

// incrementSlice bumps the counter matching the slice's outcome and returns the
// job's tallies read back atomically with the bump.
func (uc *BackfillUseCase) incrementSlice(ctx context.Context, tx database.Tx, c sliceClose) (BackfillCounters, error) {
	if c.ok {
		return uc.repo.IncrementBackfillSlicesOK(ctx, tx, c.tenantID, c.backfillJobID)
	}
	return uc.repo.IncrementBackfillSlicesError(ctx, tx, c.tenantID, c.backfillJobID)
}

// finalizeIfComplete closes the job when this increment was the last slice. The
// Status == RUNNING guard makes finalize fire exactly once: a late or over-count
// delivery reads a non-RUNNING status and no-ops, so backfill_finished is never
// re-emitted. COMPLETED when no slice failed, PARTIAL otherwise.
func (uc *BackfillUseCase) finalizeIfComplete(ctx context.Context, tx database.Tx, c sliceClose, counters BackfillCounters) error {
	done := counters.SlicesOK+counters.SlicesError == counters.TotalSlices
	if !done || counters.Status != BackfillStatusRunning {
		return nil
	}

	status := BackfillStatusCompleted
	if counters.SlicesError > 0 {
		status = BackfillStatusPartial
	}

	if err := uc.repo.FinalizeBackfillJob(ctx, tx, c.tenantID, c.backfillJobID, status); err != nil {
		return err
	}

	// Milestone: the whole onboarding backfill is done. PARTIAL (some slice failed)
	// is expected under a flaky WAF and is not itself an error — the counts say how much.
	slog.InfoContext(ctx, "acquisition: backfill finished",
		obs.KeyTenantID, c.tenantID,
		"backfill_job_id", c.backfillJobID,
		"status", status,
		"slices_ok", counters.SlicesOK,
		"slices_error", counters.SlicesError,
		"total_slices", counters.TotalSlices,
	)
	// Discovery is done. The DATAJUD enrichment (2nd phase) now OWNS the fecho of this
	// import's ENRICHMENT capture row: the batch job (enrichment_batch_requested, emitted per
	// tribunal on discovery) closes the row deterministically when its scan empties — no ETA
	// close is scheduled here (an ETA close would race the job's own close on the same row).
	return uc.outbox.Publish(ctx, tx, newBackfillFinished(c, counters, status))
}

// newBackfillFinished builds the terminal event from the closing slice and the
// final tally, minting a fresh v7 event id and hanging it on the backfill_job.
func newBackfillFinished(c sliceClose, counters BackfillCounters, status string) BackfillFinished {
	return BackfillFinished{
		Base:          events.Base{EventID: newEventID(), Aggregate: c.backfillJobID},
		TenantID:      c.tenantID,
		BackfillJobID: c.backfillJobID,
		IntegrationID: c.integrationID,
		TotalSlices:   counters.TotalSlices,
		SlicesOK:      counters.SlicesOK,
		SlicesError:   counters.SlicesError,
		Status:        status,
	}
}

// truncateToDate drops the clock time, keeping the UTC calendar date at
// midnight — the granularity the window bounds and the date columns work at.
func truncateToDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
