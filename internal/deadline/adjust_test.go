package deadline

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
)

// --- fixtures ---------------------------------------------------------------

// ptr is a tiny helper to take the address of a literal — the AdjustCommand's pointer fields
// (present-vs-absent) need addresses of ints/strings/bools/Counting in the tests.
func ptr[T any](v T) *T { return &v }

// adjustParents are the ids an ajuste anchors on plus the record it hangs on.
type adjustParents struct {
	tenantID      string
	userID        string
	deadlineID    string
	courtRecordID string
}

func newAdjustParents() adjustParents {
	return adjustParents{
		tenantID:      uuid.NewString(),
		userID:        uuid.NewString(),
		deadlineID:    uuid.NewString(),
		courtRecordID: uuid.NewString(),
	}
}

// storedDeadline is the current (PENDING/OPEN) state the ajuste reads and merges the patch
// over: MANIFESTACAO / 5 / BUSINESS / not doubled, anchored on the given start.
func storedDeadline(p adjustParents, start time.Time, status Status) *DeadlineForAdjust {
	return &DeadlineForAdjust{
		ID:            p.deadlineID,
		CourtRecordID: p.courtRecordID,
		StartDate:     start,
		Status:        status,
		Kind:          KindManifestacao,
		Days:          5,
		Counting:      CountingBusiness,
		Doubled:       false,
		DoubledReason: "",
	}
}

// adjustRepo primes a mockRepo for the ajuste path: the load returns the stored state,
// GetCourtRecordCourt returns court, and UpdateDeadlineAdjust echoes the deadline/record ids.
func adjustRepo(p adjustParents, cur *DeadlineForAdjust, court string) *mockRepo {
	return &mockRepo{
		adjustResult:       cur,
		courtRecordCourt:   court,
		updateAdjustID:     p.deadlineID,
		updateAdjustRecord: p.courtRecordID,
	}
}

// --- Adjust -----------------------------------------------------------------

// TestAdjust_AppliesOnlyPresentFields is the core of the partial patch: a body with ONLY days
// leaves kind/counting/doubled at their stored values, recomputes from the FIXED start_date
// with the new day count, flips no lifecycle, and emits deadline.updated.
func TestAdjust_AppliesOnlyPresentFields(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 18, 0, 0, 0, 0, time.UTC)
	holiday := time.Date(2024, 3, 6, 0, 0, 0, 0, time.UTC)

	repo := adjustRepo(p, storedDeadline(p, start, StatusOpen), "TJSP")
	cal := &fakeCalendar{endDate: end, holidays: []time.Time{holiday}}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, uow)

	cmd := AdjustCommand{
		TenantID:   p.tenantID,
		UserID:     p.userID,
		DeadlineID: p.deadlineID,
		Days:       ptr(10), // ONLY days present
	}
	res, err := uc.Adjust(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}

	// Tenant-scoped tx (barrier 1 + RLS); the load + court read were tenant-scoped too.
	if len(uow.scopes) != 1 || uow.scopes[0] != p.tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, p.tenantID)
	}
	if repo.gotAdjustID != p.deadlineID || repo.gotAdjustTenantID != p.tenantID {
		t.Errorf("GetDeadlineForAdjust id/tenant = %q/%q, want %q/%q", repo.gotAdjustID, repo.gotAdjustTenantID, p.deadlineID, p.tenantID)
	}

	// The recompute ran the dias-úteis motor from the fixed start with the NEW day count.
	if cal.businessCalls != 1 || cal.calendarCalls != 0 {
		t.Errorf("calendar calls business=%d calendar=%d, want 1/0", cal.businessCalls, cal.calendarCalls)
	}
	if !cal.gotStart.Equal(start) || cal.gotN != 10 || cal.gotUF != "SP" || cal.gotCourt != "TJSP" {
		t.Errorf("calendar start/n/uf/court = %v/%d/%q/%q, want %v/10/SP/TJSP", cal.gotStart, cal.gotN, cal.gotUF, cal.gotCourt, start)
	}

	// The UPDATE carried the MERGED fields: days overridden, the rest kept from storage.
	up := repo.gotUpdateAdjustParams
	if repo.updateAdjustCalls != 1 {
		t.Fatalf("UpdateDeadlineAdjust calls = %d, want 1", repo.updateAdjustCalls)
	}
	if up.DeadlineID != p.deadlineID || up.TenantID != p.tenantID {
		t.Errorf("update keyed by deadline/tenant = %q/%q, want %q/%q", up.DeadlineID, up.TenantID, p.deadlineID, p.tenantID)
	}
	if up.Kind != KindManifestacao || up.Days != 10 || up.Counting != CountingBusiness || up.Doubled {
		t.Errorf("merged kind/days/counting/doubled = %q/%d/%q/%v, want MANIFESTACAO/10/BUSINESS/false (only days patched)", up.Kind, up.Days, up.Counting, up.Doubled)
	}
	if !up.EndDate.Equal(end) || len(up.HolidaysApplied) != 1 || !up.HolidaysApplied[0].Equal(holiday) {
		t.Errorf("update end/holidays = %v/%v, want %v/[%v]", up.EndDate, up.HolidaysApplied, end, holiday)
	}

	// The result mirrors the recompute; status is unchanged (OPEN stays OPEN).
	if res.Status != StatusOpen || res.Days != 10 || !res.EndDate.Equal(end) || !res.StartDate.Equal(start) {
		t.Errorf("result = %+v, want OPEN/days10/end %v/start %v", res, end, start)
	}

	// Exactly one deadline.updated, aggregate = the deadline id (a parseable uuid).
	updated := publishedOfType[DeadlineUpdated](outbox)
	if len(updated) != 1 {
		t.Fatalf("deadline.updated events = %d, want 1", len(updated))
	}
	u := updated[0]
	if u.Type() != TypeDeadlineUpdated || u.AggregateType() != aggregateTypeDeadline || u.AggregateID() != p.deadlineID {
		t.Errorf("deadline.updated type/aggregate = %q/%q/%q", u.Type(), u.AggregateType(), u.AggregateID())
	}
	if _, err := uuid.Parse(u.AggregateID()); err != nil {
		t.Errorf("aggregate is not a uuid: %v", err)
	}
	if u.Kind != KindManifestacao || u.EndDate != "2024-03-18" || u.Counting != "BUSINESS" || u.Status != "OPEN" {
		t.Errorf("deadline.updated payload = %+v", u)
	}
}

// TestAdjust_CountingChangeRoutesCalendarMotor proves a patch flipping counting to CALENDAR
// routes the recompute through AddCalendarDays (dias corridos), honoring the human's toggle.
func TestAdjust_CountingChangeRoutesCalendarMotor(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	repo := adjustRepo(p, storedDeadline(p, start, StatusPending), "TJSP")
	cal := &fakeCalendar{}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := AdjustCommand{
		TenantID:   p.tenantID,
		DeadlineID: p.deadlineID,
		Counting:   ptr(CountingCalendar),
	}
	if _, err := uc.Adjust(context.Background(), cmd); err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}
	if cal.calendarCalls != 1 || cal.businessCalls != 0 {
		t.Errorf("calendar calls calendar=%d business=%d, want 1/0", cal.calendarCalls, cal.businessCalls)
	}
	if repo.gotUpdateAdjustParams.Counting != CountingCalendar {
		t.Errorf("merged counting = %q, want CALENDAR", repo.gotUpdateAdjustParams.Counting)
	}
	// A PENDING prazo stays PENDING after the ajuste.
	if repo.gotUpdateAdjustParams.Days != 5 {
		t.Errorf("days = %d, want 5 (kept)", repo.gotUpdateAdjustParams.Days)
	}
}

// TestAdjust_DoubledDoublesRawDays proves the dobro semantics on the ajuste path: a patch
// setting doubled feeds 2×days to the calendar motor (raw count doubled BEFORE the math),
// while days stores the base the human approved and doubled_reason is written.
func TestAdjust_DoubledDoublesRawDays(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	repo := adjustRepo(p, storedDeadline(p, start, StatusOpen), "TJSP")
	cal := &fakeCalendar{}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := AdjustCommand{
		TenantID:      p.tenantID,
		DeadlineID:    p.deadlineID,
		Days:          ptr(15),
		Doubled:       ptr(true),
		DoubledReason: ptr("FAZENDA_183"),
	}
	if _, err := uc.Adjust(context.Background(), cmd); err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}
	if cal.gotN != 30 {
		t.Errorf("calendar n = %d, want 30 (doubled 2×15)", cal.gotN)
	}
	up := repo.gotUpdateAdjustParams
	if up.Days != 15 || !up.Doubled || up.DoubledReason != "FAZENDA_183" {
		t.Errorf("update days/doubled/reason = %d/%v/%q, want 15/true/FAZENDA_183", up.Days, up.Doubled, up.DoubledReason)
	}
}

// TestAdjust_RejectsTerminalStatus proves the ajuste is gated on an ACTIVE prazo: a
// MET/MISSED/CANCELLED prazo is not adjustable (ErrDeadlineNotAdjustable → 409), and nothing
// is recomputed, updated, or emitted.
func TestAdjust_RejectsTerminalStatus(t *testing.T) {
	for _, status := range []Status{StatusMet, StatusMissed, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			p := newAdjustParents()
			start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
			repo := adjustRepo(p, storedDeadline(p, start, status), "TJSP")
			cal := &fakeCalendar{}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

			_, err := uc.Adjust(context.Background(), AdjustCommand{
				TenantID: p.tenantID, DeadlineID: p.deadlineID, Days: ptr(10),
			})
			ae, ok := apperr.From(err)
			if !ok || ae.Kind != apperr.KindConflict {
				t.Errorf("error = %v, want KindConflict (not adjustable)", err)
			}
			if repo.updateAdjustCalls != 0 || cal.businessCalls != 0 || cal.calendarCalls != 0 || len(outbox.published) != 0 {
				t.Errorf("update/compute/published ran on a terminal prazo: %d/%d/%d/%d",
					repo.updateAdjustCalls, cal.businessCalls, cal.calendarCalls, len(outbox.published))
			}
		})
	}
}

// TestAdjust_NotFound proves adjusting an unknown/foreign prazo is the repo's typed
// ErrDeadlineNotFound (→ 404): nothing is recomputed, updated, or emitted.
func TestAdjust_NotFound(t *testing.T) {
	p := newAdjustParents()
	repo := &mockRepo{adjustErr: ErrDeadlineNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	_, err := uc.Adjust(context.Background(), AdjustCommand{TenantID: p.tenantID, DeadlineID: p.deadlineID, Days: ptr(10)})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if repo.updateAdjustCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("update/published = %d/%d, want 0/0 on not-found", repo.updateAdjustCalls, len(outbox.published))
	}
}

// --- MarkMet / MarkMissed ---------------------------------------------------

// TestMarkMet_OpenToMet is the happy path for marcar cumprido: a still-OPEN prazo flips to
// MET (guarded OPEN→MET), and exactly one deadline.met is emitted with the deadline id as a
// parseable uuid aggregate + the tenant.
func TestMarkMet_OpenToMet(t *testing.T) {
	tenantID := uuid.NewString()
	deadlineID := uuid.NewString()
	repo := &mockRepo{
		checkResult:  &DeadlineForCheck{ID: deadlineID, Status: StatusOpen},
		markStatusID: deadlineID,
	}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, uow)

	res, err := uc.MarkMet(context.Background(), tenantID, deadlineID)
	if err != nil {
		t.Fatalf("MarkMet() error = %v", err)
	}
	if res.ID != deadlineID || res.Status != StatusMet {
		t.Errorf("result = %+v, want %q/MET", res, deadlineID)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	// The flip was guarded OPEN→MET, tenant-scoped.
	if repo.markStatusCalls != 1 || repo.gotMarkStatusFrom != StatusOpen || repo.gotMarkStatusTo != StatusMet {
		t.Errorf("MarkDeadlineStatus calls/from/to = %d/%q/%q, want 1/OPEN/MET", repo.markStatusCalls, repo.gotMarkStatusFrom, repo.gotMarkStatusTo)
	}
	if repo.gotMarkStatusID != deadlineID || repo.gotMarkStatusTenantID != tenantID {
		t.Errorf("flip id/tenant = %q/%q, want %q/%q", repo.gotMarkStatusID, repo.gotMarkStatusTenantID, deadlineID, tenantID)
	}

	met := publishedOfType[DeadlineMet](outbox)
	if len(met) != 1 {
		t.Fatalf("deadline.met events = %d, want 1", len(met))
	}
	m := met[0]
	if m.Type() != TypeDeadlineMet || m.AggregateType() != aggregateTypeDeadline || m.AggregateID() != deadlineID {
		t.Errorf("deadline.met type/aggregate = %q/%q/%q", m.Type(), m.AggregateType(), m.AggregateID())
	}
	if _, err := uuid.Parse(m.AggregateID()); err != nil {
		t.Errorf("aggregate is not a uuid: %v", err)
	}
	if m.DeadlineID != deadlineID || m.TenantID != tenantID {
		t.Errorf("deadline.met deadline/tenant = %q/%q, want %q/%q", m.DeadlineID, m.TenantID, deadlineID, tenantID)
	}
}

// TestMarkMissed_OpenToMissed is the manual marcar perdido: a still-OPEN prazo flips to MISSED
// (OPEN→MISSED) emitting exactly one deadline.missed — the SAME event the D+1 carência
// auto-miss emits (4b-ii), so there is no parallel type.
func TestMarkMissed_OpenToMissed(t *testing.T) {
	tenantID := uuid.NewString()
	deadlineID := uuid.NewString()
	repo := &mockRepo{
		checkResult:  &DeadlineForCheck{ID: deadlineID, Status: StatusOpen},
		markStatusID: deadlineID,
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	res, err := uc.MarkMissed(context.Background(), tenantID, deadlineID)
	if err != nil {
		t.Fatalf("MarkMissed() error = %v", err)
	}
	if res.Status != StatusMissed {
		t.Errorf("result status = %q, want MISSED", res.Status)
	}
	if repo.gotMarkStatusFrom != StatusOpen || repo.gotMarkStatusTo != StatusMissed {
		t.Errorf("flip from/to = %q/%q, want OPEN/MISSED", repo.gotMarkStatusFrom, repo.gotMarkStatusTo)
	}
	missed := publishedOfType[DeadlineMissed](outbox)
	if len(missed) != 1 {
		t.Fatalf("deadline.missed events = %d, want 1", len(missed))
	}
	if missed[0].DeadlineID != deadlineID || missed[0].TenantID != tenantID {
		t.Errorf("deadline.missed deadline/tenant = %q/%q, want %q/%q", missed[0].DeadlineID, missed[0].TenantID, deadlineID, tenantID)
	}
}

// TestMark_RequiresOpen proves both manual transitions are OPEN-only: from any non-OPEN status
// they return ErrDeadlineNotOpen (→ 409) without flipping or emitting — a PENDING suggestion
// must be confirmed first, a terminal prazo cannot transition again.
func TestMark_RequiresOpen(t *testing.T) {
	transitions := []struct {
		name string
		call func(uc *UseCase, tenantID, deadlineID string) (MarkedDeadline, error)
	}{
		{"met", func(uc *UseCase, tenantID, deadlineID string) (MarkedDeadline, error) {
			return uc.MarkMet(context.Background(), tenantID, deadlineID)
		}},
		{"missed", func(uc *UseCase, tenantID, deadlineID string) (MarkedDeadline, error) {
			return uc.MarkMissed(context.Background(), tenantID, deadlineID)
		}},
	}
	for _, tr := range transitions {
		for _, status := range []Status{StatusPending, StatusMet, StatusMissed, StatusCancelled} {
			t.Run(tr.name+"/"+string(status), func(t *testing.T) {
				tenantID := uuid.NewString()
				deadlineID := uuid.NewString()
				repo := &mockRepo{checkResult: &DeadlineForCheck{ID: deadlineID, Status: status}}
				outbox := &fakeOutbox{}
				uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

				_, err := tr.call(uc, tenantID, deadlineID)
				ae, ok := apperr.From(err)
				if !ok || ae.Kind != apperr.KindConflict {
					t.Errorf("error = %v, want KindConflict (not open)", err)
				}
				if repo.markStatusCalls != 0 || len(outbox.published) != 0 {
					t.Errorf("flip/published ran on a non-OPEN prazo: %d/%d", repo.markStatusCalls, len(outbox.published))
				}
			})
		}
	}
}

// TestMark_NotFound proves marking an unknown/foreign prazo is the typed ErrDeadlineNotFound
// (→ 404): the status re-read misses, so nothing is flipped or emitted.
func TestMark_NotFound(t *testing.T) {
	transitions := []struct {
		name string
		call func(uc *UseCase, tenantID, deadlineID string) (MarkedDeadline, error)
	}{
		{"met", func(uc *UseCase, tenantID, deadlineID string) (MarkedDeadline, error) {
			return uc.MarkMet(context.Background(), tenantID, deadlineID)
		}},
		{"missed", func(uc *UseCase, tenantID, deadlineID string) (MarkedDeadline, error) {
			return uc.MarkMissed(context.Background(), tenantID, deadlineID)
		}},
	}
	for _, tr := range transitions {
		t.Run(tr.name, func(t *testing.T) {
			repo := &mockRepo{checkErr: ErrDeadlineNotFound}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			_, err := tr.call(uc, uuid.NewString(), uuid.NewString())
			ae, ok := apperr.From(err)
			if !ok || ae.Kind != apperr.KindNotFound {
				t.Errorf("error = %v, want KindNotFound", err)
			}
			if repo.markStatusCalls != 0 || len(outbox.published) != 0 {
				t.Errorf("flip/published = %d/%d, want 0/0 on not-found", repo.markStatusCalls, len(outbox.published))
			}
		})
	}
}
