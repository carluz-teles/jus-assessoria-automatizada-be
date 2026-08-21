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
// intimação and the confirmed legal prazo. It carries NO tasks — the task lifecycle moved to
// POST/PATCH /v1/tasks (the "Análise" section), so the confirm only confirms the prazo itself.
// tenant_id / confirmed_by are NOT here — they come from the verified principal. The recomputed
// end_date is server-derived, so it is not accepted either.
type ConfirmRequest struct {
	IntimationID string              `json:"intimation_id"`
	Deadline     ConfirmDeadlineBody `json:"deadline"`
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
	// AnchorEvent is the optional termo inicial (default DEADLINE_START when ""); ManualExtraDays
	// is the optional feriado/suspensão days count (default 0). Both are validated in the closed
	// set / non-negative below.
	AnchorEvent     string `json:"anchor_event"`
	ManualExtraDays int    `json:"manual_extra_days"`
}

// Validate enforces the edge boundary rules via ozzo (method-based, not struct tags): a
// well-formed intimation_id, a positive day count, and a valid counting. A failure is a 400
// at the edge (KindInvalid) via httpx.WriteValidationError.
func (r ConfirmRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.IntimationID, validation.Required, validation.By(isUUID)),
		validation.Field(&r.Deadline),
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
		// anchor_event is optional ("" → default DEADLINE_START at toCommand); when present it
		// must be a member of the closed set. manual_extra_days is optional and must be >= 0.
		validation.Field(&b.AnchorEvent, validation.In(
			string(AnchorMadeAvailable), string(AnchorPublished), string(AnchorDeadlineStart))),
		validation.Field(&b.ManualExtraDays, validation.Min(0)),
	)
}

// AdjustRequest is the PATCH /v1/prazos/:id body (docs/erd-prazos.md §9): the partial ajuste
// of an already-derived prazo. Every field is a POINTER so an ABSENT field (kept at its stored
// value) is distinguishable from a present zero value — a partial patch, not a full replace.
// tenant_id / the prazo id / user come from the principal + path, never the body; the
// recomputed end_date is server-derived, so it is not accepted either.
type AdjustRequest struct {
	Kind            *string `json:"kind"`
	Days            *int    `json:"days"`
	Counting        *string `json:"counting"`
	Doubled         *bool   `json:"doubled"`
	DoubledReason   *string `json:"doubled_reason"`
	AnchorEvent     *string `json:"anchor_event"`
	ManualExtraDays *int    `json:"manual_extra_days"`
}

// Validate enforces the edge rules for the fields that ARE present (a nil field is a no-op,
// kept at its stored value): days > 0 and a counting in the closed set. The rules are custom
// (validation.By) rather than ozzo's Min/In because those skip a nil pointer AND a present
// zero, which would silently accept days:0 — here a PRESENT day count must be positive.
func (r AdjustRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Days, validation.By(positiveDaysIfPresent)),
		validation.Field(&r.Counting, validation.By(validCountingIfPresent)),
		validation.Field(&r.AnchorEvent, validation.By(validAnchorIfPresent)),
		validation.Field(&r.ManualExtraDays, validation.By(nonNegativeExtraDaysIfPresent)),
	)
}

// validAnchorIfPresent rejects a PRESENT anchor_event outside the closed set; an absent (nil) one
// is a no-op (the stored anchor is kept). Mirrors validCountingIfPresent.
func validAnchorIfPresent(value any) error {
	a, ok := value.(*string)
	if !ok || a == nil {
		return nil
	}
	if !validAnchorEvent(AnchorEvent(*a)) {
		return errors.New("must be MADE_AVAILABLE, PUBLISHED or DEADLINE_START")
	}
	return nil
}

// nonNegativeExtraDaysIfPresent rejects a PRESENT, negative manual_extra_days; an absent (nil) one
// is a no-op (the stored value is kept).
func nonNegativeExtraDaysIfPresent(value any) error {
	d, ok := value.(*int)
	if !ok || d == nil {
		return nil
	}
	if *d < 0 {
		return errors.New("must be greater than or equal to 0")
	}
	return nil
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
		TenantID:        tenantID,
		UserID:          userID,
		DeadlineID:      deadlineID,
		Kind:            r.Kind,
		Days:            r.Days,
		Doubled:         r.Doubled,
		DoubledReason:   r.DoubledReason,
		ManualExtraDays: r.ManualExtraDays,
	}
	if r.Counting != nil {
		c := Counting(*r.Counting)
		cmd.Counting = &c
	}
	if r.AnchorEvent != nil {
		a := AnchorEvent(*r.AnchorEvent)
		cmd.AnchorEvent = &a
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
// allowed — a task may be undated), a well-formed optional kind (empty = uncategorized, a present
// non-empty one must be a TaskKind), and well-formed optional uuids for the context FKs + assignee
// (empty = absent). A failure is a 400 at the edge (KindInvalid) via httpx.WriteValidationError.
func (r CreateTaskRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.Required),
		validation.Field(&r.DueDate, validation.Date(time.DateOnly)),
		validation.Field(&r.Kind, validation.By(validTaskKindRule)),
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
// present title must be non-empty, a present due_date must be a wire date or "" (clear), a present
// kind must be a TaskKind or "" (clear), and a present assignee must be a uuid or "" (unassign).
// The rules are custom (validation.By) rather than ozzo's built-ins because those skip a nil
// pointer AND a present zero — here a PRESENT title must not be blank.
func (r UpdateTaskRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.By(nonEmptyIfPresent)),
		validation.Field(&r.DueDate, validation.By(wireDateOrClearIfPresent)),
		validation.Field(&r.Kind, validation.By(validTaskKindIfPresent)),
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

// CreateTaskItemRequest is the POST /v1/tasks/:id/items body (§4/§10): one checklist item. Title
// is the only user input and is required; tenant_id + the task id come from the principal + path,
// and position/done are server-set (appended last, done=false).
type CreateTaskItemRequest struct {
	Title string `json:"title"`
}

// Validate enforces the one edge rule: a non-empty title. A failure is a 400 at the edge.
func (r CreateTaskItemRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.Required),
	)
}

// toCommand maps the validated request + the principal's tenant + the path task id into the
// use-case command. TenantID/TaskID come from the principal + path (never the body).
func (r CreateTaskItemRequest) toCommand(tenantID, taskID string) CreateTaskItemCommand {
	return CreateTaskItemCommand{TenantID: tenantID, TaskID: taskID, Title: r.Title}
}

// UpdateTaskItemRequest is the PATCH /v1/tasks/:id/items/:itemId body (§4/§10): the partial edit
// of a checklist item. Both fields are POINTERS so an ABSENT field keeps its stored value: a nil
// Done keeps the tick, a nil Title keeps the label. done_at is derived from Done server-side, so
// it is not accepted.
type UpdateTaskItemRequest struct {
	Title *string `json:"title"`
	Done  *bool   `json:"done"`
}

// Validate enforces the edge rule for the fields that ARE present: a present title must be
// non-empty (a nil title is a no-op — the stored value is kept). done is a bool toggle with no
// constraint.
func (r UpdateTaskItemRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.By(nonEmptyIfPresent)),
	)
}

// toCommand maps the validated request + the principal's tenant + the path ids into the use-case
// command. TenantID/TaskID/ItemID come from the principal + path (never the body); the pointer
// fields carry through so the use case merges only what was present.
func (r UpdateTaskItemRequest) toCommand(tenantID, taskID, itemID string) UpdateTaskItemCommand {
	return UpdateTaskItemCommand{
		TenantID: tenantID,
		TaskID:   taskID,
		ItemID:   itemID,
		Title:    r.Title,
		Done:     r.Done,
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

// validTaskKindRule rejects a PRESENT, non-empty task kind outside the closed TaskKind set
// (entity.go); an empty value is a no-op (an uncategorized task). It backs the CREATE body.
func validTaskKindRule(value any) error {
	s, _ := value.(string)
	if s == "" {
		return nil
	}
	if !validTaskKind(s) {
		return errors.New("must be one of ANALISE, PECA, PROTOCOLO, PROVIDENCIA, CIENCIA")
	}
	return nil
}

// validTaskKindIfPresent accepts an absent (nil) kind, a present "" (clear the kind), or a present
// valid TaskKind; anything else is a client error. It is the pointer counterpart of
// validTaskKindRule, backing the PATCH body.
func validTaskKindIfPresent(value any) error {
	s, ok := value.(*string)
	if !ok || s == nil || *s == "" {
		return nil
	}
	if !validTaskKind(*s) {
		return errors.New("must be one of ANALISE, PECA, PROTOCOLO, PROVIDENCIA, CIENCIA")
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
// TenantID and UserID come from the principal (never the body). The confirm carries no tasks.
func (r ConfirmRequest) toCommand(tenantID, userID string) ConfirmCommand {
	return ConfirmCommand{
		TenantID:        tenantID,
		UserID:          userID,
		IntimationID:    r.IntimationID,
		Kind:            r.Deadline.Kind,
		Days:            r.Deadline.Days,
		Counting:        Counting(r.Deadline.Counting),
		Doubled:         r.Deadline.Doubled,
		DoubledReason:   r.Deadline.DoubledReason,
		AnchorEvent:     defaultAnchor(r.Deadline.AnchorEvent),
		ManualExtraDays: r.Deadline.ManualExtraDays,
	}
}

// defaultAnchor maps an absent ("") anchor_event to the legacy DEADLINE_START default, so the
// domain and the NOT-NULL column always see a valid closed-set value. Validate already rejected a
// present-but-invalid value.
func defaultAnchor(s string) AnchorEvent {
	if s == "" {
		return AnchorDeadlineStart
	}
	return AnchorEvent(s)
}

// PreviewRequest is the POST /v1/prazos/preview body (§3): a live, non-persisted recompute. It
// carries EITHER an intimation_id (anchor on one of the intimação's dates) OR a start_date (the
// manual case). tenant_id comes from the verified principal, never the body. days/counting are
// required (the recompute needs them); anchor_event/doubled/manual_extra_days are optional.
type PreviewRequest struct {
	IntimationID    string `json:"intimation_id"`
	StartDate       string `json:"start_date"`
	AnchorEvent     string `json:"anchor_event"`
	Kind            string `json:"kind"`
	Days            int    `json:"days"`
	Counting        string `json:"counting"`
	Doubled         bool   `json:"doubled"`
	ManualExtraDays int    `json:"manual_extra_days"`
}

// Validate enforces the preview edge rules: exactly one anchor source (intimation_id XOR
// start_date), a positive day count, a valid counting, a well-formed optional intimation_id /
// start_date, a valid optional anchor_event and a non-negative manual_extra_days.
func (r PreviewRequest) Validate() error {
	if (r.IntimationID == "") == (r.StartDate == "") {
		return apperr.NewInvalid("preview requires exactly one of intimation_id or start_date")
	}
	return validation.ValidateStruct(&r,
		validation.Field(&r.Days, validation.Required, validation.Min(1)),
		validation.Field(&r.Counting, validation.Required,
			validation.In(string(CountingBusiness), string(CountingCalendar))),
		validation.Field(&r.IntimationID, validation.By(uuidIfPresent)),
		validation.Field(&r.StartDate, validation.Date(time.DateOnly)),
		validation.Field(&r.AnchorEvent, validation.In(
			string(AnchorMadeAvailable), string(AnchorPublished), string(AnchorDeadlineStart))),
		validation.Field(&r.ManualExtraDays, validation.Min(0)),
	)
}

// toPreviewCommand maps the validated request + the principal's tenant into the use-case command.
// The wire start_date (validated) is parsed to an optional time; anchor_event defaults to
// DEADLINE_START.
func (r PreviewRequest) toPreviewCommand(tenantID string) PreviewCommand {
	return PreviewCommand{
		TenantID:        tenantID,
		IntimationID:    r.IntimationID,
		StartDate:       parseOptionalWireDate(r.StartDate),
		AnchorEvent:     defaultAnchor(r.AnchorEvent),
		Kind:            r.Kind,
		Days:            r.Days,
		Counting:        Counting(r.Counting),
		Doubled:         r.Doubled,
		ManualExtraDays: r.ManualExtraDays,
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
