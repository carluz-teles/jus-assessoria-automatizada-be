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

// Source records where the {days, counting} came from. The creation path derives from
// the conservative rules layer (RULE); the F2 confirmation creates its tasks MANUAL.
// AI is a later slice.
type Source string

const (
	SourceRule   Source = "RULE"
	SourceAI     Source = "AI"
	SourceManual Source = "MANUAL"
)

// Task is one actionable work item (docs/erd-prazos.md §4/§10) — the checklist of steps
// toward the legal prazo. 1 legal prazo (Deadline) → N tasks; a task can also be avulsa
// (POST /v1/tasks, no deadline). The assignee lives on the task, not the prazo (the prazo
// is the fact, the task is who does it). Tasks are BORN OPEN (at F2 confirmation or via the
// manual CREATE) and move OPEN→DONE / OPEN→DISMISSED via the task write path (5b); the
// creation source is MANUAL here (RULE/AI are later slices). entity.go holds only the
// aggregate + value types (no repo/lib import).
type Task struct {
	ID             string
	TenantID       string
	CourtRecordID  string
	DeadlineID     string
	IntimationID   string
	Title          string
	Description    string
	Kind           string
	DueDate        *time.Time // optional own date (≤ Deadline.EndDate when present)
	Status         TaskStatus
	Source         Source
	AssigneeUserID string // optional responsável ("meus prazos")
	CreatedBy      string
	CompletedAt    *time.Time // stamped when the task is marked DONE; NULL while OPEN/DISMISSED
}

// TaskStatus is the task lifecycle, a closed set the DB CHECK (0024) also enforces. A
// task is born OPEN; the OPEN→DONE / OPEN→DISMISSED transitions are the task write path
// (5b, POST /v1/tasks/:id/done | .../dismiss).
type TaskStatus string

const (
	TaskStatusOpen      TaskStatus = "OPEN"
	TaskStatusDone      TaskStatus = "DONE"
	TaskStatusDismissed TaskStatus = "DISMISSED"
)

// DeadlineForConfirm is the thin anchor read the F2 confirmation loads BEFORE the
// recompute (GetDeadlineForConfirm), keyed by the 1:1 intimação: the prazo id, the
// record it hangs on (feeds the court lookup + the tasks), and the fixed StartDate the
// calendar math re-counts from. A missing prazo for the intimação is ErrDeadlineNotFound
// (→ 404), never a zero value.
type DeadlineForConfirm struct {
	ID            string
	CourtRecordID string
	StartDate     time.Time
}

// DeadlineForAdjust is the FULL adjustable state the F2 ajuste manual loads BEFORE the
// recompute (GetDeadlineForAdjust, PATCH /v1/prazos/:id), keyed by id: the prazo id, the
// record it hangs on (feeds the court lookup), the fixed StartDate the calendar re-counts
// from, the Status the ajuste is gated on (only PENDING/OPEN is adjustable), and the CURRENT
// {Kind, Days, Counting, Doubled, DoubledReason} the partial patch is applied over — a field
// absent from the body keeps its stored value. A missing prazo is ErrDeadlineNotFound (→ 404),
// never a zero value.
type DeadlineForAdjust struct {
	ID            string
	CourtRecordID string
	StartDate     time.Time
	Status        Status
	Kind          string
	Days          int
	Counting      Counting
	Doubled       bool
	DoubledReason string
}

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

// DeadlineForCheck is the thin re-read of a prazo at a scheduled mark's fire time
// (reminder_check / missed_check): the current Status the handler branches on, the EndDate
// and the context (Kind, Counting, CourtRecordID) a lembrete or MISSED fact may carry. It
// is a read value object — the fire handlers never mutate through it (MISSED goes through
// the guarded MarkMissed UPDATE). A missing id in the tenant is ErrDeadlineNotFound.
type DeadlineForCheck struct {
	ID            string
	Status        Status
	EndDate       time.Time
	CourtRecordID string
	Kind          string
	Counting      Counting
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
