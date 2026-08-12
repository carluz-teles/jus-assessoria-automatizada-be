package deadline

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
)

// --- fixtures ---------------------------------------------------------------

// confirmParents are the ids a confirmation anchors on: the derived (PENDING) prazo, the
// record it hangs on, the intimação, plus the confirming tenant/user. Fresh uuids so the
// event aggregate assertions (uuid.Parse) hold.
type confirmParents struct {
	tenantID      string
	userID        string
	intimationID  string
	deadlineID    string
	courtRecordID string
}

func newConfirmParents() confirmParents {
	return confirmParents{
		tenantID:      uuid.NewString(),
		userID:        uuid.NewString(),
		intimationID:  uuid.NewString(),
		deadlineID:    uuid.NewString(),
		courtRecordID: uuid.NewString(),
	}
}

// confirmRepo wires a mockRepo whose confirm reads/writes are primed for the given parents
// and start date: the anchor loads that prazo, GetCourtRecordCourt returns court, and
// ConfirmDeadline echoes the deadline/record ids.
func confirmRepo(p confirmParents, start time.Time, court string) *mockRepo {
	return &mockRepo{
		confirmAnchor:    &DeadlineForConfirm{ID: p.deadlineID, CourtRecordID: p.courtRecordID, StartDate: start},
		courtRecordCourt: court,
		confirmID:        p.deadlineID,
		confirmRecordID:  p.courtRecordID,
	}
}

// confirmCmd is a well-formed CONTESTACAO/15/BUSINESS confirmation with one dated,
// assigned task; tests override the fields they exercise.
func confirmCmd(p confirmParents) ConfirmCommand {
	due := time.Date(2024, 2, 5, 0, 0, 0, 0, time.UTC)
	return ConfirmCommand{
		TenantID:     p.tenantID,
		UserID:       p.userID,
		IntimationID: p.intimationID,
		Kind:         KindContestacao,
		Days:         15,
		Counting:     CountingBusiness,
		Tasks: []ConfirmTaskInput{
			{Title: "Protocolar contestação", Kind: "PECA", DueDate: &due, AssigneeUserID: uuid.NewString()},
		},
	}
}

// --- tests ------------------------------------------------------------------

// TestConfirm_RecomputesBusinessAndFlipsOpen is the happy path: a BUSINESS confirmation
// recomputes via AddBusinessDays fed the record's court + the UF derived from it
// (pkg/tribunal), flips the prazo OPEN stamping confirmed_by/at, and returns the confirmed
// prazo + task. It also proves the confirm never inserts a second deadline (update-only).
func TestConfirm_RecomputesBusinessAndFlipsOpen(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)
	holiday := time.Date(2024, 1, 25, 0, 0, 0, 0, time.UTC)
	confirmedAt := time.Date(2024, 1, 17, 9, 30, 0, 0, time.UTC)

	repo := confirmRepo(p, start, "TJSP")
	cal := &fakeCalendar{endDate: end, holidays: []time.Time{holiday}}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, uow, WithClock(func() time.Time { return confirmedAt }))

	res, err := uc.Confirm(context.Background(), confirmCmd(p))
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}

	// The tx was scoped to the principal's tenant (barrier 1 + RLS).
	if len(uow.scopes) != 1 || uow.scopes[0] != p.tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, p.tenantID)
	}

	// The recompute ran the dias-úteis motor, fed the record's court + derived UF, days=15.
	if cal.businessCalls != 1 || cal.calendarCalls != 0 {
		t.Errorf("calendar calls business=%d calendar=%d, want 1/0", cal.businessCalls, cal.calendarCalls)
	}
	if cal.gotUF != "SP" || cal.gotCourt != "TJSP" || cal.gotN != 15 {
		t.Errorf("calendar uf/court/n = %q/%q/%d, want SP/TJSP/15", cal.gotUF, cal.gotCourt, cal.gotN)
	}
	if repo.gotCourtRecordID != p.courtRecordID || repo.gotCourtTenantID != p.tenantID {
		t.Errorf("GetCourtRecordCourt record/tenant = %q/%q, want %q/%q",
			repo.gotCourtRecordID, repo.gotCourtTenantID, p.courtRecordID, p.tenantID)
	}

	// The confirm UPDATE (never an insert) carried the approved fields + recompute + stamp.
	if repo.confirmCalls != 1 || repo.insertCalls != 0 {
		t.Errorf("confirm/insertDeadline calls = %d/%d, want 1/0 (update-only)", repo.confirmCalls, repo.insertCalls)
	}
	cp := repo.gotConfirmParams
	if cp.IntimationID != p.intimationID || cp.TenantID != p.tenantID {
		t.Errorf("confirm keyed by intimation/tenant = %q/%q, want %q/%q", cp.IntimationID, cp.TenantID, p.intimationID, p.tenantID)
	}
	if cp.Kind != KindContestacao || cp.Days != 15 || cp.Counting != CountingBusiness {
		t.Errorf("confirm kind/days/counting = %q/%d/%q", cp.Kind, cp.Days, cp.Counting)
	}
	if !cp.EndDate.Equal(end) || len(cp.HolidaysApplied) != 1 || !cp.HolidaysApplied[0].Equal(holiday) {
		t.Errorf("confirm end/holidays = %v/%v, want %v/[%v]", cp.EndDate, cp.HolidaysApplied, end, holiday)
	}
	if cp.ConfirmedBy != p.userID || !cp.ConfirmedAt.Equal(confirmedAt) {
		t.Errorf("confirmed_by/at = %q/%v, want %q/%v", cp.ConfirmedBy, cp.ConfirmedAt, p.userID, confirmedAt)
	}

	// The returned prazo is OPEN with the recomputed fact.
	d := res.Deadline
	if d.Status != StatusOpen || d.ID != p.deadlineID || !d.EndDate.Equal(end) || d.ConfirmedBy != p.userID {
		t.Errorf("result deadline = %+v, want OPEN/%q/%v/%q", d, p.deadlineID, end, p.userID)
	}
	if !d.StartDate.Equal(start) {
		t.Errorf("result start = %v, want %v (anchor preserved)", d.StartDate, start)
	}
}

// TestConfirm_CalendarCounting proves a CALENDAR confirmation routes to AddCalendarDays
// (dias corridos), honoring the human's explicit toggle — no rito override on this path.
func TestConfirm_CalendarCounting(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	repo := confirmRepo(p, start, "TJSP")
	cal := &fakeCalendar{}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p)
	cmd.Counting = CountingCalendar
	cmd.Tasks = nil
	if _, err := uc.Confirm(context.Background(), cmd); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if cal.calendarCalls != 1 || cal.businessCalls != 0 {
		t.Errorf("calendar calls calendar=%d business=%d, want 1/0", cal.calendarCalls, cal.businessCalls)
	}
	if repo.gotConfirmParams.Counting != CountingCalendar {
		t.Errorf("confirm counting = %q, want CALENDAR", repo.gotConfirmParams.Counting)
	}
}

// TestConfirm_DoubledDoublesRawDays proves the dobro semantics: doubled feeds 2×days to
// the calendar motor (the raw count is doubled BEFORE the math), while days is stored as
// the base count the human approved.
func TestConfirm_DoubledDoublesRawDays(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	repo := confirmRepo(p, start, "TJSP")
	cal := &fakeCalendar{}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p)
	cmd.Days = 15
	cmd.Doubled = true
	cmd.DoubledReason = "FAZENDA_183"
	cmd.Tasks = nil
	if _, err := uc.Confirm(context.Background(), cmd); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if cal.gotN != 30 {
		t.Errorf("calendar n = %d, want 30 (doubled 2×15)", cal.gotN)
	}
	if repo.gotConfirmParams.Days != 15 || !repo.gotConfirmParams.Doubled || repo.gotConfirmParams.DoubledReason != "FAZENDA_183" {
		t.Errorf("confirm days/doubled/reason = %d/%v/%q, want 15/true/FAZENDA_183",
			repo.gotConfirmParams.Days, repo.gotConfirmParams.Doubled, repo.gotConfirmParams.DoubledReason)
	}
}

// TestConfirm_InsertsTasksWithConfirmFields proves the N tasks are inserted MANUAL/OPEN,
// created_by the principal, carrying the prazo's context ids, and that one task.created is
// emitted per task with a parseable uuid aggregate; the unassigned/undated task keeps its
// optional fields empty.
func TestConfirm_InsertsTasksWithConfirmFields(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)
	repo := confirmRepo(p, start, "TJSP")
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	due := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	assignee := uuid.NewString()
	cmd := confirmCmd(p)
	cmd.Tasks = []ConfirmTaskInput{
		{Title: "Peça", Kind: "PECA", Description: "minutar", DueDate: &due, AssigneeUserID: assignee},
		{Title: "Ciência"}, // undated, unassigned
	}

	res, err := uc.Confirm(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}

	if repo.insertTaskCalls != 2 || len(repo.insertedTasks) != 2 {
		t.Fatalf("InsertTask calls = %d, want 2", repo.insertTaskCalls)
	}
	first := repo.insertedTasks[0]
	if first.Status != TaskStatusOpen || first.Source != SourceManual || first.CreatedBy != p.userID {
		t.Errorf("task status/source/created_by = %q/%q/%q, want OPEN/MANUAL/%q", first.Status, first.Source, first.CreatedBy, p.userID)
	}
	if first.TenantID != p.tenantID || first.DeadlineID != p.deadlineID || first.CourtRecordID != p.courtRecordID || first.IntimationID != p.intimationID {
		t.Error("task context ids (tenant/deadline/court_record/intimation) not carried")
	}
	if first.AssigneeUserID != assignee || first.DueDate == nil || !first.DueDate.Equal(due) {
		t.Errorf("task assignee/due = %q/%v, want %q/%v", first.AssigneeUserID, first.DueDate, assignee, due)
	}
	if second := repo.insertedTasks[1]; second.AssigneeUserID != "" || second.DueDate != nil {
		t.Errorf("undated/unassigned task carried assignee/due = %q/%v, want empty/nil", second.AssigneeUserID, second.DueDate)
	}

	// Exactly one deadline.updated + one task.created per task, each with a uuid aggregate.
	updated := publishedOfType[DeadlineUpdated](outbox)
	created := publishedOfType[TaskCreated](outbox)
	if len(updated) != 1 || len(created) != 2 {
		t.Fatalf("events deadline.updated=%d task.created=%d, want 1/2", len(updated), len(created))
	}
	u := updated[0]
	if u.Type() != TypeDeadlineUpdated || u.AggregateType() != aggregateTypeDeadline || u.AggregateID() != p.deadlineID {
		t.Errorf("deadline.updated type/aggregate = %q/%q/%q", u.Type(), u.AggregateType(), u.AggregateID())
	}
	if u.Kind != KindContestacao || u.EndDate != "2024-02-06" || u.Counting != "BUSINESS" || u.Status != "OPEN" {
		t.Errorf("deadline.updated payload = %+v", u)
	}
	for _, tc := range created {
		if tc.Type() != TypeTaskCreated || tc.AggregateType() != aggregateTypeTask {
			t.Errorf("task.created type/aggregate = %q/%q", tc.Type(), tc.AggregateType())
		}
		if _, err := uuid.Parse(tc.AggregateID()); err != nil {
			t.Errorf("task.created aggregate is not a uuid: %v", err)
		}
		if tc.DeadlineID != p.deadlineID || tc.CourtRecordID != p.courtRecordID {
			t.Errorf("task.created deadline/court_record = %q/%q, want %q/%q", tc.DeadlineID, tc.CourtRecordID, p.deadlineID, p.courtRecordID)
		}
	}

	// The result mirrors what was persisted/emitted.
	if len(res.Tasks) != 2 || res.Deadline.Status != StatusOpen {
		t.Errorf("result tasks=%d deadline.status=%q, want 2/OPEN", len(res.Tasks), res.Deadline.Status)
	}
}

// TestConfirm_ReConfirmIsUpdateNotInsert proves idempotency on the deadline: confirming
// twice re-UPDATEs the one prazo (keyed by the 1:1 intimação) and never inserts a second —
// the phantom-free floor the design demands (§9).
func TestConfirm_ReConfirmIsUpdateNotInsert(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	repo := confirmRepo(p, start, "TJSP")
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p)
	cmd.Tasks = nil
	for i := 0; i < 2; i++ {
		if _, err := uc.Confirm(context.Background(), cmd); err != nil {
			t.Fatalf("Confirm() #%d error = %v", i, err)
		}
	}
	if repo.confirmCalls != 2 || repo.insertCalls != 0 {
		t.Errorf("confirm/insertDeadline calls = %d/%d, want 2/0 (re-confirm re-UPDATEs, never inserts)", repo.confirmCalls, repo.insertCalls)
	}
}

// TestConfirm_ReConfirmReplacesTasks proves the REPLACE semantics on tasks (ERD §9 "upsert
// idempotente por intimation_id"): every confirm deletes the prazo's tasks BEFORE re-inserting
// the submitted set, so re-confirming the same intimação leaves EXACTLY the last submit's tasks
// — never the accumulated 2N. An empty task set clears them (delete, no insert).
func TestConfirm_ReConfirmReplacesTasks(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	repo := confirmRepo(p, start, "TJSP")
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p)
	cmd.Tasks = []ConfirmTaskInput{{Title: "Peça"}, {Title: "Ciência"}}

	// Confirm the same intimação twice, each submitting 2 tasks.
	for i := 0; i < 2; i++ {
		if _, err := uc.Confirm(context.Background(), cmd); err != nil {
			t.Fatalf("Confirm() #%d error = %v", i, err)
		}
	}
	// One delete per confirm, and the live set is the last submit's 2 tasks — not 4.
	if repo.deleteTasksCalls != 2 {
		t.Errorf("DeleteTasksByDeadline calls = %d, want 2 (one per confirm)", repo.deleteTasksCalls)
	}
	if len(repo.insertedTasks) != 2 {
		t.Errorf("live tasks after re-confirm = %d, want 2 (replaced, not accumulated)", len(repo.insertedTasks))
	}
	// The delete is keyed by the confirmed deadline id and scoped to the tenant (barrier 1).
	if repo.gotDeleteDeadlineID != p.deadlineID || repo.gotDeleteTenantID != p.tenantID {
		t.Errorf("delete keyed by deadline/tenant = %q/%q, want %q/%q",
			repo.gotDeleteDeadlineID, repo.gotDeleteTenantID, p.deadlineID, p.tenantID)
	}

	// Re-confirming with NO tasks clears the deadline's tasks (delete runs, nothing re-inserted).
	empty := confirmCmd(p)
	empty.Tasks = nil
	if _, err := uc.Confirm(context.Background(), empty); err != nil {
		t.Fatalf("Confirm() empty error = %v", err)
	}
	if repo.deleteTasksCalls != 3 {
		t.Errorf("DeleteTasksByDeadline calls = %d, want 3 (empty confirm still replaces)", repo.deleteTasksCalls)
	}
	if len(repo.insertedTasks) != 0 {
		t.Errorf("live tasks after empty confirm = %d, want 0 (cleared)", len(repo.insertedTasks))
	}
}

// TestConfirm_DeadlineNotFound proves confirming an intimação with no derived prazo is the
// repo's typed ErrDeadlineNotFound (→ 404): nothing is recomputed, confirmed, or emitted.
func TestConfirm_DeadlineNotFound(t *testing.T) {
	p := newConfirmParents()
	repo := &mockRepo{confirmAnchorErr: ErrDeadlineNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	_, err := uc.Confirm(context.Background(), confirmCmd(p))
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if repo.confirmCalls != 0 || repo.insertTaskCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("confirm/insertTask/published = %d/%d/%d, want 0/0/0 on not-found",
			repo.confirmCalls, repo.insertTaskCalls, len(outbox.published))
	}
}

// TestConfirm_TaskDueDateAfterEndDate proves the ERD §4 cross-field invariant: a task
// due_date past the recomputed end_date is a KindInvalid (→ 400), aborting before any
// confirm/task write or event.
func TestConfirm_TaskDueDateAfterEndDate(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)
	repo := confirmRepo(p, start, "TJSP")
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{endDate: end}, outbox, &fakeDedup{}, &fakeUOW{})

	tooLate := time.Date(2024, 2, 7, 0, 0, 0, 0, time.UTC) // after end
	cmd := confirmCmd(p)
	cmd.Tasks = []ConfirmTaskInput{{Title: "Atrasada", DueDate: &tooLate}}

	_, err := uc.Confirm(context.Background(), cmd)
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("error = %v, want KindInvalid", err)
	}
	if repo.confirmCalls != 0 || repo.insertTaskCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("confirm/insertTask/published = %d/%d/%d, want 0/0/0 when a task due_date is past end_date",
			repo.confirmCalls, repo.insertTaskCalls, len(outbox.published))
	}
}
