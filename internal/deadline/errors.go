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

	// ErrDeadlineNotReopenable — a POST /v1/prazos/:id/reopen on a prazo that is not NO_DEADLINE.
	// Only a "mera ciência" (NO_DEADLINE) prazo can be reopened to PENDING; any other status is
	// not a reopen candidate. CONFLICT (→ 409), distinct from the 404 miss — the prazo exists,
	// but its current status forbids the reopen.
	ErrDeadlineNotReopenable = apperr.NewConflict("deadline reopen requires a NO_DEADLINE prazo")

	// ErrTaskNotFound — the requested task id resolves to no row in the tenant (PATCH
	// /v1/tasks/:id, POST /v1/tasks/:id/done | .../dismiss). Typed not-found (→ 404), never
	// (nil, nil): a foreign or unknown id is a client-facing miss, not a swallowed empty result.
	ErrTaskNotFound = apperr.NewNotFound("task not found")

	// ErrTaskNotOpen — a POST /v1/tasks/:id/done | .../dismiss transition requested on a task
	// that is not OPEN. Concluir/dispensar leaves OPEN only: a terminal (DONE/DISMISSED) task
	// cannot transition again. CONFLICT (→ 409), distinct from the 404 miss — the task exists,
	// but its current status forbids the flip. It mirrors ErrDeadlineNotOpen (same guard shape).
	ErrTaskNotOpen = apperr.NewConflict("task transition requires an OPEN task")

	// ErrIntimationNotFound — the requested intimation id resolves to no row in the tenant.
	// POST /v1/tasks hits this when intimation_id is set but the row is missing/foreign
	// while resolving the herdado assignee (GetIntimationAssignee). Typed not-found, never
	// (nil, nil).
	ErrIntimationNotFound = apperr.NewNotFound("intimation not found")

	// ErrTaskItemNotFound — the requested checklist item resolves to no row under the parent
	// task in the tenant (PATCH/DELETE /v1/tasks/:id/items/:itemId), OR the parent task itself
	// is unknown/foreign on a create (POST /v1/tasks/:id/items). Typed not-found (→ 404), never
	// (nil, nil): a foreign or cross-task itemId is a client-facing miss, not a swallowed empty.
	ErrTaskItemNotFound = apperr.NewNotFound("task item not found")

	// ErrActionItemNotFound — an actionitem.created/confirmed event's action_item_id resolves
	// to no row in the tenant (GetActionItemCourtRecordID, fatia 3). It should not happen (the
	// producer emits the id it just committed, and a confiável item is never deleted by
	// actionitem's own re-analysis guard), so it surfaces as a typed not-found — terminal
	// (isTerminal), the same treatment as ErrCourtRecordNotFound.
	ErrActionItemNotFound = apperr.NewNotFound("action item not found")

	// ErrTaskExistsForActionItem — InsertTask's ON CONFLICT (action_item_id) DO NOTHING
	// (0087's UNIQUE) found a task already bound to this providência. The use case treats this
	// as an idempotent no-op (a redelivered actionitem.created/confirmed never mints a second
	// task), not an error — CONFLICT kind only for symmetry with ErrDeadlineExists, since the
	// caller never surfaces it past the use case.
	ErrTaskExistsForActionItem = apperr.NewConflict("task already exists for action item")

	// ErrCalcMemoryExists — a calc_memory already exists for the deadline (the 1:1
	// deadline_id UNIQUE). The idempotent INSERT ... ON CONFLICT DO NOTHING yields no
	// row; the use case treats this as a no-op (the memory is already there). Typed
	// conflict, never (nil, nil).
	ErrCalcMemoryExists = apperr.NewConflict("calc memory already exists for deadline")

	// ErrCrossValidationExists — a cross_validation already exists for the deadline (the
	// 1:1 deadline_id UNIQUE). Same idempotent shape as ErrCalcMemoryExists.
	ErrCrossValidationExists = apperr.NewConflict("cross validation already exists for deadline")

	// ErrCrossValidationNotFound — POST /v1/prazos/:id/apurar-divergencia on a deadline that
	// never got a cross_validation row (no prazo_declarado at birth, so nothing was ever
	// cross-checked). Typed not-found (→ 404), never (nil, nil): there is nothing to apurar.
	ErrCrossValidationNotFound = apperr.NewNotFound("cross validation not found for deadline")

	// ErrCalcMemoryNotFound — POST /v1/prazos/:id/apurar-tipo on a deadline that has no
	// calc_memory row (the V1 audit trail is written at birth alongside every deadline, so a
	// miss here signals a pre-V1 row or a config fault). Typed not-found, never (nil, nil).
	ErrCalcMemoryNotFound = apperr.NewNotFound("calc memory not found for deadline")

	// ErrDeadlineNotApuravel — apurar-divergencia/apurar-tipo requested on a prazo that is
	// CUMPRIDO (MET) or BAIXADO_MANUAL (CANCELLED): its dates/selo are frozen, closed cases
	// are not apuráveis. CONFLICT (→ 409), distinct from the 404 miss.
	ErrDeadlineNotApuravel = apperr.NewConflict("deadline is not apuravel: only a non-terminal prazo can be apurado")

	// ErrClassifierUnavailable — the omissa-intimação IA fallback (classify.go) has no
	// generator configured (OpenRouter unset). OnIntimationObserved treats it as a signal to
	// no-op the classification (never chuta), not as a failure of the ingest.
	ErrClassifierUnavailable = apperr.NewUnavailable("type classifier unavailable: no LLM generator configured", nil)

	// ErrDeadlineNotDivergent — apurar-divergencia requested when cross_validation.resultado is
	// not "divergente", OR the divergência was already decided (decisao already set) —
	// idempotency guard: a second apuração on an already-resolved divergência is refused rather
	// than silently reprocessed. apurar-tipo reuses it when selo is already "confiavel" (nothing
	// left to apurar). CONFLICT (→ 409), distinct from the 404 miss.
	ErrDeadlineNotDivergent = apperr.NewConflict("deadline has no pending divergência or tipo to apurar")
)
