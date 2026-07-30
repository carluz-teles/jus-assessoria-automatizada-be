package billing

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/billing/billingdb"
)

// mapper.go is the boundary where driver types die (docs/erd-backend.md §4b.3):
// uuid.UUID and pgtype.* are absorbed here so the entity stays pure. The repo
// returns *Subscription, never the sqlc row.

// subscriptionToEntity maps a subscription row to the entity, collapsing the
// nullable text/int/timestamptz columns to their plain (or pointer) forms.
func subscriptionToEntity(r billingdb.Subscription) *Subscription {
	return &Subscription{
		ID:                   r.ID.String(),
		TenantID:             r.TenantID.String(),
		StripeCustomerID:     derefString(r.StripeCustomerID),
		StripeSubscriptionID: derefString(r.StripeSubscriptionID),
		Status:               Status(r.Status),
		Plan:                 derefString(r.Plan),
		CurrentPeriodEnd:     timestamptzToPtr(r.CurrentPeriodEnd),
		ActiveProcessLimit:   derefInt32(r.ActiveProcessLimit),
		CreatedAt:            r.CreatedAt.Time,
		UpdatedAt:            r.UpdatedAt.Time,
	}
}

// timestamptzToPtr collapses a nullable timestamptz to a *time.Time, nil standing
// in for SQL NULL — current_period_end is unset until a subscription.* sets it.
func timestamptzToPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// timeToTimestamptz is the inverse: a zero time is written as SQL NULL, any other
// value as a valid timestamptz. current_period_end is optional on the projection.
func timeToTimestamptz(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// derefString collapses a nullable text column (*string) to a plain string, an
// empty string standing in for SQL NULL.
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

// derefInt32 collapses a nullable int column (*int32) to a plain int, zero
// standing in for SQL NULL — active_process_limit is unset until a plan is known.
func derefInt32(n *int32) int {
	if n == nil {
		return 0
	}
	return int(*n)
}

// intToNull is the inverse: a zero limit is written as SQL NULL (no plan resolved
// yet), any positive value as its int32. A limit is never legitimately zero — the
// domain rejects that as ErrPlanUnresolved before the write.
func intToNull(n int) *int32 {
	if n == 0 {
		return nil
	}
	v := int32(n)
	return &v
}
