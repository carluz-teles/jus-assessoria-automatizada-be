package deadline

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/lib/events"
)

// TypeIntimationObserved is the dotted id this slice CONSUMES. Only the const crosses
// the boundary from acquisition (the producer); the payload SHAPE is redefined LOCALLY
// as IntimationObserved below, so this slice never imports acquisition's event struct.
// A contract round-trip test (deadline_events_test.go) marshals the producer's struct
// and unmarshals it here, guarding against silent field drift.
const TypeIntimationObserved = acquisition.TypeIntimationObserved

// IntimationObserved is the LOCAL decode shape of acquisition.intimation.observed: the
// exact subset of fields this slice reads to derive a prazo. UF is DENORMALIZED by the
// producer (derived from Court) so this slice never resolves it. Type carries the DJEN
// kind (CITACAO/INTIMACAO/COMUNICACAO) — the plain field, not the events.Event method:
// this struct is only ever DECODED here (events.Decode needs no interface), so the JSON
// tag "type" round-trips while the Go name stays clear. DeadlineStartAt is the wire date
// (2006-01-02), the anchor of the calendar math. Base yields the event id for dedup.
type IntimationObserved struct {
	events.Base
	TenantID        string `json:"tenant_id"`
	IntimationID    string `json:"intimation_id"`
	CourtRecordID   string `json:"court_record_id"`
	CaseID          string `json:"case_id"`
	Type            string `json:"type"`
	Court           string `json:"court"`
	UF              string `json:"uf"`
	DeadlineStartAt string `json:"deadline_start_at"`
}

// TypeIntimationCancelled is the SECOND dotted id this slice CONSUMES: the retraction
// counterpart of intimation.observed (the DJEN brought a data_cancelamento). As with the
// observed type, only the const crosses the boundary from acquisition; the payload SHAPE
// is redefined LOCALLY as IntimationCancelled below, so this slice never imports the
// producer's struct. A round-trip test (deadline_events_test.go) guards the shape.
const TypeIntimationCancelled = acquisition.TypeIntimationCancelled

// IntimationCancelled is the LOCAL decode shape of acquisition.intimation.cancelled: the
// exact subset the revocation reads. IntimationID keys the 1:1 deadline (the
// notification_id column); Reason is the DJEN motivo (empty when it did not disclose one)
// and rides through onto deadline.revoked for the audit. This struct is only ever DECODED
// here (events.Decode needs no interface), so it carries no Type()/AggregateType(). Base
// yields the event id for dedup.
type IntimationCancelled struct {
	events.Base
	TenantID     string `json:"tenant_id"`
	IntimationID string `json:"intimation_id"`
	Reason       string `json:"reason"`
}

// TypeDocketEntryObserved is the THIRD dotted id this slice CONSUMES: one newly landed
// andamento (docket_entry). It drives the reconcile — a prazo histórico nasce MISSED apesar
// de já haver petição/manifestação nos autos, so when a movimento de resposta lands we
// resurrect the MISSED/OPEN prazo to MET. As with the intimation types, ONLY the const
// crosses the boundary from acquisition; the payload SHAPE is redefined LOCALLY as
// DocketEntryObserved below (this slice never imports the producer's struct). A round-trip
// test (deadline_events_test.go) guards the shape. NOTE: this event has TWO consumers on two
// asynq servers (notifications' aviso + this reconcile), so the relay FANS IT OUT to both the
// "ingestao" and "deadline" queues (lib/events queuesFor) — a change made outside this slice.
const TypeDocketEntryObserved = acquisition.TypeDocketEntryObserved

// DocketEntryObserved is the LOCAL decode shape of acquisition.docket_entry_observed: the
// exact subset the reconcile reads. It deliberately carries ONLY TenantID (RLS scope + dedup)
// and CourtRecordID (the record whose MISSED/OPEN prazos we re-check) — NOT the producer's
// DocketEntryID/Hash/SyncRunID nor the movimento's tpu_code/occurred_at. The reconcile does
// NOT key off the single entry the event announces; it re-checks EVERY reconcilable prazo of
// the court_record against ALL its response movements (read directly from docket_entry),
// because the event does not carry the movimento's fields (decisão travada: não enriquecer o
// payload — é contrato compartilhado com notifications). Base yields the event id for dedup.
type DocketEntryObserved struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	CourtRecordID string `json:"court_record_id"`
}

// TypeActionItemCreated/TypeActionItemConfirmed are the two dotted ids this slice CONSUMES
// from actionitem (docs/erd-costura-providencia-tarefa-peca.md §2/§3/§6, fatia 3, "Listener-
// driven"): both fire once a providência's tipo turns confiável — a declarado/manual item is
// born ready (created); an ia-inferred one only turns confiável when the lawyer confirms it
// (confirmed). Either way the downstream effect is the SAME: create the task right away.
// Only the consts cross the boundary from actionitem (the producer); the payload SHAPE is
// redefined LOCALLY as ActionItemFact below, so this slice never imports actionitem's event
// struct or its ActionItem entity/repo. A contract round-trip test (actionitem_task_test.go)
// marshals the producer's struct and unmarshals it here, guarding against silent field drift.
const (
	TypeActionItemCreated   = actionitem.TypeActionItemCreated
	TypeActionItemConfirmed = actionitem.TypeActionItemConfirmed
)

// ActionItemFact is the LOCAL decode shape shared by actionitem.created AND
// actionitem.confirmed — both events carry the IDENTICAL payload shape on the producer side
// (actionitem's actionItemEventPayload), so one local struct decodes either. PieceProfileKey/
// DeadlineID ride as pointers so an absent value round-trips as JSON null, not "" (mirrors
// acquisition's ProvidenciaCandidate). The payload does NOT carry court_record_id (never part
// of that frozen contract) — GetActionItemCourtRecordID reads it directly off the action_item
// table instead (decisão P1, mirroring GetCourtRecordClass). Base yields the event id for
// dedup.
type ActionItemFact struct {
	events.Base
	ActionItemID    string  `json:"action_item_id"`
	TenantID        string  `json:"tenant_id"`
	IntimationID    string  `json:"intimation_id"`
	Tipo            string  `json:"tipo"`
	GeraPeca        bool    `json:"gera_peca"`
	PieceProfileKey *string `json:"piece_profile_key"`
	DeadlineID      *string `json:"deadline_id"`
}

// TypeDeadlineOpened is the dotted id this slice PRODUCES when a prazo is derived. Its
// "deadline" prefix routes it to the ingestao/default work at the relay; downstream
// slices (read models, reminders) consume it.
const TypeDeadlineOpened = "deadline.opened"

const aggregateTypeDeadline = "deadline"

// DeadlineOpened announces a freshly derived prazo (born PENDING). It carries what a
// consumer needs without reading back the deadline row: the prazo and its origin ids,
// the legible kind, the computed EndDate (wire date 2006-01-02) and the counting. The
// aggregate is the deadline, so its stream orders by the deadline id; Base carries the
// event id (consumer dedup) and that aggregate id.
type DeadlineOpened struct {
	events.Base
	DeadlineID    string `json:"deadline_id"`
	CourtRecordID string `json:"court_record_id"`
	IntimationID  string `json:"intimation_id"`
	Kind          string `json:"kind"`
	EndDate       string `json:"end_date"`
	Counting      string `json:"counting"`
}

var _ events.Event = DeadlineOpened{}

func (DeadlineOpened) Type() string          { return TypeDeadlineOpened }
func (DeadlineOpened) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineOpened builds the produced event from the persisted prazo. aggregate_id is
// the deadline id (a uuid, satisfying the outbox's uuid NOT NULL); the event id is a
// fresh uuid v7 (the consumer dedup key), mirroring how acquisition mints its ids.
func newDeadlineOpened(d *Deadline) DeadlineOpened {
	return DeadlineOpened{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: d.ID},
		DeadlineID:    d.ID,
		CourtRecordID: d.CourtRecordID,
		IntimationID:  d.IntimationID,
		Kind:          d.Kind,
		EndDate:       d.EndDate.Format(time.DateOnly),
		Counting:      string(d.Counting),
	}
}

// TypeDeadlineRevoked is the dotted id this slice PRODUCES when a prazo is revoked because
// its source intimação was retracted (ERD §7: deadline.revoked {deadline_id}; §11: uma
// intimação retificada vira prazo-fantasma). Same "deadline" prefix/routing as
// deadline.opened; downstream read models drop the prazo from the tela on it.
const TypeDeadlineRevoked = "deadline.revoked"

// DeadlineRevoked announces a derived prazo cancelled (status CANCELLED) because the
// intimação it hung on was retracted. Beyond the ERD's minimal {deadline_id} it carries
// the triggering IntimationID and the DJEN Reason (may be "") so a consumer need not read
// the row back to know why it vanished. The aggregate is the deadline, so its stream
// orders by the deadline id; Base carries the event id (consumer dedup) and that aggregate.
type DeadlineRevoked struct {
	events.Base
	DeadlineID   string `json:"deadline_id"`
	IntimationID string `json:"intimation_id"`
	Reason       string `json:"reason"`
}

var _ events.Event = DeadlineRevoked{}

func (DeadlineRevoked) Type() string          { return TypeDeadlineRevoked }
func (DeadlineRevoked) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineRevoked builds the produced event from the revoked prazo's id and the
// triggering cancellation. aggregate_id is the deadline id (a uuid, satisfying the
// outbox's uuid NOT NULL); the event id is a fresh uuid v7 (the consumer dedup key),
// mirroring newDeadlineOpened.
func newDeadlineRevoked(deadlineID, intimationID, reason string) DeadlineRevoked {
	return DeadlineRevoked{
		Base:         events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		DeadlineID:   deadlineID,
		IntimationID: intimationID,
		Reason:       reason,
	}
}

// TypeDeadlineReminderCheck is the dotted id of the INTERNAL, SCHEDULED task-cheque this
// slice both PRODUCES (at prazo birth, one per D-N mark) and CONSUMES (at the ETA). It
// carries no lembrete itself: when it fires the handler re-reads the prazo's CURRENT
// status and only THEN emits the real deadline.due_soon — so a prazo cancelled or met
// between birth and the mark never sends a stale reminder (re-check no disparo).
const TypeDeadlineReminderCheck = "deadline.reminder_check"

// DeadlineReminderCheck is the D-N mark's scheduled self-message. TenantID is the scope
// channel every tenant-scoped consumer needs — the outbox carries no tenant, so the
// payload is the only way the fire handler can open its RLS-scoped tx and filter the read
// (barrier 1), mirroring how intimation.observed carries it. DaysLeft (3/1/0) rides
// through onto the emitted due_soon. processAt is the computed ETA, kept UNEXPORTED so it
// never serializes: it feeds ProcessAt() at publish time (the relay writes it to the
// outbox row's process_at) and is irrelevant to the consumer, which re-loads by id.
type DeadlineReminderCheck struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
	DaysLeft   int    `json:"days_left"`
	processAt  time.Time
}

var (
	_ events.Event          = DeadlineReminderCheck{}
	_ events.ScheduledEvent = DeadlineReminderCheck{}
)

func (DeadlineReminderCheck) Type() string          { return TypeDeadlineReminderCheck }
func (DeadlineReminderCheck) AggregateType() string { return aggregateTypeDeadline }

// ProcessAt opts the check INTO future delivery (docs erd-backend §4c; repo directive: ETA
// work is a scheduled task, not polling). The ETA is start-of-day(end_date) minus DaysLeft
// calendar days, so the D-3/D-1/D-0 marks land at midnight of their day.
func (e DeadlineReminderCheck) ProcessAt() (time.Time, bool) { return e.processAt, true }

// newDeadlineReminderCheck builds one D-N mark for a freshly opened prazo. aggregate_id is
// the deadline id (a uuid, satisfying the outbox's uuid NOT NULL). The event id is a
// STABLE per-mark key ("deadline-reminder:{id}:{days_left}"): the relay uses it as the
// asynq TaskID so re-deriving the same prazo never schedules the mark twice (enqueue
// dedup), and the fire handler dedups the SAME key against processed_event (consumer
// dedup) — the one string threads both idempotency floors.
func newDeadlineReminderCheck(tenantID, deadlineID string, daysLeft int, endDate time.Time) DeadlineReminderCheck {
	return DeadlineReminderCheck{
		Base:       events.Base{EventID: fmt.Sprintf("deadline-reminder:%s:%d", deadlineID, daysLeft), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
		DaysLeft:   daysLeft,
		processAt:  startOfDay(endDate).AddDate(0, 0, -daysLeft),
	}
}

// TypeDeadlineMissedCheck is the dotted id of the INTERNAL, SCHEDULED carência task: at
// end_date + 1 day the handler auto-marks the prazo MISSED — but only if it is still OPEN
// (a PENDING, unconfirmed prazo is never "lost"), and the revogável D+1 grace is exactly
// the ETA gap that lets a human confirm/meet it first.
const TypeDeadlineMissedCheck = "deadline.missed_check"

// DeadlineMissedCheck is the carência mark's scheduled self-message. TenantID scopes the
// fire handler's tx/read (barrier 1) as in DeadlineReminderCheck; processAt (end_date + 1
// day) is UNEXPORTED so it never serializes.
type DeadlineMissedCheck struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
	processAt  time.Time
}

var (
	_ events.Event          = DeadlineMissedCheck{}
	_ events.ScheduledEvent = DeadlineMissedCheck{}
)

func (DeadlineMissedCheck) Type() string          { return TypeDeadlineMissedCheck }
func (DeadlineMissedCheck) AggregateType() string { return aggregateTypeDeadline }

// ProcessAt opts the carência check INTO future delivery at start-of-day(end_date) + 1
// day — the revogável D+1 grace before a still-OPEN prazo is auto-missed.
func (e DeadlineMissedCheck) ProcessAt() (time.Time, bool) { return e.processAt, true }

// newDeadlineMissedCheck builds the single carência mark for a freshly opened prazo. Like
// the reminder mark, the event id is a STABLE key ("deadline-missed:{id}") doubling as the
// asynq TaskID (schedule-once) and the consumer dedup key (process-once).
func newDeadlineMissedCheck(tenantID, deadlineID string, endDate time.Time) DeadlineMissedCheck {
	return DeadlineMissedCheck{
		Base:       events.Base{EventID: fmt.Sprintf("deadline-missed:%s", deadlineID), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
		processAt:  startOfDay(endDate).AddDate(0, 0, 1),
	}
}

// startOfDay collapses a civil date to midnight in its own location — the anchor of the
// mark math. end_date is already a date-only value (midnight UTC from lib/calendar), so
// this normalizes any stray time-of-day before the day arithmetic.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// TypeDeadlineDueSoon is the dotted id of the IMMEDIATE lembrete this slice PRODUCES when a
// reminder_check fires on a still-active prazo. It is the REAL reminder — its consumer is
// notifications (fatia 4c); until that lands it is an orphan, which is safe (nothing fires
// it in prod without a real prazo).
const TypeDeadlineDueSoon = "deadline.due_soon"

// DeadlineDueSoon announces a prazo approaching its vencimento (DaysLeft ∈ {3,1,0}).
// TenantID lets the future notifications consumer scope its send; DeadlineID + DaysLeft are
// what the lembrete needs. Immediate (does NOT implement ScheduledEvent → process_at NULL).
type DeadlineDueSoon struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
	DaysLeft   int    `json:"days_left"`
}

var _ events.Event = DeadlineDueSoon{}

func (DeadlineDueSoon) Type() string          { return TypeDeadlineDueSoon }
func (DeadlineDueSoon) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineDueSoon builds the immediate lembrete from the (re-checked) prazo id and the
// firing mark's DaysLeft. Fresh uuid v7 event id (the consumer dedup key), mirroring
// newDeadlineOpened; aggregate_id is the deadline id.
func newDeadlineDueSoon(tenantID, deadlineID string, daysLeft int) DeadlineDueSoon {
	return DeadlineDueSoon{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
		DaysLeft:   daysLeft,
	}
}

// TypeDeadlineMissed is the dotted id of the IMMEDIATE fact this slice PRODUCES when a
// missed_check auto-flips a still-OPEN, overdue prazo to MISSED. Consumer is notifications
// (4c, future); orphan-for-now is safe.
const TypeDeadlineMissed = "deadline.missed"

// DeadlineMissed announces a prazo auto-marked MISSED at the D+1 carência. TenantID scopes
// the future consumer; DeadlineID is the prazo. Immediate (no process_at).
type DeadlineMissed struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
}

var _ events.Event = DeadlineMissed{}

func (DeadlineMissed) Type() string          { return TypeDeadlineMissed }
func (DeadlineMissed) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineMissed builds the immediate MISSED fact. Fresh uuid v7 event id; aggregate_id
// is the deadline id.
func newDeadlineMissed(tenantID, deadlineID string) DeadlineMissed {
	return DeadlineMissed{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
	}
}

// TypeDeadlineMet is the dotted id this slice PRODUCES when a human marks a prazo cumprido
// (docs/erd-prazos.md §9: POST /v1/prazos/:id/met — transição de status OPEN→MET). Same
// "deadline" prefix/routing as deadline.missed; its consumer (read models, notifications)
// updates the prazo as done. It is the positive counterpart of deadline.missed.
const TypeDeadlineMet = "deadline.met"

// DeadlineMet announces a prazo manually marked MET (OPEN→MET). It mirrors DeadlineMissed:
// TenantID scopes the future consumer's read/send, DeadlineID is the prazo. Immediate (no
// process_at). The aggregate is the deadline, so its stream orders by the deadline id.
type DeadlineMet struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
}

var _ events.Event = DeadlineMet{}

func (DeadlineMet) Type() string          { return TypeDeadlineMet }
func (DeadlineMet) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineMet builds the immediate MET fact. Fresh uuid v7 event id (the consumer dedup
// key); aggregate_id is the deadline id (a uuid, satisfying the outbox's uuid NOT NULL),
// mirroring newDeadlineMissed.
func newDeadlineMet(tenantID, deadlineID string) DeadlineMet {
	return DeadlineMet{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
	}
}

// TypeDeadlineNoDeadline is the dotted id this slice PRODUCES when a human declares "mera
// ciência" on a prazo (§3: PENDING|OPEN → NO_DEADLINE via "Remover prazo" / "Não há prazo").
// Same "deadline" prefix/routing as deadline.met; its consumer (read models, notifications)
// drops the prazo from the agenda de prazos a cumprir.
const TypeDeadlineNoDeadline = "deadline.no_deadline"

// DeadlineNoDeadline announces a prazo declared mera ciência (no prazo to cumprir). It mirrors
// DeadlineMet: TenantID scopes the future consumer, DeadlineID is the prazo. Immediate (no
// process_at). The aggregate is the deadline, so its stream orders by the deadline id.
type DeadlineNoDeadline struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
}

var _ events.Event = DeadlineNoDeadline{}

func (DeadlineNoDeadline) Type() string          { return TypeDeadlineNoDeadline }
func (DeadlineNoDeadline) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineNoDeadline builds the immediate NO_DEADLINE fact. Fresh uuid v7 event id (the
// consumer dedup key); aggregate_id is the deadline id (a uuid, satisfying the outbox's uuid NOT
// NULL), mirroring newDeadlineMet.
func newDeadlineNoDeadline(tenantID, deadlineID string) DeadlineNoDeadline {
	return DeadlineNoDeadline{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
	}
}

// TypeDeadlineReopened is the dotted id this slice PRODUCES when a human reverts a mera-ciência
// declaration (§3: NO_DEADLINE → PENDING via "reabrir"). Same "deadline" prefix/routing as
// deadline.opened; its consumer re-admits the prazo to the agenda as a PENDING suggestion.
const TypeDeadlineReopened = "deadline.reopened"

// DeadlineReopened announces a prazo reopened from NO_DEADLINE back to PENDING. It mirrors
// DeadlineNoDeadline: TenantID scopes the future consumer, DeadlineID is the prazo. Immediate.
type DeadlineReopened struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
}

var _ events.Event = DeadlineReopened{}

func (DeadlineReopened) Type() string          { return TypeDeadlineReopened }
func (DeadlineReopened) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineReopened builds the immediate REOPENED fact, mirroring newDeadlineNoDeadline.
func newDeadlineReopened(tenantID, deadlineID string) DeadlineReopened {
	return DeadlineReopened{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
	}
}

// TypeDeadlineUpdated is the dotted id this slice PRODUCES when the F2 human confirmation
// flips a prazo PENDING→OPEN with the recomputed dates (docs/erd-prazos.md §7: deadline.updated
// {deadline_id, ...} — ajuste humano no F2). Same "deadline" prefix/routing as deadline.opened;
// downstream read models refresh the prazo (now OPEN, confirmed) on it.
const TypeDeadlineUpdated = "deadline.updated"

// DeadlineUpdated announces a confirmed prazo: the recomputed Kind/EndDate/Counting and the
// new Status (OPEN). It carries what a consumer needs without reading the row back. The
// aggregate is the deadline, so its stream orders by the deadline id; Base carries the event
// id (consumer dedup) and that aggregate id.
type DeadlineUpdated struct {
	events.Base
	DeadlineID string `json:"deadline_id"`
	Kind       string `json:"kind"`
	EndDate    string `json:"end_date"`
	Counting   string `json:"counting"`
	Status     string `json:"status"`
}

var _ events.Event = DeadlineUpdated{}

func (DeadlineUpdated) Type() string          { return TypeDeadlineUpdated }
func (DeadlineUpdated) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineUpdated builds the produced event from the confirmed prazo (F2 confirm, 5a).
func newDeadlineUpdated(d ConfirmedDeadline) DeadlineUpdated {
	return deadlineUpdatedEvent(d.ID, d.Kind, d.EndDate, d.Counting, d.Status)
}

// newDeadlineUpdatedFromAdjust builds the same produced event from an ajuste manual (5c,
// PATCH /v1/prazos/:id). Both the confirm and the ajuste recompute the prazo and announce it
// with deadline.updated, so both funnel through the one builder — the event contract has a
// single source (no parallel construction to drift).
func newDeadlineUpdatedFromAdjust(d AdjustedDeadline) DeadlineUpdated {
	return deadlineUpdatedEvent(d.ID, d.Kind, d.EndDate, d.Counting, d.Status)
}

// deadlineUpdatedEvent is the shared builder for deadline.updated. aggregate_id is the
// deadline id (a uuid, satisfying the outbox's uuid NOT NULL); the event id is a fresh uuid v7
// (the consumer dedup key), mirroring newDeadlineOpened.
func deadlineUpdatedEvent(id, kind string, endDate time.Time, counting Counting, status Status) DeadlineUpdated {
	return DeadlineUpdated{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: id},
		DeadlineID: id,
		Kind:       kind,
		EndDate:    endDate.Format(time.DateOnly),
		Counting:   string(counting),
		Status:     string(status),
	}
}

// TypeTaskCreated is the dotted id this slice PRODUCES per task created via POST /v1/tasks
// (docs/erd-prazos.md §7: task.created {task_id, deadline_id?, court_record_id, due_date,
// assignee_user_id?}). Tasks are created there (the "Análise" section), independently of the
// F2 confirm. Its "task" prefix routes it to the default work at the relay; its consumer (task
// read models / "meus prazos") is a later slice, so it is an orphan-for-now, which is safe.
const TypeTaskCreated = "task.created"

// aggregateTypeTask places task.created on the TASK aggregate (its own stream), unlike the
// deadline.* facts which order by the deadline id.
const aggregateTypeTask = "task"

// TaskCreated announces one freshly created action item. TenantID lets internal/actionitem's
// task.created listener (fatia 3) scope its own RLS-bound tx without a re-read; DueDate is the
// wire date (2006-01-02), omitted when the task has none; AssigneeUserID is omitted when
// unassigned. ActionItemID is omitted for a manual/avulsa task (POST /v1/tasks) and set ONLY
// when the task was born automatically from a confiável providência (docs/erd-costura-
// providencia-tarefa-peca.md §2/§6, fatia 3) — that is the field actionitem's listener filters
// on to write its own reverse pointer. The aggregate is the task, so its stream orders by the
// task id.
type TaskCreated struct {
	events.Base
	TaskID         string `json:"task_id"`
	TenantID       string `json:"tenant_id"`
	DeadlineID     string `json:"deadline_id"`
	CourtRecordID  string `json:"court_record_id"`
	DueDate        string `json:"due_date,omitempty"`
	AssigneeUserID string `json:"assignee_user_id,omitempty"`
	ActionItemID   string `json:"action_item_id,omitempty"`
}

var _ events.Event = TaskCreated{}

func (TaskCreated) Type() string          { return TypeTaskCreated }
func (TaskCreated) AggregateType() string { return aggregateTypeTask }

// newTaskCreatedFromTask builds the produced event from a persisted *Task — REUSED by both the
// manual CREATE path (POST /v1/tasks, ActionItemID empty) and the automatic path a confirmed
// providência drives (fatia 3, ActionItemID set): one construction site for task.created,
// never two parallel builders. aggregate_id is the task id (a uuid, satisfying the outbox's
// uuid NOT NULL); the event id is a fresh uuid v7 (the consumer dedup key). DueDate is
// formatted only when set, so an undated task emits no date.
func newTaskCreatedFromTask(t *Task) TaskCreated {
	ev := TaskCreated{
		Base:           events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: t.ID},
		TaskID:         t.ID,
		TenantID:       t.TenantID,
		DeadlineID:     t.DeadlineID,
		CourtRecordID:  t.CourtRecordID,
		AssigneeUserID: t.AssigneeUserID,
		ActionItemID:   t.ActionItemID,
	}
	if t.DueDate != nil {
		ev.DueDate = t.DueDate.Format(time.DateOnly)
	}
	return ev
}

// TypeTaskUpdated is the dotted id this slice PRODUCES when a task's editable fields are
// patched (docs/erd-prazos.md §9: PATCH /v1/tasks/:id → ajustar tarefa). Its "task" prefix
// routes it to the default work at the relay; its consumer (task read models / "meus prazos")
// is a later slice, so it is an orphan-for-now, which is safe.
const TypeTaskUpdated = "task.updated"

// TaskUpdated announces one task edited (title/description/kind/due_date/assignee). The ERD
// §7 task events are minimal {task_id}; the aggregate id already IS the task, so a consumer
// re-reads the row for the new field values. The aggregate is the task, so its stream orders
// by the task id.
type TaskUpdated struct {
	events.Base
	TaskID string `json:"task_id"`
}

var _ events.Event = TaskUpdated{}

func (TaskUpdated) Type() string          { return TypeTaskUpdated }
func (TaskUpdated) AggregateType() string { return aggregateTypeTask }

// newTaskUpdated builds the produced event from the patched task's id. Fresh uuid v7 event id
// (the consumer dedup key); aggregate_id is the task id, mirroring newTaskCreatedFromTask.
func newTaskUpdated(taskID string) TaskUpdated {
	return TaskUpdated{Base: events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: taskID}, TaskID: taskID}
}

// TypeTaskCompleted is the dotted id this slice PRODUCES when a task is marked DONE
// (docs/erd-prazos.md §7: task.completed {task_id}; §9: POST /v1/tasks/:id/done). Same "task"
// prefix/routing as task.created; its consumer (read models / "meus prazos") marks the task
// done. It is the positive counterpart of task.dismissed.
const TypeTaskCompleted = "task.completed"

// TaskCompleted announces one task concluded (OPEN→DONE). Minimal {task_id} per ERD §7; the
// aggregate is the task, so its stream orders by the task id.
type TaskCompleted struct {
	events.Base
	TaskID string `json:"task_id"`
}

var _ events.Event = TaskCompleted{}

func (TaskCompleted) Type() string          { return TypeTaskCompleted }
func (TaskCompleted) AggregateType() string { return aggregateTypeTask }

// newTaskCompleted builds the produced event from the completed task's id. Fresh uuid v7
// event id; aggregate_id is the task id.
func newTaskCompleted(taskID string) TaskCompleted {
	return TaskCompleted{Base: events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: taskID}, TaskID: taskID}
}

// TypeTaskDismissed is the dotted id this slice PRODUCES when a task is dispensada
// (docs/erd-prazos.md §7: task.dismissed {task_id}; §9: POST /v1/tasks/:id/dismiss —
// OPEN→DISMISSED). Same "task" prefix/routing as task.completed; its consumer drops the task
// from the checklist. It is the negative counterpart of task.completed.
const TypeTaskDismissed = "task.dismissed"

// TaskDismissed announces one task dismissed (OPEN→DISMISSED). Minimal {task_id} per ERD §7;
// the aggregate is the task, so its stream orders by the task id.
type TaskDismissed struct {
	events.Base
	TaskID string `json:"task_id"`
}

var _ events.Event = TaskDismissed{}

func (TaskDismissed) Type() string          { return TypeTaskDismissed }
func (TaskDismissed) AggregateType() string { return aggregateTypeTask }

// newTaskDismissed builds the produced event from the dismissed task's id. Fresh uuid v7
// event id; aggregate_id is the task id.
func newTaskDismissed(taskID string) TaskDismissed {
	return TaskDismissed{Base: events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: taskID}, TaskID: taskID}
}

// TypeSuggestionFeedback is the dotted id this slice PRODUCES at the F2 confirm when the
// prazo had an AI suggestion (feedback loop, camada 2): the delta between what the IA
// suggested and what the human confirmed. Its "deadline" prefix routes it like the other
// domain facts (→ "deadline"/ingestao at the relay, archived as handler-not-found today):
// its consumer — the aggregation that scores a prompt_version's kept/removed/added over
// time — is a later slice, so it is an orphan-for-now, which is safe. The aggregate is the
// deadline (aggregate_id = deadline id), so its stream orders by the prazo.
const TypeSuggestionFeedback = "deadline.suggestion_feedback"

// SuggestionFeedback announces one measured delta between an AI suggestion and the human
// confirmation. It carries the provenance (PromptVersion, Model) so a consumer can bucket
// deltas per prompt version + model without reading anything back, the counts, and the
// legible title sets (Kept/Removed/Added). This is the raw material of "quão confiável foi
// a IA" and, downstream, of refining the versioned prompt.
type SuggestionFeedback struct {
	events.Base
	DeadlineID     string   `json:"deadline_id"`
	IntimationID   string   `json:"intimation_id"`
	PromptVersion  string   `json:"prompt_version"`
	Model          string   `json:"model"`
	SuggestedCount int      `json:"suggested_count"`
	ConfirmedCount int      `json:"confirmed_count"`
	Kept           []string `json:"kept"`
	Removed        []string `json:"removed"`
	Added          []string `json:"added"`
}

var _ events.Event = SuggestionFeedback{}

func (SuggestionFeedback) Type() string          { return TypeSuggestionFeedback }
func (SuggestionFeedback) AggregateType() string { return aggregateTypeDeadline }

// newSuggestionFeedback builds the produced event from the confirmed prazo's ids and the
// computed delta. aggregate_id is the deadline id (a uuid, satisfying the outbox's uuid NOT
// NULL); the event id is a fresh uuid v7 (the consumer dedup key), mirroring the other
// deadline facts.
func newSuggestionFeedback(deadlineID, intimationID string, d SuggestionDelta) SuggestionFeedback {
	return SuggestionFeedback{
		Base:           events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		DeadlineID:     deadlineID,
		IntimationID:   intimationID,
		PromptVersion:  d.PromptVersion,
		Model:          d.Model,
		SuggestedCount: d.SuggestedCount,
		ConfirmedCount: d.ConfirmedCount,
		Kept:           d.Kept,
		Removed:        d.Removed,
		Added:          d.Added,
	}
}
