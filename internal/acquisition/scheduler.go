package acquisition

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// scheduler.go is the re-poll driver: on each tick the scheduler binary calls
// RunDuePoll, which scans court_record for records whose next_sync_at is due and
// re-emits court_record_observed for each so the enrichment consumer refreshes the
// DATAJUD movimentos. The scan is system-scoped (cross-tenant) — the scheduler is
// the one place that legitimately spans tenants — via the court_record RLS system
// escape hatch (migration 0016). DJEN discovery is by-OAB (a separate onboarding
// concern), so per-record re-poll only refreshes the DATAJUD side.

// defaultResyncBatchSize caps how many due records one tick claims, so a large
// backlog drains over several ticks instead of one giant transaction.
const defaultResyncBatchSize = 200

// DueRecord is one court record the scheduler found due for a re-poll: enough
// identity to re-emit a court_record_observed the enrichment consumer can act on.
type DueRecord struct {
	ID        string
	TenantID  string
	CaseID    string
	CNJNumber string
	Degree    string
	Court     string
}

// schedulerRepo is the narrow persistence port the scheduler drives: the
// system-scoped due scan and the per-record claim.
type schedulerRepo interface {
	DueCourtRecordsForResync(ctx context.Context, tx database.Tx, limit int) ([]DueRecord, error)
	ClaimCourtRecordResync(ctx context.Context, tx database.Tx, recordID string, nextSyncAt time.Time) error
}

// SchedulerUseCase enqueues re-poll work. It depends on the narrow schedulerRepo,
// the outbox, and the unit of work (its DoSystem cross-tenant mode); never on a
// concrete data source. now is a seam (defaults to time.Now); batchSize and
// interval are tunable at construction.
type SchedulerUseCase struct {
	repo      schedulerRepo
	outbox    publisher
	uow       database.UnitOfWork
	now       func() time.Time
	batchSize int
	interval  time.Duration
}

// SchedulerOption tunes a SchedulerUseCase at construction.
type SchedulerOption func(*SchedulerUseCase)

// WithResyncInterval overrides how far ahead a claimed record's next_sync_at is
// pushed (the effective re-poll cadence per record). A non-positive value is ignored.
func WithResyncInterval(d time.Duration) SchedulerOption {
	return func(uc *SchedulerUseCase) {
		if d > 0 {
			uc.interval = d
		}
	}
}

// WithResyncBatchSize overrides how many due records one tick claims. A non-positive
// value is ignored.
func WithResyncBatchSize(n int) SchedulerOption {
	return func(uc *SchedulerUseCase) {
		if n > 0 {
			uc.batchSize = n
		}
	}
}

// NewSchedulerUseCase wires the scheduler use case with production defaults.
func NewSchedulerUseCase(repo schedulerRepo, outbox publisher, uow database.UnitOfWork, opts ...SchedulerOption) *SchedulerUseCase {
	uc := &SchedulerUseCase{
		repo:      repo,
		outbox:    outbox,
		uow:       uow,
		now:       time.Now,
		batchSize: defaultResyncBatchSize,
		interval:  defaultSyncInterval,
	}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// RunDuePoll processes one batch of due records in a single system transaction:
// scan the due set, and for each publish a court_record_observed (a fresh event id,
// so the enrichment consumer does not dedup it against the original discovery) and
// claim it (push next_sync_at forward). The outbox write and the claim commit
// together, so a record is enqueued exactly when it is claimed. It returns how many
// records were enqueued this tick.
func (uc *SchedulerUseCase) RunDuePoll(ctx context.Context) (int, error) {
	enqueued := 0
	err := uc.uow.DoSystem(ctx, func(tx database.Tx) error {
		due, err := uc.repo.DueCourtRecordsForResync(ctx, tx, uc.batchSize)
		if err != nil {
			return err
		}
		next := uc.now().Add(uc.interval)
		for _, r := range due {
			if err := uc.outbox.Publish(ctx, tx, newResyncObserved(r)); err != nil {
				return err
			}
			if err := uc.repo.ClaimCourtRecordResync(ctx, tx, r.ID, next); err != nil {
				return err
			}
			enqueued++
		}
		return nil
	})
	return enqueued, err
}

// newResyncObserved builds the re-poll trigger: a court_record_observed carrying
// the due record's identity, with a fresh event id so it is processed anew. It has
// no sync_run id — a re-poll is not a discovery run.
func newResyncObserved(r DueRecord) CourtRecordObserved {
	return CourtRecordObserved{
		Base:          events.Base{EventID: newEventID(), Aggregate: r.ID},
		TenantID:      r.TenantID,
		CourtRecordID: r.ID,
		CaseID:        r.CaseID,
		CNJNumber:     r.CNJNumber,
		Degree:        r.Degree,
		Court:         r.Court,
	}
}
