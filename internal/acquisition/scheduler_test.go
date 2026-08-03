package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// fakeSchedulerRepo returns a preset due set and records the claims it received.
type fakeSchedulerRepo struct {
	due       []DueRecord
	claimed   []string
	claimNext []time.Time
}

func (r *fakeSchedulerRepo) DueCourtRecordsForResync(_ context.Context, _ database.Tx, _ int) ([]DueRecord, error) {
	return r.due, nil
}

func (r *fakeSchedulerRepo) ClaimCourtRecordResync(_ context.Context, _ database.Tx, id string, next time.Time) error {
	r.claimed = append(r.claimed, id)
	r.claimNext = append(r.claimNext, next)
	return nil
}

// TestSchedulerRunDuePoll proves one tick emits a re-poll (court_record_observed)
// per due record and claims each by pushing next_sync_at forward by the interval.
func TestSchedulerRunDuePoll(t *testing.T) {
	t.Parallel()

	repo := &fakeSchedulerRepo{due: []DueRecord{
		{ID: "r1", TenantID: "t1", CaseID: "c1", CNJNumber: "111", Degree: DegreeUnknown, Court: "TJSP"},
		{ID: "r2", TenantID: "t2", CaseID: "c2", CNJNumber: "222", Degree: "G1", Court: "TJRS"},
	}}
	outbox := &fakeOutbox{}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	uc := NewSchedulerUseCase(repo, outbox, &stubBackfillUoW{tx: stubTx{rows: 1}}, WithResyncInterval(24*time.Hour))
	uc.now = func() time.Time { return now }

	n, err := uc.RunDuePoll(context.Background())
	if err != nil {
		t.Fatalf("RunDuePoll: %v", err)
	}
	if n != 2 {
		t.Errorf("enqueued = %d, want 2", n)
	}

	if got := countByType(outbox.published)[TypeCourtRecordObserved]; got != 2 {
		t.Fatalf("court_record_observed events = %d, want 2", got)
	}
	// The first event mirrors the first due record's identity.
	ev, ok := outbox.published[0].(CourtRecordObserved)
	if !ok {
		t.Fatalf("published[0] is %T, want CourtRecordObserved", outbox.published[0])
	}
	if ev.CourtRecordID != "r1" || ev.TenantID != "t1" || ev.Court != "TJSP" || ev.CNJNumber != "111" {
		t.Errorf("event = %+v, want the r1 due record", ev)
	}

	wantNext := now.Add(24 * time.Hour)
	if len(repo.claimed) != 2 || repo.claimed[0] != "r1" || repo.claimed[1] != "r2" {
		t.Errorf("claims = %v, want [r1 r2]", repo.claimed)
	}
	for i, got := range repo.claimNext {
		if !got.Equal(wantNext) {
			t.Errorf("claim %d next_sync_at = %s, want %s", i, got, wantNext)
		}
	}
}

// TestSchedulerRunDuePoll_Empty proves a tick with nothing due publishes and claims
// nothing.
func TestSchedulerRunDuePoll_Empty(t *testing.T) {
	t.Parallel()

	repo := &fakeSchedulerRepo{}
	outbox := &fakeOutbox{}
	uc := NewSchedulerUseCase(repo, outbox, &stubBackfillUoW{tx: stubTx{rows: 1}})

	n, err := uc.RunDuePoll(context.Background())
	if err != nil {
		t.Fatalf("RunDuePoll: %v", err)
	}
	if n != 0 || len(outbox.published) != 0 || len(repo.claimed) != 0 {
		t.Errorf("empty tick did work: n=%d published=%d claimed=%d", n, len(outbox.published), len(repo.claimed))
	}
}
