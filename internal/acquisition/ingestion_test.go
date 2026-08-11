package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

type stubDayMatcher struct{ days []time.Time }

func (m *stubDayMatcher) MatchDay(_ context.Context, day time.Time) error {
	m.days = append(m.days, day)
	return nil
}

// TestIngestionUseCase_IngestDay sweeps every tribunal for a day: it fetches all of
// them, lands only the ones with items (empty are skipped), tolerates a tribunal that
// errors (logged + skipped, not fatal), and runs the match exactly once for the day.
func TestIngestionUseCase_IngestDay(t *testing.T) {
	t.Parallel()

	day := time.Date(2025, 8, 8, 0, 0, 0, 0, time.UTC)
	fetcher := &stubDiarioFetcher{
		errOn: "TJRJ", // one tribunal outage — must not sink the day
		byTribunal: map[string][]json.RawMessage{
			"TJSP": {diarioItem("h1", "TJSP", "2025-08-08", [2]string{"347019", "SP"})},
		},
	}
	repo := &stubIngestRepo{}
	matcher := &stubDayMatcher{}
	uc := NewIngestionUseCase(fetcher, repo, &stubMatchUoW{}, matcher)

	if err := uc.IngestDay(context.Background(), day); err != nil {
		t.Fatalf("IngestDay: %v", err)
	}

	if len(fetcher.calls) != len(tribunais) {
		t.Errorf("FetchDiario calls = %d, want %d (every tribunal swept)", len(fetcher.calls), len(tribunais))
	}
	// Only TJSP had items; the errored TJRJ and the empty rest land nothing.
	if repo.batches != 1 || repo.inserted != 1 {
		t.Errorf("inserts = {batches:%d rows:%d}, want {1 1}", repo.batches, repo.inserted)
	}
	if len(matcher.days) != 1 || !matcher.days[0].Equal(day) {
		t.Errorf("MatchDay calls = %v, want [%v]", matcher.days, day)
	}
}
