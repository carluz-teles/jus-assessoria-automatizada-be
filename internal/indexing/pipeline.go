package indexing

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// pipeline.go is the indexing fatia's use case — the async entry point behind the listener. On
// one document.extracted it: reads the extracted-text JSON from storage, chunks each page,
// embeds the chunks, then (in ONE tenant-scoped tx) dedups the event, INSERTs the chunks
// idempotently, walks the document EXTRACTED→CHUNKED→READY, and publishes document.ready — the
// writes and the outbox row committing together (transactional outbox). Any error flips the
// document to FAILED, publishes document.failed, and is returned for asynq's retry decision.

// UseCase is the indexing pipeline. It holds the ports (never transport/DB state directly): the
// object reader (extracted-text JSON), the embedder (Voyage behind a port), the transaction
// boundary, the dedup guard, the chunk/status repository and the outbox producer. The listener
// delegates to it; the binary composes the concrete adapters.
type UseCase struct {
	reader objectReader
	embed  Embedder
	uow    unitOfWork
	dedup  deduper
	repo   repository
	outbox outbox
	dim    int
}

// NewUseCase wires the pipeline to its ports. dim is the expected embedding dimensionality
// (config VOYAGE_EMBED_DIM) stamped on each chunk row; it is provenance/stat only — the repo
// writes whatever the embedder returns.
func NewUseCase(reader objectReader, embed Embedder, uow unitOfWork, dedup deduper, repo repository, ob outbox, dim int) *UseCase {
	return &UseCase{reader: reader, embed: embed, uow: uow, dedup: dedup, repo: repo, outbox: ob, dim: dim}
}

// OnDocumentExtracted is the whole pipeline for one document.extracted. It splits into two
// phases: the SIDE-EFFECT-FREE-of-DB prep (read storage + chunk + embed) runs BEFORE the tx so a
// slow embed never holds a DB connection open; then the tx does the dedup + writes + outbox
// atomically. On ANY error the document is flipped to FAILED in its own tx and document.failed is
// published, and the original error is returned so the listener maps it to asynq's retry (a
// transient embed/storage fault retries; a terminal one is archived).
func (uc *UseCase) OnDocumentExtracted(ctx context.Context, ev DocumentExtracted) error {
	if err := uc.index(ctx, ev); err != nil {
		// Best-effort failure recording: flip the document to FAILED + publish document.failed in
		// a separate tx. If THAT fails too, prefer surfacing the original cause (the failure the
		// operator needs) — the fail write is retryable on the next delivery. The original error
		// is always returned so the listener drives asynq's retry/archive decision.
		uc.recordFailure(ctx, ev, err)
		return err
	}
	return nil
}

// index is the happy-path pipeline sans failure handling. Split out so OnDocumentExtracted owns
// the single "on error → FAILED + document.failed" funnel and this reads as a straight line.
func (uc *UseCase) index(ctx context.Context, ev DocumentExtracted) error {
	// Phase 1 — DB-free prep. Read the per-page extracted text, chunk it, embed the chunks. None
	// of this touches the DB, so an idempotency replay (caught in phase 2) simply redoes cheap
	// work; keeping embed OUTSIDE the tx avoids holding a connection through a slow Voyage call.
	raw, err := uc.reader.ReadObject(ctx, ev.TextKey)
	if err != nil {
		return err
	}
	var text ExtractedText
	if err := json.Unmarshal(raw, &text); err != nil {
		// A malformed extracted-text object can never parse on retry — terminal.
		return apperr.NewInvalid(fmt.Sprintf("indexing: malformed extracted text at %s: %v", ev.TextKey, err))
	}

	chunks := chunkPages(text.Pages)
	if len(chunks) == 0 {
		// No text to index (an empty/whitespace-only extraction, e.g. a scanned PDF with no OCR
		// text layer). Not an error: the document is legitimately READY with zero chunks, so the
		// saga completes and document.ready fires with chunk_count=0. Phase 2 handles the empty
		// case (no INSERT, straight to READY).
		return uc.persist(ctx, ev, nil, "")
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vectors, model, err := uc.embed.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) != len(chunks) {
		// The embedder must return one vector per input — a mismatch is a contract violation, not
		// a transient fault; retrying re-sends the same batch. Terminal.
		return apperr.NewInvalid(fmt.Sprintf("indexing: embedder returned %d vectors for %d chunks", len(vectors), len(chunks)))
	}

	rows := make([]ChunkRow, len(chunks))
	for i, c := range chunks {
		rows[i] = ChunkRow{
			DocumentID:     ev.DocumentID,
			Page:           c.Page,
			Text:           c.Text,
			Embedding:      vectors[i],
			EmbeddingModel: model,
			Dim:            uc.dim,
			Hash:           c.Hash,
		}
	}

	return uc.persist(ctx, ev, rows, model)
}

// persist is phase 2 — the ONE tenant-scoped tx that makes the effect atomic: dedup the event
// (a replay marks nothing new and returns before any write), INSERT the chunks idempotently
// (ON CONFLICT DO NOTHING), walk the document EXTRACTED→CHUNKED→READY, and publish
// document.ready. All of it commits together or not at all. model is "" only when there are no
// rows (an empty extraction); chunk_count on the event is len(rows) (the intended index size,
// not the ON-CONFLICT-adjusted inserted count — a reprocess of the same doc still reports its
// full chunk count).
func (uc *UseCase) persist(ctx context.Context, ev DocumentExtracted, rows []ChunkRow, model string) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerIndexing, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			// A prior delivery already indexed this document — no-op and ack (the mark, chunks and
			// document.ready from the first delivery already committed). No second document.ready.
			return nil
		}

		if len(rows) > 0 {
			if _, err := uc.repo.InsertChunks(ctx, tx, rows); err != nil {
				return err
			}
			// CHUNKED is the intermediate saga state (chunks persisted, before the READY flip) — a
			// crash between here and READY leaves an auditable CHUNKED the pipeline can resume.
			if err := uc.repo.SetStatus(ctx, tx, ev.DocumentID, ev.TenantID, statusChunked); err != nil {
				return err
			}
		}

		if err := uc.repo.SetStatus(ctx, tx, ev.DocumentID, ev.TenantID, statusReady); err != nil {
			return err
		}

		return uc.outbox.Publish(ctx, tx, newDocumentReady(ev, len(rows), model))
	})
}

// recordFailure flips the document to FAILED and publishes document.failed in its OWN
// tenant-scoped tx (separate from the failed happy-path tx, which rolled back). It is
// best-effort: its own error is logged into the returned error chain only implicitly — the
// caller always returns the ORIGINAL cause, so a failure to record failure never masks the real
// problem, and the next delivery re-attempts both.
func (uc *UseCase) recordFailure(ctx context.Context, ev DocumentExtracted, cause error) {
	_ = uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if err := uc.repo.SetFailed(ctx, tx, ev.DocumentID, ev.TenantID, stageIndexing, cause.Error()); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newDocumentFailed(ev, cause))
	})
}

// isTerminal reports whether a use-case error can never succeed on redelivery, so the listener
// archives the task (asynq.SkipRetry) instead of burning the retry budget. Mirrors
// internal/deadline: a KindInvalid (malformed extracted-text JSON, an embedder contract
// violation) is terminal; a KindNotFound (the document row is gone) is terminal. Everything else
// (KindInfra/KindUnavailable — a flaky storage read or Voyage call) stays retryable.
func isTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindNotFound
}
