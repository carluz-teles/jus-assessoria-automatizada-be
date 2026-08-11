package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// diarioNamespace namespaces the deterministic v5 aggregate id of a diario_requested.
// The event has no persistent aggregate row, but the outbox's aggregate_id is uuid NOT
// NULL, so we derive a STABLE uuid from the (tribunal, day) natural key: the same unit
// of work always maps to the same aggregate id (across the daily re-request), while the
// per-emission event id stays fresh. A fixed random namespace, minted once.
var diarioNamespace = uuid.MustParse("6f1d3b2a-8c7e-4a5f-9b0d-2e4c6a8f1b3d")

// ingestion.go is the national bulk ingestion — the DJEN pivot — split into the two
// halves of an event-driven flow:
//
//   - IngestionScheduler (producer, runs in the scheduler): fans one day into one
//     diario_requested event per tribunal, on the outbox, in one system transaction.
//   - IngestionUseCase (consumer, runs in worker-ingestao): reacts to each
//     diario_requested by fetching that one tribunal's diário and landing it in the
//     publication store.
//
// The split is the fix for the synchronous sweep's flaw: the old IngestDay looped every
// tribunal in one goroutine and skipped (log+continue) any that 429'd, silently losing a
// day's data once the DJEN cumulative limit tripped mid-list. Now each tribunal is its
// own task with the "ingestao" queue's retry budget (25) behind it — a throttled court
// retries with backoff and, if it never clears, archives to the DLQ instead of vanishing,
// while every other tribunal lands independently. The match runs separately (MatchUseCase),
// on the scheduler's idempotent tick, over whatever has landed.

// diarioFetcher reads one tribunal's diário for a date range (the DJEN connector's
// FetchDiario). The narrow port keeps the ingestion testable without HTTP.
type diarioFetcher interface {
	FetchDiario(ctx context.Context, tribunal, from, to string) ([]json.RawMessage, error)
}

// ingestRepo is the write the ingestion drives: land communications in the firehose.
type ingestRepo interface {
	InsertPublications(ctx context.Context, tx database.Tx, params []PublicationParams) (newCount int, err error)
}

// IngestionScheduler is the producer half: it turns a day into national fetch work by
// emitting one diario_requested per tribunal onto the outbox (the relay publishes them
// to the "ingestao" queue). It depends only on the outbox publisher and the unit of
// work's system mode — never on a data source; the fetch itself happens in the consumer.
type IngestionScheduler struct {
	outbox publisher
	uow    database.UnitOfWork
}

// NewIngestionScheduler wires the ingestion producer.
func NewIngestionScheduler(outbox publisher, uow database.UnitOfWork) *IngestionScheduler {
	return &IngestionScheduler{outbox: outbox, uow: uow}
}

// RequestDay emits one diario_requested per tribunal for the day, all in one system
// transaction (no tenant — the firehose is national), and returns how many it enqueued.
// All-or-nothing: a publish fault rolls back the whole batch so the caller retries the
// day cleanly rather than leaving it half-requested. Each event gets a fresh id, so
// re-requesting a day (the daily lookback) enqueues a new task and re-fetches — the
// self-heal for a tribunal that failed a prior run.
func (uc *IngestionScheduler) RequestDay(ctx context.Context, day time.Time) (requested int, err error) {
	d := day.Format(dateLayout)
	err = uc.uow.DoSystem(ctx, func(tx database.Tx) error {
		requested = 0
		for _, trib := range tribunais {
			if perr := uc.outbox.Publish(ctx, tx, newDiarioRequested(trib.sigla, d)); perr != nil {
				return perr
			}
			requested++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	slog.InfoContext(ctx, "ingestion: day requested", "day", d, "tribunais", requested)
	return requested, nil
}

// newDiarioRequested builds one national fetch event: a fresh v7 event id (the relay's
// enqueue-dedup TaskID) and a deterministic v5 aggregate id from (tribunal, day) — the
// outbox's aggregate_id is a uuid, so the tribunal sigla cannot be used raw.
func newDiarioRequested(tribunal, day string) DiarioRequested {
	agg := uuid.NewSHA1(diarioNamespace, []byte(tribunal+"|"+day)).String()
	return DiarioRequested{
		Base:     events.Base{EventID: newEventID(), Aggregate: agg},
		Tribunal: tribunal,
		Day:      day,
	}
}

// IngestionUseCase is the consumer half: it fetches ONE tribunal's diário for a day and
// lands it in the publication store. It is the runnable unit behind diario_requested, so
// each tribunal ingests independently with the queue's retry + DLQ. It depends on the
// narrow fetcher and repo ports and the unit of work's system mode.
type IngestionUseCase struct {
	fetcher diarioFetcher
	repo    ingestRepo
	uow     database.UnitOfWork
}

// NewIngestionUseCase wires the ingestion consumer.
func NewIngestionUseCase(fetcher diarioFetcher, repo ingestRepo, uow database.UnitOfWork) *IngestionUseCase {
	return &IngestionUseCase{fetcher: fetcher, repo: repo, uow: uow}
}

// OnDiarioRequested fetches one tribunal's diário for the event's day and lands it. The
// error kind decides the task's fate: a fetch fault (a DJEN 429/WAF/5xx, a flaky court)
// is transient — return it so asynq retries with backoff, and when the retry budget is
// exhausted asynq archives the task (the DLQ), never dropping it silently the way the old
// synchronous sweep did. A parse fault is permanent — a malformed item never parses on
// retry — so wrap SkipRetry to archive it at once instead of burning the budget. A DB
// write fault is transient (retry, eventually DLQ). No consumer dedup: InsertPublications
// is idempotent (ON CONFLICT hash), so a redelivery re-fetches and re-lands harmlessly.
func (uc *IngestionUseCase) OnDiarioRequested(ctx context.Context, ev DiarioRequested) error {
	items, err := uc.fetcher.FetchDiario(ctx, ev.Tribunal, ev.Day, ev.Day)
	if err != nil {
		return fmt.Errorf("ingestion: fetch diário %s %s: %w", ev.Tribunal, ev.Day, err)
	}
	if len(items) == 0 {
		return nil
	}

	params, err := publicationParamsFromItems(items)
	if err != nil {
		return fmt.Errorf("ingestion: parse diário %s %s: %v: %w", ev.Tribunal, ev.Day, err, asynq.SkipRetry)
	}

	var newCount int
	if err := uc.uow.DoSystem(ctx, func(tx database.Tx) error {
		var e error
		newCount, e = uc.repo.InsertPublications(ctx, tx, params)
		return e
	}); err != nil {
		return fmt.Errorf("ingestion: land diário %s %s: %w", ev.Tribunal, ev.Day, err)
	}

	slog.InfoContext(ctx, "ingestion: tribunal ingested",
		"tribunal", ev.Tribunal, "day", ev.Day, "items", len(items), "new", newCount)
	return nil
}
