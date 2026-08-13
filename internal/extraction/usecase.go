package extraction

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/obs"
)

// Metric names for the extraction stage — the "Tesseract worked" signal. Dotted + prefixed like
// asynq.queue.depth so the backend groups the document.* family together. duration_ms + pages
// are distributions faceted by the extractor that produced them; total counts outcomes.
const (
	metricExtractionDuration = "document.extraction.duration_ms"
	metricExtractionPages    = "document.extraction.pages"
	metricExtractionTotal    = "document.extraction.total"
)

// extractionMetrics holds the instruments built once at construction (via obs.Meter(), mirroring
// lib/events/queue_metrics.go). Kept in a struct so the use case records against the same
// instrument across events.
type extractionMetrics struct {
	duration metric.Int64Histogram
	pages    metric.Int64Histogram
	total    metric.Int64Counter
}

// newExtractionMetrics builds the extraction instruments once. Instrument creation can fail; the
// error is surfaced so the worker fails boot loudly (fail-fast) instead of dropping telemetry.
func newExtractionMetrics() (*extractionMetrics, error) {
	meter := obs.Meter()

	duration, err := meter.Int64Histogram(
		metricExtractionDuration,
		metric.WithDescription("Wall-clock duration of the extractor call, in milliseconds, by extractor."),
	)
	if err != nil {
		return nil, fmt.Errorf("extraction: create %s histogram: %w", metricExtractionDuration, err)
	}

	pages, err := meter.Int64Histogram(
		metricExtractionPages,
		metric.WithDescription("Pages extracted per document, by extractor."),
	)
	if err != nil {
		return nil, fmt.Errorf("extraction: create %s histogram: %w", metricExtractionPages, err)
	}

	total, err := meter.Int64Counter(
		metricExtractionTotal,
		metric.WithDescription("Extraction attempts, by extractor and outcome (ok|failed)."),
	)
	if err != nil {
		return nil, fmt.Errorf("extraction: create %s counter: %w", metricExtractionTotal, err)
	}

	return &extractionMetrics{duration: duration, pages: pages, total: total}, nil
}

// textKeySuffix is appended to the document's storage_key to derive the object key the
// per-page text JSON is written to (the pinned intermediate-text location). Kept as a
// constant so the producer (here) and any future reader agree on the derivation.
const textKeySuffix = ".text.json"

// objectStore is the storage port the use case needs: fetch the source PDF bytes and
// write the extracted-text JSON. It is a narrow port (not lib/storage's whole surface)
// so tests inject a fake without a real bucket. The adapter (storage.go) satisfies it via
// presigned GET/PUT + http.
type objectStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, body []byte, contentType string) error
}

// docRepo is the raw-pgx write port for the document row's saga transitions. Both methods
// run inside the caller's tenant-scoped tx (RLS applied). MarkExtracting flips
// UPLOADED→EXTRACTING (a guarded UPDATE, so a duplicate/late delivery that already
// advanced is a no-op the use case tolerates); MarkExtracted finalizes →EXTRACTED with the
// extraction metadata; MarkFailed flips →FAILED and records the {stage,message} error.
type docRepo interface {
	MarkExtracting(ctx context.Context, tx database.Tx, documentID, tenantID string) error
	MarkExtracted(ctx context.Context, tx database.Tx, p MarkExtractedParams) error
	MarkFailed(ctx context.Context, tx database.Tx, documentID, tenantID, errorJSON string) error
}

// MarkExtractedParams is the final EXTRACTED update's input — the extraction metadata the
// document row records (pages, has_text_layer, extractor_version) plus the keys. extracted_at
// is stamped now() by the query itself.
type MarkExtractedParams struct {
	DocumentID       string
	TenantID         string
	Pages            int
	HasTextLayer     bool
	ExtractorVersion string
}

// deduper is the consumer-side idempotency guard port — marks (consumer, eventID) inside
// the caller's tx so the mark and the effect commit together (a crash never strands a
// marked-but-unwritten event). The adapter wraps events.Dedup bound to tx, exactly like
// the deadline slice.
type deduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// publisher is the transactional-outbox port — the producer half. *events.Outbox satisfies
// it structurally; Publish writes the document.extracted / document.failed row in the same
// tx as the effect, so entity + event commit atomically.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// UseCase runs the extraction pipeline for one uploaded document. It depends only on the
// ports above and the UnitOfWork — never a concrete implementation. It owns the transaction
// boundary; the ports only participate.
type UseCase struct {
	uow       database.UnitOfWork
	store     objectStore
	repo      docRepo
	extractor TextExtractor
	dedup     deduper
	outbox    publisher
	logger    *slog.Logger
	metrics   *extractionMetrics
}

// NewUseCase wires the use case to its ports and builds its metric instruments once. The worker
// composes it (passing its logger); tests inject fakes with a discard logger. It returns an error
// only if instrument creation fails, so the worker fails boot loudly rather than dropping
// telemetry.
func NewUseCase(uow database.UnitOfWork, store objectStore, repo docRepo, extractor TextExtractor, dedup deduper, outbox publisher, logger *slog.Logger) (*UseCase, error) {
	metrics, err := newExtractionMetrics()
	if err != nil {
		return nil, err
	}
	return &UseCase{
		uow:       uow,
		store:     store,
		repo:      repo,
		extractor: extractor,
		dedup:     dedup,
		outbox:    outbox,
		logger:    logger,
		metrics:   metrics,
	}, nil
}

// OnDocumentUploaded is the pipeline entry point: from one document.uploaded it extracts
// the document's text (text layer, else OCR), persists it, advances the saga and emits the
// outcome — all in a single tenant-scoped transaction so the dedup mark, the row transition
// and the outbox row commit together.
//
// Steps:
//  1. dedup (a replay marks nothing new and returns before any effect — no re-extraction,
//     no duplicate document.extracted);
//  2. flip UPLOADED→EXTRACTING (guarded; a late/duplicate delivery that already advanced is
//     tolerated);
//  3. fetch the PDF bytes via storage_key;
//  4. run the TextExtractor port (dispatcher: text layer first, OCR fallback);
//  5. write the per-page text JSON to <storage_key>.text.json;
//  6. finalize →EXTRACTED with {pages, has_text_layer, extractor_version, extracted_at=now};
//  7. emit document.extracted in the SAME tx.
//
// On ANY stage error (fetch/extract/persist/update) it flips the row →FAILED, records the
// {stage,message} error jsonb, emits document.failed IN THE SAME failure tx, and RETURNS the
// original error so asynq retries (the FAILED write is a best-effort audit trail, not a
// terminal ack — a genuine infra fault stays retryable). tenantID comes from the event
// payload (a trusted producer inside the same system) and scopes the tx's RLS.
func (uc *UseCase) OnDocumentUploaded(ctx context.Context, ev DocumentUploaded) error {
	// Idempotency + the EXTRACTING flip run in their own committed tx BEFORE the (possibly
	// slow, network-bound) extraction, so the "processando…" state is visible while the OCR
	// runs and a replay is caught early. seen short-circuits the whole pipeline.
	var proceed bool
	if err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerExtraction, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}
		if err := uc.repo.MarkExtracting(ctx, tx, ev.DocumentID, ev.TenantID); err != nil {
			return err
		}
		proceed = true
		return nil
	}); err != nil {
		return err
	}
	if !proceed {
		return nil // replay: already handled, ack.
	}

	// The extraction itself runs OUTSIDE a tx (no DB held while the OCR round-trips). Any
	// fault here routes to the FAILED path, which opens its own tx.
	textKey, pages, hasTextLayer, version, durationMS, err := uc.extract(ctx, ev)
	if err != nil {
		return uc.fail(ctx, ev, err)
	}

	// Success telemetry — the "Tesseract worked" signal. The consumer span already exists
	// (events.Observe); enrich it with the extraction dimensions, record the distributions +
	// the ok outcome, and log the one happy-path line (the pipeline's per-document proof).
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("extractor.version", version),
		attribute.Int("document.pages", pages),
		attribute.Bool("document.has_text_layer", hasTextLayer),
	)
	extractorAttr := metric.WithAttributes(attribute.String("extractor", version))
	uc.metrics.duration.Record(ctx, durationMS, extractorAttr)
	uc.metrics.pages.Record(ctx, int64(pages), extractorAttr)
	uc.metrics.total.Add(ctx, 1, metric.WithAttributes(
		attribute.String("extractor", version),
		attribute.String("outcome", "ok"),
	))
	uc.logger.LogAttrs(ctx, slog.LevelInfo, "document extracted",
		slog.String("document_id", ev.DocumentID),
		slog.String("tenant_id", ev.TenantID),
		slog.String("extractor_version", version),
		slog.Int("pages", pages),
		slog.Bool("has_text_layer", hasTextLayer),
		slog.Int64("duration_ms", durationMS),
	)

	// Finalize + publish in one tx: the EXTRACTED transition and the document.extracted row
	// commit together (transactional outbox).
	if err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if err := uc.repo.MarkExtracted(ctx, tx, MarkExtractedParams{
			DocumentID:       ev.DocumentID,
			TenantID:         ev.TenantID,
			Pages:            pages,
			HasTextLayer:     hasTextLayer,
			ExtractorVersion: version,
		}); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newDocumentExtracted(ev, textKey, pages, hasTextLayer, version))
	}); err != nil {
		return uc.fail(ctx, ev, err)
	}
	return nil
}

// extract fetches the PDF bytes, runs the extractor, writes the per-page text JSON to
// <storage_key>.text.json and returns the text key + the extraction metadata. It is the
// tx-free heart of the pipeline (the network + CPU work); its errors are typed so the
// caller can build the FAILED message.
func (uc *UseCase) extract(ctx context.Context, ev DocumentUploaded) (textKey string, pages int, hasTextLayer bool, version string, durationMS int64, err error) {
	pdf, err := uc.store.Get(ctx, ev.StorageKey)
	if err != nil {
		return "", 0, false, "", 0, err
	}

	// Measure duration_ms strictly around the extractor call (the CPU/OCR work the metric
	// tracks) — storage fetch/put are separate concerns not attributed to the extractor.
	start := time.Now()
	pageTexts, hasTextLayer, version, err := uc.extractor.Extract(ctx, pdf)
	durationMS = time.Since(start).Milliseconds()
	if err != nil {
		return "", 0, false, "", durationMS, err
	}

	textKey = ev.StorageKey + textKeySuffix
	body, err := json.Marshal(textDocument{ExtractorVersion: version, Pages: pageTexts})
	if err != nil {
		return "", 0, false, "", durationMS, apperr.NewInfra("extraction: marshal text json", err)
	}
	if err := uc.store.Put(ctx, textKey, body, "application/json"); err != nil {
		return "", 0, false, "", durationMS, err
	}
	return textKey, len(pageTexts), hasTextLayer, version, durationMS, nil
}

// fail records the terminal FAILED state for the document and emits document.failed, then
// returns the ORIGINAL cause so asynq retries. The FAILED write + the document.failed row
// commit in one tx (audit + fact together). If the FAILED write itself fails, that error is
// joined so the log shows both — but the returned error still wraps the original cause, so
// the retry/archive decision keys off the real fault, not the audit write.
func (uc *UseCase) fail(ctx context.Context, ev DocumentUploaded, cause error) error {
	// Failure telemetry: the metric only (Observe already logs the error — don't double-log).
	// The extractor is unknown on failure (the extractor call may not have returned a version),
	// so the counter carries an empty extractor facet + the failed outcome; the span records
	// the faulted stage.
	uc.metrics.total.Add(ctx, 1, metric.WithAttributes(
		attribute.String("extractor", ""),
		attribute.String("outcome", "failed"),
	))
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("document.stage", stageExtraction))

	errorJSON, _ := json.Marshal(struct {
		Stage   string `json:"stage"`
		Message string `json:"message"`
	}{Stage: stageExtraction, Message: cause.Error()})

	if werr := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if err := uc.repo.MarkFailed(ctx, tx, ev.DocumentID, ev.TenantID, string(errorJSON)); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newDocumentFailed(ev, cause.Error()))
	}); werr != nil {
		return fmt.Errorf("extraction failed (%w); recording FAILED also failed: %w", cause, werr)
	}
	return cause
}
