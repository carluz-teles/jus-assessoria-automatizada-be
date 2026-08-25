package certificate

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"
)

// fakeCipher — implementação em-memória de Cipher pra testes: gera DEK, cifra
// localmente, e "wrappa" a DEK com XOR contra uma master key fake (obviamente
// não é seguro; só simula a interface do KMS sem chamar a rede).
type fakeCipher struct{ master []byte }

func newFakeCipher(t *testing.T) *fakeCipher {
	t.Helper()
	m := make([]byte, 32)
	if _, err := rand.Read(m); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return &fakeCipher{master: m}
}

func (f *fakeCipher) Seal(_ context.Context, plaintext []byte) (*Envelope, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(dek)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	// "wrap" fake: xor DEK com master. Real KMS.Encrypt é opaco.
	wrapped := xor(dek, f.master)
	return &Envelope{Ciphertext: ct, Nonce: nonce, WrappedDEK: wrapped, KEKRef: "fake"}, nil
}

func (f *fakeCipher) Open(_ context.Context, env *Envelope) ([]byte, error) {
	dek := xor(env.WrappedDEK, f.master)
	block, _ := aes.NewCipher(dek)
	gcm, _ := cipher.NewGCM(block)
	return gcm.Open(nil, env.Nonce, env.Ciphertext, nil)
}

func (f *fakeCipher) Close() error { return nil }

func xor(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i%len(b)]
	}
	return out
}

// Contrato: Seal/Open é round-trip; nonce/dek fresh a cada Seal (ciphertext
// diferente pra mesmo plaintext); tampering rejeitado.

func TestCipher_RoundTrip(t *testing.T) {
	c := newFakeCipher(t)
	pt := []byte("um PFX qualquer \x00\x01\xff")
	env, err := c.Seal(context.Background(), pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(env.Ciphertext, pt) {
		t.Fatal("ciphertext == plaintext")
	}
	out, err := c.Open(context.Background(), env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(out, pt) {
		t.Fatalf("round-trip mismatch:\n want %q\n got  %q", pt, out)
	}
}

func TestCipher_NonceIsFresh(t *testing.T) {
	c := newFakeCipher(t)
	pt := []byte("mesmo plaintext")
	a, _ := c.Seal(context.Background(), pt)
	b, _ := c.Seal(context.Background(), pt)
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("mesmo plaintext gerou ciphertext igual (DEK/nonce reutilizado)")
	}
}

func TestCipher_TamperingRejected(t *testing.T) {
	c := newFakeCipher(t)
	env, _ := c.Seal(context.Background(), []byte("secreto"))
	env.Ciphertext[len(env.Ciphertext)-1] ^= 0x01
	if _, err := c.Open(context.Background(), env); err == nil {
		t.Fatal("Open deveria falhar em ciphertext adulterado")
	}
}
