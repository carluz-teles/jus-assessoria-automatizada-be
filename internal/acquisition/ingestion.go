package acquisition

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// diarioFetcher reads one tribunal's diário for a date range (the DJEN connector's
// FetchDiario). The narrow port keeps the ingestion testable without HTTP.
type diarioFetcher interface {
	FetchDiario(ctx context.Context, tribunal, from, to string) ([]json.RawMessage, error)
}

// ingestRepo is the write the ingestion drives: land communications in the firehose.
type ingestRepo interface {
	InsertPublications(ctx context.Context, tx database.Tx, params []PublicationParams) (newCount int, err error)
}

// dayMatcher fans a day's landed publications out to tenants (MatchUseCase.MatchDay).
type dayMatcher interface {
	MatchDay(ctx context.Context, day time.Time) error
}

// IngestionUseCase runs the national bulk ingestion for one day: it sweeps EVERY DJEN
// tribunal (the registry), lands each one's communications in the publication store,
// then runs the local match to fan out to tenant intimações. This is the runnable
// unit the scheduler calls (daily for yesterday + a lookback) and the historical
// bootstrap calls (per day). Cost scales with the national daily volume, not with the
// number of tenants — the whole point of the pivot. A tribunal that errors is logged
// and skipped so one court's outage never sinks the day; a DB write failure aborts.
type IngestionUseCase struct {
	fetcher diarioFetcher
	repo    ingestRepo
	uow     database.UnitOfWork
	matcher dayMatcher
}

// NewIngestionUseCase wires the ingestion use case.
func NewIngestionUseCase(fetcher diarioFetcher, repo ingestRepo, uow database.UnitOfWork, matcher dayMatcher) *IngestionUseCase {
	return &IngestionUseCase{fetcher: fetcher, repo: repo, uow: uow, matcher: matcher}
}

// IngestDay ingests every tribunal for the given day, then matches it to tenants.
func (uc *IngestionUseCase) IngestDay(ctx context.Context, day time.Time) error {
	d := day.Format(dateLayout)
	total := 0
	for _, trib := range tribunais {
		items, err := uc.fetcher.FetchDiario(ctx, trib.sigla, d, d)
		if err != nil {
			// One tribunal's outage/throttle must not sink the whole day; the daily
			// lookback re-fetches, so a skipped tribunal self-heals next run.
			slog.ErrorContext(ctx, "ingestion: fetch tribunal failed",
				"tribunal", trib.sigla, "day", d, "error", err)
			continue
		}
		if len(items) == 0 {
			continue
		}
		params, err := publicationParamsFromItems(items)
		if err != nil {
			slog.ErrorContext(ctx, "ingestion: parse tribunal items failed",
				"tribunal", trib.sigla, "day", d, "error", err)
			continue
		}
		var newCount int
		if err := uc.uow.DoSystem(ctx, func(tx database.Tx) error {
			var e error
			newCount, e = uc.repo.InsertPublications(ctx, tx, params)
			return e
		}); err != nil {
			return err
		}
		total += newCount
		slog.InfoContext(ctx, "ingestion: tribunal ingested",
			"tribunal", trib.sigla, "day", d, "items", len(items), "new", newCount)
	}
	slog.InfoContext(ctx, "ingestion: day ingested", "day", d, "new_publications", total)

	// Fan the day's firehose out to the watching tenants.
	return uc.matcher.MatchDay(ctx, day)
}
