package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
)

type stubDiarioFetcher struct {
	byTribunal map[string][]json.RawMessage
	errOn      string
	calls      []string
}

func (f *stubDiarioFetcher) FetchDiario(_ context.Context, tribunal, _, _ string) ([]json.RawMessage, error) {
	f.calls = append(f.calls, tribunal)
	if tribunal == f.errOn {
		return nil, errors.New("boom")
	}
	return f.byTribunal[tribunal], nil
}

type stubIngestRepo struct {
	batches  int
	inserted int
}

func (r *stubIngestRepo) InsertPublications(_ context.Context, _ database.Tx, params []PublicationParams) (int, error) {
	r.batches++
	r.inserted += len(params)
	return len(params), nil
}

// TestIngestionScheduler_RequestDay proves the producer fans a day into one
// diario_requested per tribunal, in one system transaction: every tribunal is covered
// exactly once, each event carries the day and a distinct event id, and the count equals
// the registry size.
func TestIngestionScheduler_RequestDay(t *testing.T) {
	t.Parallel()

	day := time.Date(2025, 8, 8, 0, 0, 0, 0, time.UTC)
	outbox := &fakeOutbox{}
	uc := NewIngestionScheduler(outbox, &stubMatchUoW{})

	n, err := uc.RequestDay(context.Background(), day)
	if err != nil {
		t.Fatalf("RequestDay: %v", err)
	}
	if n != len(tribunais) {
		t.Fatalf("requested = %d, want %d (one per tribunal)", n, len(tribunais))
	}
	if len(outbox.published) != len(tribunais) {
		t.Fatalf("published = %d events, want %d", len(outbox.published), len(tribunais))
	}

	seenTribunal := make(map[string]bool, len(tribunais))
	seenEventID := make(map[string]bool, len(tribunais))
	for _, ev := range outbox.published {
		dr, ok := ev.(DiarioRequested)
		if !ok {
			t.Fatalf("published event type = %T, want DiarioRequested", ev)
		}
		if dr.Day != "2025-08-08" {
			t.Errorf("event day = %q, want 2025-08-08", dr.Day)
		}
		// The outbox's event_id AND aggregate_id are both uuid columns; assert the
		// invariant here so a non-uuid (e.g. the raw sigla) fails in the unit test
		// instead of only at the DB insert in prod.
		if _, err := uuid.Parse(dr.EventID); err != nil {
			t.Errorf("event id %q not a uuid: %v", dr.EventID, err)
		}
		if _, err := uuid.Parse(dr.AggregateID()); err != nil {
			t.Errorf("aggregate id %q not a uuid: %v", dr.AggregateID(), err)
		}
		if seenEventID[dr.EventID] {
			t.Errorf("event id %q duplicated", dr.EventID)
		}
		seenEventID[dr.EventID] = true
		seenTribunal[dr.Tribunal] = true
	}
	if len(seenTribunal) != len(tribunais) {
		t.Errorf("distinct tribunais = %d, want %d", len(seenTribunal), len(tribunais))
	}
}

// TestIngestionScheduler_RequestDay_PublishError proves the batch is all-or-nothing: a
// publish fault aborts the whole day (error surfaced, zero reported) so the caller
// retries the day cleanly rather than half-requesting it.
func TestIngestionScheduler_RequestDay_PublishError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("outbox down")
	outbox := &fakeOutbox{failAt: 3, err: sentinel}
	uc := NewIngestionScheduler(outbox, &stubMatchUoW{})

	n, err := uc.RequestDay(context.Background(), time.Date(2025, 8, 8, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if n != 0 {
		t.Errorf("requested = %d, want 0 on abort", n)
	}
}

// TestIngestionUseCase_OnDiarioRequested lands one tribunal's diário: it fetches the
// requested tribunal/day, parses the items, and inserts them once. The match is NOT run
// here — that is the scheduler's separate idempotent tick.
func TestIngestionUseCase_OnDiarioRequested(t *testing.T) {
	t.Parallel()

	fetcher := &stubDiarioFetcher{
		byTribunal: map[string][]json.RawMessage{
			"TJSP": {diarioItem("h1", "TJSP", "2025-08-08", [2]string{"347019", "SP"})},
		},
	}
	repo := &stubIngestRepo{}
	uc := NewIngestionUseCase(fetcher, repo, &stubMatchUoW{})

	ev := DiarioRequested{Tribunal: "TJSP", Day: "2025-08-08"}
	if err := uc.OnDiarioRequested(context.Background(), ev); err != nil {
		t.Fatalf("OnDiarioRequested: %v", err)
	}
	if fetcher.calls[0] != "TJSP" || len(fetcher.calls) != 1 {
		t.Errorf("fetch calls = %v, want [TJSP]", fetcher.calls)
	}
	if repo.batches != 1 || repo.inserted != 1 {
		t.Errorf("inserts = {batches:%d rows:%d}, want {1 1}", repo.batches, repo.inserted)
	}
}

// TestIngestionUseCase_OnDiarioRequested_Empty acks a tribunal that returns nothing
// without touching the store.
func TestIngestionUseCase_OnDiarioRequested_Empty(t *testing.T) {
	t.Parallel()

	fetcher := &stubDiarioFetcher{byTribunal: map[string][]json.RawMessage{}}
	repo := &stubIngestRepo{}
	uc := NewIngestionUseCase(fetcher, repo, &stubMatchUoW{})

	if err := uc.OnDiarioRequested(context.Background(), DiarioRequested{Tribunal: "TJSP", Day: "2025-08-08"}); err != nil {
		t.Fatalf("OnDiarioRequested: %v", err)
	}
	if repo.batches != 0 {
		t.Errorf("inserts = %d batches, want 0 for an empty diário", repo.batches)
	}
}

// TestIngestionUseCase_OnDiarioRequested_FetchError_Retryable proves a fetch fault (a
// 429/WAF/flaky court) is RETRYABLE: the error surfaces without SkipRetry, so asynq
// retries with backoff (and archives to the DLQ only when the budget is exhausted).
func TestIngestionUseCase_OnDiarioRequested_FetchError_Retryable(t *testing.T) {
	t.Parallel()

	fetcher := &stubDiarioFetcher{errOn: "TJSP"}
	uc := NewIngestionUseCase(fetcher, &stubIngestRepo{}, &stubMatchUoW{})

	err := uc.OnDiarioRequested(context.Background(), DiarioRequested{Tribunal: "TJSP", Day: "2025-08-08"})
	if err == nil {
		t.Fatal("OnDiarioRequested: want error on fetch fault")
	}
	if errors.Is(err, asynq.SkipRetry) {
		t.Errorf("fetch fault must be retryable, but wraps SkipRetry: %v", err)
	}
}

// TestIngestionUseCase_OnDiarioRequested_ParseError_SkipRetry proves a parse fault (a
// malformed item that never parses on retry) is PERMANENT: the error wraps SkipRetry so
// asynq archives the task at once instead of burning the retry budget.
func TestIngestionUseCase_OnDiarioRequested_ParseError_SkipRetry(t *testing.T) {
	t.Parallel()

	fetcher := &stubDiarioFetcher{
		byTribunal: map[string][]json.RawMessage{
			"TJSP": {json.RawMessage("{ not valid json")},
		},
	}
	repo := &stubIngestRepo{}
	uc := NewIngestionUseCase(fetcher, repo, &stubMatchUoW{})

	err := uc.OnDiarioRequested(context.Background(), DiarioRequested{Tribunal: "TJSP", Day: "2025-08-08"})
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("parse fault must wrap SkipRetry, got: %v", err)
	}
	if repo.batches != 0 {
		t.Errorf("inserts = %d, want 0 when parse fails before the write", repo.batches)
	}
}
