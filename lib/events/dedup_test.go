package events

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/jusassessoria/platform/lib/apperr"
)

// SeenOrMark reads the insert's RowsAffected: 1 means this call won the insert
// (first sighting, seen=false); 0 means ON CONFLICT skipped it (duplicate, seen=true).
func TestDedup_SeenOrMark(t *testing.T) {
	tests := []struct {
		name         string
		rowsAffected int64
		wantSeen     bool
	}{
		{name: "first sighting marks and proceeds", rowsAffected: 1, wantSeen: false},
		{name: "duplicate already recorded", rowsAffected: 0, wantSeen: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := newMockPool(t)
			mock.
				ExpectExec("INSERT INTO processed_event").
				WithArgs("revisao-listener", "evt-1").
				WillReturnResult(pgxmock.NewResult("INSERT", tt.rowsAffected))

			seen, err := NewDedup(mock).SeenOrMark(context.Background(), "revisao-listener", "evt-1")
			if err != nil {
				t.Fatalf("SeenOrMark() error = %v", err)
			}
			if seen != tt.wantSeen {
				t.Errorf("SeenOrMark() seen = %v, want %v", seen, tt.wantSeen)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// A driver failure surfaces as a typed infra error so the listener leaves the task
// retryable rather than acking it.
func TestDedup_SeenOrMark_InfraError(t *testing.T) {
	mock := newMockPool(t)
	mock.
		ExpectExec("INSERT INTO processed_event").
		WithArgs("revisao-listener", "evt-1").
		WillReturnError(context.DeadlineExceeded)

	_, err := NewDedup(mock).SeenOrMark(context.Background(), "revisao-listener", "evt-1")
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInfra {
		t.Fatalf("SeenOrMark() error = %v, want INFRA_ERROR AppError", err)
	}
}
