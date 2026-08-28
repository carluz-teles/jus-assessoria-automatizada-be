// Command gen-fake-cert generates a self-signed PKCS#12 (.pfx) file so the
// certificate wizard can be exercised in dev WITHOUT a real ICP-Brasil A1
// certificate. The cert is intentionally FAKE — no ICP-Brasil verifier will
// accept it — but it lets us prove that the parse → KMS → PAdES → upload
// pipeline works end-to-end.
//
// Usage:
//
//	go run ./cmd/gen-fake-cert                       # writes /tmp/fake-cert.pfx (password: senha123)
//	go run ./cmd/gen-fake-cert -out ~/mycert.pfx -password test -cn "MARIA SILVA" -oab 123456/SP
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// oidICPBrasilOAB é o OtherName ICP-Brasil que carrega OAB do titular
// (mesma OID que o slice certificate parseia). Formato do value: encoded
// string simples "NNNNN/UF" — o parser é permissivo (extractOAB best-effort).
var oidICPBrasilOAB = asn1.ObjectIdentifier{2, 16, 76, 1, 3, 4}

func main() {
	var (
		outPath  = flag.String("out", "/tmp/fake-cert.pfx", "caminho do .pfx gerado")
		password = flag.String("password", "senha123", "senha do PKCS#12")
		cn       = flag.String("cn", "TESTE FAKE DEV", "Common Name do titular")
		oab      = flag.String("oab", "", "OAB no formato NNNNN/UF (opcional; embutido no SubjectAltName ICP-Brasil)")
		days     = flag.Int("days", 365, "validade em dias a partir de agora")
	)
	flag.Parse()

	// 1) Chave RSA-2048 (mesmo tamanho de e-CPF típico).
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		die("gerar chave RSA", err)
	}

	// 2) Cert template — self-signed, sem CA (Chain fica só com o próprio).
	now := time.Now().UTC()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   *cn,
			Country:      []string{"BR"},
			Organization: []string{"jus-assessoria FAKE CA (dev only)"},
		},
		Issuer: pkix.Name{
			CommonName:   "AC FAKE jus-assessoria",
			Country:      []string{"BR"},
			Organization: []string{"jus-assessoria FAKE CA (dev only)"},
		},
		NotBefore:             now.Add(-1 * time.Hour), // 1h de folga pra clock skew
		NotAfter:              now.Add(time.Duration(*days) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	// 3) Se OAB foi passada, embute OtherName ICP-Brasil no SubjectAltName.
	// Estrutura: SAN é uma SEQUENCE de GeneralNames; OtherName tem tag [0]
	// contendo {OID, value}. O value aqui é uma PrintableString "NNNNN/UF"
	// (o parser do slice extractOAB() aceita esse shape best-effort).
	if *oab != "" {
		otherName, err := makeICPBrasilOAB(*oab)
		if err != nil {
			die("montar OtherName OAB", err)
		}
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, otherName)
	}

	// 4) Cria cert self-signed.
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		die("criar certificado", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		die("re-parsear certificado", err)
	}

	// 5) Empacota PKCS#12 (formato "Modern2023" — melhor tolerância cross-lib).
	pfxBytes, err := pkcs12.Modern2023.Encode(key, cert, nil, *password)
	if err != nil {
		die("encode PKCS#12", err)
	}

	// 6) Grava e imprime resumo.
	if err := os.WriteFile(*outPath, pfxBytes, 0o600); err != nil {
		die("gravar .pfx", err)
	}

	fmt.Println("Fake certificate generated:")
	fmt.Printf("  path      %s\n", *outPath)
	fmt.Printf("  password  %s\n", *password)
	fmt.Printf("  subject   CN=%s\n", *cn)
	if *oab != "" {
		fmt.Printf("  oab       %s\n", *oab)
	}
	fmt.Printf("  valid     %s → %s\n",
		tmpl.NotBefore.Format(time.RFC3339),
		tmpl.NotAfter.Format(time.RFC3339))
	fmt.Println()
	fmt.Println("Sobe via Configurações → Certificado no FE, ou:")
	fmt.Printf("  curl -F file=@%s -F password=%s -H \"Authorization: Bearer <TOKEN>\" \\\n", *outPath, *password)
	fmt.Println("       http://localhost:8080/v1/certificates")
}

// makeICPBrasilOAB monta uma extension SubjectAltName com um único OtherName
// carregando a OAB. Só o suficiente pra o extractOAB do slice detectar —
// não pretende ser conformante 100% com a spec ITI.
func makeICPBrasilOAB(oab string) (pkix.Extension, error) {
	// value: PrintableString com "NNNNN/UF" — extractOAB do slice aceita.
	valueBytes, err := asn1.Marshal(oab)
	if err != nil {
		return pkix.Extension{}, err
	}
	oidBytes, err := asn1.Marshal(oidICPBrasilOAB)
	if err != nil {
		return pkix.Extension{}, err
	}
	// OtherName ::= SEQUENCE { type-id OID, value [0] EXPLICIT ANY }, mas o
	// GeneralName choice (`otherName [0] OtherName`) usa tagging IMPLICIT —
	// o tag [0] SUBSTITUI o tag SEQUENCE do OtherName, não o envolve numa
	// camada extra. extractOAB() do slice (internal/certificate/pkcs12.go)
	// espera exatamente isso: {OID}{[0]{value}} sem SEQUENCE aninhada.
	valueWrapped, err := asn1.Marshal(asn1.RawValue{Tag: 0, Class: asn1.ClassContextSpecific, IsCompound: true, Bytes: valueBytes})
	if err != nil {
		return pkix.Extension{}, err
	}
	otherNameContent := append(oidBytes, valueWrapped...)
	// GeneralNames [0] wrapper (OtherName tag)
	san, err := asn1.Marshal([]asn1.RawValue{{Tag: 0, Class: asn1.ClassContextSpecific, IsCompound: true, Bytes: otherNameContent}})
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{
		Id:    asn1.ObjectIdentifier{2, 5, 29, 17}, // SubjectAltName
		Value: san,
	}, nil
}

func die(what string, err error) {
	fmt.Fprintf(os.Stderr, "gen-fake-cert: %s: %v\n", what, err)
	os.Exit(1)
}
