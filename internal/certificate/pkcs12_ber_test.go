package certificate

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os/exec"
	"testing"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// berifyOuterSequence rewrites a DER-encoded top-level SEQUENCE (the PKCS#12
// PFX structure starts with one) to use BER indefinite-length encoding
// instead — tag + 0x80 length octet, content unchanged, terminated by a
// two-byte End-of-Contents marker. This is exactly the class of file
// go-pkcs12 rejects (encoding/asn1 only supports DER) but that real .pfx
// exports sometimes use and openssl reads natively — see the RFC7292 test
// evidence in the reuse-check for this feature.
func berifyOuterSequence(t *testing.T, der []byte) []byte {
	t.Helper()
	n := int(der[1])
	lenBytes := 1
	if n >= 0x80 {
		lenBytes = 1 + (n & 0x7f)
	}
	content := der[1+lenBytes:]
	ber := []byte{der[0], 0x80}
	ber = append(ber, content...)
	ber = append(ber, 0x00, 0x00)
	return ber
}

func requireOpenSSL(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl não disponível neste ambiente — pulando teste do fallback BER")
	}
}

func TestParsePFX_BERFallback_Recovers(t *testing.T) {
	requireOpenSSL(t)
	der := generateTestPFX(t, "BER TEST", "senha123", time.Hour)
	ber := berifyOuterSequence(t, der)

	// Confirma que a fixture reproduz o sintoma real: go-pkcs12 puro rejeita
	// o BER com erro genérico (não relacionado a senha) antes do fallback.
	if _, err := decodeDER(ber, "senha123"); err != ErrPKCS12Parse {
		t.Fatalf("fixture BER deveria falhar em decodeDER com ErrPKCS12Parse, obtido %v", err)
	}

	p, err := parsePFX(t.Context(), ber, "senha123")
	if err != nil {
		t.Fatalf("parsePFX deveria recuperar via openssl, obtido erro: %v", err)
	}
	if p.Leaf.Subject.CommonName != "BER TEST" {
		t.Fatalf("CN esperado BER TEST, obtido %q", p.Leaf.Subject.CommonName)
	}
}

// generateLegacyRC2TestPFX mirrors generateTestPFX but encodes with
// pkcs12.LegacyRC2 instead of Modern2023 — real .pfx exports old enough to
// need the BER fallback (Windows certutil/certmgr, older tokens/HSMs) very
// often also encrypt the cert bag with RC2-40-CBC, which the fallback's
// openssl hop must explicitly opt into via "-legacy" on OpenSSL 3.0+.
func generateLegacyRC2TestPFX(t *testing.T, cn, password string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pfx, err := pkcs12.LegacyRC2.Encode(key, cert, nil, password)
	if err != nil {
		t.Fatalf("encode legacy RC2 pfx: %v", err)
	}
	return pfx
}

func TestParsePFX_BERFallback_LegacyRC2Cipher(t *testing.T) {
	requireOpenSSL(t)
	der := generateLegacyRC2TestPFX(t, "BER RC2 TEST", "senha123")
	ber := berifyOuterSequence(t, der)

	// Confirma que a fixture reproduz o sintoma real: go-pkcs12 puro rejeita
	// o BER antes mesmo de tentar decifrar o cert bag em RC2-40.
	if _, err := decodeDER(ber, "senha123"); err != ErrPKCS12Parse {
		t.Fatalf("fixture BER deveria falhar em decodeDER com ErrPKCS12Parse, obtido %v", err)
	}

	p, err := parsePFX(t.Context(), ber, "senha123")
	if err != nil {
		t.Fatalf("parsePFX deveria recuperar via openssl -legacy, obtido erro: %v", err)
	}
	if p.Leaf.Subject.CommonName != "BER RC2 TEST" {
		t.Fatalf("CN esperado BER RC2 TEST, obtido %q", p.Leaf.Subject.CommonName)
	}
}

func TestParsePFX_BERFallback_WrongPassword(t *testing.T) {
	requireOpenSSL(t)
	der := generateTestPFX(t, "BER WRONG PW", "certa", time.Hour)
	ber := berifyOuterSequence(t, der)

	_, err := parsePFX(t.Context(), ber, "errada")
	if err != ErrPKCS12BadPassword {
		t.Fatalf("esperado ErrPKCS12BadPassword, obtido %v", err)
	}
}

func TestParsePFX_BERFallback_OpenSSLMissing(t *testing.T) {
	der := generateTestPFX(t, "NO OPENSSL", "senha123", time.Hour)
	ber := berifyOuterSequence(t, der)

	t.Setenv("PATH", t.TempDir()) // PATH sem openssl

	_, err := parsePFX(t.Context(), ber, "senha123")
	if err != ErrPKCS12Parse {
		t.Fatalf("sem openssl no PATH, esperado degradar pra ErrPKCS12Parse, obtido %v", err)
	}
}
