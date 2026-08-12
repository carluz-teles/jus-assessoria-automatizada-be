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

// AdjustRequest is the PATCH /v1/prazos/:id body (docs/erd-prazos.md §9): the partial ajuste
// of an already-derived prazo. Every field is a POINTER so an ABSENT field (kept at its stored
// value) is distinguishable from a present zero value — a partial patch, not a full replace.
// tenant_id / the prazo id / user come from the principal + path, never the body; the
// recomputed end_date is server-derived, so it is not accepted either.
type AdjustRequest struct {
	Kind          *string `json:"kind"`
	Days          *int    `json:"days"`
	Counting      *string `json:"counting"`
	Doubled       *bool   `json:"doubled"`
	DoubledReason *string `json:"doubled_reason"`
}

// Validate enforces the edge rules for the fields that ARE present (a nil field is a no-op,
// kept at its stored value): days > 0 and a counting in the closed set. The rules are custom
// (validation.By) rather than ozzo's Min/In because those skip a nil pointer AND a present
// zero, which would silently accept days:0 — here a PRESENT day count must be positive.
func (r AdjustRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Days, validation.By(positiveDaysIfPresent)),
		validation.Field(&r.Counting, validation.By(validCountingIfPresent)),
	)
}

// positiveDaysIfPresent rejects a PRESENT, non-positive day count; an absent (nil) days is a
// no-op (the stored value is kept).
func positiveDaysIfPresent(value any) error {
	days, ok := value.(*int)
	if !ok || days == nil {
		return nil
	}
	if *days < 1 {
		return errors.New("must be greater than 0")
	}
	return nil
}

// validCountingIfPresent rejects a PRESENT counting outside the closed set (the same set the
// DB CHECK enforces); an absent (nil) counting is a no-op.
func validCountingIfPresent(value any) error {
	c, ok := value.(*string)
	if !ok || c == nil {
		return nil
	}
	switch Counting(*c) {
	case CountingBusiness, CountingCalendar:
		return nil
	default:
		return errors.New("must be BUSINESS or CALENDAR")
	}
}

// toAdjustCommand maps the validated request + the principal's ids + the path id into the
// use-case command. TenantID/UserID come from the principal and DeadlineID from the path
// (never the body); the pointer fields carry through so the use case merges only what was
// present. Counting is converted *string → *Counting.
func (r AdjustRequest) toAdjustCommand(tenantID, userID, deadlineID string) AdjustCommand {
	cmd := AdjustCommand{
		TenantID:      tenantID,
		UserID:        userID,
		DeadlineID:    deadlineID,
		Kind:          r.Kind,
		Days:          r.Days,
		Doubled:       r.Doubled,
		DoubledReason: r.DoubledReason,
	}
	if r.Counting != nil {
		c := Counting(*r.Counting)
		cmd.Counting = &c
	}
	return cmd
}

// CreateTaskRequest is the POST /v1/tasks body (docs/erd-prazos.md §9): a manual task. Title is
// required; the context FKs (court_record_id, deadline_id, intimation_id) and the assignee are
// optional (a task can be avulsa / unassigned) but must be well-formed uuids when present.
// tenant_id / created_by come from the verified principal; status/source are server-set
// (OPEN/MANUAL), so they are not accepted from the body.
type CreateTaskRequest struct {
	CourtRecordID  string `json:"court_record_id"`
	DeadlineID     string `json:"deadline_id"`
	IntimationID   string `json:"intimation_id"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	Kind           string `json:"kind"`
	DueDate        string `json:"due_date"`
	AssigneeUserID string `json:"assignee_user_id"`
}

// Validate enforces the edge rules: a non-empty title, a well-formed optional due_date (empty is
// allowed — a task may be undated), and well-formed optional uuids for the context FKs + assignee
// (empty = absent). A failure is a 400 at the edge (KindInvalid) via httpx.WriteValidationError.
func (r CreateTaskRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.Required),
		validation.Field(&r.DueDate, validation.Date(time.DateOnly)),
		validation.Field(&r.CourtRecordID, validation.By(uuidIfPresent)),
		validation.Field(&r.DeadlineID, validation.By(uuidIfPresent)),
		validation.Field(&r.IntimationID, validation.By(uuidIfPresent)),
		validation.Field(&r.AssigneeUserID, validation.By(uuidIfPresent)),
	)
}

// toCommand maps the validated request + the principal's ids into the use-case command.
// TenantID and UserID come from the principal (never the body); the wire due_date is parsed here
// (Validate guaranteed the format) into an optional time.
func (r CreateTaskRequest) toCommand(tenantID, userID string) CreateTaskCommand {
	return CreateTaskCommand{
		TenantID:       tenantID,
		UserID:         userID,
		CourtRecordID:  r.CourtRecordID,
		DeadlineID:     r.DeadlineID,
		IntimationID:   r.IntimationID,
		Title:          r.Title,
		Description:    r.Description,
		Kind:           r.Kind,
		DueDate:        parseOptionalWireDate(r.DueDate),
		AssigneeUserID: r.AssigneeUserID,
	}
}

// UpdateTaskRequest is the PATCH /v1/tasks/:id body (docs/erd-prazos.md §9): the partial ajuste
// of a task. Every field is a POINTER so an ABSENT field (kept at its stored value) is
// distinguishable from a present value — a partial patch, not a full replace. A present due_date
// of "" clears the date; a present assignee of "" unassigns. tenant_id / the task id come from
// the principal + path, never the body; status is changed only via done/dismiss.
type UpdateTaskRequest struct {
	Title          *string `json:"title"`
	Description    *string `json:"description"`
	Kind           *string `json:"kind"`
	DueDate        *string `json:"due_date"`
	AssigneeUserID *string `json:"assignee_user_id"`
}

// Validate enforces the edge rules for the fields that ARE present (a nil field is a no-op): a
// present title must be non-empty, a present due_date must be a wire date or "" (clear), and a
// present assignee must be a uuid or "" (unassign). The rules are custom (validation.By) rather
// than ozzo's built-ins because those skip a nil pointer AND a present zero — here a PRESENT
// title must not be blank.
func (r UpdateTaskRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.By(nonEmptyIfPresent)),
		validation.Field(&r.DueDate, validation.By(wireDateOrClearIfPresent)),
		validation.Field(&r.AssigneeUserID, validation.By(uuidOrClearIfPresent)),
	)
}

// toUpdateCommand maps the validated request + the principal's tenant + the path id into the
// use-case command. TenantID comes from the principal and TaskID from the path (never the body);
// the pointer fields carry through so the use case merges only what was present.
func (r UpdateTaskRequest) toUpdateCommand(tenantID, taskID string) UpdateTaskCommand {
	return UpdateTaskCommand{
		TenantID:       tenantID,
		TaskID:         taskID,
		Title:          r.Title,
		Description:    r.Description,
		Kind:           r.Kind,
		DueDate:        r.DueDate,
		AssigneeUserID: r.AssigneeUserID,
	}
}

// uuidIfPresent rejects a PRESENT, non-empty, malformed uuid; an empty value is a no-op (an
// absent optional id). It backs the CREATE body's optional context FKs + assignee.
func uuidIfPresent(value any) error {
	s, _ := value.(string)
	if s == "" {
		return nil
	}
	if _, err := uuid.Parse(s); err != nil {
		return errors.New("must be a valid uuid")
	}
	return nil
}

// nonEmptyIfPresent rejects a PRESENT but blank title; an absent (nil) title is a no-op (the
// stored value is kept). It is the pointer counterpart of validation.Required.
func nonEmptyIfPresent(value any) error {
	s, ok := value.(*string)
	if !ok || s == nil {
		return nil
	}
	if *s == "" {
		return errors.New("cannot be blank")
	}
	return nil
}

// wireDateOrClearIfPresent accepts an absent (nil) due_date, a present "" (clear the date), or a
// present wire date (2006-01-02); anything else is a client error.
func wireDateOrClearIfPresent(value any) error {
	s, ok := value.(*string)
	if !ok || s == nil || *s == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, *s); err != nil {
		return errors.New("must be a valid date (YYYY-MM-DD)")
	}
	return nil
}

// uuidOrClearIfPresent accepts an absent (nil) assignee, a present "" (unassign), or a present
// valid uuid; anything else is a client error.
func uuidOrClearIfPresent(value any) error {
	s, ok := value.(*string)
	if !ok || s == nil || *s == "" {
		return nil
	}
	if _, err := uuid.Parse(*s); err != nil {
		return errors.New("must be a valid uuid")
	}
	return nil
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
