package certificate

import (
	"os/exec"
	"testing"
	"time"
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
