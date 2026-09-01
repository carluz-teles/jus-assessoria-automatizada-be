package pieceprofile

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/pieceprofile/pieceprofiledb"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types (uuid.UUID, pgtype.*) die: the entity
// and the use case stay pure Go. The repo returns *PieceProfile/*ProfileSection/...,
// never the sqlc row.

// derefString collapses a nullable text column (*string) to a plain string, "" standing
// in for SQL NULL.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textToNull is the inverse: an empty string is written as SQL NULL, not "".
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// timestamptzToTime collapses a pgtype.Timestamptz to a plain time.Time, the zero
// value standing in for SQL NULL.
func timestamptzToTime(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

// pgTimestamptz lifts a wall-clock time to a valid pgtype.Timestamptz (vigente_desde
// is always set at CreateVersion time, never NULL).
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// parseUUID parses an id that came from a path param or a prior DB row. A malformed
// value is an infra fault (the handler validates well-formed uuids do not reach here
// in the happy path, but a foreign/garbled id must not panic), wrapped so the edge
// treats it as 500 and the cause is logged.
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, database.WrapInfra(err)
	}
	return id, nil
}

func profileFromRow(r pieceprofiledb.PieceProfile) *PieceProfile {
	return &PieceProfile{
		Key:              r.Key,
		Nome:             r.Nome,
		Polo:             r.Polo,
		MatterKey:        r.MatterKey,
		BaseSkeletonKey:  r.BaseSkeletonKey,
		FormatProfileKey: derefString(r.FormatProfileKey),
		VersionAtual:     r.VersionAtual,
		FonteLegal:       r.FonteLegal,
		CreatedAt:        timestamptzToTime(r.CreatedAt),
		UpdatedAt:        timestamptzToTime(r.UpdatedAt),
	}
}

func sectionFromRow(r pieceprofiledb.ProfileSection) *ProfileSection {
	return &ProfileSection{
		ID:              r.ID.String(),
		PieceProfileKey: r.PieceProfileKey,
		Key:             r.Key,
		Titulo:          r.Titulo,
		Ordem:           int(r.Ordem),
		Obrigatoria:     r.Obrigatoria,
		Origem:          r.Origem,
		AceitaTeses:     r.AceitaTeses,
		FonteLegal:      textToBytes(r.FonteLegal),
	}
}

func requirementFromRow(r pieceprofiledb.ProfileRequirement) *ProfileRequirement {
	return &ProfileRequirement{
		ID:              r.ID.String(),
		PieceProfileKey: r.PieceProfileKey,
		Campo:           r.Campo,
		Obrigatorio:     r.Obrigatorio,
		FonteLegal:      textToBytes(r.FonteLegal),
	}
}

func versionFromRow(r pieceprofiledb.PieceProfileVersion) *PieceProfileVersion {
	return &PieceProfileVersion{
		ID:              r.ID.String(),
		PieceProfileKey: r.PieceProfileKey,
		Version:         r.Version,
		VigenteDesde:    timestamptzToTime(r.VigenteDesde),
		Snapshot:        r.Snapshot,
	}
}

func matterFromRow(r pieceprofiledb.Matter) *Matter {
	return &Matter{Key: r.Key, Nome: r.Nome}
}

func baseSkeletonFromRow(r pieceprofiledb.BaseSkeleton) *BaseSkeleton {
	return &BaseSkeleton{Key: r.Key, Slots: r.Slots}
}

func formatProfileFromRow(r pieceprofiledb.FormatProfile) *FormatProfile {
	return &FormatProfile{
		Key:                 r.Key,
		Fonte:               r.Fonte,
		TamanhoCorpo:        int(r.TamanhoCorpo),
		TamanhoCitacaoLonga: int(r.TamanhoCitacaoLonga),
		Espacamento:         r.Espacamento,
		Alinhamento:         r.Alinhamento,
		Margens:             r.Margens,
		CitacaoLonga:        r.CitacaoLonga,
		Export:              r.Export,
	}
}

// textToBytes lifts a nullable text column (fonte_legal on profile_section/
// profile_requirement is `text`, not `jsonb`) to the entity's []byte field, nil
// standing in for SQL NULL.
func textToBytes(s *string) []byte {
	if s == nil {
		return nil
	}
	return []byte(*s)
}

// bytesToText is the inverse: an empty/nil []byte is written as SQL NULL.
func bytesToText(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	return &s
}
