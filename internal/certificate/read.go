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
