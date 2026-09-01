package actionitem

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/actionitem/actionitemdb"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types (uuid.UUID, pgtype.*) die: the entity and
// the use case stay pure Go. The repo returns *ActionItem, never the sqlc row.
//
// Every query in queries/actionitem.sql projects the FULL, same-order action_item column
// list, so sqlc generates a single row shape (actionitemdb.ActionItem) for Insert/Get/
// Confirm/Discard alike — one fromRow below covers them all.

// parseUUID parses a REQUIRED id. A malformed value is an infra fault (never client input
// on the ids this slice controls itself), wrapped so the edge treats it as 500.
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, database.WrapInfra(err)
	}
	return id, nil
}

// pgOptionalUUID lifts an OPTIONAL id to a pgtype.UUID: "" is SQL NULL, anything else is
// parsed. court_record_id/deadline_id are the case — a providência may not carry either yet.
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

// uuidText collapses a nullable uuid column to a plain string, "" standing in for SQL NULL.
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// textToNull is the inverse of a plain-string deref: "" is written as SQL NULL.
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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

// fromRow maps a generated actionitemdb.ActionItem row into the pure-Go aggregate.
func fromRow(r actionitemdb.ActionItem) *ActionItem {
	return &ActionItem{
		ID:              r.ID.String(),
		TenantID:        r.TenantID.String(),
		IntimationID:    r.IntimationID.String(),
		CourtRecordID:   uuidText(r.CourtRecordID),
		Tipo:            r.Tipo,
		GeraPeca:        r.GeraPeca,
		PieceProfileKey: derefString(r.PieceProfileKey),
		TipoOrigem:      TipoOrigem(r.TipoOrigem),
		TipoStatus:      TipoStatus(r.TipoStatus),
		DeadlineID:      uuidText(r.DeadlineID),
		Confianca:       r.Confianca,
		Status:          Status(r.Status),
		TaskID:          uuidText(r.TaskID),
		CreatedAt:       timestamptzToTime(r.CreatedAt),
		UpdatedAt:       timestamptzToTime(r.UpdatedAt),
	}
}
