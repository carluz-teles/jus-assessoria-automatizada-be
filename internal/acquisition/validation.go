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

// UpdateProcessoManualRequest é o corpo do PATCH /v1/processos/:id: os campos que o
// advogado preenche à mão no cockpit — a fase (override manual, vence a derivada) e o valor
// da causa. Ambos *ponteiro: omitir deixa o campo como está (PATCH parcial). tenant_id vem do
// principal, nunca do body.
type UpdateProcessoManualRequest struct {
	Phase      *string  `json:"phase"`
	ClaimValue *float64 `json:"claim_value"`
}

// Validate: quando presente, a fase tem que estar no conjunto fechado do stepper e o valor
// da causa não pode ser negativo (400 na borda). Ambos nil é válido (no-op).
func (r UpdateProcessoManualRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Phase, validation.In(
			FaseConhecimento, FaseInstrucao, FaseSentenca, FaseRecurso, FaseExecucao,
		)),
		validation.Field(&r.ClaimValue, validation.Min(0.0)),
	)
}

// BulkAssignResponsibleRequest é o corpo de POST /v1/processos/bulk/responsavel:
// atribui o responsável a vários processos. Dois modos: All=true aplica a TODA a
// faixa/filtro atual (filtros espelham o GET /processos; inclui os itens ainda não
// paginados); senão aplica aos IDs (court_record ids — mesma granularidade do
// PUT /processos/:id/responsavel). UserID nil desatribui; mesmo nome de campo do
// endpoint single-item (AssignResponsibleRequest.UserID), pra manter consistência.
type BulkAssignResponsibleRequest struct {
	UserID *string  `json:"user_id"`
	All    bool     `json:"all"`
	IDs    []string `json:"ids"`
	// filtros (usados só quando All=true) — espelham o GET /processos.
	Search    string `json:"search"`
	Court     string `json:"court"`
	Lifecycle string `json:"lifecycle"`
	Degree    string `json:"degree"`
	Assignee  string `json:"assignee"`
}

// Validate: user_id (quando presente) uuid; no modo por-ids, ao menos um id, cada
// um uuid. A pertinência do user_id ao tenant é checada no caso de uso (sob a tx).
func (r BulkAssignResponsibleRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.UserID, is.UUID),
		validation.Field(&r.IDs,
			validation.When(!r.All, validation.Required),
			validation.Each(is.UUID),
		),
	)
}

// AssignIntimacaoResponsavelRequest is the PUT /v1/intimacoes/:id/responsavel body:
// o responsável único da intimação (0057, ex-conductor/reviewer). *string permite
// null explícito para desatribuir. tenant_id vem do principal, nunca do body.
type AssignIntimacaoResponsavelRequest struct {
	AssigneeUserID *string `json:"assignee_user_id"`
}

// Validate: quando presente o id precisa ser um uuid bem formado (400 na borda,
// antes de qualquer DB hop). nil é válido (desatribuir). Se o uuid corresponde a
// um membro real do tenant é responsabilidade do caso de uso (ErrResponsibleNotMember).
func (r AssignIntimacaoResponsavelRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.AssigneeUserID, is.UUID),
	)
}

// BulkAssignResponsavelRequest é o corpo de POST /v1/intimacoes/bulk/responsavel:
// atribui o responsável a várias intimações. Dois modos: All=true aplica a TODA a
// faixa/filtro atual (filtros espelham o GET /intimacoes; inclui os itens ainda não
// paginados); senão aplica aos IDs. AssigneeUserID nil desatribui.
type BulkAssignResponsavelRequest struct {
	AssigneeUserID *string  `json:"assignee_user_id"`
	All            bool     `json:"all"`
	IDs            []string `json:"ids"`
	// filtros (usados só quando All=true) — espelham o GET /intimacoes.
	Urgencia      string `json:"urgencia"`
	NaoConfirmado bool   `json:"nao_confirmado"`
	Search        string `json:"search"`
	Type          string `json:"type"`
	UserStatus    string `json:"user_status"`
	Court         string `json:"court"`
	Assignee      string `json:"assignee"`
}

// Validate: assignee (quando presente) uuid; no modo por-ids, ao menos um id, cada
// um uuid. A pertinência do assignee ao tenant é checada no caso de uso (sob a tx).
func (r BulkAssignResponsavelRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.AssigneeUserID, is.UUID),
		validation.Field(&r.IDs,
			validation.When(!r.All, validation.Required),
			validation.Each(is.UUID),
		),
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

// AddWatchedOABRequest is the POST /v1/acquisition/watched-oabs body: one OAB
// registration ("UFNUMBER", e.g. "SP123456") to add to the tenant's DJEN watch.
type AddWatchedOABRequest struct {
	OAB string `json:"oab"`
}

// Validate enforces the boundary rule: OAB must be present and a well-formed
// registration (reuses oabRegex — the same shape ActivateIntegrationRequest checks).
func (r AddWatchedOABRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.OAB, validation.Required, validation.Match(oabRegex)),
	)
}

// ToggleWatchedOABRequest is the PATCH /v1/acquisition/watched-oabs/:oab body: the
// Termos liga/desliga switch. No further shape validation — a bool is always valid.
type ToggleWatchedOABRequest struct {
	Enabled bool `json:"enabled"`
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
