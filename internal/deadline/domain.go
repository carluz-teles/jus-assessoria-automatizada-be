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

// intimationTypeComunicacao is the generic-communication kind (Edital and anything that
// is not an INTIMACAO/CITACAO). It opens no prazo. The lean-ingestion cut already drops
// these at the DJEN parser, so in the current pipeline no intimation.observed carries it;
// this local const backs a belt-and-suspenders no-op in OnIntimationObserved that protects
// this slice against a FUTURE producer that emits one. Kept as a LOCAL literal (not an
// acquisition import) to preserve the slice's rule of reading the value, never importing
// the producer's entity — it mirrors acquisition.IntimationTypeComunicacao by contract,
// guarded by the events round-trip test.
const intimationTypeComunicacao = "COMUNICACAO"

// consumerReconcile is the SEPARATE processed_event consumer name the docket-entry reconcile
// dedups under. Dedup is per-consumer (docs §4c.3), so the reconcile MUST NOT reuse
// consumerDeadline: docket_entry_observed and intimation.observed are different events, but
// keeping distinct consumer names also means a future second deadline consumer of the same
// event never collides with this one on the (consumer, event_id) key.
const consumerReconcile = "deadline.reconcile"

// responseTPUcodes are the CNJ/TPU movement codes that mark a peça de RESPOSTA já nos autos
// (petição, contestação, manifestação, impugnação, recurso). A docket_entry carrying one of
// these — on/after the prazo's start_date — means the prazo foi cumprido, so a prazo histórico
// nascido MISSED (backfill) is reconciled to MET. It is the positive half of the predicate;
// the negative half (a movimento sem tpu_code) falls back to the text regex in the query.
// int32 to match the docket_entry.tpu_code column (deadlinedb.DocketEntry.TpuCode *int32).
var responseTPUCodes = []int32{85, 118, 383, 433, 235}

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
	// GetDeadlineForConfirm loads the F2 confirmation anchor (id, court_record_id,
	// start_date) by the 1:1 intimação, scoped to tenantID (barrier 1). start_date is
	// the fixed anchor the recompute re-counts from. A missing prazo for the intimação
	// is ErrDeadlineNotFound (→ 404 at the edge), never (nil, nil).
	GetDeadlineForConfirm(ctx context.Context, tx database.Tx, intimationID, tenantID string) (*DeadlineForConfirm, error)
	// GetIntimationAnchors loads the three observed dates of the intimação (made_available_at,
	// published_at, deadline_start_at) the confirmation panel can re-anchor a prazo on, scoped
	// to tenantID (barrier 1). All three are NOT NULL date columns. A missing/foreign intimação
	// is ErrDeadlineNotFound (→ 404 at the edge, the prazo's anchor cannot be resolved), never
	// (nil, nil).
	GetIntimationAnchors(ctx context.Context, tx database.Tx, intimationID, tenantID string) (IntimationAnchors, error)
	// GetIntimationAssignee reads ONLY the intimação's vigente assignee_user_id, scoped to
	// tenantID (barrier 1). POST /v1/tasks uses it to snapshot the responsável onto a new
	// task at CREATE time when intimation_id is set and assignee_user_id is not — a one-time
	// inheritance, not a continuous link. nil means the intimação has no assignee (not an
	// error); a missing/foreign intimação is ErrIntimationNotFound, never (nil, nil).
	GetIntimationAssignee(ctx context.Context, tx database.Tx, intimationID, tenantID string) (*string, error)
	// GetPreviewContext loads the confirmation panel's preview context (the three anchor dates +
	// the process's court sigla) for an intimação, scoped to tenantID (barrier 1). It backs the
	// read-only POST /v1/prazos/preview: the use case passes the pool (not a tx) as the Tx, so
	// the read runs off the transactional path. A missing/foreign intimação is ErrDeadlineNotFound.
	GetPreviewContext(ctx context.Context, tx database.Tx, intimationID, tenantID string) (PreviewContext, error)
	// GetCourtRecordCourt reads the court sigla for the record, scoped to tenantID
	// (barrier 1). The confirm recompute derives the UF from it (pkg/tribunal.UF); it is
	// the confirm counterpart of GetCourtRecordClass (same read-the-table, never import
	// acquisition, decisão P1). A missing record is ErrCourtRecordNotFound; court is NOT NULL.
	GetCourtRecordCourt(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) (string, error)
	// ConfirmDeadline flips the prazo PENDING→OPEN with the human-approved fields + the
	// recomputed dates, keyed by the 1:1 intimação and scoped to tenantID (barrier 1). It
	// is idempotent on the deadline: re-confirming re-UPDATEs the one row (the 1:1
	// notification_id), never a second prazo. A no-match is ErrDeadlineNotFound. Returns the
	// confirmed prazo id and the record it hangs on (RETURNING id, court_record_id).
	ConfirmDeadline(ctx context.Context, tx database.Tx, p ConfirmDeadlineParams) (deadlineID, courtRecordID string, err error)
	// ListTaskTitlesByDeadline reads the titles of the tasks that CURRENTLY exist for the
	// confirmed prazo inside the caller's tx, scoped to (deadlineID, tenantID) (barrier 1 +
	// RLS). The F2 confirm reads it to measure the AI-suggestion delta (feedback loop, camada 2)
	// against the tasks that REALLY exist — the Análise section creates them via POST /v1/tasks,
	// so the confirm no longer owns the task lifecycle. Only the title is needed (the delta keys
	// off title alone). A prazo with no tasks yields an empty slice, never an error.
	ListTaskTitlesByDeadline(ctx context.Context, tx database.Tx, deadlineID, tenantID string) ([]string, error)
	// GetDeadlineEndDate reads ONLY a prazo's end_date by its id, scoped to tenantID (barrier 1).
	// The task write path (POST/PATCH /v1/tasks) reads it inside its tx to enforce ERD §4's task
	// invariant: a task's due_date cannot fall after its prazo's end_date. It is a deliberately
	// narrow read — the caller needs the end_date alone, not the whole prazo. A missing id in the
	// tenant is ErrDeadlineNotFound (→ 404), never (zero, nil).
	GetDeadlineEndDate(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (time.Time, error)
	// InsertTask persists one task inside the caller's tx and returns it with its DB-assigned
	// id (echoing the entity, like InsertDeadline). Scoping is via the entity's TenantID + RLS.
	// Reused by BOTH the F2 confirm (the N approved tasks) and the manual CREATE (POST /v1/tasks).
	InsertTask(ctx context.Context, tx database.Tx, t *Task) (*Task, error)
	// GetTaskForUpdate loads a task's editable state (title, description, kind, due_date,
	// assignee, status) by its id, scoped to tenantID (barrier 1). It backs PATCH /v1/tasks/:id:
	// the partial patch is applied over these current values. A missing id in the tenant is
	// ErrTaskNotFound (→ 404), never (nil, nil).
	GetTaskForUpdate(ctx context.Context, tx database.Tx, taskID, tenantID string) (*TaskForUpdate, error)
	// UpdateTask writes the merged editable fields keyed by the task id and scoped to tenantID
	// (barrier 1); status/source/created_by/completed_at and the context FKs are left as-is (the
	// edit never changes the lifecycle nor re-parents). A no-match is ErrTaskNotFound. Returns
	// the full saved task so the handler renders the response without a re-read.
	UpdateTask(ctx context.Context, tx database.Tx, p UpdateTaskParams) (*Task, error)
	// GetTaskForTransition re-reads a task's status by its id, scoped to tenantID (barrier 1).
	// It backs POST /v1/tasks/:id/done | .../dismiss: the use case pre-checks the transition on
	// this status (distinguishing a 404 miss from a 409 invalid transition). A missing id in the
	// tenant is ErrTaskNotFound.
	GetTaskForTransition(ctx context.Context, tx database.Tx, taskID, tenantID string) (TaskStatus, error)
	// MarkTaskStatus flips a task from `from` to `to` keyed by its id and scoped to tenantID
	// (barrier 1), stamping completed_at (a time for DONE, nil for DISMISSED), guarded by
	// `status = from` so the write is safe under a racing flip. The use case pre-checks the
	// transition; this guarded UPDATE is the concurrency floor. A no-match (already transitioned)
	// is ErrTaskNotFound. Returns the flipped task's id.
	MarkTaskStatus(ctx context.Context, tx database.Tx, taskID, tenantID string, from, to TaskStatus, completedAt *time.Time) (string, error)
	// EnsureTaskExistsInTenant guards the comment create: it confirms the parent task exists in
	// the tenant (barrier 1) BEFORE the comment write. A miss is ErrTaskNotFound (→ 404) — a
	// comment on a foreign/unknown task is a task miss (distinct from the checklist's item miss).
	EnsureTaskExistsInTenant(ctx context.Context, tx database.Tx, taskID, tenantID string) error
	// InsertTaskComment appends one comment to a task's thread inside the caller's tx, scoped by
	// the entity's TenantID + RLS. Returns the comment with its DB-assigned id + created_at.
	InsertTaskComment(ctx context.Context, tx database.Tx, c *TaskComment) (*TaskComment, error)
	// InsertTaskActivity appends one audit-log row inside the caller's tx (the SAME tx as the
	// mutation it records), scoped by the entity's TenantID + RLS. It returns only the error —
	// the caller does not need the row back (the Atividade tab re-reads the whole log).
	InsertTaskActivity(ctx context.Context, tx database.Tx, a *TaskActivity) error
	// GetDeadlineForAdjust loads a prazo's FULL adjustable state (id, court_record_id,
	// start_date, status, and the current kind/days/counting/doubled/doubled_reason) by its
	// id, scoped to tenantID (barrier 1). It backs the ajuste manual (PATCH /v1/prazos/:id):
	// the partial patch is applied over these current values, and the recompute re-counts from
	// the fixed start_date. A missing id in the tenant is ErrDeadlineNotFound (→ 404), never
	// (nil, nil).
	GetDeadlineForAdjust(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (*DeadlineForAdjust, error)
	// UpdateDeadlineAdjust writes the patched fields + the recomputed {end_date,
	// holidays_applied} keyed by the prazo id and scoped to tenantID (barrier 1); status/
	// source/start_date are left as-is (the ajuste never changes the lifecycle or the anchor).
	// A no-match is ErrDeadlineNotFound. Returns the prazo id + the record it hangs on.
	UpdateDeadlineAdjust(ctx context.Context, tx database.Tx, p UpdateDeadlineAdjustParams) (deadlineID, courtRecordID string, err error)
	// MarkDeadlineStatus flips a prazo from `from` to `to` keyed by its id and scoped to
	// tenantID (barrier 1), guarded by `status = from` so the write is safe under a racing
	// flip. The use case pre-checks the transition (distinguishing a 404 miss from a 409
	// invalid transition); this guarded UPDATE is the concurrency floor. A no-match (already
	// transitioned) is ErrDeadlineNotFound. Returns the flipped prazo's id.
	MarkDeadlineStatus(ctx context.Context, tx database.Tx, deadlineID, tenantID string, from, to Status) (string, error)
	// GetDeadlineForCheck re-reads a prazo at a scheduled mark's fire time, keyed by its id
	// and scoped to tenantID (barrier 1). It backs OnReminderCheck's re-check: the CURRENT
	// status decides whether a lembrete is still due. A missing id in the tenant (a prazo
	// hard-gone — it should not happen, prazos only change status) is ErrDeadlineNotFound,
	// never (nil, nil).
	GetDeadlineForCheck(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (*DeadlineForCheck, error)
	// MarkMissed auto-flips a prazo to MISSED at the D+1 carência, scoped to tenantID
	// (barrier 1). The UPDATE's status='OPEN' AND end_date < CURRENT_DATE guard makes it a
	// safe, idempotent no-op in every other case — a PENDING (unconfirmed) prazo, an already
	// terminal one, or one not yet overdue touches no row and returns ErrDeadlineNotFound.
	// On a hit it returns the missed prazo's id so deadline.missed commits in the same tx.
	MarkMissed(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (string, error)
	// ListReconcilableDeadlines loads the prazos of a court_record the reconcile may resurrect
	// — status IN ('MISSED','OPEN') — scoped to tenantID (barrier 1). Each carries its id (to
	// flip) and the fixed start_date the response-movement predicate compares against. A record
	// with no reconcilable prazo yields an empty slice (a :many never returns pgx.ErrNoRows),
	// never an error.
	ListReconcilableDeadlines(ctx context.Context, tx database.Tx, courtRecordID, tenantID string) ([]ReconcilableDeadline, error)
	// HasResponseMovement reports whether the court_record holds an andamento de RESPOSTA on/
	// after startDate: a docket_entry whose tpu_code is one of tpuCodes, OR (no tpu_code) whose
	// text matches the peça-de-resposta regex. docket_entry has no tenant_id of its own, so the
	// read JOINs court_record and filters tenantID (barrier 1) — a cross-table read the reconcile
	// needs (decisão P1: read the table, never import acquisition). Returns a bool.
	HasResponseMovement(ctx context.Context, tx database.Tx, courtRecordID, tenantID string, startDate time.Time, tpuCodes []int32) (bool, error)
	// MarkMet reconciles a prazo MISSED/OPEN → MET, keyed by its id and scoped to tenantID
	// (barrier 1). The `status IN ('MISSED','OPEN')` guard makes the flip SAFE and IDEMPOTENT:
	// a redelivery, an already-MET/CANCELLED, or a PENDING prazo touches no row and returns
	// ErrDeadlineNotFound (the reconcile's no-op). On a hit it returns the id so deadline.met
	// commits in the same tx. It mirrors MarkMissed, widened to the two reconcilable statuses.
	MarkMet(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (string, error)
	// MarkNoDeadline flips a prazo PENDING|OPEN → NO_DEADLINE ("mera ciência"), stamping
	// confirmed_by/at, keyed by its id and scoped to tenantID (barrier 1). The
	// `status IN ('PENDING','OPEN')` guard makes the flip safe and idempotent; the use case
	// pre-reads the status to distinguish a 404 miss from a 409 terminal. A no-match is
	// ErrDeadlineNotFound. Returns the flipped prazo's id so deadline.no_deadline commits in the
	// same tx.
	MarkNoDeadline(ctx context.Context, tx database.Tx, deadlineID, tenantID, confirmedBy string, confirmedAt time.Time) (string, error)
	// ReopenNoDeadline reverts a NO_DEADLINE prazo → PENDING, clearing confirmed_by/at, keyed by
	// its id and scoped to tenantID (barrier 1). The `status = 'NO_DEADLINE'` guard makes it safe
	// and idempotent; the use case pre-reads the status to distinguish a 404 miss from a 409
	// not-NO_DEADLINE. A no-match is ErrDeadlineNotFound. Returns the reopened prazo's id so
	// deadline.reopened commits in the same tx.
	ReopenNoDeadline(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (string, error)
	// EnsureTaskInTenant guards the checklist writes: it confirms the parent task exists in the
	// tenant (POST /v1/tasks/:id/items) before any item write, scoped to tenantID (barrier 1). A
	// missing/foreign task id is ErrTaskItemNotFound (→ 404), so an item can never be grafted onto
	// another tenant's — or a non-existent — task.
	EnsureTaskInTenant(ctx context.Context, tx database.Tx, taskID, tenantID string) error
	// NextTaskItemPosition returns the append slot for a new checklist item (one past the current
	// max, 0 when empty), scoped to (taskID, tenantID). It keeps appended items ordered last.
	NextTaskItemPosition(ctx context.Context, tx database.Tx, taskID, tenantID string) (int, error)
	// InsertTaskItem persists one checklist item (born done=false) inside the caller's tx and
	// returns it with its DB-assigned id. Scoping is via the entity's TenantID + RLS.
	InsertTaskItem(ctx context.Context, tx database.Tx, item *TaskItem) (*TaskItem, error)
	// GetTaskItemForUpdate loads a checklist item's editable {title, done} by (itemID, taskID),
	// scoped to tenantID (barrier 1). Binding taskID means an item under a different task is a
	// miss. A missing item is ErrTaskItemNotFound (→ 404), never (nil, nil).
	GetTaskItemForUpdate(ctx context.Context, tx database.Tx, itemID, taskID, tenantID string) (*TaskItemForUpdate, error)
	// UpdateTaskItem writes the merged {title, done, done_at} keyed by (item, task, tenant)
	// (barrier 1); position/created_at are left as-is. A no-match is ErrTaskItemNotFound. Returns
	// the full saved item so the handler renders it without a re-read.
	UpdateTaskItem(ctx context.Context, tx database.Tx, p UpdateTaskItemParams) (*TaskItem, error)
	// DeleteTaskItem removes one checklist item keyed by (item, task, tenant) (barrier 1). A
	// no-match (foreign/unknown item) is ErrTaskItemNotFound (→ 404), never a silent no-op.
	DeleteTaskItem(ctx context.Context, tx database.Tx, itemID, taskID, tenantID string) error
	// RevokeDeadlineByIntimation cancels the prazo derived from the intimação (keyed by
	// the 1:1 notification_id), scoped to tenantID (barrier 1). The UPDATE's status <>
	// CANCELLED guard makes it idempotent: when it touches no row — no prazo for the
	// intimação (the cancel raced ahead of the observe, or the intimação was dead on
	// arrival), or one already CANCELLED — it returns ErrDeadlineNotFound, the use case's
	// safe no-op. On a hit it returns the revoked prazo so deadline.revoked commits in the
	// same tx.
	RevokeDeadlineByIntimation(ctx context.Context, tx database.Tx, intimationID, tenantID string) (*RevokedDeadline, error)
	// GetActionItemCourtRecordID reads ONLY action_item.court_record_id, scoped to tenantID
	// (barrier 1) — fatia 3's automatic task creation needs it because actionitem.created/
	// confirmed's payload does not carry court_record_id (decisão P1: read the table directly,
	// never import internal/actionitem). "" when the column is NULL; a missing/foreign
	// action_item id is ErrActionItemNotFound, never (empty, nil).
	GetActionItemCourtRecordID(ctx context.Context, tx database.Tx, tenantID, actionItemID string) (string, error)

	// GetLatestSuggestion loads the most recent AI suggestion for the prazo (by the 1:1
	// intimação, scoped to tenantID — barrier 1) so the F2 confirm can diff it against the
	// human's confirmed tasks (feedback loop, camada 2). Unlike the other reads a MISS is
	// NOT an error: a prazo the lawyer never asked the IA about (or one confirmed before the
	// suggester existed) simply has no suggestion, so it returns ok=false, nil — the confirm
	// then emits no feedback event. Read inside the confirm's tx so it sees the same snapshot.
	GetLatestSuggestion(ctx context.Context, tx database.Tx, tenantID, intimationID string) (rec SuggestionRecord, ok bool, err error)
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

// UseCase derives a prazo from an observed intimação and drives its scheduled marks. It
// depends only on the ports above and the UnitOfWork — never a concrete implementation
// (docs §2.5). now is the clock the birth-time future-guard compares mark ETAs against;
// it defaults to time.Now and is overridable in tests via WithClock.
type UseCase struct {
	repo   Repository
	cal    businessCalendar
	outbox publisher
	dedup  deduper
	uow    database.UnitOfWork
	now    func() time.Time
	// pool backs the read-only POST /v1/prazos/preview (no UoW, off the transactional path): it
	// is passed as a database.Tx to the repo's pool-safe reads (barrier 1 filter, no RLS SET
	// LOCAL — the same posture as the read models). nil disables Preview (the composition always
	// injects it via WithPreviewPool in the api).
	pool database.Tx
}

// Option configures a UseCase at construction. Kept as functional options so the clock
// seam can be injected in tests without breaking the worker's positional call.
type Option func(*UseCase)

// WithClock overrides the reference clock the birth-time future-guard uses when deciding
// which marks are still in the future. Production leaves the default (time.Now); tests pin
// it to assert deterministically which D-N marks a given end_date schedules.
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// WithPreviewPool injects the connection pool the read-only Preview uses (as a database.Tx) so
// POST /v1/prazos/preview reads the intimação's anchors/court off the transactional path. The
// api composes it; the worker (which never serves preview) leaves it nil.
func WithPreviewPool(pool database.Tx) Option {
	return func(uc *UseCase) { uc.pool = pool }
}

// NewUseCase wires the use case to its repository, calendar, outbox publisher, dedup
// guard and unit of work. opts is variadic so existing callers stay source-compatible;
// the clock defaults to time.Now.
func NewUseCase(repo Repository, cal businessCalendar, outbox publisher, dedup deduper, uow database.UnitOfWork, opts ...Option) *UseCase {
	uc := &UseCase{repo: repo, cal: cal, outbox: outbox, dedup: dedup, uow: uow, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// reminderDaysLeft are the D-N marks a freshly opened prazo schedules a reminder_check for:
// D-3, D-1 and the vencimento (D-0). Each becomes a scheduled deadline.reminder_check whose
// ETA is start-of-day(end_date) minus that many calendar days; marks already in the past at
// birth are skipped (a prazo born <3 dias do fim gets no D-3).
var reminderDaysLeft = []int{3, 1, 0}

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
	// Defensive no-op: a generic COMUNICACAO opens no prazo. The DJEN parser already
	// drops these before they become an intimation.observed, so this never fires in the
	// current pipeline — it is the belt-and-suspenders guard against a future producer.
	// Return before opening any transaction so a stray event is a clean, cheap ack.
	if ev.Type == intimationTypeComunicacao {
		return nil
	}

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

		// Um prazo NASCIDO já vencido — a carência D+1 (startOfDay(end)+1) já passou no
		// momento da criação, típico do backfill de uma intimação histórica — nasce
		// MISSED, não PENDING. Sem isto o carência missed_check (agendado só para ETAs
		// FUTURAS em scheduleChecks) nunca é enfileirado e o prazo ficaria PENDING para
		// sempre — o "prazo órfão". Silencioso por decisão: NÃO emitimos deadline.missed
		// (um prazo que nunca esteve aberto não gera aviso "Prazo vencido"); o
		// deadline.opened abaixo registra o fato, e o status já reflete o vencimento.
		status := StatusPending
		if carencia := startOfDay(endDate).AddDate(0, 0, 1); !carencia.After(uc.now()) {
			status = StatusMissed
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
			Status:          status,
			Source:          SourceRule,
			RulesVersion:    rule.RulesVersion,
			AnchorEvent:     AnchorDeadlineStart, // born on the derived deadline_start_at anchor
			LegalCitation:   rule.LegalCitation,  // snapshot the rule's citation at derivation
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

		if err := uc.outbox.Publish(ctx, tx, newDeadlineOpened(saved)); err != nil {
			return err
		}

		// Schedule the D-N reminder marks and the D+1 carência mark in the SAME tx (docs
		// erd-prazos.md §12; repo directive: ETA work is a scheduled task, not polling). Each
		// is a ScheduledEvent whose process_at the relay turns into an asynq.ProcessAt, so the
		// checks live in asynq, never in a polling loop.
		return uc.scheduleChecks(ctx, tx, saved)
	})
}

// scheduleChecks publishes the prazo's future self-messages: one deadline.reminder_check
// per D-N mark plus one deadline.missed_check, each with its ETA. Only marks whose ETA is
// STRICTLY in the future are scheduled (uc.now is the reference): a prazo born past a mark
// never fires a stale reminder, and a prazo born already overdue schedules no carência (it
// is PENDING, so a missed_check would no-op anyway). Publish inserts each row in the
// caller's tx, so the marks commit atomically with the prazo and its deadline.opened.
func (uc *UseCase) scheduleChecks(ctx context.Context, tx database.Tx, d *Deadline) error {
	now := uc.now()
	for _, daysLeft := range reminderDaysLeft {
		mark := newDeadlineReminderCheck(d.TenantID, d.ID, daysLeft, d.EndDate)
		if at, _ := mark.ProcessAt(); !at.After(now) {
			continue
		}
		if err := uc.outbox.Publish(ctx, tx, mark); err != nil {
			return err
		}
	}
	missed := newDeadlineMissedCheck(d.TenantID, d.ID, d.EndDate)
	if at, _ := missed.ProcessAt(); at.After(now) {
		if err := uc.outbox.Publish(ctx, tx, missed); err != nil {
			return err
		}
	}
	return nil
}

// OnReminderCheck is a mark's FIRE path: a scheduled deadline.reminder_check arrived at its
// ETA, so re-read the prazo and decide whether the lembrete is still due. The re-check is
// the whole point of the two-step design (schedule a check, not the lembrete): a prazo
// cancelled, met or already missed between birth and the mark must NOT send an obsolete
// reminder. Only a PENDING or OPEN prazo still deserves it (decisão travada: due_soon avisa
// OPEN e PENDING, viés seguro). The dedup mark and the emitted deadline.due_soon commit in
// ONE tenant-scoped tx.
//
// Steps:
//  1. dedup — a redelivered task (same stable event id) marks nothing new and returns;
//  2. re-read the prazo (tenant-scoped); a gone prazo is a terminal not-found;
//  3. status ∈ {PENDING, OPEN} → emit deadline.due_soon; anything terminal → NO-OP.
func (uc *UseCase) OnReminderCheck(ctx context.Context, ev DeadlineReminderCheck) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerDeadline, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		d, err := uc.repo.GetDeadlineForCheck(ctx, tx, ev.DeadlineID, ev.TenantID)
		if err != nil {
			return err
		}
		if d.Status != StatusPending && d.Status != StatusOpen {
			// CANCELLED / MET / MISSED — the mark is obsolete; never send a stale lembrete.
			return nil
		}

		return uc.outbox.Publish(ctx, tx, newDeadlineDueSoon(ev.TenantID, d.ID, ev.DaysLeft))
	})
}

// OnMissedCheck is the carência mark's FIRE path: a scheduled deadline.missed_check arrived
// at end_date + 1 day, so auto-mark the prazo MISSED — but ONLY if it is still OPEN and
// overdue (decisão travada: MISSED auto D+1, SÓ em OPEN — perder um PENDING não confirmado
// não faz sentido). The guarded MarkMissed UPDATE collapses every other case (PENDING, a
// terminal status, or not yet overdue) to a safe no-op that touches no row, mirroring the
// revoke path. The dedup mark, the status flip and deadline.missed commit in ONE tx.
//
// Steps:
//  1. dedup — a redelivered task returns before any write;
//  2. MarkMissed (status='OPEN' AND end_date < CURRENT_DATE guard) → no row → NO-OP;
//  3. otherwise emit deadline.missed in the SAME tx.
func (uc *UseCase) OnMissedCheck(ctx context.Context, ev DeadlineMissedCheck) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerDeadline, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		missedID, err := uc.repo.MarkMissed(ctx, tx, ev.DeadlineID, ev.TenantID)
		if errors.Is(err, ErrDeadlineNotFound) {
			// Not OPEN, not yet overdue, or already terminal — nothing to miss. The dedup
			// mark still commits, so a redelivery stays a no-op and no phantom deadline.missed
			// is emitted (the safe bias).
			return nil
		}
		if err != nil {
			return err
		}

		return uc.outbox.Publish(ctx, tx, newDeadlineMissed(ev.TenantID, missedID))
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

// OnDocketEntryObserved is the RECONCILE path (docs: prazo histórico nasce MISSED apesar de
// já haver petição/manifestação nos autos). From one acquisition.docket_entry_observed it
// re-checks EVERY reconcilable (MISSED/OPEN) prazo of the event's court_record: if the record
// holds an andamento de RESPOSTA on/after that prazo's start_date, the prazo foi cumprido, so
// it is flipped to MET and deadline.met is emitted — all in one tenant-scoped tx so the dedup
// mark, the status flips and the outbox rows commit together.
//
// The event announces ONE new andamento but carries none of its fields (no tpu_code/
// occurred_at — decisão travada: não enriquecer o payload, contrato compartilhado com
// notifications); so the reconcile does not key off that single entry. It re-evaluates the
// court_record's whole reconcilable set against ALL its response movements — a court_record
// may hold several MISSED/OPEN prazos, and any newly landed movimento may cumprir more than
// one. Silencioso no backfill em massa não se aplica aqui (isto é o caminho LIVE de um
// andamento novo): each real cumprimento legitimately emits deadline.met.
//
// Steps:
//  1. dedup under consumerReconcile — a replay marks nothing new and returns before any write;
//  2. load the MISSED/OPEN prazos of the court_record (each with its start_date);
//  3. for each: HasResponseMovement(start_date, responseTPUCodes)? no → skip;
//  4. MarkMet (guarded MISSED/OPEN→MET) — a racing flip touches no row (ErrDeadlineNotFound),
//     a safe per-prazo no-op that emits nothing;
//  5. on a hit, emit deadline.met in the SAME tx.
//
// tenantID comes from the trusted event payload (no Clerk token on the worker) and scopes the
// transaction's RLS.
func (uc *UseCase) OnDocketEntryObserved(ctx context.Context, ev DocketEntryObserved) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerReconcile, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		prazos, err := uc.repo.ListReconcilableDeadlines(ctx, tx, ev.CourtRecordID, ev.TenantID)
		if err != nil {
			return err
		}

		for _, p := range prazos {
			hasResponse, err := uc.repo.HasResponseMovement(ctx, tx, ev.CourtRecordID, ev.TenantID, p.StartDate, responseTPUCodes)
			if err != nil {
				return err
			}
			if !hasResponse {
				continue
			}

			metID, err := uc.repo.MarkMet(ctx, tx, p.ID, ev.TenantID)
			if errors.Is(err, ErrDeadlineNotFound) {
				// A racing flip already moved this prazo out of MISSED/OPEN (met/cancelled by
				// another path between the list and the guarded UPDATE): touch no row, emit
				// nothing, keep reconciling the rest. The dedup mark still commits.
				continue
			}
			if err != nil {
				return err
			}

			if err := uc.outbox.Publish(ctx, tx, newDeadlineMet(ev.TenantID, metID)); err != nil {
				return err
			}
		}
		return nil
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

// computeWithExtra is the confirmation-panel recompute: it runs the SAME motor over
// effectiveDays(days, doubled) PLUS the lawyer's manual_extra_days, all in ONE pass so the extra
// days are counted by the motor (respecting BUSINESS/CALENDAR) with NO crude date arithmetic and
// NO double counting against the automatic holidays. It is the single source of truth for the
// panel's date math, reused by Confirm, Adjust and Preview.
func (uc *UseCase) computeWithExtra(ctx context.Context, counting Counting, start time.Time, days int, doubled bool, manualExtraDays int, uf, court string) (time.Time, []time.Time, error) {
	n := effectiveDays(days, doubled) + manualExtraDays
	return uc.compute(ctx, counting, start, n, uf, court)
}

// resolveStart maps the chosen AnchorEvent to the intimação's matching observed date, re-anchoring
// the recompute's start. DEADLINE_START (the default) keeps the stored anchor without an extra
// read; MADE_AVAILABLE / PUBLISHED read the intimação's real dates (GetIntimationAnchors).
func (uc *UseCase) resolveStart(ctx context.Context, tx database.Tx, intimationID, tenantID string, anchor AnchorEvent, stored time.Time) (time.Time, error) {
	if anchor == "" || anchor == AnchorDeadlineStart {
		return stored, nil
	}
	anchors, err := uc.repo.GetIntimationAnchors(ctx, tx, intimationID, tenantID)
	if err != nil {
		return time.Time{}, err
	}
	return anchors.startFor(anchor), nil
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
