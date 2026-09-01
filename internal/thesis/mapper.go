package thesis

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/thesis/thesisdb"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types (uuid.UUID, pgtype.*) die: the
// entity and the use case stay pure Go. The repo returns *Thesis/*ThesisAnchor/...,
// never the sqlc row.

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timestamptzToTime(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

// parseUUID parses a REQUIRED id (never SQL NULL). A malformed value is an infra
// fault, wrapped so the edge treats it as 500 and the cause is logged.
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, database.WrapInfra(err)
	}
	return id, nil
}

// pgOptionalUUID lifts an OPTIONAL id string to a pgtype.UUID: "" is SQL NULL,
// anything else is parsed. A malformed non-empty value degrades to NULL rather than
// failing the whole write — these are optional cross-references
// (thesis.piece_profile_key's sibling notification_id, thesis_anchor.alvo_documento,
// draft_segment.profile_section_id), never the row's own required identity.
func pgOptionalUUID(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

// uuidText collapses a nullable uuid column (pgtype.UUID) to a plain string, ""
// standing in for SQL NULL.
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func thesisFromRow(r thesisdb.Thesis) *Thesis {
	return &Thesis{
		ID:              r.ID.String(),
		TenantID:        r.TenantID.String(),
		DraftID:         r.DraftID.String(),
		PieceProfileKey: derefString(r.PieceProfileKey),
		NotificationID:  uuidText(r.NotificationID),
		Enunciado:       r.Enunciado,
		Forca:           r.Forca,
		Estado:          r.Estado,
		CreatedAt:       timestamptzToTime(r.CreatedAt),
	}
}

func thesisAnchorFromRow(r thesisdb.ThesisAnchor) *ThesisAnchor {
	return &ThesisAnchor{
		ID:            r.ID.String(),
		ThesisID:      r.ThesisID.String(),
		Tipo:          r.Tipo,
		AlvoDocumento: uuidText(r.AlvoDocumento),
		AlvoFonte:     derefString(r.AlvoFonte),
		Motivo:        r.Motivo,
		Status:        r.Status,
	}
}

func draftSegmentFromRow(r thesisdb.DraftSegment) *DraftSegment {
	return &DraftSegment{
		ID:               r.ID.String(),
		TenantID:         r.TenantID.String(),
		DraftID:          r.DraftID.String(),
		ThesisID:         r.ThesisID.String(),
		ProfileSectionID: uuidText(r.ProfileSectionID),
		Conteudo:         r.Conteudo,
		CreatedAt:        timestamptzToTime(r.CreatedAt),
	}
}

func segmentAnchorFromRow(r thesisdb.SegmentAnchor) *SegmentAnchor {
	return &SegmentAnchor{
		ID:             r.ID.String(),
		DraftSegmentID: r.DraftSegmentID.String(),
		ThesisAnchorID: r.ThesisAnchorID.String(),
		Status:         r.Status,
	}
}

func thesisCoverageFromRow(r thesisdb.ThesisCoverage) *ThesisCoverage {
	return &ThesisCoverage{
		ID:        r.ID.String(),
		ThesisID:  r.ThesisID.String(),
		Resultado: r.Resultado,
		Detalhe:   derefString(r.Detalhe),
		CreatedAt: timestamptzToTime(r.CreatedAt),
	}
}
