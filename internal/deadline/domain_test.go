package deadline

import (
	"context"
	"errors"
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

	// captured inputs
	gotClassTenantID string
	gotClassRecordID string
	gotRuleType      string
	gotRuleCourt     string
	gotRuleVersion   string
	inserted         *Deadline
	insertCalls      int
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
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

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

	// Exactly one deadline.opened, aggregate = the new deadline id (a parseable uuid).
	if len(outbox.published) != 1 {
		t.Fatalf("published events = %d, want 1", len(outbox.published))
	}
	opened, ok := outbox.published[0].(DeadlineOpened)
	if !ok {
		t.Fatalf("published[0] type = %T, want DeadlineOpened", outbox.published[0])
	}
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
