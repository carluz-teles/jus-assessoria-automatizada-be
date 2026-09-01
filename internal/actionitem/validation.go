package actionitem

import (
	"errors"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// validation.go holds the app-level closed-set checks (text + CHECK columns never enforce
// more than the DB needs) and the motor de precedência that turns "did the teor declare the
// tipo" into {TipoOrigem, TipoStatus} — the SAME precedence shape as the Motor de Prazos
// (declarado > ia > manual), applied to the tipo instead of the date (docs §3).

// ReclassifyRequest is the POST /v1/action-items/:id/reclassificar body (fatia 5, docs §7
// questão 4). tenant_id/id come from the principal/path, never the body.
type ReclassifyRequest struct {
	PieceProfileKey string `json:"piece_profile_key"`
	Tipo            string `json:"tipo"`
}

// Validate enforces both fields are present AND members of their closed sets — the SAME
// allowlists sanitizeCandidate/validate() use — so a bad value is a 400 at the edge
// (KindInvalid via httpx.WriteValidationError), never an opaque FK/CHECK-violation 500.
func (r ReclassifyRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.PieceProfileKey, validation.Required, validation.By(isKnownPieceProfileKey)),
		validation.Field(&r.Tipo, validation.Required, validation.By(isValidTipoField)),
	)
}

// isKnownPieceProfileKey adapts validPieceProfileKey to ozzo's validation.By signature.
func isKnownPieceProfileKey(value any) error {
	s, _ := value.(string)
	if !validPieceProfileKey(s) {
		return errors.New("unknown piece_profile_key")
	}
	return nil
}

// isValidTipoField adapts validTipo to ozzo's validation.By signature. Named with the
// "Field" suffix to avoid colliding with the unexported validTipo it wraps.
func isValidTipoField(value any) error {
	s, _ := value.(string)
	if !validTipo(s) {
		return errors.New("invalid tipo")
	}
	return nil
}

func validTipoOrigem(o TipoOrigem) bool {
	switch o {
	case TipoOrigemDeclarado, TipoOrigemIA, TipoOrigemManual:
		return true
	default:
		return false
	}
}

func validTipoStatus(s TipoStatus) bool {
	switch s {
	case TipoStatusConfiavel, TipoStatusAConfirmar:
		return true
	default:
		return false
	}
}

func validTipo(t string) bool {
	switch t {
	case TipoContestar, TipoRecorrer, TipoManifestar, TipoCumprir, TipoCiencia:
		return true
	default:
		return false
	}
}

// validPieceProfileKey reports whether key is one of the v1 catalog's known types —
// the guard that keeps a hallucinated classifier output from reaching the FK as a hard
// 500 (see entity.go's knownPieceProfileKeys for why this is a local allowlist, not a
// cross-slice query).
func validPieceProfileKey(key string) bool {
	return knownPieceProfileKeys[key]
}

// deriveTipoOrigemStatus is the motor de precedência (docs §3): a teor that DECLARES the
// providência's tipo explicitly is trusted outright (declarado/confiável, no friction,
// exactly like a declared prazo); anything else is the classifier's inference and is born
// a_confirmar — the piso that never lets an IA guess become confiável on its own. "manual"
// (o advogado sobrepõe) is not produced by this function: it is only ever set by a future
// override endpoint, out of this fatia's scope (see docs/erd-costura-providencia-tarefa-
// peca.md §3's three-source table).
func deriveTipoOrigemStatus(declarado bool) (TipoOrigem, TipoStatus) {
	if declarado {
		return TipoOrigemDeclarado, TipoStatusConfiavel
	}
	return TipoOrigemIA, TipoStatusAConfirmar
}

// sanitizeCandidate degrades a classifier-proposed tipo/gera_peca/piece_profile_key into a
// value the FK/CHECKs can always accept, applying the repo's "viés seguro" bias (the same
// pattern analise.go's resolveAssignee/clampDueDate already use): an unknown tipo degrades
// to ciência (no peça, ever inferred safely); gera_peca without a KNOWN piece_profile_key
// degrades to no peça at all, rather than risking an FK violation that would fail the whole
// materialization event.
func sanitizeCandidate(tipo string, geraPeca bool, pieceProfileKey string) (string, bool, string) {
	if !validTipo(tipo) {
		return TipoCiencia, false, ""
	}
	if !geraPeca {
		return tipo, false, ""
	}
	if !validPieceProfileKey(pieceProfileKey) {
		return tipo, false, ""
	}
	return tipo, true, pieceProfileKey
}

// validate enforces the insert-time invariants migration 0078's CHECKs also enforce
// (belt-and-suspenders, same posture as every other slice's entity.validate()):
// gera_peca and piece_profile_key travel together, and confianca only accompanies an
// ia-derived classification.
func (a *ActionItem) validate() error {
	if !validTipoOrigem(a.TipoOrigem) {
		return errInvalidTipoOrigem
	}
	if !validTipoStatus(a.TipoStatus) {
		return errInvalidTipoStatus
	}
	if a.GeraPeca && a.PieceProfileKey == "" {
		return errGeraPecaWithoutProfile
	}
	if !a.GeraPeca && a.PieceProfileKey != "" {
		return errProfileWithoutGeraPeca
	}
	if a.TipoOrigem != TipoOrigemIA && a.Confianca != nil {
		return errConfiancaWithoutIA
	}
	return nil
}
