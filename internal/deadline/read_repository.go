package deadline

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/deadline/deadlinedb"
	"github.com/jusassessoria/platform/lib/database"
)

// read_repository.go is the pool-backed read adapter — the screen reads, off the
// transactional write path (which uses the stateless, tx-bound pgRepository). It holds
// its own deadlinedb bound to the pool; every query filters tenant_id explicitly
// (barrier 1) from the trusted principal, so a caller only ever sees its own prazos.
// The mapper here absorbs the driver types (pgtype.Date, the jsonb bytes, the
// interface{} sqlc infers for the confirmed expression) so the read models stay pure.

// pgReadRepository serves the read port off the connection pool. Reads are not part of
// the use case's write tx, so the repo owns its own Queries (bound once at construction).
type pgReadRepository struct {
	q *deadlinedb.Queries
}

var _ readRepo = (*pgReadRepository)(nil)

// NewReadRepository returns the read port over the pool. Share nothing with the write
// repo: the read side never enrolls in the write transaction.
func NewReadRepository(pool deadlinedb.DBTX) readRepo {
	return &pgReadRepository{q: deadlinedb.New(pool)}
}

// ListPrazosByProcesso reads one process's prazos (ascending keyset by end_date, soonest
// first) on the pool, filtered by tenant_id and court_record_id. The caller passes the
// min sentinel cursor for the first page.
func (r *pgReadRepository) ListPrazosByProcesso(ctx context.Context, q PrazosByProcessoQuery) ([]PrazoView, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return nil, err
	}
	crid, err := parseUUID(q.CourtRecordID)
	if err != nil {
		return nil, err
	}
	lastID, err := parseUUID(q.LastID)
	if err != nil {
		return nil, err
	}
	lastEnd, err := keysetDate(q.LastEnd)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListPrazosByProcesso(ctx, deadlinedb.ListPrazosByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
		LastEnd:       lastEnd,
		LastID:        lastID,
		PageLimit:     int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]PrazoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, PrazoView{
			ID:              row.ID.String(),
			Kind:            derefString(row.Kind),
			EndDate:         row.EndDate.Time,
			DaysLeft:        int(row.DaysLeft),
			Counting:        row.Counting,
			Doubled:         row.Doubled,
			DoubledReason:   derefString(row.DoubledReason),
			Status:          row.Status,
			HolidaysApplied: holidaysFromJSON(row.HolidaysApplied),
			IntimationID:    row.NotificationID.String(),
			Confirmed:       confirmedBool(row.Confirmed),
		})
	}
	return out, nil
}

// CountPrazosByProcesso returns the "X de Y" total for the Prazos tab, scoped by the
// same tenant + court_record as the list.
func (r *pgReadRepository) CountPrazosByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return 0, err
	}
	crid, err := parseUUID(courtRecordID)
	if err != nil {
		return 0, err
	}
	total, err := r.q.CountPrazosByProcesso(ctx, deadlinedb.CountPrazosByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

// ListPrazos reads the tenant's agenda (ascending keyset by end_date) on the pool, with
// the optional status/window filters applied. The process context (cnj/court) comes from
// the court_record join.
func (r *pgReadRepository) ListPrazos(ctx context.Context, q PrazosQuery) ([]AgendaPrazoView, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return nil, err
	}
	lastID, err := parseUUID(q.LastID)
	if err != nil {
		return nil, err
	}
	lastEnd, err := keysetDate(q.LastEnd)
	if err != nil {
		return nil, err
	}
	from, err := optionalFilterDate(q.From)
	if err != nil {
		return nil, err
	}
	to, err := optionalFilterDate(q.To)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListPrazos(ctx, deadlinedb.ListPrazosParams{
		TenantID:  tid,
		Status:    q.Status,
		FromDate:  from,
		ToDate:    to,
		LastEnd:   lastEnd,
		LastID:    lastID,
		PageLimit: int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]AgendaPrazoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, AgendaPrazoView{
			ID:              row.ID.String(),
			Kind:            derefString(row.Kind),
			EndDate:         row.EndDate.Time,
			DaysLeft:        int(row.DaysLeft),
			Counting:        row.Counting,
			Doubled:         row.Doubled,
			DoubledReason:   derefString(row.DoubledReason),
			Status:          row.Status,
			HolidaysApplied: holidaysFromJSON(row.HolidaysApplied),
			IntimationID:    row.NotificationID.String(),
			Confirmed:       confirmedBool(row.Confirmed),
			CourtRecordID:   row.CourtRecordID.String(),
			CNJNumber:       row.CnjNumber,
			Court:           row.Court,
		})
	}
	return out, nil
}

// CountPrazos returns the agenda's "X de Y": the filtered count (Status/window) and the
// tenant-wide count. When no filter is active the two coincide, so a single tenant COUNT
// fills both (mirrors the acquisition read model) — one query instead of two.
func (r *pgReadRepository) CountPrazos(ctx context.Context, q PrazosQuery) (int64, int64, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return 0, 0, err
	}

	total, err := r.q.CountPrazosByTenant(ctx, tid)
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}

	if q.Status == "" && q.From == "" && q.To == "" {
		return total, total, nil
	}

	from, err := optionalFilterDate(q.From)
	if err != nil {
		return 0, 0, err
	}
	to, err := optionalFilterDate(q.To)
	if err != nil {
		return 0, 0, err
	}
	totalCount, err := r.q.CountPrazos(ctx, deadlinedb.CountPrazosParams{
		TenantID: tid,
		Status:   q.Status,
		FromDate: from,
		ToDate:   to,
	})
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}
	return totalCount, total, nil
}

// GetPrazo reads one prazo's audit detail on the pool, filtered by tenant_id. A miss —
// or a foreign tenant's row — maps to the typed ErrDeadlineNotFound (→ 404), never
// (nil, nil).
func (r *pgReadRepository) GetPrazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return PrazoDetailView{}, err
	}
	pid, err := parseUUID(id)
	if err != nil {
		return PrazoDetailView{}, err
	}

	row, err := r.q.GetPrazo(ctx, deadlinedb.GetPrazoParams{ID: pid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return PrazoDetailView{}, ErrDeadlineNotFound
	}
	if err != nil {
		return PrazoDetailView{}, database.WrapInfra(err)
	}

	return PrazoDetailView{
		ID:              row.ID.String(),
		CourtRecordID:   row.CourtRecordID.String(),
		Kind:            derefString(row.Kind),
		StartDate:       row.StartDate.Time,
		EndDate:         row.EndDate.Time,
		DaysLeft:        int(row.DaysLeft),
		Days:            int(row.Days),
		Counting:        row.Counting,
		Doubled:         row.Doubled,
		DoubledReason:   derefString(row.DoubledReason),
		Status:          row.Status,
		Source:          row.Source,
		HolidaysApplied: holidaysFromJSON(row.HolidaysApplied),
		IntimationID:    row.NotificationID.String(),
		RulesVersion:    row.RulesVersion,
		Confirmed:       confirmedBool(row.Confirmed),
	}, nil
}

// holidaysFromJSON decodes the holidays_applied jsonb (an array of "2006-01-02" strings)
// into the read model's []string. The column is NOT NULL DEFAULT '[]', so it is always
// valid JSON; the slice is initialized so an empty audit serializes as [], never null.
func holidaysFromJSON(raw []byte) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	// A malformed audit blob is an infra fault, not client input; the read still returns
	// the row with an empty audit rather than failing the whole screen over one bad cell.
	_ = json.Unmarshal(raw, &out)
	return out
}

// confirmedBool coerces the interface{} sqlc infers for the (confirmed_by IS NOT NULL)
// expression: pgx scans the boolean into a Go bool, so the assertion holds; a nil/other
// value conservatively reads as false (unconfirmed).
func confirmedBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// keysetDate parses a keyset cursor's date (always present — the handler fills the min
// sentinel for the first page) into a pgtype.Date. A malformed value is an infra fault
// (the cursor is server-issued), wrapped so the edge treats it as 500. Named apart from
// domain.go's parseWireDate, which speaks the event anchor (a terminal KindInvalid).
func keysetDate(s string) (pgtype.Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return pgtype.Date{}, database.WrapInfra(err)
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}

// optionalFilterDate parses an optional filter date: "" is the open bound (a NULL
// pgtype.Date the query reads as "no bound"). A non-empty malformed value is validated
// away at the handler, so reaching here with one is an infra fault.
func optionalFilterDate(s string) (pgtype.Date, error) {
	if s == "" {
		return pgtype.Date{}, nil
	}
	return keysetDate(s)
}
