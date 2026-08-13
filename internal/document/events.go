package document

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

// TypeDocumentUploaded is the dotted id this slice PRODUCES when a document's bytes are
// confirmed landed (POST /v1/documentos/:id/complete, PENDING→UPLOADED). Its "document"
// prefix routes it to the default work at the relay; its consumer is the extraction fatia
// (it kicks off the extract/OCR pipeline), so until that lands it is an orphan — which is
// safe (nothing fires it in prod without a real complete).
const TypeDocumentUploaded = "document.uploaded"

// aggregateTypeDocument places document.* facts on the DOCUMENT aggregate (its stream orders
// by the document id).
const aggregateTypeDocument = "document"

// DocumentUploaded announces a freshly uploaded document (bytes confirmed in storage). It
// carries what the extraction consumer needs without reading the row back: the document + its
// origin process (CourtRecordID, "" for an avulsa upload), the StorageKey to fetch the bytes
// and the MimeType to pick the extractor. TenantID scopes the future consumer's RLS tx/read
// (the outbox carries no tenant, so the payload is the only channel), mirroring how the
// deadline scheduled events carry it. The aggregate is the document, so its stream orders by
// the document id; Base carries the event id (consumer dedup) and that aggregate id.
type DocumentUploaded struct {
	events.Base
	DocumentID    string `json:"document_id"`
	TenantID      string `json:"tenant_id"`
	CourtRecordID string `json:"court_record_id,omitempty"`
	StorageKey    string `json:"storage_key"`
	MimeType      string `json:"mime_type,omitempty"`
}

var _ events.Event = DocumentUploaded{}

func (DocumentUploaded) Type() string          { return TypeDocumentUploaded }
func (DocumentUploaded) AggregateType() string { return aggregateTypeDocument }

// newDocumentUploaded builds the produced event from the completed document. aggregate_id is
// the document id (a uuid, satisfying the outbox's uuid NOT NULL); the event id is a fresh
// uuid v7 (the consumer dedup key), mirroring how the deadline slice mints its ids.
func newDocumentUploaded(d *Document) DocumentUploaded {
	return DocumentUploaded{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: d.ID},
		DocumentID:    d.ID,
		TenantID:      d.TenantID,
		CourtRecordID: d.CourtRecordID,
		StorageKey:    d.StorageKey,
		MimeType:      d.MimeType,
	}
}
