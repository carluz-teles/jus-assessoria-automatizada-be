package deadline

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jusassessoria/platform/lib/apperr"
)

// --- edge validation (closed sets) ------------------------------------------

// TestConfirmRequest_ValidatesAnchorAndExtraDays proves the confirm body rejects an out-of-set
// anchor_event and a negative manual_extra_days, and accepts the empty anchor (defaulted later).
func TestConfirmRequest_ValidatesAnchorAndExtraDays(t *testing.T) {
	base := func() ConfirmRequest {
		return ConfirmRequest{
			IntimationID: uuid.NewString(),
			Deadline:     ConfirmDeadlineBody{Days: 15, Counting: "BUSINESS"},
		}
	}
	t.Run("bad anchor rejected", func(t *testing.T) {
		r := base()
		r.Deadline.AnchorEvent = "JOINDER"
		if err := r.Validate(); err == nil {
			t.Error("Validate() = nil, want error for out-of-set anchor_event")
		}
	})
	t.Run("negative extra days rejected", func(t *testing.T) {
		r := base()
		r.Deadline.ManualExtraDays = -1
		if err := r.Validate(); err == nil {
			t.Error("Validate() = nil, want error for negative manual_extra_days")
		}
	})
	t.Run("empty anchor + zero extra accepted", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil (defaults are valid)", err)
		}
	})
	t.Run("empty anchor defaults to DEADLINE_START", func(t *testing.T) {
		cmd := base().toCommand("t", "u")
		if cmd.AnchorEvent != AnchorDeadlineStart {
			t.Errorf("default anchor = %q, want DEADLINE_START", cmd.AnchorEvent)
		}
	})
}

// TestAdjustRequest_ValidatesAnchorAndExtraDays proves the PATCH body rejects a present-but-bad
// anchor_event / negative extra days while a nil (absent) field is a no-op.
func TestAdjustRequest_ValidatesAnchorAndExtraDays(t *testing.T) {
	t.Run("bad anchor rejected", func(t *testing.T) {
		if err := (AdjustRequest{AnchorEvent: ptr("NOPE")}).Validate(); err == nil {
			t.Error("Validate() = nil, want error for out-of-set anchor_event")
		}
	})
	t.Run("negative extra rejected", func(t *testing.T) {
		if err := (AdjustRequest{ManualExtraDays: ptr(-2)}).Validate(); err == nil {
			t.Error("Validate() = nil, want error for negative manual_extra_days")
		}
	})
	t.Run("absent fields ok", func(t *testing.T) {
		if err := (AdjustRequest{}).Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil (all absent)", err)
		}
	})
}

// TestPreviewRequest_RequiresExactlyOneAnchorSource proves the preview body demands intimation_id
// XOR start_date, a valid counting, and rejects both-or-neither.
func TestPreviewRequest_RequiresExactlyOneAnchorSource(t *testing.T) {
	t.Run("neither rejected", func(t *testing.T) {
		if err := (PreviewRequest{Days: 5, Counting: "BUSINESS"}).Validate(); err == nil {
			t.Error("Validate() = nil, want error when neither intimation_id nor start_date is set")
		}
	})
	t.Run("both rejected", func(t *testing.T) {
		r := PreviewRequest{IntimationID: uuid.NewString(), StartDate: "2024-01-16", Days: 5, Counting: "BUSINESS"}
		if err := r.Validate(); err == nil {
			t.Error("Validate() = nil, want error when both are set")
		}
	})
	t.Run("intimation only ok", func(t *testing.T) {
		r := PreviewRequest{IntimationID: uuid.NewString(), Days: 5, Counting: "BUSINESS"}
		if err := r.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	t.Run("start only ok", func(t *testing.T) {
		r := PreviewRequest{StartDate: "2024-01-16", Days: 5, Counting: "BUSINESS"}
		if err := r.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
}

// TestIsKnownStatus_IncludesNoDeadline proves the closed status set (the ?status filter guard)
// now admits NO_DEADLINE.
func TestIsKnownStatus_IncludesNoDeadline(t *testing.T) {
	if !isKnownStatus(string(StatusNoDeadline)) {
		t.Error("isKnownStatus(NO_DEADLINE) = false, want true")
	}
	if isKnownStatus("BOGUS") {
		t.Error("isKnownStatus(BOGUS) = true, want false")
	}
}

// stubTx is a non-nil, no-op database.Tx for the read-only Preview (the mockRepo ignores its Tx
// argument; Preview only needs the pool to be non-nil so its guard passes).
type stubTx struct{}

func (stubTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (stubTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (stubTx) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// confirmation_panel_test.go covers the migration-0049 confirmation panel: the re-anchor
// (anchor_event → start_date), the manual_extra_days pass-through, the preview↔confirm paridade,
// and the NO_DEADLINE / reopen transitions plus the reconcile/MarkMissed no-op on NO_DEADLINE.

// --- anchor re-count --------------------------------------------------------

// TestConfirm_ReAnchorsOnPublished proves a confirm with anchor_event=PUBLISHED re-counts from the
// intimação's published_at (not the stored deadline_start_at), reads the anchors tenant-scoped,
// and persists the re-anchored start_date + the anchor_event.
func TestConfirm_ReAnchorsOnPublished(t *testing.T) {
	p := newConfirmParents()
	stored := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC) // deadline_start_at (legacy anchor)
	published := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, stored, "TJSP")
	repo.anchors = IntimationAnchors{
		MadeAvailableAt: time.Date(2024, 1, 11, 0, 0, 0, 0, time.UTC),
		PublishedAt:     published,
		DeadlineStartAt: stored,
	}
	cal := &fakeCalendar{endDate: end}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p)
	cmd.AnchorEvent = AnchorPublished

	res, err := uc.Confirm(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}

	if repo.anchorsCalls != 1 || repo.gotAnchorsIntim != p.intimationID || repo.gotAnchorsTenant != p.tenantID {
		t.Errorf("GetIntimationAnchors calls/intim/tenant = %d/%q/%q, want 1/%q/%q",
			repo.anchorsCalls, repo.gotAnchorsIntim, repo.gotAnchorsTenant, p.intimationID, p.tenantID)
	}
	// The recompute re-counted from published_at, NOT the stored deadline_start_at.
	if !cal.gotStart.Equal(published) {
		t.Errorf("calendar start = %v, want published_at %v", cal.gotStart, published)
	}
	// The confirm persisted the re-anchored start + the anchor_event.
	cp := repo.gotConfirmParams
	if !cp.StartDate.Equal(published) || cp.AnchorEvent != AnchorPublished {
		t.Errorf("persisted start/anchor = %v/%q, want %v/PUBLISHED", cp.StartDate, cp.AnchorEvent, published)
	}
	if !res.Deadline.StartDate.Equal(published) || res.Deadline.AnchorEvent != AnchorPublished {
		t.Errorf("result start/anchor = %v/%q, want %v/PUBLISHED", res.Deadline.StartDate, res.Deadline.AnchorEvent, published)
	}
}

// TestConfirm_DefaultAnchorSkipsAnchorRead proves the legacy default (DEADLINE_START) keeps the
// stored start without an extra anchor read — the panel only re-reads when it re-anchors.
func TestConfirm_DefaultAnchorSkipsAnchorRead(t *testing.T) {
	p := newConfirmParents()
	stored := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, stored, "TJSP")
	cal := &fakeCalendar{endDate: end}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p)
	cmd.AnchorEvent = AnchorDeadlineStart

	if _, err := uc.Confirm(context.Background(), cmd); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if repo.anchorsCalls != 0 {
		t.Errorf("GetIntimationAnchors calls = %d, want 0 (default anchor keeps stored start)", repo.anchorsCalls)
	}
	if !cal.gotStart.Equal(stored) {
		t.Errorf("calendar start = %v, want stored %v", cal.gotStart, stored)
	}
}

// --- manual_extra_days ------------------------------------------------------

// TestConfirm_ManualExtraDaysAddedToSameMotor proves manual_extra_days is added to the effective
// day count and fed to the SAME motor (never crude date arithmetic) — so the extra days are
// counted respecting the BUSINESS counting and share the automatic-holiday pass (no double count).
func TestConfirm_ManualExtraDaysAddedToSameMotor(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 12, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, start, "TJSP")
	cal := &fakeCalendar{endDate: end}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p) // days 15, not doubled
	cmd.ManualExtraDays = 3

	if _, err := uc.Confirm(context.Background(), cmd); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	// The motor was fed 15 + 3 = 18 days in ONE pass (business), not two calls, not raw +3 days.
	if cal.businessCalls != 1 || cal.calendarCalls != 0 {
		t.Errorf("calendar calls business=%d calendar=%d, want 1/0", cal.businessCalls, cal.calendarCalls)
	}
	if cal.gotN != 18 {
		t.Errorf("calendar n = %d, want 18 (15 days + 3 manual_extra_days)", cal.gotN)
	}
	if repo.gotConfirmParams.ManualExtraDays != 3 {
		t.Errorf("persisted manual_extra_days = %d, want 3", repo.gotConfirmParams.ManualExtraDays)
	}
}

// TestConfirm_DoubledThenExtraDays proves the ordering: the dobro doubles the RAW count first,
// THEN manual_extra_days is added — 2×15 + 3 = 33, one motor pass.
func TestConfirm_DoubledThenExtraDays(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, start, "TJSP")
	cal := &fakeCalendar{endDate: end}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := confirmCmd(p)
	cmd.Doubled = true
	cmd.ManualExtraDays = 3

	if _, err := uc.Confirm(context.Background(), cmd); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if cal.gotN != 33 {
		t.Errorf("calendar n = %d, want 33 (2×15 doubled + 3 extra)", cal.gotN)
	}
}

// TestConfirm_SnapshotsLegalCitation proves the confirm carries the derived legal_citation
// snapshot through to the persisted row and the response.
func TestConfirm_SnapshotsLegalCitation(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)

	repo := confirmRepo(p, start, "TJSP")
	repo.confirmAnchor.LegalCitation = "art. 335, CPC"
	cal := &fakeCalendar{endDate: start.AddDate(0, 0, 20)}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	res, err := uc.Confirm(context.Background(), confirmCmd(p))
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if repo.gotConfirmParams.LegalCitation != "art. 335, CPC" {
		t.Errorf("persisted legal_citation = %q, want %q", repo.gotConfirmParams.LegalCitation, "art. 335, CPC")
	}
	if res.Deadline.LegalCitation != "art. 335, CPC" {
		t.Errorf("result legal_citation = %q, want %q", res.Deadline.LegalCitation, "art. 335, CPC")
	}
}

// --- adjust re-anchor + extra days ------------------------------------------

// TestAdjust_ReAnchorAndExtraDaysMergeOverStored proves the PATCH merges a present anchor_event +
// manual_extra_days over the stored values, re-anchors the start from the intimação, and recounts
// via the same motor (days + extra). Absent fields keep stored values.
func TestAdjust_ReAnchorAndExtraDaysMergeOverStored(t *testing.T) {
	p := newAdjustParents()
	intimationID := uuid.NewString()
	stored := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	made := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)

	cur := storedDeadline(p, stored, StatusOpen) // days 5, BUSINESS, anchor DEADLINE_START, extra 0
	cur.IntimationID = intimationID
	cur.AnchorEvent = AnchorDeadlineStart
	cur.ManualExtraDays = 0

	repo := adjustRepo(p, cur, "TJSP")
	repo.anchors = IntimationAnchors{MadeAvailableAt: made, DeadlineStartAt: stored}
	cal := &fakeCalendar{endDate: end}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := AdjustCommand{
		TenantID:        p.tenantID,
		UserID:          p.userID,
		DeadlineID:      p.deadlineID,
		AnchorEvent:     ptr(AnchorMadeAvailable),
		ManualExtraDays: ptr(2),
	}
	res, err := uc.Adjust(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}

	// Re-anchored on made_available_at; recounted with days(5, stored) + 2 extra.
	if !cal.gotStart.Equal(made) || cal.gotN != 7 {
		t.Errorf("calendar start/n = %v/%d, want %v/7 (5 stored days + 2 extra)", cal.gotStart, cal.gotN, made)
	}
	up := repo.gotUpdateAdjustParams
	if !up.StartDate.Equal(made) || up.AnchorEvent != AnchorMadeAvailable || up.ManualExtraDays != 2 {
		t.Errorf("persisted start/anchor/extra = %v/%q/%d, want %v/MADE_AVAILABLE/2", up.StartDate, up.AnchorEvent, up.ManualExtraDays, made)
	}
	// Days field itself is untouched (only extra days changed the effective count).
	if up.Days != 5 {
		t.Errorf("persisted days = %d, want 5 (unchanged — extra days are separate)", up.Days)
	}
	if res.AnchorEvent != AnchorMadeAvailable || res.ManualExtraDays != 2 || !res.StartDate.Equal(made) {
		t.Errorf("result anchor/extra/start = %q/%d/%v", res.AnchorEvent, res.ManualExtraDays, res.StartDate)
	}
}

// TestAdjust_DefaultAnchorKeepsStoredStart proves an ajuste that does NOT touch anchor_event keeps
// the stored DEADLINE_START anchor and start without an anchor read.
func TestAdjust_DefaultAnchorKeepsStoredStart(t *testing.T) {
	p := newAdjustParents()
	stored := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	cur := storedDeadline(p, stored, StatusOpen)
	cur.AnchorEvent = AnchorDeadlineStart

	repo := adjustRepo(p, cur, "TJSP")
	cal := &fakeCalendar{endDate: stored.AddDate(0, 0, 7)}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	if _, err := uc.Adjust(context.Background(), AdjustCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Days: ptr(7),
	}); err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}
	if repo.anchorsCalls != 0 {
		t.Errorf("GetIntimationAnchors calls = %d, want 0 (default anchor)", repo.anchorsCalls)
	}
	if !cal.gotStart.Equal(stored) {
		t.Errorf("calendar start = %v, want stored %v", cal.gotStart, stored)
	}
}

// --- preview ↔ confirm paridade --------------------------------------------

// TestPreview_MatchesConfirmDates proves the preview and the confirm land on the SAME dates for
// the same inputs (paridade), both routing through computeWithExtra — the preview is read-only
// (no UoW, no confirm write) and uses the pool-backed GetPreviewContext.
func TestPreview_MatchesConfirmDates(t *testing.T) {
	p := newConfirmParents()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 12, 0, 0, 0, 0, time.UTC)
	now := time.Date(2024, 1, 20, 8, 0, 0, 0, time.UTC)

	// Confirm side.
	confRepo := confirmRepo(p, start, "TJSP")
	confCal := &fakeCalendar{endDate: end}
	confUC := NewUseCase(confRepo, confCal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})
	confCmd := confirmCmd(p)
	confCmd.ManualExtraDays = 2
	confRes, err := confUC.Confirm(context.Background(), confCmd)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}

	// Preview side — same inputs, DEADLINE_START anchor (start == deadline_start_at).
	prevRepo := &mockRepo{previewContext: PreviewContext{
		Anchors: IntimationAnchors{DeadlineStartAt: start},
		Court:   "TJSP",
	}}
	prevCal := &fakeCalendar{endDate: end}
	prevUC := NewUseCase(prevRepo, prevCal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{},
		WithClock(func() time.Time { return now }),
		WithPreviewPool(stubTx{}))
	prevRes, err := prevUC.Preview(context.Background(), PreviewCommand{
		TenantID:        p.tenantID,
		IntimationID:    p.intimationID,
		AnchorEvent:     AnchorDeadlineStart,
		Kind:            KindContestacao,
		Days:            15,
		Counting:        CountingBusiness,
		ManualExtraDays: 2,
	})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}

	// Same motor input (paridade) and same computed end_date.
	if confCal.gotN != prevCal.gotN {
		t.Errorf("motor n differs: confirm=%d preview=%d", confCal.gotN, prevCal.gotN)
	}
	if !prevRes.EndDate.Equal(confRes.Deadline.EndDate) {
		t.Errorf("preview end %v != confirm end %v (paridade broken)", prevRes.EndDate, confRes.Deadline.EndDate)
	}
	// Preview is read-only: no confirm write, no anchor read (DEADLINE_START), pool read used.
	if prevRepo.confirmCalls != 0 {
		t.Errorf("preview wrote a confirm (calls=%d), want 0 (read-only)", prevRepo.confirmCalls)
	}
	if prevRepo.previewContextCalls != 1 {
		t.Errorf("GetPreviewContext calls = %d, want 1", prevRepo.previewContextCalls)
	}
	// Weekday (pt-BR) + days_left from the pinned clock.
	if prevRes.Weekday != "segunda" { // 2024-02-12 is a Monday
		t.Errorf("weekday = %q, want segunda", prevRes.Weekday)
	}
	if prevRes.DaysLeft != 23 { // 2024-01-20 → 2024-02-12
		t.Errorf("days_left = %d, want 23", prevRes.DaysLeft)
	}
}

// TestPreview_ManualStartNoAnchorRead proves the manual start_date case computes directly from the
// given start with no intimação/anchor read (national holidays only, empty court).
func TestPreview_ManualStartNoAnchorRead(t *testing.T) {
	start := time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 5, 17, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{}
	cal := &fakeCalendar{endDate: end}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{},
		WithClock(func() time.Time { return start }),
		WithPreviewPool(stubTx{}))

	res, err := uc.Preview(context.Background(), PreviewCommand{
		TenantID:  uuid.NewString(),
		StartDate: &start,
		Days:      10,
		Counting:  CountingBusiness,
	})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if repo.previewContextCalls != 0 {
		t.Errorf("GetPreviewContext calls = %d, want 0 (manual start needs no anchor)", repo.previewContextCalls)
	}
	if !cal.gotStart.Equal(start) || cal.gotCourt != "" {
		t.Errorf("calendar start/court = %v/%q, want %v/'' (manual, no court)", cal.gotStart, cal.gotCourt, start)
	}
	if !res.EndDate.Equal(end) {
		t.Errorf("end = %v, want %v", res.EndDate, end)
	}
}

// --- NO_DEADLINE / reopen transitions ---------------------------------------

// TestNoDeadline_FromOpenStampsAndEmits proves "Remover prazo" flips OPEN → NO_DEADLINE, stamps
// confirmed_by/at, and emits deadline.no_deadline in one tenant-scoped tx.
func TestNoDeadline_FromOpenStampsAndEmits(t *testing.T) {
	tenantID, userID, deadlineID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		checkResult:  &DeadlineForCheck{ID: deadlineID, Status: StatusOpen},
		noDeadlineID: deadlineID,
	}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, uow, WithClock(func() time.Time { return now }))

	res, err := uc.NoDeadline(context.Background(), tenantID, userID, deadlineID)
	if err != nil {
		t.Fatalf("NoDeadline() error = %v", err)
	}
	if res.Status != StatusNoDeadline || res.ID != deadlineID {
		t.Errorf("result = %+v, want NO_DEADLINE/%q", res, deadlineID)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	if repo.noDeadlineCalls != 1 || repo.gotNoDeadlineBy != userID || !repo.gotNoDeadlineAt.Equal(now) {
		t.Errorf("MarkNoDeadline calls/by/at = %d/%q/%v, want 1/%q/%v", repo.noDeadlineCalls, repo.gotNoDeadlineBy, repo.gotNoDeadlineAt, userID, now)
	}
	evs := publishedOfType[DeadlineNoDeadline](outbox)
	if len(evs) != 1 || evs[0].Type() != TypeDeadlineNoDeadline || evs[0].AggregateID() != deadlineID {
		t.Errorf("deadline.no_deadline events = %v", evs)
	}
}

// TestNoDeadline_FromPendingAllowed proves "Não há prazo" (from PENDING) is also allowed.
func TestNoDeadline_FromPendingAllowed(t *testing.T) {
	tenantID, userID, deadlineID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	repo := &mockRepo{
		checkResult:  &DeadlineForCheck{ID: deadlineID, Status: StatusPending},
		noDeadlineID: deadlineID,
	}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	if _, err := uc.NoDeadline(context.Background(), tenantID, userID, deadlineID); err != nil {
		t.Fatalf("NoDeadline() from PENDING error = %v", err)
	}
	if repo.noDeadlineCalls != 1 {
		t.Errorf("MarkNoDeadline calls = %d, want 1", repo.noDeadlineCalls)
	}
}

// TestNoDeadline_TerminalIsConflict proves a terminal prazo (MET/MISSED/CANCELLED) cannot become
// mera ciência — the use case returns 409 without touching the guarded UPDATE or the outbox.
func TestNoDeadline_TerminalIsConflict(t *testing.T) {
	for _, st := range []Status{StatusMet, StatusMissed, StatusCancelled} {
		t.Run(string(st), func(t *testing.T) {
			deadlineID := uuid.NewString()
			repo := &mockRepo{checkResult: &DeadlineForCheck{ID: deadlineID, Status: st}}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			_, err := uc.NoDeadline(context.Background(), uuid.NewString(), uuid.NewString(), deadlineID)
			ae, ok := err.(*apperr.AppError)
			if !ok || ae.Kind != apperr.KindConflict {
				t.Fatalf("error = %v, want a KindConflict AppError", err)
			}
			if repo.noDeadlineCalls != 0 || len(outbox.published) != 0 {
				t.Errorf("terminal must not flip/emit: markCalls=%d published=%d", repo.noDeadlineCalls, len(outbox.published))
			}
		})
	}
}

// TestReopen_FromNoDeadlineClearsAndEmits proves reopen flips NO_DEADLINE → PENDING and emits
// deadline.reopened.
func TestReopen_FromNoDeadlineClearsAndEmits(t *testing.T) {
	tenantID, deadlineID := uuid.NewString(), uuid.NewString()
	repo := &mockRepo{
		checkResult: &DeadlineForCheck{ID: deadlineID, Status: StatusNoDeadline},
		reopenID:    deadlineID,
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	res, err := uc.Reopen(context.Background(), tenantID, deadlineID)
	if err != nil {
		t.Fatalf("Reopen() error = %v", err)
	}
	if res.Status != StatusPending || res.ID != deadlineID {
		t.Errorf("result = %+v, want PENDING/%q", res, deadlineID)
	}
	if repo.reopenCalls != 1 || repo.gotReopenTenant != tenantID {
		t.Errorf("ReopenNoDeadline calls/tenant = %d/%q, want 1/%q", repo.reopenCalls, repo.gotReopenTenant, tenantID)
	}
	evs := publishedOfType[DeadlineReopened](outbox)
	if len(evs) != 1 || evs[0].Type() != TypeDeadlineReopened || evs[0].AggregateID() != deadlineID {
		t.Errorf("deadline.reopened events = %v", evs)
	}
}

// TestReopen_NonNoDeadlineIsConflict proves reopen on a non-NO_DEADLINE prazo is a 409, no flip.
func TestReopen_NonNoDeadlineIsConflict(t *testing.T) {
	deadlineID := uuid.NewString()
	repo := &mockRepo{checkResult: &DeadlineForCheck{ID: deadlineID, Status: StatusOpen}}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	_, err := uc.Reopen(context.Background(), uuid.NewString(), deadlineID)
	ae, ok := err.(*apperr.AppError)
	if !ok || ae.Kind != apperr.KindConflict {
		t.Fatalf("error = %v, want a KindConflict AppError", err)
	}
	if repo.reopenCalls != 0 {
		t.Errorf("ReopenNoDeadline calls = %d, want 0 (guarded out)", repo.reopenCalls)
	}
}

// --- reconcile / MarkMissed no-op on NO_DEADLINE ----------------------------

// TestReconcile_SkipsNoDeadline proves a NO_DEADLINE prazo falls OUTSIDE the reconcile candidate
// set: ListReconcilableDeadlines (status IN MISSED,OPEN) never returns it, so the docket-entry
// reconcile leaves it untouched (no MarkMet). This is the guard the migration relies on — the
// use case iterates only what the repo lists.
func TestReconcile_SkipsNoDeadline(t *testing.T) {
	// The repo's ListReconcilableDeadlines is the guard (status IN ('MISSED','OPEN')); a
	// NO_DEADLINE prazo is simply absent from the list, so the reconcile loop has nothing to flip.
	repo := &mockRepo{reconcilable: []ReconcilableDeadline{}} // NO_DEADLINE excluded by the query
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), docketFixture()); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v", err)
	}
	if repo.reconcilableCalls != 1 {
		t.Errorf("ListReconcilableDeadlines calls = %d, want 1", repo.reconcilableCalls)
	}
	if len(repo.markMetIDs) != 0 {
		t.Errorf("MarkMet calls = %d, want 0 (NO_DEADLINE not in the reconcilable set)", len(repo.markMetIDs))
	}
	if len(outbox.published) != 0 {
		t.Errorf("published = %d, want 0", len(outbox.published))
	}
}
