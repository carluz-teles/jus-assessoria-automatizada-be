package deadline

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types die (docs §4b.3): uuid.UUID, pgtype.*
// and the raw jsonb bytes are absorbed here so the entity and the use case stay pure
// Go. The repo returns *Deadline, never the sqlc row.
//
// Quirk it absorbs: deadline.notification_id references intimation(id) — the column
// keeps its pre-rename name (migration 0006 renamed the table notification → intimation
// but not the FK column), so the domain's IntimationID maps to the notification_id
// param. Documented once here so no other layer has to know.

// derefString collapses a nullable text column (*string) to a plain string, "" standing
// in for SQL NULL (an absent class, a NULL kind).
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textToNull is the inverse: an empty string is written as SQL NULL, not "". doubled_reason
// is NULL in this slice (the dobro reason is a later, human-confirmed concern).
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// pgDate lifts a civil date to a pgtype.Date. The Calendar works on date-only values,
// so the time-of-day is irrelevant; Valid is always true here (the dates are computed,
// never NULL).
func pgDate(t time.Time) pgtype.Date {
	return pgtype.Date{Time: t, Valid: true}
}

// parseUUID parses an id that came from a decoded event or a prior DB row. A malformed
// value is an infra fault (never client input on this async path), wrapped so the edge
// treats it as 500 and the cause is logged.
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, database.WrapInfra(err)
	}
	return id, nil
}

// pgUUID lifts a REQUIRED id string to a valid pgtype.UUID — for a nullable uuid column
// (e.g. task.deadline_id/court_record_id/created_by) that confirm always fills. A
// malformed value is the same infra fault parseUUID reports.
func pgUUID(s string) (pgtype.UUID, error) {
	id, err := parseUUID(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}, nil
}

// pgOptionalUUID lifts an OPTIONAL id to a pgtype.UUID: an empty string is SQL NULL
// (the invalid zero value), anything else is parsed. task.assignee_user_id is the case —
// a task may be unassigned.
func pgOptionalUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	return pgUUID(s)
}

// pgOptionalDate lifts an OPTIONAL civil date to a pgtype.Date: nil is SQL NULL,
// otherwise the date (time-of-day irrelevant). task.due_date is the case — a task may
// carry no own date.
func pgOptionalDate(t *time.Time) pgtype.Date {
	if t == nil {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: *t, Valid: true}
}

// pgTimestamptz lifts a wall-clock time to a valid pgtype.Timestamptz — for confirmed_at,
// stamped at confirmation (never NULL on this path).
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// pgOptionalTimestamptz lifts an OPTIONAL wall-clock time to a pgtype.Timestamptz: nil is
// SQL NULL, otherwise the timestamp. task.completed_at is the case — set on DONE (a real
// time), left NULL on DISMISSED (dispensar is not a completion).
func pgOptionalTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// uuidText collapses a nullable uuid column (pgtype.UUID) to a plain string, "" standing in
// for SQL NULL (an avulsa task's absent court_record_id/deadline_id, an unassigned task). The
// task read/write mappers use it for the nullable FKs the entity carries as strings.
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// datePtr collapses a nullable date column (pgtype.Date) to an optional time, nil standing in
// for SQL NULL. task.due_date is the case — a task may carry no own date.
func datePtr(d pgtype.Date) *time.Time {
	if !d.Valid {
		return nil
	}
	t := d.Time
	return &t
}

// timestampPtr collapses a nullable timestamp column (pgtype.Timestamptz) to an optional time,
// nil standing in for SQL NULL. task.completed_at is the case — NULL while OPEN/DISMISSED.
func timestampPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// optionalBool lifts a Go bool to a *bool for a nullable boolean column. calc_memory
// has a DEFAULT true on dias_uteis, but the mapper provides the value explicitly so the
// column is never left to the default on an insert path.
func optionalBool(b bool) *bool {
	return &b
}

// derefBool collapses a nullable boolean column (*bool) to a plain bool, false standing in
// for SQL NULL — the read-side counterpart of optionalBool (calc_memory.dias_uteis is nullable
// on a LEFT JOIN, since a pre-calc_memory prazo has no row at all).
func derefBool(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}

// derefFloat64 collapses a nullable double precision column (*float64) to a plain float64, 0
// standing in for SQL NULL — calc_memory.ia_confianca is NULL until the IA classifier runs.
func derefFloat64(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// derefInt32 collapses a nullable int4 column (*int32) to a plain int, 0 standing in for SQL
// NULL — cross_validation.dif_dias is NULL until the cross-validation is computed.
func derefInt32(i *int32) int {
	if i == nil {
		return 0
	}
	return int(*i)
}

// marshalHolidays encodes the skipped-days audit as a jsonb array of "2006-01-02"
// strings — human-legible in the row (the whole point of holidays_applied: "por que
// dia 14?"). An empty slice becomes "[]" so the column never holds JSON null.
func marshalHolidays(days []time.Time) ([]byte, error) {
	out := make([]string, len(days))
	for i, d := range days {
		out[i] = d.Format(time.DateOnly)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return b, nil
}
