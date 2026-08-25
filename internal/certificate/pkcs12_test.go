package certificate

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// helper — gera um .pfx sintético em memória (par RSA + certificado self-signed
// + senha). Chega bem próximo do que o parser vê num e-CPF, mas SEM a extensão
// OtherName ICP-Brasil (por isso o teste de OAB verifica string vazia).
func generateTestPFX(t *testing.T, cn, password string, ttl time.Duration) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		Issuer:       pkix.Name{CommonName: "AC Teste"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ttl),
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pfx, err := pkcs12.Modern2023.Encode(key, cert, nil, password)
	if err != nil {
		t.Fatalf("encode pfx: %v", err)
	}
	return pfx
}

func TestParsePFX_Success(t *testing.T) {
	pfx := generateTestPFX(t, "LUAN GOMES", "senha123", time.Hour)
	p, err := parsePFX(pfx, "senha123")
	if err != nil {
		t.Fatalf("parsePFX: %v", err)
	}
	if p.Leaf.Subject.CommonName != "LUAN GOMES" {
		t.Fatalf("CN esperado LUAN GOMES, obtido %q", p.Leaf.Subject.CommonName)
	}
}

func TestParsePFX_WrongPassword(t *testing.T) {
	pfx := generateTestPFX(t, "X", "certa", time.Hour)
	_, err := parsePFX(pfx, "errada")
	if err != ErrPKCS12BadPassword {
		t.Fatalf("esperado ErrPKCS12BadPassword, obtido %v", err)
	}
}

func TestParsePFX_CorruptFile(t *testing.T) {
	_, err := parsePFX([]byte("isso não é pfx nenhum"), "qualquer")
	if err != ErrPKCS12Parse && err != ErrPKCS12BadPassword {
		t.Fatalf("esperado ErrPKCS12Parse ou BadPassword, obtido %v", err)
	}
}

func TestToMetadataAndChecks(t *testing.T) {
	pfx := generateTestPFX(t, "MARIA SILVA", "senha", 24*time.Hour)
	p, _ := parsePFX(pfx, "senha")
	m := toMetadata(p)
	if m.SubjectCN != "MARIA SILVA" {
		t.Fatalf("SubjectCN = %q", m.SubjectCN)
	}
	if m.Fingerprint == "" || len(m.Fingerprint) != 64 {
		t.Fatalf("fingerprint sha256 hex esperado 64 chars, got %d: %q", len(m.Fingerprint), m.Fingerprint)
	}
	checks := checkPFX(m)
	if !checks.NaoExpirado {
		t.Fatal("cert novo deveria estar não-expirado")
	}
	if !checks.TitularConfere {
		t.Fatal("CN preenchido, titular_confere deveria ser true")
	}
	// cadeia_ok = false aqui (só a leaf; sem CA). O teste evidencia esse edge.
	if checks.CadeiaOk {
		t.Fatal("com só a leaf, cadeia_ok deveria ser false")
	}
}

func TestSignSHA256_RoundTrip(t *testing.T) {
	pfx := generateTestPFX(t, "TEST", "s", time.Hour)
	p, _ := parsePFX(pfx, "s")
	digest := sha256.Sum256([]byte("documento fake"))
	sig, err := signSHA256(p, digest[:])
	if err != nil {
		t.Fatalf("signSHA256: %v", err)
	}
	if len(sig) == 0 {
		t.Fatal("signature vazia")
	}
	// tamanho de assinatura RSA-2048 = 256 bytes
	if len(sig) != 256 {
		t.Fatalf("esperado 256 bytes (RSA-2048), got %d", len(sig))
	}
}
