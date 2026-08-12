package acquisition

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
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
						"id", "court_record_id", "type", "status", "deadline_start_at",
						"cancel_reason", "inserted", "court", "case_id", "old_status",
					}).AddRow(
						uuid.New(), uuid.New(), (*string)(nil), tt.newStatus,
						pgtype.Date{Time: time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC), Valid: true},
						(*string)(nil), tt.inserted, "TJSP", uuid.New(), tt.oldStatus,
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
