package deadline

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. This domain is async (a listener, not an HTTP
// edge), so the Kind mostly drives logs and the asynq retry decision rather than a
// status code. Absence is always a typed error from the repository, never (nil, nil).
var (
	// ErrCourtRecordNotFound — the event's court_record_id resolves to no row in the
	// tenant. It should not happen (the producer emits the id it just wrote), so it
	// surfaces as a typed not-found rather than being swallowed.
	ErrCourtRecordNotFound = apperr.NewNotFound("court record not found")

	// ErrRuleNotFound — no active deadline_rule matched, not even the '*' catch-all.
	// The 0024 seed guarantees a catch-all, so this signals a missing/broken seed
	// (a config fault), not a normal path. Typed, never (nil, nil).
	ErrRuleNotFound = apperr.NewNotFound("deadline rule not found")

	// ErrDeadlineExists — a deadline already exists for the intimação (the 1:1
	// notification_id UNIQUE). The idempotent INSERT ... ON CONFLICT DO NOTHING yields
	// no row; the use case treats this as a no-op (the prazo is already there), so a
	// second observed for the same intimação never opens a phantom prazo.
	ErrDeadlineExists = apperr.NewConflict("deadline already exists for intimation")

	// ErrDeadlineNotFound — the requested prazo id resolves to no row in the tenant
	// (GET /v1/prazos/:id). Typed not-found (→ 404 at the edge), never (nil, nil): a
	// foreign or unknown id is a client-facing miss, not a swallowed empty result.
	ErrDeadlineNotFound = apperr.NewNotFound("deadline not found")

	// ErrDeadlineNotAdjustable — a PATCH /v1/prazos/:id ajuste on a prazo that is not in an
	// active state. Only a PENDING (suggestion) or OPEN (confirmed) prazo may be recalculated;
	// a MET/MISSED/CANCELLED prazo is closed and its dates are frozen. It is a CONFLICT (→ 409),
	// a distinct signal from the 404 miss — the prazo exists, but its state forbids the ajuste.
	ErrDeadlineNotAdjustable = apperr.NewConflict("deadline is not adjustable: only a PENDING or OPEN prazo can be adjusted")

	// ErrDeadlineNotOpen — a POST /v1/prazos/:id/met | .../missed transition requested on a
	// prazo that is not OPEN. Marking cumprido/perdido leaves OPEN only: a PENDING suggestion
	// must be confirmed first, and a terminal prazo cannot transition again. CONFLICT (→ 409),
	// distinct from the 404 miss — the prazo exists, but its current status forbids the flip.
	ErrDeadlineNotOpen = apperr.NewConflict("deadline transition requires an OPEN prazo")

	// ErrTaskNotFound — the requested task id resolves to no row in the tenant (PATCH
	// /v1/tasks/:id, POST /v1/tasks/:id/done | .../dismiss). Typed not-found (→ 404), never
	// (nil, nil): a foreign or unknown id is a client-facing miss, not a swallowed empty result.
	ErrTaskNotFound = apperr.NewNotFound("task not found")

	// ErrTaskNotOpen — a POST /v1/tasks/:id/done | .../dismiss transition requested on a task
	// that is not OPEN. Concluir/dispensar leaves OPEN only: a terminal (DONE/DISMISSED) task
	// cannot transition again. CONFLICT (→ 409), distinct from the 404 miss — the task exists,
	// but its current status forbids the flip. It mirrors ErrDeadlineNotOpen (same guard shape).
	ErrTaskNotOpen = apperr.NewConflict("task transition requires an OPEN task")

	// ErrTaskItemNotFound — the requested checklist item resolves to no row under the parent
	// task in the tenant (PATCH/DELETE /v1/tasks/:id/items/:itemId), OR the parent task itself
	// is unknown/foreign on a create (POST /v1/tasks/:id/items). Typed not-found (→ 404), never
	// (nil, nil): a foreign or cross-task itemId is a client-facing miss, not a swallowed empty.
	ErrTaskItemNotFound = apperr.NewNotFound("task item not found")
)
