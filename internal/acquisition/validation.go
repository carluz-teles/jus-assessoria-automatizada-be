package acquisition

import (
	"regexp"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// oabRegex matches an OAB registration: a two-letter uppercase UF followed by 1–6
// digits (e.g. "SP123456"). Compiled once at package level (compilation is O(n)
// and allocates).
var oabRegex = regexp.MustCompile(`^[A-Z]{2}\d{1,6}$`)

// ActivateIntegrationRequest is the POST /v1/acquisition/integrations body: the
// set of sources to activate and the scope they all share. tenant_id is NOT here
// — it comes from the verified principal. credential_ref is NOT here either — it
// is never accepted from the client (v0 leaves it NULL).
type ActivateIntegrationRequest struct {
	Sources []string `json:"sources"`
	Scope   Scope    `json:"scope"`
}

// Validate enforces the boundary rules via ozzo (method-based, not struct tags):
// sources must be non-empty and a subset of the activatable sources (DJEN,
// DATAJUD — UPLOAD/MNI and anything else are rejected), and the scope must be
// valid. A failure here is a 400 at the edge (KindInvalid → 400).
func (r ActivateIntegrationRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Sources,
			validation.Required,
			validation.Each(validation.In(SourceDJEN, SourceDATAJUD)),
		),
		validation.Field(&r.Scope),
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
