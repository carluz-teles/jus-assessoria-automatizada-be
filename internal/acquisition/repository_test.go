package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

// validDate is a pgxmock argument matcher asserting a bound date column is a real
// (non-NULL) date — the evidence that cancelled_at is written, not left NULL, when
// a retracted intimation flows through the upsert.
type validDate struct{}

func (validDate) Match(v any) bool {
	d, ok := v.(pgtype.Date)
	return ok && d.Valid
}

// TestPgRepositoryUpsertIntimations proves the DJEN cancellation contract at the
// query boundary: the generated InsertIntimation is ON CONFLICT ... DO UPDATE (not
// DO NOTHING), so a retracted intimation (status=CANCELLED + cancelled_at) UPDATES
// the existing row instead of being silently dropped. It runs against pgxmock (no
// real Postgres), so it stays in the plain green gate. The mock matches the actual
// SQL sqlc generated, so a regression to DO NOTHING fails the expectation.
func TestPgRepositoryUpsertIntimations(t *testing.T) {
	t.Parallel()

	const doUpdateSQL = `ON CONFLICT \(tenant_id, case_id, hash\) DO UPDATE SET`

	cancelledAt := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		param        IntimationParams
		inserted     bool // what the query's (xmax = 0) flag returns
		wantNewCount int
		wantStatus   string
		wantCancel   pgxmock.Argument
	}{
		{
			name: "cancelled intimation updates the existing row (not new)",
			param: IntimationParams{
				TenantID:      uuid.NewString(),
				CaseID:        uuid.NewString(),
				CourtRecordID: uuid.NewString(),
				Hash:          "stub-notif-1",
				Status:        IntimationStatusCancelled,
				CancelledAt:   cancelledAt,
				CancelReason:  "retratada pelo tribunal",
			},
			inserted:     false,
			wantNewCount: 0,
			wantStatus:   IntimationStatusCancelled,
			wantCancel:   validDate{},
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
			wantStatus:   IntimationStatusActive,
			wantCancel:   pgxmock.AnyArg(), // no cancellation date on a fresh publication
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

			// Args 1-9 are the identity/date/content columns (not under test here); the
			// DJEN columns follow. Asserting Status and cancelled_at proves the retraction
			// data reaches the query.
			mock.ExpectQuery(doUpdateSQL).
				WithArgs(
					pgxmock.AnyArg(), // tenant_id
					pgxmock.AnyArg(), // case_id
					pgxmock.AnyArg(), // court_record_id
					pgxmock.AnyArg(), // hash
					pgxmock.AnyArg(), // made_available_at
					pgxmock.AnyArg(), // published_at
					pgxmock.AnyArg(), // deadline_start_at
					pgxmock.AnyArg(), // content
					pgxmock.AnyArg(), // source
					pgxmock.AnyArg(), // type
					tt.wantStatus,    // status
					pgxmock.AnyArg(), // source_url
					tt.wantCancel,    // cancelled_at
					pgxmock.AnyArg(), // cancel_reason
					pgxmock.AnyArg(), // recipients
				).
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
