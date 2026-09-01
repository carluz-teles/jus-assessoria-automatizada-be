package acquisition

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
)

// jsonbContains is a pgxmock argument matcher asserting the BatchUpsertIntimations jsonb
// payload (a []byte) contains all the given substrings — enough to prove the Go side maps
// status/cancelled_at into the row objects the set-based upsert unpacks.
type jsonbContains struct{ subs []string }

func (j jsonbContains) Match(v any) bool {
	b, ok := v.([]byte)
	if !ok {
		return false
	}
	s := string(b)
	for _, sub := range j.subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestPgRepositoryUpsertIntimations proves the DJEN cancellation contract at the query
// boundary: UpsertIntimations now issues ONE set-based BatchUpsertIntimations whose SQL is
// ON CONFLICT ... DO UPDATE (not DO NOTHING), and the Go side marshals status +
// cancelled_at into the jsonb payload — so a retracted intimation (CANCELLED +
// cancelled_at) UPDATES the existing row instead of being silently dropped, and a fresh
// one counts as new. Runs against pgxmock (no real Postgres); the full jsonb_to_recordset
// round-trip is covered by the integration test.
func TestPgRepositoryUpsertIntimations(t *testing.T) {
	t.Parallel()

	const doUpdateSQL = `ON CONFLICT \(tenant_id, case_id, hash\) DO UPDATE SET`

	tests := []struct {
		name          string
		param         IntimationParams
		inserted      bool   // what the query's (xmax = 0) flag returns
		oldStatus     string // the status BEFORE this upsert (the correlated subquery)
		newStatus     string // the status AFTER this upsert (RETURNING status)
		wantNew       int    // rows classified as newRows (→ observed)
		wantCancelled int    // rows classified as cancelledRows (→ cancelled)
		wantJSON      []string
	}{
		{
			name: "fresh active intimation inserts → new (observed)",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-2",
				Status:        IntimationStatusActive,
			},
			inserted:      true,
			oldStatus:     "",
			newStatus:     IntimationStatusActive,
			wantNew:       1,
			wantCancelled: 0,
			wantJSON:      []string{`"status":"` + IntimationStatusActive + `"`, `"cancelled_at":null`},
		},
		{
			name: "active → cancelled transition → cancelled (not new, not observed)",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-1",
				Status:        IntimationStatusCancelled,
				CancelledAt:   time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
				CancelReason:  "retratada pelo tribunal",
			},
			inserted:      false,
			oldStatus:     IntimationStatusActive,
			newStatus:     IntimationStatusCancelled,
			wantNew:       0,
			wantCancelled: 1,
			wantJSON:      []string{`"status":"` + IntimationStatusCancelled + `"`, `"cancelled_at":"2024-02-01"`},
		},
		{
			name: "reobserved active (deduped) → neither event",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-3",
				Status:        IntimationStatusActive,
			},
			inserted:      false,
			oldStatus:     IntimationStatusActive,
			newStatus:     IntimationStatusActive,
			wantNew:       0,
			wantCancelled: 0,
			wantJSON:      []string{`"status":"` + IntimationStatusActive + `"`},
		},
		{
			// Dead-on-arrival: a hash never seen before whose payload already carries
			// CANCELLED (the DJEN retracted it inside the same window we first ingest).
			// The row returns Inserted=true with Status=CANCELLED and old_status='' (no
			// prior row). It must emit NEITHER: no deadline to open, none to revoke.
			name: "fresh insert already cancelled → neither event (dead-on-arrival)",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-5",
				Status:        IntimationStatusCancelled,
				CancelledAt:   time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC),
				CancelReason:  "retratada pelo tribunal",
			},
			inserted:      true,
			oldStatus:     "",
			newStatus:     IntimationStatusCancelled,
			wantNew:       0,
			wantCancelled: 0,
			wantJSON:      []string{`"status":"` + IntimationStatusCancelled + `"`, `"cancelled_at":"2024-02-02"`},
		},
		{
			name: "already-cancelled re-arrives → neither event (no re-emit)",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-4",
				Status:        IntimationStatusCancelled,
				CancelReason:  "retratada pelo tribunal",
			},
			inserted:      false,
			oldStatus:     IntimationStatusCancelled,
			newStatus:     IntimationStatusCancelled,
			wantNew:       0,
			wantCancelled: 0,
			wantJSON:      []string{`"status":"` + IntimationStatusCancelled + `"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool: %v", err)
			}
			defer mock.Close()

			// The set-based upsert takes exactly two args: the tenant id and the jsonb rows
			// payload. Matching the DO-UPDATE SQL guards against a regression to DO NOTHING;
			// the jsonb matcher proves the retraction fields reach the query. The returned
			// rows carry the enriched columns (court joined, old_status subquery) the mapper
			// classifies into new vs cancelled.
			mock.ExpectQuery(doUpdateSQL).
				WithArgs(pgxmock.AnyArg(), jsonbContains{tt.wantJSON}).
				WillReturnRows(
					pgxmock.NewRows([]string{
						"id", "court_record_id", "type", "status", "deadline_start_at", "content",
						"cancel_reason", "inserted", "court", "case_id", "old_status",
					}).AddRow(
						uuid.New(), uuid.New(), (*string)(nil), tt.newStatus,
						pgtype.Date{Time: time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC), Valid: true},
						"", (*string)(nil), tt.inserted, "TJSP", uuid.New(), tt.oldStatus,
					),
				)

			r := &pgRepository{}
			newRows, cancelledRows, err := r.UpsertIntimations(context.Background(), mock, []IntimationParams{tt.param})
			if err != nil {
				t.Fatalf("UpsertIntimations: %v", err)
			}
			if len(newRows) != tt.wantNew {
				t.Errorf("newRows = %d, want %d", len(newRows), tt.wantNew)
			}
			if len(cancelledRows) != tt.wantCancelled {
				t.Errorf("cancelledRows = %d, want %d", len(cancelledRows), tt.wantCancelled)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// TestBuildIntimacaoHistory proves the unified Trilha (Architect decisão 2): deadline_event
// rows merge into the SAME chronological timeline as the existing capture/confirmation/analysis
// signals — interleaved by occurred_at, not appended after them.
func TestBuildIntimacaoHistory(t *testing.T) {
	t.Parallel()

	captured := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	calculado := time.Date(2024, 3, 2, 9, 0, 0, 0, time.UTC)
	confirmed := time.Date(2024, 3, 5, 14, 0, 0, 0, time.UTC)
	validado := time.Date(2024, 3, 6, 10, 0, 0, 0, time.UTC)
	analyzed := time.Date(2024, 3, 10, 8, 0, 0, 0, time.UTC)

	got := buildIntimacaoHistory(
		pgtype.Date{Time: captured, Valid: true},
		pgtype.Timestamptz{Time: confirmed, Valid: true},
		strPtr("Dra. Ana"),
		strPtr("OPEN"),
		pgtype.Timestamptz{Time: analyzed, Valid: true},
		[]acquisitiondb.ListDeadlineEventsByDeadlineIDRow{
			// Deliberately unordered — the function must re-sort, not trust append order.
			{Em: pgtype.Timestamptz{Time: validado, Valid: true}, Detalhe: strPtr("divergência apurada: aceita_calculado")},
			{Em: pgtype.Timestamptz{Time: calculado, Valid: true}, Detalhe: strPtr("prazo calculado: 15 dias úteis")},
			{Em: pgtype.Timestamptz{}, Detalhe: strPtr("skipped: em is NULL")}, // invalid Em must be dropped
		},
	)

	wantLabels := []string{
		"Capturada do DJEN",                     // captured
		"prazo calculado: 15 dias úteis",        // calculado
		"Prazo confirmado por Dra. Ana",         // confirmed
		"divergência apurada: aceita_calculado", // validado
		"Providências geradas",                  // analyzed
	}
	if len(got) != len(wantLabels) {
		t.Fatalf("entries = %d, want %d (got %+v)", len(got), len(wantLabels), got)
	}
	for i, label := range wantLabels {
		if got[i].Label != label {
			t.Errorf("entries[%d].Label = %q, want %q", i, got[i].Label, label)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].OccurredAt.Before(got[i-1].OccurredAt) {
			t.Errorf("entries not sorted ASC by occurred_at: [%d]=%v before [%d]=%v", i, got[i].OccurredAt, i-1, got[i-1].OccurredAt)
		}
	}
}
