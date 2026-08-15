package billing

import (
	"time"

	"github.com/google/uuid"
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
		ID:                         r.ID.String(),
		TenantID:                   r.TenantID.String(),
		StripeCustomerID:           derefString(r.StripeCustomerID),
		StripeSubscriptionID:       derefString(r.StripeSubscriptionID),
		Status:                     Status(r.Status),
		Plan:                       derefString(r.Plan),
		CurrentPeriodEnd:           timestamptzToPtr(r.CurrentPeriodEnd),
		ActiveProcessLimit:         derefInt32(r.ActiveProcessLimit),
		CreatedAt:                  r.CreatedAt.Time,
		UpdatedAt:                  r.UpdatedAt.Time,
		PlanID:                     uuidToPtr(r.PlanID),
		CustomPricePerProcessCents: derefInt32Ptr(r.CustomPricePerProcessCents),
		TrialEndsAt:                timestamptzToPtr(r.TrialEndsAt),
	}
}

// planToEntity maps a plan row to the entity, collapsing the nullable
// max_processes/stripe_price_id columns to their pointer forms.
func planToEntity(r billingdb.Plan) *Plan {
	return &Plan{
		ID:                   r.ID.String(),
		Code:                 r.Code,
		Name:                 r.Name,
		MinProcesses:         int(r.MinProcesses),
		MaxProcesses:         int32PtrToIntPtr(r.MaxProcesses),
		PricePerProcessCents: int(r.PricePerProcessCents),
		StripePriceID:        r.StripePriceID,
		Active:               r.Active,
	}
}

// trialPolicyToEntity maps a trial_policy row to the entity.
func trialPolicyToEntity(r billingdb.TrialPolicy) *TrialPolicy {
	return &TrialPolicy{
		Name:               r.Name,
		IsDefault:          r.IsDefault,
		TrialDays:          int(r.TrialDays),
		ActiveProcessLimit: int(r.ActiveProcessLimit),
		Active:             r.Active,
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

// timePtrToTimestamptz is timeToTimestamptz's pointer-input sibling: nil (no
// value known) is written as SQL NULL, same as a zero time.Time.
func timePtrToTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return timeToTimestamptz(*t)
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

// uuidToPtr collapses a nullable pgtype.UUID (plan_id) to a *string, nil standing
// in for SQL NULL — plan_id is unset until the next webhook re-projects it.
func uuidToPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// strPtrToUUID is the inverse: nil is written as SQL NULL, an invalid uuid string
// also collapses to NULL rather than failing the write (the caller — the repo —
// only ever passes a uuid.Parse-validated string, so this branch is defensive).
func strPtrToUUID(s *string) pgtype.UUID {
	if s == nil {
		return pgtype.UUID{}
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// derefInt32Ptr collapses a nullable int column (*int32) to a *int, preserving
// NULL as nil (unlike derefInt32, zero is a legitimate override value here — a
// negotiated $0/process price is not "no override").
func derefInt32Ptr(n *int32) *int {
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

// intPtrToNull32 is the inverse of derefInt32Ptr: nil stays SQL NULL, any value
// (including zero) is written as-is.
func intPtrToNull32(n *int) *int32 {
	if n == nil {
		return nil
	}
	v := int32(*n)
	return &v
}

// int32PtrToIntPtr collapses a nullable int32 column (*int32, e.g. max_processes)
// to a *int, nil standing in for SQL NULL ("no ceiling").
func int32PtrToIntPtr(n *int32) *int {
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}
