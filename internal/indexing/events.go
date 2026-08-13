package indexing

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

// events.go is the indexing fatia's event surface: the ONE fact it CONSUMES (document.extracted,
// produced by the extraction fatia) and the TWO it PRODUCES (document.ready on success,
// document.failed on error). The consumed shape is a LOCAL struct that mirrors the extraction
// agent's pinned contract — this slice never imports internal/extraction (no cross-slice import).

// aggregateTypeDocument places the produced document.* facts on the DOCUMENT aggregate (its
// stream orders by the document id), matching how internal/document tags document.uploaded.
const aggregateTypeDocument = "document"

// Consumed task types --------------------------------------------------------

// TypeDocumentExtracted is the dotted id this slice CONSUMES: the extraction fatia emits it once
// a document's per-page text has been written to object storage. It kicks off chunking +
// embedding + indexing. The prefix is "document" (its stream), matching document.uploaded.
const TypeDocumentExtracted = "document.extracted"

// DocumentExtracted is the LOCAL decode shape for the consumed document.extracted, mirroring the
// extraction agent's pinned producer contract exactly. It carries what the indexing pipeline
// needs without reading the row back: the document + tenant (TenantID scopes the RLS tx — the
// worker has no Clerk token), the optional origin process, the TextKey to fetch the per-page
// extracted text from storage, the page count and the extractor version. Base carries the event
// id (consumer dedup) and the aggregate (document) id.
type DocumentExtracted struct {
	events.Base
	DocumentID       string `json:"document_id"`
	TenantID         string `json:"tenant_id"`
	CourtRecordID    string `json:"court_record_id,omitempty"`
	TextKey          string `json:"text_key"`
	Pages            int    `json:"pages"`
	HasTextLayer     bool   `json:"has_text_layer"`
	ExtractorVersion string `json:"extractor_version"`
}

// ExtractedText is the JSON document at TextKey in object storage: the per-page extracted text
// the extraction fatia wrote. The indexing pipeline reads it, then chunks each page's text.
type ExtractedText struct {
	ExtractorVersion string          `json:"extractor_version"`
	Pages            []ExtractedPage `json:"pages"`
}

// ExtractedPage is one page's extracted text inside ExtractedText: its 1-based page number and
// the text the chunker slices into windows (each window keeps this page number).
type ExtractedPage struct {
	Page int    `json:"page"`
	Text string `json:"text"`
}

// Produced events ------------------------------------------------------------

// TypeDocumentReady is the dotted id this slice PRODUCES on success: a document's chunks are
// embedded and indexed and the row is READY (the advisory may now cite it). Its "document"
// prefix keeps it on the document stream.
const TypeDocumentReady = "document.ready"

// TypeDocumentFailed is the dotted id this slice PRODUCES on error: indexing failed for a
// document (read/chunk/embed/persist). It carries the stage ("indexing") and the error message
// so the UI can show "reprocessar".
const TypeDocumentFailed = "document.failed"

// DocumentReady announces a fully indexed document. It carries the document + tenant, the number
// of chunks indexed (ChunkCount) and the embedding model that produced their vectors
// (EmbeddingModel) — the provenance the advisory/retrieval consumer wants without reading the
// rows back. The aggregate is the document; the event id is a fresh uuid v7 (the consumer dedup
// key), mirroring how internal/document mints document.uploaded.
type DocumentReady struct {
	events.Base
	DocumentID     string `json:"document_id"`
	TenantID       string `json:"tenant_id"`
	ChunkCount     int    `json:"chunk_count"`
	EmbeddingModel string `json:"embedding_model"`
}

var _ events.Event = DocumentReady{}

func (DocumentReady) Type() string          { return TypeDocumentReady }
func (DocumentReady) AggregateType() string { return aggregateTypeDocument }

// newDocumentReady builds the produced success event from the extracted event + the indexing
// outcome. aggregate_id is the document id (a uuid, satisfying the outbox's uuid NOT NULL); the
// event id is a fresh uuid v7 (the consumer dedup key).
func newDocumentReady(ev DocumentExtracted, chunkCount int, embeddingModel string) DocumentReady {
	return DocumentReady{
		Base:           events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: ev.DocumentID},
		DocumentID:     ev.DocumentID,
		TenantID:       ev.TenantID,
		ChunkCount:     chunkCount,
		EmbeddingModel: embeddingModel,
	}
}

// DocumentFailed announces that indexing failed for a document. Stage is always "indexing" (the
// fatia's name in the pipeline), and Error is the failure's message (for the UI's "reprocessar"
// affordance). The aggregate is the document; the event id is a fresh uuid v7.
type DocumentFailed struct {
	events.Base
	DocumentID string `json:"document_id"`
	TenantID   string `json:"tenant_id"`
	Stage      string `json:"stage"`
	Error      string `json:"error"`
}

var _ events.Event = DocumentFailed{}

func (DocumentFailed) Type() string          { return TypeDocumentFailed }
func (DocumentFailed) AggregateType() string { return aggregateTypeDocument }

// stageIndexing is the fixed stage tag on every document.failed this slice emits — the pipeline
// step (chunking/embedding/indexing) collapsed to one milestone name for the UI.
const stageIndexing = "indexing"

// newDocumentFailed builds the produced failure event from the extracted event + the error. The
// event id is a fresh uuid v7 (a distinct fact from the success path, so a later ready never
// dedups against a prior failed).
func newDocumentFailed(ev DocumentExtracted, cause error) DocumentFailed {
	return DocumentFailed{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: ev.DocumentID},
		DocumentID: ev.DocumentID,
		TenantID:   ev.TenantID,
		Stage:      stageIndexing,
		Error:      cause.Error(),
	}
}
