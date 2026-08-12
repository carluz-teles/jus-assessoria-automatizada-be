// Package deadline is the prazos slice: the CREATION path of a legal deadline. Its
// listener consumes acquisition.intimation.observed, derives the prazo
// DETERMINISTICALLY (rules layer → lib/calendar), and persists it born PENDING while
// emitting deadline.opened — all in one idempotent, tenant-scoped transaction.
//
// It is a vertical slice: it talks to acquisition ONLY by event contract (it imports
// the produced type's const, never acquisition's entity/repo), and it never touches
// another slice's tables beyond the read of court_record.class it needs for the rito.
package deadline

import "time"

// Deadline is the legal countdown derived from an intimação — the product's core fact
// (docs/erd-prazos.md §1). It anchors on a court_record (FK) and is 1:1 with the
// intimação (the notification_id UNIQUE column). A rule-derived prazo is BORN PENDING
// (a suggestion) and only becomes OPEN on the human F2 confirmation (a later slice);
// the calendar math (EndDate, HolidaysApplied) is deterministic and auditable, so
// "por que dia 14 e não 12?" is answerable from the row.
//
// entity.go holds only the aggregate and its value types — it imports no repository,
// listener, or lib (the slice's inward dependency rule).
type Deadline struct {
	ID            string
	TenantID      string
	CourtRecordID string
	// IntimationID is persisted in the deadline.notification_id column: that column
	// keeps its pre-rename name but references intimation(id) (migration 0006). The
	// mapper documents the quirk; the domain speaks IntimationID.
	IntimationID    string
	Kind            string
	Days            int
	Counting        Counting
	Doubled         bool
	DoubledReason   string
	HolidaysApplied []time.Time
	StartDate       time.Time
	EndDate         time.Time
	Status          Status
	Source          Source
	RulesVersion    string
}

// Counting is how the days are counted. Cível/CPC counts in dias úteis (art. 219);
// some ritos (trabalhista/CLT) count corrido. The value drives which lib/calendar
// motor computes EndDate.
type Counting string

const (
	CountingBusiness Counting = "BUSINESS"
	CountingCalendar Counting = "CALENDAR"
)

// Status is the prazo lifecycle, a closed set the DB CHECK (0024) also enforces. A
// rule-derived prazo is born PENDING; the confirmation/revocation/expiry transitions
// are later slices — this slice only ever writes PENDING.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusOpen      Status = "OPEN"
	StatusMet       Status = "MET"
	StatusMissed    Status = "MISSED"
	StatusCancelled Status = "CANCELLED"
)

// Source records where the {days, counting} came from. This slice derives from the
// conservative rules layer only, so it always writes RULE; AI/MANUAL are later slices.
type Source string

const (
	SourceRule   Source = "RULE"
	SourceAI     Source = "AI"
	SourceManual Source = "MANUAL"
)

// Kind constants — the legible prazo kinds the v0 rules layer emits (docs/erd-prazos.md
// §4/§8). GENERICO is the safe catch-all the UI later flags "confirme".
const (
	KindContestacao  = "CONTESTACAO"
	KindManifestacao = "MANIFESTACAO"
	KindGenerico     = "GENERICO"
)

// RevokedDeadline is the thin result of revoking a prazo by its intimação: the id of the
// row that flipped to CANCELLED and the record it hung on. The revoke path needs only the
// id (it anchors the deadline.revoked event); CourtRecordID is carried for symmetry with
// the open path and any consumer that keys off the record. A no-op revoke — no prazo, or
// one already CANCELLED — yields no RevokedDeadline (ErrDeadlineNotFound), never a zero value.
type RevokedDeadline struct {
	ID            string
	CourtRecordID string
}

// DeadlineRule is the resolved conservative rule (a deadline_rule row, §8): how many
// days, counted which way, under which kind, and whether the rule already implies the
// dobro. It is a read value object — the resolver returns the most specific active
// match for (intimation_type, court), falling back to the '*' catch-all.
type DeadlineRule struct {
	RulesVersion string
	Kind         string
	Days         int
	Counting     Counting
	Doubled      bool
}
