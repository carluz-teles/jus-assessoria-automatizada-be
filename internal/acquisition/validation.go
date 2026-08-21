package acquisition

import (
	"regexp"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"

	"github.com/jusassessoria/platform/lib/apperr"
)

// oabRegex matches an OAB registration: a two-letter uppercase UF followed by 1–6
// digits (e.g. "SP123456"). Compiled once at package level (compilation is O(n)
// and allocates).
var oabRegex = regexp.MustCompile(`^[A-Z]{2}\d{1,6}$`)

// ActivateIntegrationRequest is the POST /v1/acquisition/integrations body: the
// scope to watch. tenant_id is NOT here — it comes from the verified principal.
// credential_ref is NOT here either — it is never accepted from the client (v0
// leaves it NULL). There is no source selector: DJEN is the only activatable
// source (the sole one that DISCOVERS a process nationally, by OAB) and every
// activation targets it — DATAJUD only ENRICHES an already-discovered process
// (by number), triggered by court_record_observed, never by this endpoint.
type ActivateIntegrationRequest struct {
	Scope Scope `json:"scope"`
}

// Validate enforces the boundary rule via ozzo (method-based, not struct tags):
// the scope must be valid. A failure is a 400 at the edge (KindInvalid).
func (r ActivateIntegrationRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Scope),
	)
}

// AssignResponsibleRequest is the PUT /v1/processos/:id/responsavel body: the user to
// make responsável for the process, or null to desatribuir. UserID is a *string so the
// caller can send an explicit null (unset the responsável) distinctly from omitting it.
// tenant_id is NOT here — it comes from the verified principal. Membership (the user
// belongs to the escritório) is a domain check under the tx, not a boundary rule.
type AssignResponsibleRequest struct {
	UserID *string `json:"user_id"`
}

// Validate enforces the boundary rule via ozzo: WHEN a user_id is present it must be a
// well-formed uuid (a bad shape is a 400 at the edge, before any DB hop). A nil user_id is
// valid — it is desatribuir. Whether that uuid names a real member is a domain concern the
// use case checks under the tx (ErrResponsibleNotMember), not a shape rule here.
func (r AssignResponsibleRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.UserID, is.UUID),
	)
}

// AssignIntimacaoResponsaveisRequest is the PUT /v1/intimacoes/:id/responsaveis body:
// the two optional roles — condutor do prazo (ConductorUserID) and revisor/assinador
// (ReviewerUserID). Each is a *string so the caller can send an explicit null to
// desatribuir that specific role. Both null = remove all assignments. tenant_id comes
// from the verified principal, never the body.
type AssignIntimacaoResponsaveisRequest struct {
	ConductorUserID *string `json:"conductor_user_id"`
	ReviewerUserID  *string `json:"reviewer_user_id"`
}

// Validate enforces the boundary rules via ozzo: when either ID is present it must be
// a well-formed uuid (bad shape → 400 at the edge, before any DB hop). A nil is valid
// (desatribuir). Whether a uuid names a real member of the tenant is a domain concern
// the use case checks under the tx (ErrResponsibleNotMember), not a shape rule here.
func (r AssignIntimacaoResponsaveisRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.ConductorUserID, is.UUID),
		validation.Field(&r.ReviewerUserID, is.UUID),
	)
}

// Validate enforces the scope rules: at least one OAB, each a well-formed
// registration. tax ids are optional and unconstrained in v0. Declaring Validate
// on Scope lets ozzo validate it automatically when it is a request field.
func (s Scope) Validate() error {
	return validation.ValidateStruct(&s,
		validation.Field(&s.OAB,
			validation.Required,
			validation.Each(validation.Match(oabRegex)),
		),
	)
}

// parseOAB validates a combined "UFNÚMERO" registration (e.g. "SP123456") and
// splits it into the OABEntry the DJEN connector queries by. A bad format is a
// typed Invalid error (→ 400) raised before any network call — same shape as
// normalizeCNPJ/normalizeCEP in the lookup slice.
func parseOAB(raw string) (OABEntry, error) {
	if !oabRegex.MatchString(raw) {
		return OABEntry{}, apperr.NewInvalid("oab must be UF (2 letters) + 1-6 digits, e.g. SP123456")
	}
	return OABEntry{UF: raw[:2], Number: raw[2:]}, nil
}
