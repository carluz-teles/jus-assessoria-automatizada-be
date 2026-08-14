package deadline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- fakes ------------------------------------------------------------------

// mockRepo is a hand-rolled Repository: each method returns a configured value and
// records what it was asked, so a test can drive the rule/rito and assert the derived
// prazo. InsertDeadline echoes the entity back with a real uuid id (as the DB would).
type mockRepo struct {
	class    string
	classErr error

	rule    DeadlineRule
	ruleErr error

	insertID  string
	insertErr error

	revokeResult *RevokedDeadline
	revokeErr    error

	checkResult *DeadlineForCheck
	checkErr    error

	markMissedID  string
	markMissedErr error

	// confirm path
	confirmAnchor    *DeadlineForConfirm
	confirmAnchorErr error
	courtRecordCourt string
	courtRecordErr   error
	confirmID        string
	confirmRecordID  string
	confirmErr       error
	insertTaskErr    error

	// feedback loop (suggestion delta at confirm, camada 2)
	latestSuggestion      SuggestionRecord
	latestSuggestionOK    bool
	latestSuggestionErr   error
	latestSuggestionCalls int

	// adjust + manual transition path (5c)
	adjustResult       *DeadlineForAdjust
	adjustErr          error
	updateAdjustID     string
	updateAdjustRecord string
	updateAdjustErr    error
	markStatusID       string
	markStatusErr      error

	// task write path (5b)
	taskForUpdate     *TaskForUpdate
	taskForUpdateErr  error
	updatedTask       *Task
	updateTaskErr     error
	taskTransition    TaskStatus
	taskTransitionErr error
	markTaskStatusID  string
	markTaskStatusErr error

	// task_item write path (checklist / subtarefas, 0031)
	ensureTaskErr    error
	nextItemPosition int
	nextItemPosErr   error
	insertedItem     *TaskItem
	insertItemErr    error
	itemForUpdate    *TaskItemForUpdate
	itemForUpdateErr error
	updatedItem      *TaskItem
	updateItemErr    error
	deleteItemErr    error

	// captured inputs
	gotClassTenantID      string
	gotClassRecordID      string
	gotRuleType           string
	gotRuleCourt          string
	gotRuleVersion        string
	inserted              *Deadline
	insertCalls           int
	gotRevokeIntimationID string
	gotRevokeTenantID     string
	revokeCalls           int
	gotCheckID            string
	gotCheckTenantID      string
	checkCalls            int
	gotMissedID           string
	gotMissedTenantID     string
	markMissedCalls       int
	gotConfirmIntimation  string
	gotConfirmTenantID    string
	confirmAnchorCalls    int
	gotCourtRecordID      string
	gotCourtTenantID      string
	courtCalls            int
	gotConfirmParams      ConfirmDeadlineParams
	confirmCalls          int
	insertedTasks         []*Task
	insertTaskCalls       int
	gotDeleteDeadlineID   string
	gotDeleteTenantID     string
	deleteTasksCalls      int
	gotAdjustID           string
	gotAdjustTenantID     string
	adjustReadCalls       int
	gotUpdateAdjustParams UpdateDeadlineAdjustParams
	updateAdjustCalls     int
	gotMarkStatusID       string
	gotMarkStatusTenantID string
	gotMarkStatusFrom     Status
	gotMarkStatusTo       Status
	markStatusCalls       int
	gotTaskUpdateID       string
	gotTaskUpdateTenantID string
	taskForUpdateCalls    int
	gotUpdateTaskParams   UpdateTaskParams
	updateTaskCalls       int
	gotTaskTransitionID   string
	gotTaskTransitionTID  string
	taskTransitionCalls   int
	gotMarkTaskID         string
	gotMarkTaskTenantID   string
	gotMarkTaskFrom       TaskStatus
	gotMarkTaskTo         TaskStatus
	gotMarkTaskCompleted  *time.Time
	markTaskStatusCalls   int
	gotEnsureTaskID       string
	gotEnsureTenantID     string
	ensureTaskCalls       int
	gotNextPosTaskID      string
	nextPosCalls          int
	insertedItems         []*TaskItem
	insertItemCalls       int
	gotItemForUpdateID    string
	gotItemForUpdateTask  string
	itemForUpdateCalls    int
	gotUpdateItemParams   UpdateTaskItemParams
	updateItemCalls       int
	gotDeleteItemID       string
	gotDeleteItemTask     string
	gotDeleteItemTenant   string
	deleteItemCalls       int
}

func (m *mockRepo) GetCourtRecordClass(_ context.Context, _ database.Tx, tenantID, courtRecordID string) (string, error) {
	m.gotClassTenantID = tenantID
	m.gotClassRecordID = courtRecordID
	return m.class, m.classErr
}

func (m *mockRepo) ResolveRule(_ context.Context, _ database.Tx, rulesVersion, intimationType, court string) (DeadlineRule, error) {
	m.gotRuleVersion, m.gotRuleType, m.gotRuleCourt = rulesVersion, intimationType, court
	return m.rule, m.ruleErr
}

func (m *mockRepo) InsertDeadline(_ context.Context, _ database.Tx, d *Deadline) (*Deadline, error) {
	m.insertCalls++
	m.inserted = d
	if m.insertErr != nil {
		return nil, m.insertErr
	}
	saved := *d
	saved.ID = m.insertID
	return &saved, nil
}

func (m *mockRepo) RevokeDeadlineByIntimation(_ context.Context, _ database.Tx, intimationID, tenantID string) (*RevokedDeadline, error) {
	m.revokeCalls++
	m.gotRevokeIntimationID = intimationID
	m.gotRevokeTenantID = tenantID
	return m.revokeResult, m.revokeErr
}

func (m *mockRepo) GetLatestSuggestion(_ context.Context, _ database.Tx, _, _ string) (SuggestionRecord, bool, error) {
	m.latestSuggestionCalls++
	return m.latestSuggestion, m.latestSuggestionOK, m.latestSuggestionErr
}

func (m *mockRepo) GetDeadlineForCheck(_ context.Context, _ database.Tx, deadlineID, tenantID string) (*DeadlineForCheck, error) {
	m.checkCalls++
	m.gotCheckID = deadlineID
	m.gotCheckTenantID = tenantID
	return m.checkResult, m.checkErr
}

func (m *mockRepo) MarkMissed(_ context.Context, _ database.Tx, deadlineID, tenantID string) (string, error) {
	m.markMissedCalls++
	m.gotMissedID = deadlineID
	m.gotMissedTenantID = tenantID
	return m.markMissedID, m.markMissedErr
}

func (m *mockRepo) GetDeadlineForConfirm(_ context.Context, _ database.Tx, intimationID, tenantID string) (*DeadlineForConfirm, error) {
	m.confirmAnchorCalls++
	m.gotConfirmIntimation = intimationID
	m.gotConfirmTenantID = tenantID
	return m.confirmAnchor, m.confirmAnchorErr
}

func (m *mockRepo) GetCourtRecordCourt(_ context.Context, _ database.Tx, tenantID, courtRecordID string) (string, error) {
	m.courtCalls++
	m.gotCourtTenantID = tenantID
	m.gotCourtRecordID = courtRecordID
	return m.courtRecordCourt, m.courtRecordErr
}

func (m *mockRepo) ConfirmDeadline(_ context.Context, _ database.Tx, p ConfirmDeadlineParams) (string, string, error) {
	m.confirmCalls++
	m.gotConfirmParams = p
	if m.confirmErr != nil {
		return "", "", m.confirmErr
	}
	return m.confirmID, m.confirmRecordID, nil
}

// DeleteTasksByDeadline models the REPLACE step: it drops the confirmed prazo's live tasks
// (clearing insertedTasks) before the confirm re-inserts the submitted set, so insertedTasks
// reflects only the last submit — the way a real DELETE + re-INSERT would. Records the count
// and the (deadlineID, tenantID) scoping so a test can assert the 2-barrier key.
func (m *mockRepo) DeleteTasksByDeadline(_ context.Context, _ database.Tx, deadlineID, tenantID string) error {
	m.deleteTasksCalls++
	m.gotDeleteDeadlineID = deadlineID
	m.gotDeleteTenantID = tenantID
	m.insertedTasks = nil
	return nil
}

// GetDeadlineForAdjust returns the configured adjustable state and records the (id, tenant)
// scoping so an ajuste test can assert the 2-barrier key.
func (m *mockRepo) GetDeadlineForAdjust(_ context.Context, _ database.Tx, deadlineID, tenantID string) (*DeadlineForAdjust, error) {
	m.adjustReadCalls++
	m.gotAdjustID = deadlineID
	m.gotAdjustTenantID = tenantID
	return m.adjustResult, m.adjustErr
}

// UpdateDeadlineAdjust captures the merged/recomputed params and echoes the configured ids,
// so a test can assert the patch was applied over the stored values and the recompute landed.
func (m *mockRepo) UpdateDeadlineAdjust(_ context.Context, _ database.Tx, p UpdateDeadlineAdjustParams) (string, string, error) {
	m.updateAdjustCalls++
	m.gotUpdateAdjustParams = p
	if m.updateAdjustErr != nil {
		return "", "", m.updateAdjustErr
	}
	return m.updateAdjustID, m.updateAdjustRecord, nil
}

// MarkDeadlineStatus records the (id, tenant, from, to) of a manual transition and returns the
// configured flipped id (or error), so a met/missed test can assert the guarded flip's args.
func (m *mockRepo) MarkDeadlineStatus(_ context.Context, _ database.Tx, deadlineID, tenantID string, from, to Status) (string, error) {
	m.markStatusCalls++
	m.gotMarkStatusID = deadlineID
	m.gotMarkStatusTenantID = tenantID
	m.gotMarkStatusFrom = from
	m.gotMarkStatusTo = to
	if m.markStatusErr != nil {
		return "", m.markStatusErr
	}
	return m.markStatusID, nil
}

// InsertTask echoes the task back with a real uuid id (as the DB would), so a test can
// assert task.created's aggregate is a parseable uuid, and captures every inserted task.
func (m *mockRepo) InsertTask(_ context.Context, _ database.Tx, t *Task) (*Task, error) {
	m.insertTaskCalls++
	if m.insertTaskErr != nil {
		return nil, m.insertTaskErr
	}
	saved := *t
	saved.ID = uuid.NewString()
	m.insertedTasks = append(m.insertedTasks, &saved)
	return &saved, nil
}

// GetTaskForUpdate returns the configured editable state and records the (id, tenant) scoping so
// a PATCH test can assert the 2-barrier key.
func (m *mockRepo) GetTaskForUpdate(_ context.Context, _ database.Tx, taskID, tenantID string) (*TaskForUpdate, error) {
	m.taskForUpdateCalls++
	m.gotTaskUpdateID = taskID
	m.gotTaskUpdateTenantID = tenantID
	return m.taskForUpdate, m.taskForUpdateErr
}

// UpdateTask captures the merged params and echoes the configured saved task (or error), so a
// test can assert the patch was applied over the stored values.
func (m *mockRepo) UpdateTask(_ context.Context, _ database.Tx, p UpdateTaskParams) (*Task, error) {
	m.updateTaskCalls++
	m.gotUpdateTaskParams = p
	if m.updateTaskErr != nil {
		return nil, m.updateTaskErr
	}
	return m.updatedTask, nil
}

// GetTaskForTransition returns the configured status and records the (id, tenant) scoping so a
// done/dismiss test can assert the 2-barrier key.
func (m *mockRepo) GetTaskForTransition(_ context.Context, _ database.Tx, taskID, tenantID string) (TaskStatus, error) {
	m.taskTransitionCalls++
	m.gotTaskTransitionID = taskID
	m.gotTaskTransitionTID = tenantID
	return m.taskTransition, m.taskTransitionErr
}

// MarkTaskStatus records the (id, tenant, from, to, completed_at) of a manual task transition and
// returns the configured flipped id (or error), so a done/dismiss test can assert the guarded flip.
func (m *mockRepo) MarkTaskStatus(_ context.Context, _ database.Tx, taskID, tenantID string, from, to TaskStatus, completedAt *time.Time) (string, error) {
	m.markTaskStatusCalls++
	m.gotMarkTaskID = taskID
	m.gotMarkTaskTenantID = tenantID
	m.gotMarkTaskFrom = from
	m.gotMarkTaskTo = to
	m.gotMarkTaskCompleted = completedAt
	if m.markTaskStatusErr != nil {
		return "", m.markTaskStatusErr
	}
	return m.markTaskStatusID, nil
}

// EnsureTaskInTenant records the (task, tenant) guard scoping and returns the configured error
// (nil = the parent task exists), so a checklist-create test can assert the parent guard runs first.
func (m *mockRepo) EnsureTaskInTenant(_ context.Context, _ database.Tx, taskID, tenantID string) error {
	m.ensureTaskCalls++
	m.gotEnsureTaskID = taskID
	m.gotEnsureTenantID = tenantID
	return m.ensureTaskErr
}

// NextTaskItemPosition returns the configured append slot, recording the task it was asked for.
func (m *mockRepo) NextTaskItemPosition(_ context.Context, _ database.Tx, taskID, _ string) (int, error) {
	m.nextPosCalls++
	m.gotNextPosTaskID = taskID
	if m.nextItemPosErr != nil {
		return 0, m.nextItemPosErr
	}
	return m.nextItemPosition, nil
}

// InsertTaskItem records the inserted item and echoes back the configured saved item (with its id),
// mirroring how the DB would return the row.
func (m *mockRepo) InsertTaskItem(_ context.Context, _ database.Tx, item *TaskItem) (*TaskItem, error) {
	m.insertItemCalls++
	m.insertedItems = append(m.insertedItems, item)
	if m.insertItemErr != nil {
		return nil, m.insertItemErr
	}
	if m.insertedItem != nil {
		return m.insertedItem, nil
	}
	saved := *item
	saved.ID = uuid.NewString()
	return &saved, nil
}

// GetTaskItemForUpdate returns the configured editable state, recording the (item, task) scoping so
// a patch test can assert the cross-task guard key.
func (m *mockRepo) GetTaskItemForUpdate(_ context.Context, _ database.Tx, itemID, taskID, _ string) (*TaskItemForUpdate, error) {
	m.itemForUpdateCalls++
	m.gotItemForUpdateID = itemID
	m.gotItemForUpdateTask = taskID
	if m.itemForUpdateErr != nil {
		return nil, m.itemForUpdateErr
	}
	return m.itemForUpdate, nil
}

// UpdateTaskItem records the merged params (the caller-derived done_at included) and returns the
// configured saved item, so a patch test can assert the merge + done_at logic.
func (m *mockRepo) UpdateTaskItem(_ context.Context, _ database.Tx, p UpdateTaskItemParams) (*TaskItem, error) {
	m.updateItemCalls++
	m.gotUpdateItemParams = p
	if m.updateItemErr != nil {
		return nil, m.updateItemErr
	}
	return m.updatedItem, nil
}

// DeleteTaskItem records the (item, task, tenant) scoping and returns the configured error, so a
// delete test can assert the 404-on-miss and the barrier key.
func (m *mockRepo) DeleteTaskItem(_ context.Context, _ database.Tx, itemID, taskID, tenantID string) error {
	m.deleteItemCalls++
	m.gotDeleteItemID = itemID
	m.gotDeleteItemTask = taskID
	m.gotDeleteItemTenant = tenantID
	return m.deleteItemErr
}

// fakeCalendar records which motor was called (business vs calendar) and the args, and
// returns a configured end date + skipped days. The default end date is well after any
// start so validate() passes.
type fakeCalendar struct {
	businessCalls int
	calendarCalls int
	gotStart      time.Time
	gotN          int
	gotUF         string
	gotCourt      string
	endDate       time.Time
	holidays      []time.Time
	err           error
}

func (c *fakeCalendar) AddBusinessDays(_ context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error) {
	c.businessCalls++
	c.gotStart, c.gotN, c.gotUF, c.gotCourt = start, n, uf, court
	return c.result(start, n)
}

func (c *fakeCalendar) AddCalendarDays(_ context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error) {
	c.calendarCalls++
	c.gotStart, c.gotN, c.gotUF, c.gotCourt = start, n, uf, court
	return c.result(start, n)
}

func (c *fakeCalendar) result(start time.Time, n int) (time.Time, []time.Time, error) {
	if c.err != nil {
		return time.Time{}, nil, c.err
	}
	end := c.endDate
	if end.IsZero() {
		end = start.AddDate(0, 0, n)
	}
	return end, c.holidays, nil
}

// fakeUOW runs fn with a nil tx (the mocked repo/dedup never touch it) and records the
// RLS scope each Do asked for.
type fakeUOW struct {
	scopes []string
	err    error
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, tenantID)
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, "system")
	return fn(nil)
}

// fakeDedup reports every event as first-seen by default; set seen=true to model an
// at-least-once replay.
type fakeDedup struct {
	seen      bool
	err       error
	marked    []string
	consumers []string
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, consumer, eventID string) (bool, error) {
	d.consumers = append(d.consumers, consumer)
	d.marked = append(d.marked, eventID)
	return d.seen, d.err
}

// fakeOutbox captures every published event so a test can assert the deadline.opened
// contract (aggregate, kind, end_date).
type fakeOutbox struct {
	published []events.Event
	err       error
}

func (o *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	o.published = append(o.published, ev)
	return o.err
}

// --- fixtures ---------------------------------------------------------------

// observedFixture builds a well-formed observed event for a cível record (TJSP/SP), the
// happy default; tests override the fields they exercise.
func observedFixture() IntimationObserved {
	return IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:        uuid.NewString(),
		IntimationID:    uuid.NewString(),
		CourtRecordID:   uuid.NewString(),
		CaseID:          uuid.NewString(),
		Type:            "CITACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2024-01-16",
	}
}

func citacaoRule() DeadlineRule {
	return DeadlineRule{RulesVersion: "v0", Kind: KindContestacao, Days: 15, Counting: CountingBusiness}
}

// cancelledFixture builds a well-formed intimation.cancelled, the revocation trigger;
// tests override the fields they exercise.
func cancelledFixture() IntimationCancelled {
	return IntimationCancelled{
		Base:         events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:     uuid.NewString(),
		IntimationID: uuid.NewString(),
		Reason:       "retificada pelo tribunal",
	}
}

// --- tests ------------------------------------------------------------------

// TestOnIntimationObserved_DerivesPendingRuleDeadline is the happy path: a CITACAO on a
// cível court derives a CONTESTACAO/15/BUSINESS prazo, born PENDING + source RULE +
// unconfirmed, computed via AddBusinessDays with the event's UF/court, and emits exactly
// one deadline.opened whose aggregate is the new deadline id.
func TestOnIntimationObserved_DerivesPendingRuleDeadline(t *testing.T) {
	ev := observedFixture()
	deadlineID := uuid.NewString()
	holiday := time.Date(2024, 1, 25, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{class: "Procedimento Comum Cível", rule: citacaoRule(), insertID: deadlineID}
	cal := &fakeCalendar{endDate: end, holidays: []time.Time{holiday}}
	outbox := &fakeOutbox{}
	// Relógio ANTES do fim do prazo → nasce PENDING (o born-MISSED só vale depois da
	// carência D+1 — ver TestOnIntimationObserved_OverdueBornMissed).
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{},
		WithClock(func() time.Time { return time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC) }))

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	// The persisted prazo.
	d := repo.inserted
	if d == nil {
		t.Fatal("expected a deadline to be inserted")
	}
	if d.Status != StatusPending {
		t.Errorf("Status = %q, want PENDING (born as a suggestion)", d.Status)
	}
	if d.Source != SourceRule {
		t.Errorf("Source = %q, want RULE", d.Source)
	}
	if d.Kind != KindContestacao || d.Days != 15 || d.Counting != CountingBusiness {
		t.Errorf("kind/days/counting = %q/%d/%q, want CONTESTACAO/15/BUSINESS", d.Kind, d.Days, d.Counting)
	}
	if d.RulesVersion != "v0" {
		t.Errorf("RulesVersion = %q, want v0", d.RulesVersion)
	}
	if !d.EndDate.Equal(end) {
		t.Errorf("EndDate = %v, want %v", d.EndDate, end)
	}
	if len(d.HolidaysApplied) != 1 || !d.HolidaysApplied[0].Equal(holiday) {
		t.Errorf("HolidaysApplied = %v, want [%v]", d.HolidaysApplied, holiday)
	}
	if d.IntimationID != ev.IntimationID || d.CourtRecordID != ev.CourtRecordID || d.TenantID != ev.TenantID {
		t.Error("origin ids not carried onto the deadline")
	}

	// Cível → the dias-úteis motor, fed the event's UF/court (denormalized by producer).
	if cal.businessCalls != 1 || cal.calendarCalls != 0 {
		t.Errorf("calendar calls business=%d calendar=%d, want 1/0 (cível → dias úteis)", cal.businessCalls, cal.calendarCalls)
	}
	if cal.gotUF != "SP" || cal.gotCourt != "TJSP" || cal.gotN != 15 {
		t.Errorf("calendar args uf/court/n = %q/%q/%d, want SP/TJSP/15", cal.gotUF, cal.gotCourt, cal.gotN)
	}

	// The read used the event's record id (decisão P1), scoped to the event's tenant
	// (barrier 1 — the explicit tenant filter).
	if repo.gotClassRecordID != ev.CourtRecordID {
		t.Errorf("GetCourtRecordClass got %q, want %q", repo.gotClassRecordID, ev.CourtRecordID)
	}
	if repo.gotClassTenantID != ev.TenantID {
		t.Errorf("GetCourtRecordClass tenant = %q, want %q", repo.gotClassTenantID, ev.TenantID)
	}
	// The resolver got the event's type + court.
	if repo.gotRuleType != "CITACAO" || repo.gotRuleCourt != "TJSP" || repo.gotRuleVersion != rulesVersion {
		t.Errorf("ResolveRule got type/court/version = %q/%q/%q", repo.gotRuleType, repo.gotRuleCourt, repo.gotRuleVersion)
	}

	// Exactly one deadline.opened (o agendamento dos checks tem teste próprio), aggregate =
	// o novo deadline id (uuid parseável).
	openeds := publishedOfType[DeadlineOpened](outbox)
	if len(openeds) != 1 {
		t.Fatalf("deadline.opened publicados = %d, want 1", len(openeds))
	}
	opened := openeds[0]
	if opened.Type() != TypeDeadlineOpened || opened.AggregateType() != aggregateTypeDeadline {
		t.Errorf("event type/aggregate = %q/%q", opened.Type(), opened.AggregateType())
	}
	if opened.AggregateID() != deadlineID {
		t.Errorf("aggregate id = %q, want the deadline id %q", opened.AggregateID(), deadlineID)
	}
	if _, err := uuid.Parse(opened.AggregateID()); err != nil {
		t.Errorf("aggregate id is not a uuid: %v", err)
	}
	if opened.Kind != KindContestacao || opened.EndDate != "2024-02-06" || opened.Counting != "BUSINESS" {
		t.Errorf("opened kind/end/counting = %q/%q/%q", opened.Kind, opened.EndDate, opened.Counting)
	}
}

// TestOnIntimationObserved_OverdueBornMissed cobre o "prazo órfão" do backfill: quando a
// intimação é histórica e o prazo já NASCE vencido (a carência D+1 já passou no now da
// criação), ele nasce MISSED — não PENDING — senão o missed_check (agendado só para ETAs
// futuras em scheduleChecks) nunca é enfileirado e o prazo ficaria PENDING para sempre. É
// silencioso: emite só deadline.opened, nunca um deadline.missed nem checks agendados.
func TestOnIntimationObserved_OverdueBornMissed(t *testing.T) {
	ev := observedFixture()
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC) // ~2 anos após o fim → carência passou

	repo := &mockRepo{class: "Procedimento Comum Cível", rule: citacaoRule(), insertID: uuid.NewString()}
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{},
		WithClock(func() time.Time { return now }))

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if repo.inserted == nil {
		t.Fatal("expected a deadline to be inserted")
	}
	if repo.inserted.Status != StatusMissed {
		t.Errorf("Status = %q, want MISSED (nascido já vencido)", repo.inserted.Status)
	}
	// Silencioso: só deadline.opened; nenhum deadline.missed, missed_check ou reminder_check.
	if got := len(outbox.published); got != 1 {
		t.Fatalf("published events = %d, want 1 (só deadline.opened)", got)
	}
	if _, ok := outbox.published[0].(DeadlineOpened); !ok {
		t.Errorf("published[0] = %T, want DeadlineOpened", outbox.published[0])
	}
	if n := len(publishedOfType[DeadlineMissedCheck](outbox)); n != 0 {
		t.Errorf("missed_check agendados = %d, want 0 (marca no passado)", n)
	}
	if n := len(publishedOfType[DeadlineReminderCheck](outbox)); n != 0 {
		t.Errorf("reminder_check agendados = %d, want 0 (marcas no passado)", n)
	}
}

// TestOnIntimationObserved_RuleToDeadline covers the rule-derived fields across the safe
// v0 rules the resolver returns (the resolution itself is SQL — here the mock stands in):
// each type maps to its kind/days, and an unknown type falls back to the GENERICO
// catch-all. Proves the domain builds the prazo straight from whatever the resolver gives.
func TestOnIntimationObserved_RuleToDeadline(t *testing.T) {
	tests := []struct {
		name     string
		typ      string
		rule     DeadlineRule
		wantKind string
		wantDays int
	}{
		{
			name:     "CITACAO → CONTESTACAO 15",
			typ:      "CITACAO",
			rule:     DeadlineRule{RulesVersion: "v0", Kind: KindContestacao, Days: 15, Counting: CountingBusiness},
			wantKind: KindContestacao,
			wantDays: 15,
		},
		{
			name:     "INTIMACAO → MANIFESTACAO 5",
			typ:      "INTIMACAO",
			rule:     DeadlineRule{RulesVersion: "v0", Kind: KindManifestacao, Days: 5, Counting: CountingBusiness},
			wantKind: KindManifestacao,
			wantDays: 5,
		},
		{
			name:     "unknown → GENERICO catch-all",
			typ:      "SOMETHING_ELSE",
			rule:     DeadlineRule{RulesVersion: "v0", Kind: KindGenerico, Days: 5, Counting: CountingBusiness},
			wantKind: KindGenerico,
			wantDays: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := observedFixture()
			ev.Type = tt.typ
			repo := &mockRepo{class: "Cível", rule: tt.rule, insertID: uuid.NewString()}
			uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
				t.Fatalf("OnIntimationObserved() error = %v", err)
			}
			if repo.inserted.Kind != tt.wantKind || repo.inserted.Days != tt.wantDays {
				t.Errorf("kind/days = %q/%d, want %q/%d", repo.inserted.Kind, repo.inserted.Days, tt.wantKind, tt.wantDays)
			}
		})
	}
}

// TestOnIntimationObserved_LaborRiteUsesCalendarDays proves decisão P2: a labor court
// (TRT) overrides the rule's BUSINESS suggestion to CALENDAR (dias corridos), routing to
// AddCalendarDays and persisting counting=CALENDAR — even though the resolved rule
// suggested BUSINESS.
func TestOnIntimationObserved_LaborRiteUsesCalendarDays(t *testing.T) {
	ev := observedFixture()
	ev.Court = "TRT2"
	ev.UF = "SP"
	repo := &mockRepo{
		class:    "Reclamação Trabalhista",
		rule:     DeadlineRule{RulesVersion: "v0", Kind: KindGenerico, Days: 5, Counting: CountingBusiness},
		insertID: uuid.NewString(),
	}
	cal := &fakeCalendar{}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	if cal.calendarCalls != 1 || cal.businessCalls != 0 {
		t.Errorf("calendar calls calendar=%d business=%d, want 1/0 (labor → dias corridos)", cal.calendarCalls, cal.businessCalls)
	}
	if repo.inserted.Counting != CountingCalendar {
		t.Errorf("Counting = %q, want CALENDAR", repo.inserted.Counting)
	}
}

// TestOnIntimationObserved_Idempotent proves a replay (dedup reports seen) is a pure
// no-op: no rule read, no insert, no event — exactly one prazo can exist per event.
func TestOnIntimationObserved_Idempotent(t *testing.T) {
	ev := observedFixture()
	repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: uuid.NewString()}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	if repo.insertCalls != 0 {
		t.Errorf("InsertDeadline calls = %d, want 0 on a replay", repo.insertCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
	// It still dedups under the slice-specific consumer name.
	if len(dedup.consumers) != 1 || dedup.consumers[0] != consumerDeadline {
		t.Errorf("dedup consumer = %v, want [%q]", dedup.consumers, consumerDeadline)
	}
}

// TestOnIntimationObserved_ExistingDeadlineNoPhantom proves the 1:1 floor: when the
// insert reports ErrDeadlineExists (a prazo already exists for the intimação), the use
// case no-ops (returns nil, emits nothing) rather than opening a phantom prazo.
func TestOnIntimationObserved_ExistingDeadlineNoPhantom(t *testing.T) {
	ev := observedFixture()
	repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertErr: ErrDeadlineExists}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v, want nil (idempotent no-op)", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 (no phantom deadline.opened)", len(outbox.published))
	}
}

// TestOnIntimationObserved_Errors proves the infra errors propagate (retryable) and the
// malformed anchor is a terminal invalid — each aborts before any event is published.
func TestOnIntimationObserved_Errors(t *testing.T) {
	infra := errors.New("boom")
	tests := []struct {
		name     string
		mutate   func(ev *IntimationObserved, r *mockRepo)
		wantKind apperr.Kind
		wantErr  bool
	}{
		{
			name:     "malformed anchor → invalid",
			mutate:   func(ev *IntimationObserved, _ *mockRepo) { ev.DeadlineStartAt = "not-a-date" },
			wantKind: apperr.KindInvalid,
			wantErr:  true,
		},
		{
			name:    "court_record read fails → propagate",
			mutate:  func(_ *IntimationObserved, r *mockRepo) { r.classErr = infra },
			wantErr: true,
		},
		{
			name:    "rule resolve fails → propagate",
			mutate:  func(_ *IntimationObserved, r *mockRepo) { r.ruleErr = ErrRuleNotFound },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := observedFixture()
			repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: uuid.NewString()}
			tt.mutate(&ev, repo)
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			err := uc.OnIntimationObserved(context.Background(), ev)
			if tt.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if tt.wantKind != "" {
				ae, ok := apperr.From(err)
				if !ok || ae.Kind != tt.wantKind {
					t.Errorf("error kind = %v, want %q", err, tt.wantKind)
				}
			}
			if len(outbox.published) != 0 {
				t.Errorf("published events = %d, want 0 on error", len(outbox.published))
			}
		})
	}
}

// publishedOfType returns every published event of the concrete type T — the reminder/miss
// marks and lembretes are asserted per-type without index bookkeeping.
func publishedOfType[T events.Event](out *fakeOutbox) []T {
	var res []T
	for _, ev := range out.published {
		if v, ok := ev.(T); ok {
			res = append(res, v)
		}
	}
	return res
}

// TestOnIntimationObserved_SchedulesReminderAndMissedChecks proves the 4b-ii creation
// extension: after the deadline.opened, a fresh prazo whose end_date is comfortably in the
// future schedules all three D-N reminder_check marks (days_left 3/1/0, each with the right
// ETA and stable idempotency key) plus one missed_check at D+1 — every mark carrying the
// tenant and the deadline id as aggregate.
func TestOnIntimationObserved_SchedulesReminderAndMissedChecks(t *testing.T) {
	ev := observedFixture()
	deadlineID := uuid.NewString()
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: deadlineID}
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	// deadline.opened is still emitted exactly once (the marks are additive).
	if got := len(publishedOfType[DeadlineOpened](outbox)); got != 1 {
		t.Fatalf("deadline.opened events = %d, want 1", got)
	}

	// Three reminder_check marks: days_left 3/1/0, ETA = start-of-day(end) − days_left.
	reminders := publishedOfType[DeadlineReminderCheck](outbox)
	if len(reminders) != 3 {
		t.Fatalf("reminder_check marks = %d, want 3", len(reminders))
	}
	wantAt := map[int]time.Time{
		3: time.Date(2024, 1, 29, 0, 0, 0, 0, time.UTC),
		1: time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC),
		0: end,
	}
	seen := map[int]bool{}
	for _, r := range reminders {
		seen[r.DaysLeft] = true
		at, ok := r.ProcessAt()
		if !ok || !at.Equal(wantAt[r.DaysLeft]) {
			t.Errorf("reminder days_left=%d ProcessAt = %v (ok=%v), want %v", r.DaysLeft, at, ok, wantAt[r.DaysLeft])
		}
		if r.TenantID != ev.TenantID {
			t.Errorf("reminder days_left=%d tenant = %q, want %q", r.DaysLeft, r.TenantID, ev.TenantID)
		}
		if r.AggregateType() != aggregateTypeDeadline || r.AggregateID() != deadlineID {
			t.Errorf("reminder aggregate = %q/%q, want deadline/%q", r.AggregateType(), r.AggregateID(), deadlineID)
		}
		if want := fmt.Sprintf("deadline-reminder:%s:%d", deadlineID, r.DaysLeft); r.IdempotencyKey() != want {
			t.Errorf("reminder idempotency key = %q, want %q", r.IdempotencyKey(), want)
		}
	}
	for _, dl := range reminderDaysLeft {
		if !seen[dl] {
			t.Errorf("missing reminder_check for days_left=%d", dl)
		}
	}

	// One missed_check at D+1, stable key, tenant + aggregate carried.
	missed := publishedOfType[DeadlineMissedCheck](outbox)
	if len(missed) != 1 {
		t.Fatalf("missed_check marks = %d, want 1", len(missed))
	}
	at, ok := missed[0].ProcessAt()
	if !ok || !at.Equal(time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("missed_check ProcessAt = %v (ok=%v), want 2024-02-02", at, ok)
	}
	if missed[0].TenantID != ev.TenantID || missed[0].AggregateID() != deadlineID {
		t.Errorf("missed_check tenant/aggregate = %q/%q", missed[0].TenantID, missed[0].AggregateID())
	}
	if want := "deadline-missed:" + deadlineID; missed[0].IdempotencyKey() != want {
		t.Errorf("missed_check idempotency key = %q, want %q", missed[0].IdempotencyKey(), want)
	}

	// opened + 3 reminders + 1 missed = 5 total, nothing else.
	if len(outbox.published) != 5 {
		t.Errorf("total published = %d, want 5", len(outbox.published))
	}
}

// TestOnIntimationObserved_SkipsPastMarks proves the birth-time future-guard: a mark whose
// ETA already passed at birth is never scheduled. A prazo born <3 dias do fim skips D-3; a
// prazo born after its vencimento schedules nothing at all.
func TestOnIntimationObserved_SkipsPastMarks(t *testing.T) {
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		now          time.Time
		wantDaysLeft []int
		wantMissed   bool
		wantTotalPub int
	}{
		{
			name:         "born <3 days before end skips D-3",
			now:          time.Date(2024, 1, 30, 12, 0, 0, 0, time.UTC), // D-3 (01-29) already past
			wantDaysLeft: []int{1, 0},
			wantMissed:   true,
			wantTotalPub: 4, // opened + 2 reminders + missed
		},
		{
			name:         "born after vencimento schedules nothing",
			now:          time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC), // even D+1 is past
			wantDaysLeft: nil,
			wantMissed:   false,
			wantTotalPub: 1, // opened only
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := observedFixture()
			repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: uuid.NewString()}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{endDate: end}, outbox, &fakeDedup{}, &fakeUOW{},
				WithClock(func() time.Time { return tt.now }))

			if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
				t.Fatalf("OnIntimationObserved() error = %v", err)
			}

			reminders := publishedOfType[DeadlineReminderCheck](outbox)
			if len(reminders) != len(tt.wantDaysLeft) {
				t.Fatalf("reminder marks = %d, want %d", len(reminders), len(tt.wantDaysLeft))
			}
			got := map[int]bool{}
			for _, r := range reminders {
				got[r.DaysLeft] = true
			}
			for _, dl := range tt.wantDaysLeft {
				if !got[dl] {
					t.Errorf("missing reminder for days_left=%d", dl)
				}
			}
			if gotMissed := len(publishedOfType[DeadlineMissedCheck](outbox)) == 1; gotMissed != tt.wantMissed {
				t.Errorf("missed_check scheduled = %v, want %v", gotMissed, tt.wantMissed)
			}
			if len(outbox.published) != tt.wantTotalPub {
				t.Errorf("total published = %d, want %d", len(outbox.published), tt.wantTotalPub)
			}
		})
	}
}

// TestOnIntimationCancelled_RevokesExistingDeadline is the revocation happy path: the
// repo reports the prazo was flipped to CANCELLED, so the use case emits exactly one
// deadline.revoked whose aggregate is the revoked deadline id (a parseable uuid) and whose
// payload carries the triggering intimação + the DJEN reason. The revoke is scoped to the
// event's tenant/intimação (barrier 1 args threaded through).
func TestOnIntimationCancelled_RevokesExistingDeadline(t *testing.T) {
	ev := cancelledFixture()
	deadlineID := uuid.NewString()
	courtRecordID := uuid.NewString()

	repo := &mockRepo{revokeResult: &RevokedDeadline{ID: deadlineID, CourtRecordID: courtRecordID}}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnIntimationCancelled(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationCancelled() error = %v", err)
	}

	// The revoke read the event's intimação, scoped to the event's tenant (barrier 1).
	if repo.revokeCalls != 1 {
		t.Fatalf("RevokeDeadlineByIntimation calls = %d, want 1", repo.revokeCalls)
	}
	if repo.gotRevokeIntimationID != ev.IntimationID || repo.gotRevokeTenantID != ev.TenantID {
		t.Errorf("revoke args intimation/tenant = %q/%q, want %q/%q",
			repo.gotRevokeIntimationID, repo.gotRevokeTenantID, ev.IntimationID, ev.TenantID)
	}

	// Exactly one deadline.revoked, aggregate = the revoked deadline id (a uuid).
	if len(outbox.published) != 1 {
		t.Fatalf("published events = %d, want 1", len(outbox.published))
	}
	revoked, ok := outbox.published[0].(DeadlineRevoked)
	if !ok {
		t.Fatalf("published[0] type = %T, want DeadlineRevoked", outbox.published[0])
	}
	if revoked.Type() != TypeDeadlineRevoked || revoked.AggregateType() != aggregateTypeDeadline {
		t.Errorf("event type/aggregate = %q/%q", revoked.Type(), revoked.AggregateType())
	}
	if revoked.AggregateID() != deadlineID {
		t.Errorf("aggregate id = %q, want the deadline id %q", revoked.AggregateID(), deadlineID)
	}
	if _, err := uuid.Parse(revoked.AggregateID()); err != nil {
		t.Errorf("aggregate id is not a uuid: %v", err)
	}
	if revoked.DeadlineID != deadlineID || revoked.IntimationID != ev.IntimationID || revoked.Reason != ev.Reason {
		t.Errorf("revoked deadline/intimation/reason = %q/%q/%q, want %q/%q/%q",
			revoked.DeadlineID, revoked.IntimationID, revoked.Reason, deadlineID, ev.IntimationID, ev.Reason)
	}
}

// TestOnIntimationCancelled_NoDeadlineNoOp proves the safe bias: when there is no prazo to
// revoke — none was ever derived (the cancel raced ahead of the observe, or the intimação
// was dead on arrival) OR it is already CANCELLED — the repo returns ErrDeadlineNotFound
// and the use case no-ops: no error, no phantom deadline.revoked. Both collapse to the
// same path because the query's status <> CANCELLED guard touches no row in either case.
func TestOnIntimationCancelled_NoDeadlineNoOp(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "no prazo exists for the intimação"},
		{name: "prazo already CANCELLED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := cancelledFixture()
			repo := &mockRepo{revokeErr: ErrDeadlineNotFound}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnIntimationCancelled(context.Background(), ev); err != nil {
				t.Fatalf("OnIntimationCancelled() error = %v, want nil (safe no-op)", err)
			}
			if repo.revokeCalls != 1 {
				t.Errorf("RevokeDeadlineByIntimation calls = %d, want 1", repo.revokeCalls)
			}
			if len(outbox.published) != 0 {
				t.Errorf("published events = %d, want 0 (no phantom deadline.revoked)", len(outbox.published))
			}
		})
	}
}

// TestOnIntimationCancelled_Idempotent proves a replay (dedup reports seen) is a pure
// no-op: no revoke, no event — the dedup floor guards the revocation exactly as it guards
// the creation path.
func TestOnIntimationCancelled_Idempotent(t *testing.T) {
	ev := cancelledFixture()
	repo := &mockRepo{revokeResult: &RevokedDeadline{ID: uuid.NewString(), CourtRecordID: uuid.NewString()}}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnIntimationCancelled(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationCancelled() error = %v", err)
	}
	if repo.revokeCalls != 0 {
		t.Errorf("RevokeDeadlineByIntimation calls = %d, want 0 on a replay", repo.revokeCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
	// It still dedups under the slice-specific consumer name.
	if len(dedup.consumers) != 1 || dedup.consumers[0] != consumerDeadline {
		t.Errorf("dedup consumer = %v, want [%q]", dedup.consumers, consumerDeadline)
	}
}

// TestOnIntimationCancelled_InfraErrorPropagates proves an infra fault from the revoke
// aborts the tx (retryable) and emits nothing — only ErrDeadlineNotFound is a no-op.
func TestOnIntimationCancelled_InfraErrorPropagates(t *testing.T) {
	ev := cancelledFixture()
	repo := &mockRepo{revokeErr: apperr.NewInfra("db down", errors.New("boom"))}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	err := uc.OnIntimationCancelled(context.Background(), ev)
	if err == nil {
		t.Fatal("expected an error to propagate")
	}
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInfra {
		t.Errorf("error kind = %v, want KindInfra", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on error", len(outbox.published))
	}
}

// TestDecideCounting exercises the pure rito decision (P2): only a positive labor signal
// (a labor court sigla, or a class naming the rito) flips the rule's suggestion to
// CALENDAR; anything else keeps the conservative suggestion (viés seguro).
func TestDecideCounting(t *testing.T) {
	tests := []struct {
		name      string
		suggested Counting
		court     string
		class     string
		want      Counting
	}{
		{"cível keeps business", CountingBusiness, "TJSP", "Procedimento Comum", CountingBusiness},
		{"TRT court → calendar", CountingBusiness, "TRT2", "", CountingCalendar},
		{"TST court → calendar", CountingBusiness, "TST", "", CountingCalendar},
		{"class names trabalhista → calendar", CountingBusiness, "TJSP", "Ação Trabalhista", CountingCalendar},
		{"unknown court keeps suggestion", CountingBusiness, "TJRJ", "Cível", CountingBusiness},
		{"rule already calendar is kept", CountingCalendar, "TJSP", "", CountingCalendar},
		{"empty court keeps suggestion", CountingBusiness, "", "", CountingBusiness},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideCounting(tt.suggested, tt.court, tt.class); got != tt.want {
				t.Errorf("decideCounting(%q, %q, %q) = %q, want %q", tt.suggested, tt.court, tt.class, got, tt.want)
			}
		})
	}
}

// TestParseWireDate covers the anchor parse: the wire format succeeds, anything else is
// a terminal KindInvalid.
func TestParseWireDate(t *testing.T) {
	got, err := parseWireDate("2024-01-16")
	if err != nil {
		t.Fatalf("parseWireDate valid: %v", err)
	}
	if !got.Equal(time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parsed = %v, want 2024-01-16 UTC", got)
	}

	_, err = parseWireDate("16/01/2024")
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("parseWireDate invalid = %v, want KindInvalid", err)
	}
}

// reminderCheckFixture builds a fired reminder_check for a given prazo id/tenant/days_left.
// processAt is irrelevant on the fire path (the handler re-loads by id), so it stays zero.
func reminderCheckFixture(tenantID, deadlineID string, daysLeft int) DeadlineReminderCheck {
	return DeadlineReminderCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
		DaysLeft:   daysLeft,
	}
}

// TestOnReminderCheck_ReChecksStatus proves the re-check no disparo (decisão travada:
// due_soon avisa OPEN e PENDING, viés seguro): a fired reminder emits exactly one
// deadline.due_soon only when the prazo is still PENDING or OPEN, and is a pure no-op for
// any terminal status (a prazo cancelled/met/missed between birth and the mark never sends
// a stale lembrete).
func TestOnReminderCheck_ReChecksStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   Status
		wantEmit bool
	}{
		{"PENDING → due_soon", StatusPending, true},
		{"OPEN → due_soon", StatusOpen, true},
		{"CANCELLED → no-op", StatusCancelled, false},
		{"MET → no-op", StatusMet, false},
		{"MISSED → no-op", StatusMissed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deadlineID := uuid.NewString()
			ev := reminderCheckFixture(uuid.NewString(), deadlineID, 3)
			repo := &mockRepo{checkResult: &DeadlineForCheck{ID: deadlineID, Status: tt.status}}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnReminderCheck(context.Background(), ev); err != nil {
				t.Fatalf("OnReminderCheck() error = %v", err)
			}

			// The re-read was tenant-scoped to the event (barrier 1 args threaded).
			if repo.checkCalls != 1 || repo.gotCheckID != deadlineID || repo.gotCheckTenantID != ev.TenantID {
				t.Errorf("GetDeadlineForCheck calls/id/tenant = %d/%q/%q, want 1/%q/%q",
					repo.checkCalls, repo.gotCheckID, repo.gotCheckTenantID, deadlineID, ev.TenantID)
			}

			dueSoon := publishedOfType[DeadlineDueSoon](outbox)
			if !tt.wantEmit {
				if len(dueSoon) != 0 {
					t.Fatalf("due_soon events = %d, want 0 (obsolete mark)", len(dueSoon))
				}
				return
			}
			if len(dueSoon) != 1 {
				t.Fatalf("due_soon events = %d, want 1", len(dueSoon))
			}
			d := dueSoon[0]
			if d.Type() != TypeDeadlineDueSoon || d.AggregateType() != aggregateTypeDeadline {
				t.Errorf("due_soon type/aggregate = %q/%q", d.Type(), d.AggregateType())
			}
			if d.AggregateID() != deadlineID {
				t.Errorf("due_soon aggregate id = %q, want %q", d.AggregateID(), deadlineID)
			}
			if _, err := uuid.Parse(d.AggregateID()); err != nil {
				t.Errorf("due_soon aggregate id is not a uuid: %v", err)
			}
			if d.DeadlineID != deadlineID || d.DaysLeft != ev.DaysLeft || d.TenantID != ev.TenantID {
				t.Errorf("due_soon deadline/days/tenant = %q/%d/%q, want %q/%d/%q",
					d.DeadlineID, d.DaysLeft, d.TenantID, deadlineID, ev.DaysLeft, ev.TenantID)
			}
		})
	}
}

// TestOnReminderCheck_Idempotent proves a replay (dedup reports seen) is a pure no-op: no
// re-read, no lembrete — the dedup floor guards the fire path as it guards creation.
func TestOnReminderCheck_Idempotent(t *testing.T) {
	ev := reminderCheckFixture(uuid.NewString(), uuid.NewString(), 1)
	repo := &mockRepo{checkResult: &DeadlineForCheck{ID: ev.DeadlineID, Status: StatusOpen}}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnReminderCheck(context.Background(), ev); err != nil {
		t.Fatalf("OnReminderCheck() error = %v", err)
	}
	if repo.checkCalls != 0 {
		t.Errorf("GetDeadlineForCheck calls = %d, want 0 on a replay", repo.checkCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
	if len(dedup.consumers) != 1 || dedup.consumers[0] != consumerDeadline {
		t.Errorf("dedup consumer = %v, want [%q]", dedup.consumers, consumerDeadline)
	}
}

// TestOnReminderCheck_DeadlineGoneIsTerminal proves a re-read that finds no prazo surfaces
// the typed not-found (KindNotFound → the listener SkipRetries it), emitting nothing.
func TestOnReminderCheck_DeadlineGoneIsTerminal(t *testing.T) {
	ev := reminderCheckFixture(uuid.NewString(), uuid.NewString(), 0)
	repo := &mockRepo{checkErr: ErrDeadlineNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	err := uc.OnReminderCheck(context.Background(), ev)
	aerr, ok := apperr.From(err)
	if !ok || aerr.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on error", len(outbox.published))
	}
}

// TestOnMissedCheck_MarksMissed proves the auto-miss (decisão travada: MISSED auto D+1, SÓ
// em OPEN): when the guarded UPDATE flips a still-OPEN, overdue prazo it emits exactly one
// deadline.missed; when the guard touches no row (PENDING, terminal, or not yet overdue —
// all collapsed to ErrDeadlineNotFound) it is a pure no-op.
func TestOnMissedCheck_MarksMissed(t *testing.T) {
	tests := []struct {
		name     string
		markID   string
		markErr  error
		wantEmit bool
	}{
		{"OPEN & overdue → MISSED + event", "", nil, true},
		{"not OPEN / not overdue → no-op", "", ErrDeadlineNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deadlineID := uuid.NewString()
			ev := DeadlineMissedCheck{
				Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
				TenantID:   uuid.NewString(),
				DeadlineID: deadlineID,
			}
			repo := &mockRepo{markMissedErr: tt.markErr}
			if tt.markErr == nil {
				repo.markMissedID = deadlineID
			}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnMissedCheck(context.Background(), ev); err != nil {
				t.Fatalf("OnMissedCheck() error = %v", err)
			}

			// The flip was tenant-scoped to the event (barrier 1 args threaded).
			if repo.markMissedCalls != 1 || repo.gotMissedID != deadlineID || repo.gotMissedTenantID != ev.TenantID {
				t.Errorf("MarkMissed calls/id/tenant = %d/%q/%q, want 1/%q/%q",
					repo.markMissedCalls, repo.gotMissedID, repo.gotMissedTenantID, deadlineID, ev.TenantID)
			}

			missed := publishedOfType[DeadlineMissed](outbox)
			if !tt.wantEmit {
				if len(missed) != 0 {
					t.Fatalf("deadline.missed events = %d, want 0 (safe no-op)", len(missed))
				}
				return
			}
			if len(missed) != 1 {
				t.Fatalf("deadline.missed events = %d, want 1", len(missed))
			}
			m := missed[0]
			if m.Type() != TypeDeadlineMissed || m.AggregateType() != aggregateTypeDeadline {
				t.Errorf("missed type/aggregate = %q/%q", m.Type(), m.AggregateType())
			}
			if m.AggregateID() != deadlineID {
				t.Errorf("missed aggregate id = %q, want %q", m.AggregateID(), deadlineID)
			}
			if _, err := uuid.Parse(m.AggregateID()); err != nil {
				t.Errorf("missed aggregate id is not a uuid: %v", err)
			}
			if m.DeadlineID != deadlineID || m.TenantID != ev.TenantID {
				t.Errorf("missed deadline/tenant = %q/%q, want %q/%q", m.DeadlineID, m.TenantID, deadlineID, ev.TenantID)
			}
		})
	}
}

// TestOnMissedCheck_Idempotent proves a replay is a pure no-op: no flip, no event.
func TestOnMissedCheck_Idempotent(t *testing.T) {
	deadlineID := uuid.NewString()
	ev := DeadlineMissedCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   uuid.NewString(),
		DeadlineID: deadlineID,
	}
	repo := &mockRepo{markMissedID: deadlineID}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnMissedCheck(context.Background(), ev); err != nil {
		t.Fatalf("OnMissedCheck() error = %v", err)
	}
	if repo.markMissedCalls != 0 {
		t.Errorf("MarkMissed calls = %d, want 0 on a replay", repo.markMissedCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
}

// TestOnMissedCheck_InfraErrorPropagates proves an infra fault from the flip aborts the tx
// (retryable) and emits nothing — only ErrDeadlineNotFound is a no-op.
func TestOnMissedCheck_InfraErrorPropagates(t *testing.T) {
	deadlineID := uuid.NewString()
	ev := DeadlineMissedCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   uuid.NewString(),
		DeadlineID: deadlineID,
	}
	repo := &mockRepo{markMissedErr: apperr.NewInfra("db down", errors.New("boom"))}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	err := uc.OnMissedCheck(context.Background(), ev)
	aerr, ok := apperr.From(err)
	if !ok || aerr.Kind != apperr.KindInfra {
		t.Errorf("error = %v, want KindInfra", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on error", len(outbox.published))
	}
}
