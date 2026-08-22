package certificate

import "time"

// CertificateView is the client-facing read model — the ONLY shape that crosses
// the API boundary. It is METADATA ONLY: the ciphertext, nonce, wrapped DEK and
// KEK ref never appear here, so no key material is ever serialized to a response.
//
// The JSON contract is fixed (the FE consumes it verbatim). owner_user_name is a
// pointer so it serializes as null when the joined app_user is gone; revoked_at is
// a pointer so an active certificate serializes it as null.
type CertificateView struct {
	ID            string     `json:"id"`
	SubjectCN     string     `json:"subject_cn"`
	OAB           string     `json:"oab"`
	Issuer        string     `json:"issuer"`
	Serial        string     `json:"serial"`
	NotBefore     time.Time  `json:"not_before"`
	NotAfter      time.Time  `json:"not_after"`
	Fingerprint   string     `json:"fingerprint"`
	OwnerUserID   string     `json:"owner_user_id"`
	OwnerUserName *string    `json:"owner_user_name"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
}

// PreviewResult is the POST /v1/certificates/preview response — the read-only
// "Validação" step of the FE wizard. It PARSES the .pfx and REPORTS the metadata
// plus a set of checks, WITHOUT storing anything and WITHOUT persisting or logging
// the password. Nothing here is a secret: the private key never leaves the parse.
//
// A wrong password / malformed file still fails with the typed 4xx error (the FE
// shows the field error); a successful parse returns this, even when a check is
// false (e.g. an expired cert previews with nao_expirado=false so the wizard can
// warn instead of blindly POSTing).
type PreviewResult struct {
	SubjectCN   string        `json:"subject_cn"`
	OAB         string        `json:"oab"`
	Issuer      string        `json:"issuer"`
	Serial      string        `json:"serial"`
	NotBefore   time.Time     `json:"not_before"`
	NotAfter    time.Time     `json:"not_after"`
	Fingerprint string        `json:"fingerprint"`
	Checks      PreviewChecks `json:"checks"`
}

// PreviewChecks are the boolean results of the validation step. Only checks the
// BACKEND can genuinely determine at preview time are here:
//   - NaoExpirado: the cert's validity window contains "now" (not expired AND not
//     not-yet-valid).
//   - CadeiaOk: a CA chain was bundled in the .pfx (an ICP-Brasil A1 ships its AC
//     chain); a bare leaf previews CadeiaOk=false.
//   - TitularConfere: the leaf carries a non-empty subject CN (a titular is
//     identifiable). NOTE (débito honesto): matching the titular against the
//     EXPECTED lawyer, and OAB-is-monitored, require context this slice does not
//     own (the wizard's expected name; the acquisition slice's monitored-OAB set,
//     which cannot be read cross-slice without breaking slice isolation). The FE
//     cross-checks those using OAB/SubjectCN returned above; a future event-driven
//     enrichment can add them here.
type PreviewChecks struct {
	NaoExpirado    bool `json:"nao_expirado"`
	CadeiaOk       bool `json:"cadeia_ok"`
	TitularConfere bool `json:"titular_confere"`
}
