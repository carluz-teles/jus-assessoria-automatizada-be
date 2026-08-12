package acquisition

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
		name         string
		param        IntimationParams
		inserted     bool // what the query's (xmax = 0) flag returns
		wantNewCount int
		wantJSON     []string // substrings the marshaled row payload must contain
	}{
		{
			name: "cancelled intimation updates the existing row (not new)",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-1",
				Status:        IntimationStatusCancelled,
				CancelledAt:   time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
				CancelReason:  "retratada pelo tribunal",
			},
			inserted:     false,
			wantNewCount: 0,
			wantJSON:     []string{`"status":"` + IntimationStatusCancelled + `"`, `"cancelled_at":"2024-02-01"`},
		},
		{
			name: "fresh active intimation inserts and counts as new",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-2",
				Status:        IntimationStatusActive,
			},
			inserted:     true,
			wantNewCount: 1,
			wantJSON:     []string{`"status":"` + IntimationStatusActive + `"`, `"cancelled_at":null`},
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
			// the jsonb matcher proves the retraction fields reach the query.
			mock.ExpectQuery(doUpdateSQL).
				WithArgs(pgxmock.AnyArg(), jsonbContains{tt.wantJSON}).
				WillReturnRows(
					pgxmock.NewRows([]string{"id", "inserted"}).
						AddRow(uuid.New(), tt.inserted),
				)

			r := &pgRepository{}
			newCount, err := r.UpsertIntimations(context.Background(), mock, []IntimationParams{tt.param})
			if err != nil {
				t.Fatalf("UpsertIntimations: %v", err)
			}
			if newCount != tt.wantNewCount {
				t.Errorf("newCount = %d, want %d", newCount, tt.wantNewCount)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}
