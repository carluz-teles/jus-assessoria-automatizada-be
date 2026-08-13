// Package extraction is the text-extraction + OCR slice of the Documentos milestone
// (Fatia 5+6). It is driven entirely by events — there is no HTTP surface. It CONSUMES
// document.uploaded (produced by the document slice when a file's bytes land) and, per
// document, runs a two-adapter TextExtractor port: a pure-Go PDF text-layer reader
// (Fatia 5) and, when the PDF has no usable text layer, a Claude-vision OCR fallback
// (Fatia 6). The per-page text is persisted to object storage at
// <storage_key>.text.json (NOT the DB, NOT the event); the document row's saga advances
// UPLOADED→EXTRACTING→EXTRACTED, and document.extracted is emitted on the transactional
// outbox for the indexing slice to consume. On any stage error the row goes FAILED and
// document.failed is emitted.
//
// It mirrors internal/deadline's listener → use case shape: a tenant-scoped write tx
// (RLS via SET LOCAL app.tenant_id), processed_event idempotency, and the outbox row
// committing in the SAME tx as the effect. It depends only on ports (extractor,
// storage, outbox, dedup, uow) — never a concrete implementation — so the worker
// composes it and tests substitute fakes.
package extraction

import "context"

// consumerExtraction is the processed_event consumer name this slice dedups under. Dedup
// is per-consumer (docs §4c.3), so it is slice-specific — marking here never blocks
// another consumer of the same document.uploaded event.
const consumerExtraction = "extraction"

// stageExtraction is the value the document.failed event and the document.error jsonb
// carry for a fault in this slice. The pipeline has one stage per fatia; this slice owns
// "extraction" (text-layer + OCR are the same stage — both produce the text_key).
const stageExtraction = "extraction"

// PageText is one page's extracted text — the unit both adapters return and the unit
// persisted to the text JSON. Page is 1-based (mirrors the PDF page number the OCR and
// the text-layer reader iterate).
type PageText struct {
	Page int    `json:"page"`
	Text string `json:"text"`
}

// TextExtractor is the port the use case calls to turn PDF bytes into per-page text. Two
// adapters sit behind it: the text-layer reader (Fatia 5) and the Claude-vision OCR
// (Fatia 6), composed by a dispatcher that tries the text layer first and falls back to
// OCR when hasTextLayer is false. version encodes which adapter+model produced the text
// (e.g. "pdftext-v1" / "claude-ocr-opus-4-8"), stamped onto the document row and the
// document.extracted event so a re-extraction is auditable.
type TextExtractor interface {
	Extract(ctx context.Context, pdf []byte) (pages []PageText, hasTextLayer bool, version string, err error)
}

// textDocument is the JSON object persisted to <storage_key>.text.json — the pinned
// intermediate text contract the indexing slice reads back to chunk + embed. It carries
// the extractor version (so the indexer knows what produced the text) and the per-page
// text; the DB and the event stay text-free (only the key travels).
type textDocument struct {
	ExtractorVersion string     `json:"extractor_version"`
	Pages            []PageText `json:"pages"`
}
