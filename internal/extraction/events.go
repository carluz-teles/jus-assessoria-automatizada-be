package extraction

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/document"
	"github.com/jusassessoria/platform/lib/events"
)

// aggregateTypeDocument places extraction facts on the DOCUMENT aggregate (its stream
// orders by the document id) — the same aggregate the document slice's document.uploaded
// rides, so the whole pipeline for one file lives on one stream.
const aggregateTypeDocument = "document"

// typeDocumentExtracted / typeDocumentFailed are the dotted ids this slice PRODUCES. The
// consumed id (document.uploaded) is imported from the document slice as the pinned
// contract — this slice never re-declares another slice's produced name.
const (
	typeDocumentExtracted = "document.extracted"
	typeDocumentFailed    = "document.failed"
)

// DocumentUploaded is the LOCAL decode shape of the document.uploaded event this slice
// consumes. It mirrors the pinned wire contract (event_id, aggregate_id, document_id,
// tenant_id, court_record_id?, storage_key, mime_type?): TenantID scopes the RLS tx,
// StorageKey fetches the PDF bytes, and EventID is the dedup key. It is a decode-only
// struct (never published from here), so it carries no Event methods.
type DocumentUploaded struct {
	events.Base
	DocumentID    string `json:"document_id"`
	TenantID      string `json:"tenant_id"`
	CourtRecordID string `json:"court_record_id,omitempty"`
	StorageKey    string `json:"storage_key"`
	MimeType      string `json:"mime_type,omitempty"`
}

// DocumentExtracted announces a document's text has been extracted (text layer or OCR) and
// persisted to storage. It carries the text_key (where the per-page text JSON landed),
// pages, has_text_layer (false = OCR produced the text) and extractor_version — the facts
// the indexing slice needs to read the text back and chunk/embed it. TenantID scopes the
// consumer's RLS; CourtRecordID is optional (an avulsa upload hangs on no process). The
// aggregate is the document, so its stream orders by the document id.
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

var _ events.Event = DocumentExtracted{}

func (DocumentExtracted) Type() string          { return typeDocumentExtracted }
func (DocumentExtracted) AggregateType() string { return aggregateTypeDocument }

// newDocumentExtracted builds the produced fact from the consumed event + the extraction
// outcome. aggregate_id is the document id (a uuid, satisfying the outbox's uuid NOT NULL);
// the event id is a fresh uuid v7 (the consumer dedup key), mirroring how the document +
// deadline slices mint their ids.
func newDocumentExtracted(ev DocumentUploaded, textKey string, pages int, hasTextLayer bool, version string) DocumentExtracted {
	return DocumentExtracted{
		Base:             events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: ev.Aggregate},
		DocumentID:       ev.DocumentID,
		TenantID:         ev.TenantID,
		CourtRecordID:    ev.CourtRecordID,
		TextKey:          textKey,
		Pages:            pages,
		HasTextLayer:     hasTextLayer,
		ExtractorVersion: version,
	}
}

// DocumentFailed announces a stage fault in the extraction pipeline. It carries the fixed
// stage ("extraction") and the human-readable error so the UI can show "reprocessar" and
// an operator can triage without reading the row. It is emitted in the SAME tx that flips
// the row to FAILED (the effect and the fact commit together).
type DocumentFailed struct {
	events.Base
	DocumentID    string `json:"document_id"`
	TenantID      string `json:"tenant_id"`
	CourtRecordID string `json:"court_record_id,omitempty"`
	Stage         string `json:"stage"`
	Error         string `json:"error"`
}

var _ events.Event = DocumentFailed{}

func (DocumentFailed) Type() string          { return typeDocumentFailed }
func (DocumentFailed) AggregateType() string { return aggregateTypeDocument }

// newDocumentFailed builds the failure fact from the consumed event + the error message.
// stage is pinned to "extraction"; the event id is a fresh uuid v7, the aggregate the
// document id — the same shape as the success fact.
func newDocumentFailed(ev DocumentUploaded, msg string) DocumentFailed {
	return DocumentFailed{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: ev.Aggregate},
		DocumentID:    ev.DocumentID,
		TenantID:      ev.TenantID,
		CourtRecordID: ev.CourtRecordID,
		Stage:         stageExtraction,
		Error:         msg,
	}
}

// typeDocumentUploaded re-exports the document slice's produced id so the listener mounts
// on the exact name the document slice emits — a rename there is caught at compile time
// here, not silently at runtime.
const typeDocumentUploaded = document.TypeDocumentUploaded
