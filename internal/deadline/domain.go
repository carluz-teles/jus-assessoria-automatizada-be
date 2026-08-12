package deadline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// consumerDeadline is the processed_event consumer name this slice dedups under. Dedup
// is per-consumer (docs §4c.3), so it is slice-specific — marking here never blocks
// another consumer of the same intimation.observed event.
const consumerDeadline = "deadline"

// rulesVersion pins which seeded deadline_rule set this slice resolves against. The
// derived prazo records it (rules_version) so "por que 15 dias?" is answerable and the
// rule set can evolve without touching this code (docs §8).
const rulesVersion = "v0"

// Repository is the persistence port the use case depends on (never the concrete impl,
// docs §2.5). Every method takes the caller's tx so it participates in the use case's
// unit of work and RLS scopes the reads/writes to the event's tenant.
type Repository interface {
	// GetCourtRecordClass reads the rito signal (court_record.class) for the record,
	// scoped to tenantID (barrier 1, the explicit filter — RLS is barrier 2). This is
	// the ONLY cross-table read the slice needs; it reads the table directly (no import
	// of the acquisition package — slices talk by event, decisão P1). A missing record
	// (or one owned by another tenant) is ErrCourtRecordNotFound, never (nil, nil).
	// class may be "".
	GetCourtRecordClass(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) (string, error)
	// ResolveRule returns the most specific active rule for (intimationType, court) in
	// rulesVersion, falling back to the '*' catch-all (the resolution is in SQL —
	// specificity + priority DESC + LIMIT 1). No match at all is ErrRuleNotFound.
	ResolveRule(ctx context.Context, tx database.Tx, rulesVersion, intimationType, court string) (DeadlineRule, error)
	// InsertDeadline persists the derived prazo idempotently (ON CONFLICT on the 1:1
	// notification_id DO NOTHING) and returns it with its DB-assigned id. A conflict
	// (a prazo already exists for the intimação) is ErrDeadlineExists.
	InsertDeadline(ctx context.Context, tx database.Tx, d *Deadline) (*Deadline, error)
	// RevokeDeadlineByIntimation cancels the prazo derived from the intimação (keyed by
	// the 1:1 notification_id), scoped to tenantID (barrier 1). The UPDATE's status <>
	// CANCELLED guard makes it idempotent: when it touches no row — no prazo for the
	// intimação (the cancel raced ahead of the observe, or the intimação was dead on
	// arrival), or one already CANCELLED — it returns ErrDeadlineNotFound, the use case's
	// safe no-op. On a hit it returns the revoked prazo so deadline.revoked commits in the
	// same tx.
	RevokeDeadlineByIntimation(ctx context.Context, tx database.Tx, intimationID, tenantID string) (*RevokedDeadline, error)
}

// deduper is the consumer-side idempotency guard port. It marks (consumer, eventID)
// inside the caller's tx so the mark and the prazo commit together — a crash can never
// leave an event marked-but-not-applied. The adapter wraps events.Dedup bound to tx.
type deduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// publisher is the transactional-outbox port — the producer half. *events.Outbox
// satisfies it structurally; Publish writes the deadline.opened row in the same tx as
// the deadline insert, so entity + event commit atomically (transactional outbox).
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// businessCalendar is the narrow slice of lib/calendar the derivation needs: both the
// dias-úteis (CPC art. 219) and dias-corridos motors, each returning the auditable
// holidays_applied. Defined consumer-side so the unit test injects a fake without a
// HolidaySource; *calendar.Calendar satisfies it.
type businessCalendar interface {
	AddBusinessDays(ctx context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error)
	AddCalendarDays(ctx context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error)
}

// UseCase derives a prazo from an observed intimação. It depends only on the ports
// above and the UnitOfWork — never a concrete implementation (docs §2.5).
type UseCase struct {
	repo   Repository
	cal    businessCalendar
	outbox publisher
	dedup  deduper
	uow    database.UnitOfWork
}

// NewUseCase wires the use case to its repository, calendar, outbox publisher, dedup
// guard and unit of work.
func NewUseCase(repo Repository, cal businessCalendar, outbox publisher, dedup deduper, uow database.UnitOfWork) *UseCase {
	return &UseCase{repo: repo, cal: cal, outbox: outbox, dedup: dedup, uow: uow}
}

// OnIntimationObserved is the creation path: from one acquisition.intimation.observed
// it derives a prazo and opens it, all in a single tenant-scoped transaction so the
// dedup mark, the deadline row and the deadline.opened outbox row commit together.
//
// Steps (docs/erd-prazos.md §6):
//  1. dedup (a replay marks nothing new and returns before any write — no second prazo);
//  2. parse the anchor (deadline_start_at) — a malformed anchor is a terminal invalid;
//  3. read the rito (court_record.class) to inform the counting;
//  4. resolve the conservative rule for (type, court) → {kind, days, counting, doubled};
//  5. decide the counting: the rule suggests it, the rito may override to CALENDAR (P2);
//  6. compute end_date + holidays_applied via the chosen lib/calendar motor;
//  7. persist the prazo born PENDING, source RULE (idempotent on the 1:1 intimação);
//  8. emit deadline.opened in the SAME tx.
//
// tenantID comes from the event payload (a trusted producer inside the same system, no
// Clerk token on the worker) and scopes the transaction's RLS.
func (uc *UseCase) OnIntimationObserved(ctx context.Context, ev IntimationObserved) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerDeadline, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		start, err := parseWireDate(ev.DeadlineStartAt)
		if err != nil {
			return err
		}

		class, err := uc.repo.GetCourtRecordClass(ctx, tx, ev.TenantID, ev.CourtRecordID)
		if err != nil {
			return err
		}

		rule, err := uc.repo.ResolveRule(ctx, tx, rulesVersion, ev.Type, ev.Court)
		if err != nil {
			return err
		}

		counting := decideCounting(rule.Counting, ev.Court, class)

		endDate, holidays, err := uc.compute(ctx, counting, start, rule.Days, ev.UF, ev.Court)
		if err != nil {
			return err
		}

		d := &Deadline{
			TenantID:        ev.TenantID,
			CourtRecordID:   ev.CourtRecordID,
			IntimationID:    ev.IntimationID,
			Kind:            rule.Kind,
			Days:            rule.Days,
			Counting:        counting,
			Doubled:         rule.Doubled,
			HolidaysApplied: holidays,
			StartDate:       start,
			EndDate:         endDate,
			Status:          StatusPending,
			Source:          SourceRule,
			RulesVersion:    rule.RulesVersion,
		}
		if err := d.validate(); err != nil {
			return err
		}

		saved, err := uc.repo.InsertDeadline(ctx, tx, d)
		if errors.Is(err, ErrDeadlineExists) {
			// A prazo already exists for this intimação (the 1:1 notification_id). The
			// dedup mark above still commits, so this event is not reprocessed and no
			// phantom prazo is opened — the idempotent no-op the design demands (§3.2).
			return nil
		}
		if err != nil {
			return err
		}

		return uc.outbox.Publish(ctx, tx, newDeadlineOpened(saved))
	})
}

// OnIntimationCancelled is the REVOCATION path — the counterpart of OnIntimationObserved
// (docs/erd-prazos.md §7/§11). From one acquisition.intimation.cancelled it cancels the
// prazo derived from that intimação and emits deadline.revoked, so a retificação never
// leaves a prazo-fantasma standing. The dedup mark, the status flip and the outbox row
// commit in ONE tenant-scoped tx (transactional outbox).
//
// Steps:
//  1. dedup — a replay marks nothing new and returns before any write;
//  2. revoke the prazo keyed by the 1:1 intimação (idempotent: status <> CANCELLED);
//  3. no prazo revoked (none exists, the cancel raced ahead of the observe, or it was
//     already CANCELLED) → NO-OP (return nil): cancelling the inexistent is safe, and never
//     emitting a phantom revoked is the conservative bias (§3.5);
//  4. otherwise emit deadline.revoked in the SAME tx.
//
// tenantID comes from the trusted event payload (no Clerk token on the worker) and scopes
// the transaction's RLS.
func (uc *UseCase) OnIntimationCancelled(ctx context.Context, ev IntimationCancelled) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerDeadline, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		revoked, err := uc.repo.RevokeDeadlineByIntimation(ctx, tx, ev.IntimationID, ev.TenantID)
		if errors.Is(err, ErrDeadlineNotFound) {
			// No prazo flipped: none exists for this intimação, the cancel arrived before
			// the observe, or the prazo is already CANCELLED. The dedup mark above still
			// commits, so a redelivery stays a no-op and no phantom deadline.revoked is
			// emitted — the safe bias the design demands (§3.5).
			return nil
		}
		if err != nil {
			return err
		}

		return uc.outbox.Publish(ctx, tx, newDeadlineRevoked(revoked.ID, ev.IntimationID, ev.Reason))
	})
}

// decideCounting implements decisão travada P2/P4: the rule SUGGESTS a counting, but
// the RITO can override it to CALENDAR. Cível/CPC counts in dias úteis (art. 219 →
// BUSINESS, the rule's default); the trabalhista/CLT rito counts corrido (→ CALENDAR,
// AddCalendarDays). The override is ONE-WAY and only on a POSITIVE labor signal, so the
// default stays the conservative BUSINESS whenever the rito is unknown — never lengthen
// a prazo without evidence (viés seguro, §3.5). Covered by TestDecideCounting.
func decideCounting(suggested Counting, court, class string) Counting {
	if isLaborRite(court, class) {
		return CountingCalendar
	}
	return suggested
}

// isLaborRite reports the Justiça do Trabalho rito from cheap deterministic signals: a
// labor court sigla (TRT<n> regional, TST superior) or a class naming the rito. It is
// conservative by construction — an unrecognized court/class is NOT labor, so the caller
// keeps the safe BUSINESS default.
func isLaborRite(court, class string) bool {
	c := strings.ToUpper(strings.TrimSpace(court))
	if strings.HasPrefix(c, "TRT") || c == "TST" {
		return true
	}
	return strings.Contains(strings.ToUpper(class), "TRABALH")
}

// compute runs the chosen lib/calendar motor: dias corridos for CALENDAR, dias úteis
// otherwise. Both exclude the start day and return the auditable holidays_applied.
func (uc *UseCase) compute(ctx context.Context, counting Counting, start time.Time, n int, uf, court string) (time.Time, []time.Time, error) {
	if counting == CountingCalendar {
		return uc.cal.AddCalendarDays(ctx, start, n, uf, court)
	}
	return uc.cal.AddBusinessDays(ctx, start, n, uf, court)
}

// parseWireDate parses the event's anchor (2006-01-02, the acquisition wire format).
// A malformed date came from a decoded event and can never become valid on retry, so
// it is a terminal KindInvalid — the domain owns the invariant; the transport (listener)
// decides what to do with it. In practice the producer always formats a valid date.
func parseWireDate(s string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, apperr.NewInvalid(fmt.Sprintf("invalid deadline_start_at %q", s))
	}
	return t, nil
}
