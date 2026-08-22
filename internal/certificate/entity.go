// Package certificate is the vertical slice that stores a tenant's A1 digital
// certificate (a .pfx/.p12 holding an x509 cert + its private key) SECURELY and
// exposes the metadata CRUD.
//
// SECURITY CONTRACT (non-negotiable):
//   - The private key is NEVER persisted in plaintext. The whole .pfx blob is
//     encrypted with AES-256-GCM under a per-record data key (DEK); the DEK is
//     wrapped by lib/vault (KMS in prod, a local KEK in dev) and only the wrapped
//     form is stored (envelope encryption). ciphertext + nonce + wrapped_dek +
//     kek_ref together are the only representation of the key at rest.
//   - The .pfx password is used ONLY to parse the file at upload time and is then
//     DISCARDED — never persisted, never logged. The private key extracted during
//     parse is likewise discarded; only the (re-)encrypted original blob is kept.
//   - Reads that back a screen (List) expose ONLY metadata (CertificateView),
//     never the ciphertext, nonce, DEK, or key material.
//
// Anatomy follows the project's slice convention (entity/errors/validation/mapper/
// domain/handler/repository/queries) and reuses lib/apperr, lib/database (UoW),
// lib/events (outbox), lib/httpx and lib/vault.
package certificate

import "time"

// Certificate is the aggregate: the metadata extracted from the parsed x509
// certificate plus the encrypted key material. The three byte fields
// (Ciphertext/WrappedDEK/Nonce) and KEKRef are the envelope; they are the ONLY
// place the private key lives, and never cross the API boundary — the read models
// (CertificateView) omit them entirely.
type Certificate struct {
	ID          string
	TenantID    string
	OwnerUserID string

	// Metadata parsed from the certificate — safe to display.
	SubjectCN   string
	OAB         string
	Issuer      string
	Serial      string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string // hex SHA-256 of the DER cert

	// Encrypted key material — the envelope. NEVER serialized to the client.
	Ciphertext []byte // AES-256-GCM(.pfx bytes) under the DEK
	Nonce      []byte // GCM nonce for Ciphertext
	WrappedDEK []byte // the DEK wrapped by the vault
	KEKRef     string // opaque label of the wrapping key (rotation)

	CreatedAt time.Time
	RevokedAt *time.Time // soft revoke; nil = active
}

// IsRevoked reports whether the certificate has been revoked. Revoke is a soft
// delete (revoked_at stamped) so the record stays for audit.
func (c *Certificate) IsRevoked() bool { return c.RevokedAt != nil }
