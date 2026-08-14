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

// confirmCmd is a well-formed CONTESTACAO/15/BUSINESS confirmation; tests override the fields
// they exercise. The confirm carries no tasks — those are managed via POST/PATCH /v1/tasks.
func confirmCmd(p confirmParents) ConfirmCommand {
	return ConfirmCommand{
		TenantID:     p.tenantID,
		UserID:       p.userID,
		IntimationID: p.intimationID,
		Kind:         KindContestacao,
		Days:         15,
		Counting:     CountingBusiness,
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

// TestConfirm_DoesNotTouchTasks is the bug's regression floor: the confirm no longer owns the
// task lifecycle (tasks are created via POST /v1/tasks, the "Análise" section), so a confirm
// must NEVER create — nor delete — tasks. Even re-confirming the same intimação leaves the
// prazo's tasks untouched. It emits only deadline.updated (plus, when applicable, the feedback
// delta), never a task.created.
func TestConfirm_DoesNotTouchTasks(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)
	repo := confirmRepo(p, start, "TJSP")
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	// Confirm the same intimação twice — the prazo already has tasks (created via the Análise
	// section); the confirm must not create or drop any of them.
	for i := 0; i < 2; i++ {
		res, err := uc.Confirm(context.Background(), confirmCmd(p))
		if err != nil {
			t.Fatalf("Confirm() #%d error = %v", i, err)
		}
		if res.Deadline.Status != StatusOpen {
			t.Errorf("result deadline status = %q, want OPEN", res.Deadline.Status)
		}
	}

	if repo.insertTaskCalls != 0 {
		t.Errorf("InsertTask calls = %d, want 0 (confirm never creates tasks)", repo.insertTaskCalls)
	}
	if got := len(publishedOfType[TaskCreated](outbox)); got != 0 {
		t.Errorf("task.created events = %d, want 0 (confirm never creates tasks)", got)
	}
	// Only deadline.updated is emitted (one per confirm); no suggestion on record → no feedback.
	if got := len(publishedOfType[DeadlineUpdated](outbox)); got != 2 {
		t.Errorf("deadline.updated events = %d, want 2 (one per confirm)", got)
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
	for i := 0; i < 2; i++ {
		if _, err := uc.Confirm(context.Background(), cmd); err != nil {
			t.Fatalf("Confirm() #%d error = %v", i, err)
		}
	}
	if repo.confirmCalls != 2 || repo.insertCalls != 0 {
		t.Errorf("confirm/insertDeadline calls = %d/%d, want 2/0 (re-confirm re-UPDATEs, never inserts)", repo.confirmCalls, repo.insertCalls)
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

// TestConfirm_EmitsSuggestionFeedbackDelta proves the feedback loop's camada 2 AFTER the
// redesign: the delta is measured against the tasks that REALLY exist for the prazo (read via
// ListTaskTitlesByDeadline — the Análise section created them via POST /v1/tasks), NOT against
// the confirm body (which now carries none). The lawyer approved 2 of the 3 suggested tasks
// and added one of their own. Suggested {Protocolar contestação, Analisar sentença, Juntar
// procuração}; real tasks {Protocolar contestação, Analisar sentença, Falar com cliente} →
// kept 2, removed 1, added 1. The read is keyed by the confirmed deadline id, scoped to tenant.
func TestConfirm_EmitsSuggestionFeedbackDelta(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, start, "TJSP")
	repo.latestSuggestionOK = true
	repo.latestSuggestion = SuggestionRecord{
		PromptVersion: "suggest_tasks/v1",
		Model:         "openai/gpt-4o-mini",
		Tasks: []SuggestedTask{
			{Title: "Protocolar contestação", Kind: "PECA"},
			{Title: "Analisar sentença", Kind: "ANALISE"},
			{Title: "Juntar procuração", Kind: "PROVIDENCIA"},
		},
	}
	// The tasks that really exist for the prazo (created via the Análise section): 2 of the
	// suggested ones were kept, plus one the lawyer wrote.
	repo.taskTitles = []string{"Protocolar contestação", "Analisar sentença", "Falar com cliente"}
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	if _, err := uc.Confirm(context.Background(), confirmCmd(p)); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if repo.latestSuggestionCalls != 1 {
		t.Fatalf("GetLatestSuggestion calls = %d, want 1", repo.latestSuggestionCalls)
	}
	// The confirmed set came from the real tasks, keyed by the deadline id and scoped to tenant.
	if repo.taskTitlesCalls != 1 {
		t.Fatalf("ListTaskTitlesByDeadline calls = %d, want 1", repo.taskTitlesCalls)
	}
	if repo.gotTitlesDeadlineID != p.deadlineID || repo.gotTitlesTenantID != p.tenantID {
		t.Errorf("titles read keyed by deadline/tenant = %q/%q, want %q/%q",
			repo.gotTitlesDeadlineID, repo.gotTitlesTenantID, p.deadlineID, p.tenantID)
	}

	fb := publishedOfType[SuggestionFeedback](outbox)
	if len(fb) != 1 {
		t.Fatalf("suggestion.feedback events = %d, want 1", len(fb))
	}
	f := fb[0]
	if f.Type() != TypeSuggestionFeedback || f.AggregateType() != aggregateTypeDeadline || f.AggregateID() != p.deadlineID {
		t.Errorf("feedback type/aggregate = %q/%q/%q", f.Type(), f.AggregateType(), f.AggregateID())
	}
	if f.PromptVersion != "suggest_tasks/v1" || f.Model != "openai/gpt-4o-mini" {
		t.Errorf("feedback provenance = %q/%q", f.PromptVersion, f.Model)
	}
	if f.SuggestedCount != 3 || f.ConfirmedCount != 3 {
		t.Errorf("feedback counts suggested/confirmed = %d/%d, want 3/3", f.SuggestedCount, f.ConfirmedCount)
	}
	if len(f.Kept) != 2 || f.Kept[0] != "Protocolar contestação" || f.Kept[1] != "Analisar sentença" {
		t.Errorf("kept = %v, want [Protocolar contestação, Analisar sentença]", f.Kept)
	}
	if len(f.Removed) != 1 || f.Removed[0] != "Juntar procuração" {
		t.Errorf("removed = %v, want [Juntar procuração]", f.Removed)
	}
	if len(f.Added) != 1 || f.Added[0] != "Falar com cliente" {
		t.Errorf("added = %v, want [Falar com cliente]", f.Added)
	}
}

// TestConfirm_SuggestionButNoTasks proves the "no tasks associated" edge (TDD): a prazo with
// an AI suggestion but confirmed before the lawyer created any task via the Análise section.
// The confirm still succeeds; the delta reports every suggestion as removed (0 kept, 0 added).
func TestConfirm_SuggestionButNoTasks(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, start, "TJSP")
	repo.latestSuggestionOK = true
	repo.latestSuggestion = SuggestionRecord{
		PromptVersion: "suggest_tasks/v1",
		Model:         "openai/gpt-4o-mini",
		Tasks:         []SuggestedTask{{Title: "Protocolar contestação", Kind: "PECA"}},
	}
	repo.taskTitles = nil // no tasks created yet
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{endDate: end}, outbox, &fakeDedup{}, &fakeUOW{})

	res, err := uc.Confirm(context.Background(), confirmCmd(p))
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if res.Deadline.Status != StatusOpen {
		t.Errorf("result deadline status = %q, want OPEN", res.Deadline.Status)
	}

	fb := publishedOfType[SuggestionFeedback](outbox)
	if len(fb) != 1 {
		t.Fatalf("suggestion.feedback events = %d, want 1", len(fb))
	}
	f := fb[0]
	if f.ConfirmedCount != 0 || len(f.Kept) != 0 || len(f.Added) != 0 {
		t.Errorf("with no real tasks: confirmed/kept/added = %d/%d/%d, want 0/0/0", f.ConfirmedCount, len(f.Kept), len(f.Added))
	}
	if len(f.Removed) != 1 || f.Removed[0] != "Protocolar contestação" {
		t.Errorf("removed = %v, want [Protocolar contestação] (all suggestions removed)", f.Removed)
	}
}

// TestConfirm_NoSuggestion_NoFeedback: a prazo the lawyer never asked the IA about (or one
// confirmed before the suggester existed) emits NO feedback event — the confirm is unaffected.
func TestConfirm_NoSuggestion_NoFeedback(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, start, "TJSP")
	repo.latestSuggestionOK = false // no suggestion on record
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	if _, err := uc.Confirm(context.Background(), confirmCmd(p)); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if repo.latestSuggestionCalls != 1 {
		t.Fatalf("GetLatestSuggestion calls = %d, want 1 (always checked)", repo.latestSuggestionCalls)
	}
	// No suggestion → the confirmed tasks are never read (the delta would have no baseline).
	if repo.taskTitlesCalls != 0 {
		t.Errorf("ListTaskTitlesByDeadline calls = %d, want 0 (skipped when no suggestion)", repo.taskTitlesCalls)
	}
	if fb := publishedOfType[SuggestionFeedback](outbox); len(fb) != 0 {
		t.Errorf("suggestion.feedback events = %d, want 0 (no suggestion)", len(fb))
	}
}
