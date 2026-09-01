package actionitem

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors (CLAUDE.md inegociável). Absence is always a typed
// error from the repository, never (nil, nil).
var (
	// ErrActionItemNotFound — the requested action_item id resolves to no row in the
	// tenant (POST /v1/action-items/:id/confirmar | .../descartar). Typed not-found
	// (→ 404 at the edge), never (nil, nil).
	ErrActionItemNotFound = apperr.NewNotFound("action item not found")

	// ErrActionItemDiscarded — a confirmar attempt on an already-DISCARDED action_item.
	// Descartar is terminal: an item the lawyer already dismissed cannot be confirmed back
	// into existence. CONFLICT (→ 409), distinct from the 404 miss — the row exists, but
	// its current status forbids the transition. Descartar itself has no such guard: it is
	// idempotent (redescartar an already-DISCARDED item is a no-op success).
	ErrActionItemDiscarded = apperr.NewConflict("action item already discarded")

	// ErrActionItemConflict — a confirmar/descartar guarded UPDATE touched zero rows even
	// though the pre-read found the item in a transitionable state: a concurrent request
	// won the race between the read and the write. Rare; the caller may safely retry.
	ErrActionItemConflict = apperr.NewConflict("action item was concurrently modified, retry")

	// ErrActionItemHasFiledDraft — POST /v1/action-items/:id/reclassificar on a providência
	// whose task already produced a FILED (protocolada) draft. Reclassifying after filing
	// would orphan the paper trail — the providência is frozen once its peça reached the
	// court. CONFLICT (→ 409); no UPDATE happens.
	ErrActionItemHasFiledDraft = apperr.NewConflict("action item already has a filed draft; cannot reclassify")

	// ErrUnknownPieceProfileKey — POST /v1/action-items/:id/reclassificar with a
	// piece_profile_key outside the v1 catalog (knownPieceProfileKeys, entity.go). The
	// handler's ozzo Validate() already catches this at the edge (400); Reclassificar
	// re-checks defensively so a direct (non-HTTP) caller never reaches the FK with a bad
	// key. INVALID.
	ErrUnknownPieceProfileKey = apperr.NewInvalid("unknown piece_profile_key")

	// ErrInvalidTipoReclassify — POST /v1/action-items/:id/reclassificar with a tipo outside
	// the closed set (validTipo). Same defensive posture as ErrUnknownPieceProfileKey.
	ErrInvalidTipoReclassify = apperr.NewInvalid("invalid tipo")

	// errInvalidTipoOrigem/errInvalidTipoStatus/errGeraPecaWithoutProfile/
	// errProfileWithoutGeraPeca/errConfiancaWithoutIA back ActionItem.validate()'s
	// belt-and-suspenders checks (validation.go) — the same invariants migration 0078's
	// CHECK constraints enforce at the DB layer. Unexported: these never cross the
	// materialization listener as a client-facing response, only as a defensive guard
	// against a bug in this slice's own construction of the entity.
	errInvalidTipoOrigem      = apperr.NewInvalid("invalid tipo_origem")
	errInvalidTipoStatus      = apperr.NewInvalid("invalid tipo_status")
	errGeraPecaWithoutProfile = apperr.NewInvalid("gera_peca requires a piece_profile_key")
	errProfileWithoutGeraPeca = apperr.NewInvalid("piece_profile_key is only set when gera_peca is true")
	errConfiancaWithoutIA     = apperr.NewInvalid("confianca is only set when tipo_origem is ia")
)
