// Package vault implements envelope encryption for secrets the platform must
// custody at rest (the portal_credential password is the first producer,
// docs/erd-tribunal-scraping.md §6). It is pure infra — no domain rule, no SQL,
// no HTTP — so any slice can inject it exactly like lib/storage: the slice's
// repository persists the Sealed bytes, this package only seals/opens them.
//
// Scheme: envelope encryption with AES-256-GCM at both layers. Each secret gets
// its own random 32-byte Data Encryption Key (DEK), which encrypts the plaintext;
// the DEK itself is encrypted under a single Key Encryption Key (KEK) sourced
// from an env var (lib/config, caarlos0/env). A DEK-per-secret bounds the blast
// radius of any one row leaking; the KEK never touches the database, only the
// process environment. Rotating the KEK later only requires re-wrapping every
// stored DEK, not re-encrypting every ciphertext — the migration path to a real
// KMS (docs §6 Option B) re-wraps the same way without touching Sealed.Ciphertext.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// keySize is the length in bytes of both the KEK and every DEK — AES-256.
const keySize = 32

// ErrInvalidKEK reports a KEK that is not valid base64 or does not decode to
// exactly 32 bytes. Returned by New so the caller (the slice's boot wiring)
// fails fast instead of sealing secrets under a malformed key.
var ErrInvalidKEK = errors.New("vault: KEK must be base64-encoded 32 bytes (AES-256)")

// ErrSealedSecretTampered reports that Open's authentication tag check failed —
// either the stored bytes were corrupted/tampered, or the KEK does not match the
// one that sealed them (e.g. after an unplanned KEK rotation). AES-GCM's tag
// check surfaces both as the same generic failure, by design (no oracle for an
// attacker to distinguish "wrong key" from "tampered ciphertext").
var ErrSealedSecretTampered = errors.New("vault: secret failed authentication (tampered, corrupted, or wrong key)")

// Sealed is a secret at rest: the plaintext ciphertext under its own DEK, and
// that DEK's own ciphertext under the KEK. Every field is safe to persist as-is
// (bytea columns) — none of them, alone or together, discloses the plaintext
// without the KEK. This is the shape lib/vault hands to a slice's repository.
type Sealed struct {
	Ciphertext    []byte // plaintext encrypted under the DEK (AES-GCM, DEKNonce)
	Nonce         []byte // GCM nonce used for Ciphertext
	DEKCiphertext []byte // DEK encrypted under the KEK (AES-GCM, DEKNonce... see DEKNonce)
	DEKNonce      []byte // GCM nonce used for DEKCiphertext
}

// Vault seals and opens secrets under one KEK. It holds no persistence port and
// no tenant concept — those belong to the slice that owns the secret's table
// (docs §6: portal_credential.credential_ref points at the slice's own row).
type Vault struct {
	kek []byte
}

// New builds a Vault from a base64-encoded 32-byte KEK (typically read from an
// env var by the caller — see lib/config). It validates the key eagerly so a
// malformed KEK fails at the point of use (boot wiring), the same convention
// storage.New and the other optional adapters follow, never mid-request.
func New(kekBase64 string) (*Vault, error) {
	kek, err := decodeKEK(kekBase64)
	if err != nil {
		return nil, err
	}
	return &Vault{kek: kek}, nil
}

func decodeKEK(kekBase64 string) ([]byte, error) {
	kek, err := base64.StdEncoding.DecodeString(kekBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKEK, err)
	}
	if len(kek) != keySize {
		return nil, ErrInvalidKEK
	}
	return kek, nil
}

// Seal encrypts plaintext under a freshly generated DEK, then encrypts that DEK
// under the Vault's KEK. The returned Sealed is ready to persist; plaintext never
// appears in it. A fresh DEK per call means two seals of the same plaintext never
// produce the same Sealed — no ciphertext-equality oracle for a reader of the table.
func (v *Vault) Seal(plaintext string) (Sealed, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, fmt.Errorf("vault: generating DEK: %w", err)
	}

	ciphertext, nonce, err := encrypt(dek, []byte(plaintext))
	if err != nil {
		return Sealed{}, fmt.Errorf("vault: sealing plaintext under DEK: %w", err)
	}

	dekCiphertext, dekNonce, err := encrypt(v.kek, dek)
	if err != nil {
		return Sealed{}, fmt.Errorf("vault: sealing DEK under KEK: %w", err)
	}

	return Sealed{
		Ciphertext:    ciphertext,
		Nonce:         nonce,
		DEKCiphertext: dekCiphertext,
		DEKNonce:      dekNonce,
	}, nil
}

// Open reverses Seal: unwraps the DEK under the KEK, then decrypts the
// ciphertext under the recovered DEK. Any authentication failure at either layer
// — tampering, corruption, or a KEK that no longer matches — collapses to
// ErrSealedSecretTampered, never a partial/best-effort plaintext.
func (v *Vault) Open(s Sealed) (string, error) {
	dek, err := decrypt(v.kek, s.DEKCiphertext, s.DEKNonce)
	if err != nil {
		return "", ErrSealedSecretTampered
	}

	plaintext, err := decrypt(dek, s.Ciphertext, s.Nonce)
	if err != nil {
		return "", ErrSealedSecretTampered
	}

	return string(plaintext), nil
}

// encrypt runs AES-256-GCM with a fresh random nonce, returning the ciphertext
// (tag included, as crypto/cipher.Seal appends it) and the nonce used.
func encrypt(key, plaintext []byte) (ciphertext, nonce []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generating nonce: %w", err)
	}

	return gcm.Seal(nil, nonce, plaintext, nil), nonce, nil
}

// decrypt reverses encrypt. A wrong key or tampered ciphertext/nonce fails the
// GCM authentication tag check and returns an error — never a garbled plaintext.
func decrypt(key, ciphertext, nonce []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("building AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// GenerateKEK returns a fresh, base64-encoded 32-byte key suitable for the
// VAULT_KEK_BASE64 env var. It is exported for operators provisioning a new
// environment (e.g. a one-off `go run` or a small CLI), not used by the
// application at runtime.
func GenerateKEK() (string, error) {
	kek := make([]byte, keySize)
	if _, err := rand.Read(kek); err != nil {
		return "", fmt.Errorf("vault: generating KEK: %w", err)
	}
	return base64.StdEncoding.EncodeToString(kek), nil
}
