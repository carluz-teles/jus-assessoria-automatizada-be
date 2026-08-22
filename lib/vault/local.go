package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/jusassessoria/platform/lib/apperr"
)

// localVault wraps a DEK under a locally-held 32-byte KEK using AES-256-GCM. It
// is the DEV vault (no external dependency); production uses kmsVault instead.
// The KEK is held in memory only, sourced from the CERT_KEK env var at
// construction — never logged, never persisted.
type localVault struct {
	aead cipher.AEAD
}

var _ SecretVault = (*localVault)(nil)

// kekLen is the required KEK length: AES-256 needs exactly 32 bytes.
const kekLen = 32

// NewLocalVault builds a local vault from a base64-encoded 32-byte KEK (the
// CERT_KEK env value). A missing, malformed, or wrong-length KEK is a caller
// configuration mistake, so it returns apperr.Invalid — the slice validates this
// lazily at its own construction, never globally at boot, so binaries that do not
// use certificates are unaffected.
func NewLocalVault(kekBase64 string) (SecretVault, error) {
	if kekBase64 == "" {
		return nil, apperr.NewInvalid("vault: CERT_KEK is required for the local vault")
	}

	kek, err := base64.StdEncoding.DecodeString(kekBase64)
	if err != nil {
		return nil, apperr.NewInvalid("vault: CERT_KEK must be valid base64")
	}
	if len(kek) != kekLen {
		return nil, apperr.NewInvalid(fmt.Sprintf("vault: CERT_KEK must decode to %d bytes (AES-256)", kekLen))
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		// Unreachable given the length check above, but never assume.
		return nil, apperr.NewInvalid("vault: CERT_KEK is not a valid AES key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, apperr.NewInfra("vault: init GCM", err)
	}

	return &localVault{aead: aead}, nil
}

// Wrap seals the DEK under the local KEK with AES-256-GCM. The nonce is prepended
// to the ciphertext so Unwrap is self-contained (nonce || ciphertext+tag).
func (v *localVault) Wrap(_ context.Context, plaintextDEK []byte) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, apperr.NewInfra("vault: read nonce", err)
	}
	// Seal appends the ciphertext to nonce, so the returned slice is nonce||ct.
	return v.aead.Seal(nonce, nonce, plaintextDEK, nil), nil
}

// Unwrap opens a wrapped DEK produced by Wrap. A too-short blob or a failed
// authentication (wrong KEK, tampered bytes) is an infra error — never a silent
// wrong key. The plaintext DEK is returned to the caller, who must zero it after
// use.
func (v *localVault) Unwrap(_ context.Context, wrappedDEK []byte) ([]byte, error) {
	ns := v.aead.NonceSize()
	if len(wrappedDEK) < ns {
		return nil, apperr.NewInfra("vault: wrapped dek too short", nil)
	}
	nonce, ct := wrappedDEK[:ns], wrappedDEK[ns:]
	plaintext, err := v.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, apperr.NewInfra("vault: unwrap dek", err)
	}
	return plaintext, nil
}

// KekRef reports the local wrapping-key label.
func (v *localVault) KekRef() string { return KekRefLocal }
