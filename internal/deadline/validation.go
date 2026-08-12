package deadline

import (
	"errors"
	"fmt"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
)

// ConfirmRequest is the POST /v1/prazos/confirm body (docs/erd-prazos.md §9): the 1:1
// intimação, the confirmed legal prazo, and the N tasks. tenant_id / confirmed_by are NOT
// here — they come from the verified principal. The recomputed end_date is server-derived,
// so it is not accepted either.
type ConfirmRequest struct {
	IntimationID string               `json:"intimation_id"`
	Deadline     ConfirmDeadlineBody  `json:"deadline"`
	Tasks        []ConfirmTaskRequest `json:"tasks"`
}

// ConfirmDeadlineBody is the {kind, days, counting, doubled, doubled_reason} the F2 form
// approves. days/counting drive the recompute; doubled/doubled_reason carry the human's
// explicit dobro toggle (never inferred — viés seguro, §8).
type ConfirmDeadlineBody struct {
	Kind          string `json:"kind"`
	Days          int    `json:"days"`
	Counting      string `json:"counting"`
	Doubled       bool   `json:"doubled"`
	DoubledReason string `json:"doubled_reason"`
}

// ConfirmTaskRequest is one action item the F2 submits. Title is required; the rest are
// optional. DueDate is the wire date (2006-01-02) — its ≤ end_date bound is checked in the
// use case (the recomputed end_date is only known there), not here.
type ConfirmTaskRequest struct {
	Title          string `json:"title"`
	Kind           string `json:"kind"`
	Description    string `json:"description"`
	DueDate        string `json:"due_date"`
	AssigneeUserID string `json:"assignee_user_id"`
}

// Validate enforces the edge boundary rules via ozzo (method-based, not struct tags): a
// well-formed intimation_id, a positive day count, a valid counting, and every task's
// title non-empty with a well-formed optional due_date. Cross-field/semantic rules that
// need the recomputed end_date (due_date ≤ end_date) are deferred to the use case. A
// failure is a 400 at the edge (KindInvalid) via httpx.WriteValidationError.
func (r ConfirmRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.IntimationID, validation.Required, validation.By(isUUID)),
		validation.Field(&r.Deadline),
		validation.Field(&r.Tasks),
	)
}

// Validate enforces the confirmed prazo's rules: days > 0 and a counting in the closed set
// (the same set the DB CHECK enforces). Declaring Validate on the body lets ozzo validate
// it automatically when it is a request field.
func (b ConfirmDeadlineBody) Validate() error {
	return validation.ValidateStruct(&b,
		validation.Field(&b.Days, validation.Required, validation.Min(1)),
		validation.Field(&b.Counting, validation.Required,
			validation.In(string(CountingBusiness), string(CountingCalendar))),
	)
}

// Validate enforces one task's rules: a non-empty title and, when present, a wire-format
// due_date (empty is allowed — a task may be undated). Declaring Validate on the task lets
// ozzo validate each element of Tasks automatically.
func (t ConfirmTaskRequest) Validate() error {
	return validation.ValidateStruct(&t,
		validation.Field(&t.Title, validation.Required),
		validation.Field(&t.DueDate, validation.Date(time.DateOnly)),
	)
}

// isUUID is an ozzo rule that accepts only a parseable uuid — reusing google/uuid (the
// same parser the repo uses) rather than a separate regex/dependency. An empty value is
// handled by the accompanying validation.Required, so this only sees non-empty strings.
func isUUID(value any) error {
	s, _ := value.(string)
	if _, err := uuid.Parse(s); err != nil {
		return errors.New("must be a valid uuid")
	}
	return nil
}

// toCommand maps the validated request + the principal's ids into the use-case command.
// TenantID and UserID come from the principal (never the body); the wire due_dates are
// parsed here (Validate already guaranteed the format) into optional times.
func (r ConfirmRequest) toCommand(tenantID, userID string) ConfirmCommand {
	tasks := make([]ConfirmTaskInput, 0, len(r.Tasks))
	for _, t := range r.Tasks {
		tasks = append(tasks, ConfirmTaskInput{
			Title:          t.Title,
			Kind:           t.Kind,
			Description:    t.Description,
			DueDate:        parseOptionalWireDate(t.DueDate),
			AssigneeUserID: t.AssigneeUserID,
		})
	}
	return ConfirmCommand{
		TenantID:      tenantID,
		UserID:        userID,
		IntimationID:  r.IntimationID,
		Kind:          r.Deadline.Kind,
		Days:          r.Deadline.Days,
		Counting:      Counting(r.Deadline.Counting),
		Doubled:       r.Deadline.Doubled,
		DoubledReason: r.Deadline.DoubledReason,
		Tasks:         tasks,
	}
}

// parseOptionalWireDate turns a validated wire date ("" → no date) into an optional time.
// Validate already rejected malformed values, so a parse error here is impossible; it
// collapses to nil defensively rather than propagating.
func parseOptionalWireDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil
	}
	return &t
}

// validate is the invariant check on a derived prazo BEFORE it is persisted — a
// belt-and-suspenders on top of the DB CHECKs (0024) for this safety-critical
// (deadline) data. The days come from the rules layer (CHECK days > 0) and the dates
// from lib/calendar, so a violation here means a bug upstream, not bad user input; it
// returns a typed KindInvalid so the fault is loud, not a silently persisted bad prazo.
func (d *Deadline) validate() error {
	if d.Days <= 0 {
		return apperr.NewInvalid(fmt.Sprintf("deadline days must be > 0, got %d", d.Days))
	}
	if d.Counting != CountingBusiness && d.Counting != CountingCalendar {
		return apperr.NewInvalid(fmt.Sprintf("invalid deadline counting %q", d.Counting))
	}
	// The end is the n-th day AFTER the start (start excluded, CPC art. 224), so it is
	// always strictly after the start; an end on/before the start is impossible math.
	if !d.EndDate.After(d.StartDate) {
		return apperr.NewInvalid("deadline end date must be after start date")
	}
	return nil
}
