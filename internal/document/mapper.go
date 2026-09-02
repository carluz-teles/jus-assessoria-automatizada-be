package document

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/document/documentdb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types die (docs §4b.3): uuid.UUID, pgtype.* and the
// nullable *string/*int columns are absorbed here so the entity, the use case and the read
// models stay pure Go. The repo returns *Document / *View, never the sqlc row.

// derefString collapses a nullable text column (*string) to a plain string, "" standing in for
// SQL NULL (an absent title/mime_type/checksum/original_filename).
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textToNull is the inverse: an empty string is written as SQL NULL, not "". An absent title /
// checksum / mime_type is NULL in the row, not an empty string.
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefInt32 collapses a nullable int column (*int32) to a plain int, 0 standing in for SQL
// NULL (pages before extraction fills it).
func derefInt32(n *int32) int {
	if n == nil {
		return 0
	}
	return int(*n)
}

// derefInt64 collapses a nullable bigint column (*int64) to a plain int64, 0 standing in for
// SQL NULL (size_bytes on a row where the client omitted it).
func derefInt64(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// int64ToNull lifts an OPTIONAL size to a *int64: 0 is written as SQL NULL (size unknown),
// anything else is the value.
func int64ToNull(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// parseUUID parses an id that came from the verified request path/body or a prior DB row. A
// malformed value on the request path is validated at the edge, so reaching here with one is an
// infra fault (wrapped so the edge treats it as 500).
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, database.WrapInfra(err)
	}
	return id, nil
}

// pgOptionalUUID lifts an OPTIONAL id to a pgtype.UUID: an empty string is SQL NULL (an avulsa
// upload's absent court_record_id), anything else is parsed.
func pgOptionalUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	id, err := parseUUID(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}, nil
}

// uuidText collapses a nullable uuid column (pgtype.UUID) to a plain string, "" standing in for
// SQL NULL (an avulsa document's absent court_record_id).
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// cursorTimeLayout is the wire layout of the keyset cursor's created_at value: RFC3339 with
// microsecond precision (Postgres timestamptz resolution), so the round-trip through the opaque
// cursor is lossless and the next-page keyset predicate resumes exactly.
const cursorTimeLayout = "2006-01-02T15:04:05.999999Z07:00"

// parseCursorTime parses the keyset cursor's created_at value (always present — the handler
// fills the max sentinel for the first page) into a time.Time. A malformed value is a bad query
// param (a decoded cursor field), so it maps to KindInvalid (→ 400) via the edge, never a 500.
func parseCursorTime(s string) (time.Time, error) {
	t, err := time.Parse(cursorTimeLayout, s)
	if err != nil {
		return time.Time{}, apperr.NewInvalid("invalid cursor")
	}
	return t, nil
}

// pgTimestamptz lifts a wall-clock time to a valid pgtype.Timestamptz — for deleted_at, stamped
// at the soft delete (never NULL on that write path).
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// timeToTimestamptz lifts an OPTIONAL time to a pgtype.Timestamptz: a zero time is SQL NULL
// (a human UPLOAD carries no court event date), anything else is the value. The inverse of
// timestamptzToTime.
func timeToTimestamptz(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// timestamptzToTime collapses a nullable timestamptz column to a plain time.Time, the zero
// time standing in for SQL NULL (court_event_date on a human UPLOAD, or a COURT doc fetched
// before the column existed).
func timestamptzToTime(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

// documentFromInsertRow maps the InsertDocument RETURNING row into the pure *Document entity.
func documentFromInsertRow(row documentdb.InsertDocumentRow) *Document {
	return &Document{
		ID:               row.ID.String(),
		TenantID:         row.TenantID.String(),
		CourtRecordID:    uuidText(row.CourtRecordID),
		DocumentType:     row.DocumentType,
		Origin:           Origin(row.Origin),
		StorageKey:       derefString(row.StorageKey),
		Pages:            derefInt32(row.Pages),
		HasTextLayer:     row.HasTextLayer,
		MimeType:         derefString(row.MimeType),
		SizeBytes:        derefInt64(row.SizeBytes),
		Checksum:         derefString(row.Checksum),
		Title:            derefString(row.Title),
		OriginalFilename: derefString(row.OriginalFilename),
		Status:           Status(row.Status),
		CreatedAt:        row.CreatedAt.Time,
		CourtEventDate:   timestamptzToTime(row.CourtEventDate),
	}
}

// documentFromUploadedRow maps the MarkDocumentUploaded RETURNING row into the pure *Document
// entity (same projection as the insert row).
func documentFromUploadedRow(row documentdb.MarkDocumentUploadedRow) *Document {
	return &Document{
		ID:               row.ID.String(),
		TenantID:         row.TenantID.String(),
		CourtRecordID:    uuidText(row.CourtRecordID),
		DocumentType:     row.DocumentType,
		Origin:           Origin(row.Origin),
		StorageKey:       derefString(row.StorageKey),
		Pages:            derefInt32(row.Pages),
		HasTextLayer:     row.HasTextLayer,
		MimeType:         derefString(row.MimeType),
		SizeBytes:        derefInt64(row.SizeBytes),
		Checksum:         derefString(row.Checksum),
		Title:            derefString(row.Title),
		OriginalFilename: derefString(row.OriginalFilename),
		Status:           Status(row.Status),
		CreatedAt:        row.CreatedAt.Time,
	}
}

// viewFromListRow maps one ListDocumentsByProcesso row into the DocumentView wire shape.
func viewFromListRow(row documentdb.ListDocumentsByProcessoRow) DocumentView {
	return DocumentView{
		ID:               row.ID.String(),
		CourtRecordID:    uuidText(row.CourtRecordID),
		DocumentType:     row.DocumentType,
		Origin:           row.Origin,
		Title:            derefString(row.Title),
		OriginalFilename: derefString(row.OriginalFilename),
		MimeType:         derefString(row.MimeType),
		SizeBytes:        derefInt64(row.SizeBytes),
		Pages:            derefInt32(row.Pages),
		Status:           row.Status,
		HasTextLayer:     row.HasTextLayer,
		Checksum:         derefString(row.Checksum),
		CreatedAt:        row.CreatedAt.Time,
	}
}

// viewFromGetRow maps the GetDocument row into the DocumentView wire shape (same projection as
// the list row).
func viewFromGetRow(row documentdb.GetDocumentRow) DocumentView {
	return DocumentView{
		ID:               row.ID.String(),
		CourtRecordID:    uuidText(row.CourtRecordID),
		DocumentType:     row.DocumentType,
		Origin:           row.Origin,
		Title:            derefString(row.Title),
		OriginalFilename: derefString(row.OriginalFilename),
		MimeType:         derefString(row.MimeType),
		SizeBytes:        derefInt64(row.SizeBytes),
		Pages:            derefInt32(row.Pages),
		Status:           row.Status,
		HasTextLayer:     row.HasTextLayer,
		Checksum:         derefString(row.Checksum),
		CreatedAt:        row.CreatedAt.Time,
	}
}
