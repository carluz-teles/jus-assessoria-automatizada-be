// Package vault is the KEY-WRAPPING core for envelope encryption: it wraps and
// unwraps a per-record data encryption key (DEK) so a plaintext DEK never rests
// anywhere. It is infra, not domain — slices inject a SecretVault and never see
// KMS or the local KEK directly.
//
// Envelope encryption (the shape the certificate slice uses): a random 32-byte
// DEK encrypts the secret blob with AES-256-GCM; the DEK itself is then WRAPPED
// by this vault (by a KMS CMK in prod, or by a local KEK in dev) and only the
// wrapped form is persisted alongside the ciphertext. Reading reverses it:
// Unwrap the DEK, then AES-GCM-open the blob. The plaintext KEK/CMK material
// never leaves the vault boundary (for KMS it never leaves AWS at all).
//
// KekRef is an opaque label persisted with each record so a future key rotation
// can tell which key wrapped a given DEK. It is NOT a secret.
package vault

import "context"

// KekRefLocal is the KekRef stamped by the local (dev) vault. KekRefKMS is the
// prefix the KMS vault stamps, followed by the CMK id it wrapped under, so a
// rotated deployment can still identify the wrapping key of an old record.
const (
	KekRefLocal = "local"
	KekRefKMS   = "kms"
)

// SecretVault wraps and unwraps a data encryption key (DEK). Wrap turns a
// plaintext DEK into an opaque wrapped form safe to persist; Unwrap reverses it.
// KekRef reports the opaque label of the wrapping key, persisted with the record
// for rotation. Implementations must be safe for concurrent use.
type SecretVault interface {
	Wrap(ctx context.Context, plaintextDEK []byte) (wrappedDEK []byte, err error)
	Unwrap(ctx context.Context, wrappedDEK []byte) (plaintextDEK []byte, err error)
	KekRef() string
}
