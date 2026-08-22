package certificate

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/vault"
)

// dekLen is the DEK size: AES-256 needs 32 bytes.
const dekLen = 32

// envelope is the encrypted form of a .pfx blob: the AES-256-GCM ciphertext, its
// nonce, the vault-wrapped DEK, and the wrapping key's opaque ref. It is exactly
// what the entity persists — nothing here reveals the plaintext key.
type envelope struct {
	Ciphertext []byte
	Nonce      []byte
	WrappedDEK []byte
	KEKRef     string
}

// seal envelope-encrypts plaintext (the raw .pfx bytes): it mints a fresh random
// 32-byte DEK, AES-256-GCM-encrypts the blob under it with a per-record nonce,
// then wraps the DEK via the vault. The plaintext DEK is zeroed before returning
// so it does not linger on the heap. Any failure is infra (a 500) — a crypto/rand
// or vault failure is never the client's fault.
func seal(ctx context.Context, v vault.SecretVault, plaintext []byte) (envelope, error) {
	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return envelope{}, apperr.NewInfra("certificate: generate dek", err)
	}
	defer zero(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return envelope{}, apperr.NewInfra("certificate: init cipher", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return envelope{}, apperr.NewInfra("certificate: init gcm", err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return envelope{}, apperr.NewInfra("certificate: generate nonce", err)
	}

	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	wrapped, err := v.Wrap(ctx, dek)
	if err != nil {
		return envelope{}, err
	}

	return envelope{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		WrappedDEK: wrapped,
		KEKRef:     v.KekRef(),
	}, nil
}

// open reverses seal: it unwraps the DEK via the vault and AES-256-GCM-decrypts
// the blob, returning the original .pfx bytes. The plaintext DEK is zeroed after
// use. It is not called by the CRUD in this fatia (no download-the-key endpoint
// exists — the key never leaves the vault boundary), but it is the read half of
// the envelope and is unit-tested for round-trip integrity so a future
// server-side signing fatia can rely on it. A tampered/wrong-key blob fails
// authentication (infra error), never returns garbage.
func open(ctx context.Context, v vault.SecretVault, e envelope) ([]byte, error) {
	dek, err := v.Unwrap(ctx, e.WrappedDEK)
	if err != nil {
		return nil, err
	}
	defer zero(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, apperr.NewInfra("certificate: init cipher", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, apperr.NewInfra("certificate: init gcm", err)
	}

	plaintext, err := aead.Open(nil, e.Nonce, e.Ciphertext, nil)
	if err != nil {
		return nil, apperr.NewInfra("certificate: decrypt blob", err)
	}
	return plaintext, nil
}

// zero overwrites b so key material does not linger in memory after use. Go's GC
// may still copy the slice, so this is defense-in-depth, not a guarantee.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
