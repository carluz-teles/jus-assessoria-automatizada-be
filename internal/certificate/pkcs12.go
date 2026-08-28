package certificate

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// parsedPFX is the in-memory representation of a decoded PKCS#12: private key,
// leaf cert, and chain (leaf primeiro na DER slice de retorno). Never stored;
// only lives during a single request.
type parsedPFX struct {
	Key   any // *rsa.PrivateKey (99% dos e-CPF) ou *ecdsa.PrivateKey
	Leaf  *x509.Certificate
	Chain []*x509.Certificate // leaf primeiro
}

// parsePFX decodes the .pfx bytes using the given password. Returns typed
// errors: BadPassword vs Parse — the FE distinguishes them (senha errada vs
// arquivo inválido).
func parsePFX(ctx context.Context, pfx []byte, password string) (*parsedPFX, error) {
	p, err := decodeDER(pfx, password)
	if err != ErrPKCS12Parse {
		return p, err
	}
	// go-pkcs12 only reads DER (its own doc: "only DER-encoded ...
	// encoding/asn1 only supports DER"). Some real .pfx files (Windows
	// certutil/certmgr exports, some token/HSM middlewares) use BER
	// (indefinite-length encoding) and land here with a generic error even
	// though the certificate itself is valid. Before giving up, normalize
	// via openssl — whose ASN.1 decoder reads BER natively — and retry.
	der, newPassword, nerr := normalizeBERToDER(ctx, pfx, password)
	if nerr != nil {
		return nil, nerr
	}
	return decodeDER(der, newPassword)
}

// decodeDER reads a strictly DER-encoded PKCS#12.
func decodeDER(pfx []byte, password string) (*parsedPFX, error) {
	// go-pkcs12 devolve (key, cert, cadeia). O primeiro cert é a folha; a
	// cadeia vem no 3º retorno (pode estar vazia se o PFX é minimal).
	key, cert, caCerts, err := pkcs12.DecodeChain(pfx, password)
	if err != nil {
		return nil, classifyPKCS12Error(err)
	}
	if cert == nil {
		return nil, ErrPKCS12Parse
	}
	chain := append([]*x509.Certificate{cert}, caCerts...)
	return &parsedPFX{Key: key, Leaf: cert, Chain: chain}, nil
}

// toMetadata extracts the domain metadata (public info) from the parsed PFX.
// The OAB parser reads the ICP-Brasil OtherName extension (OID 2.16.76.1.3.4)
// when present — a plain e-CPF sem OAB deixa OAB="".
func toMetadata(p *parsedPFX) CertMetadata {
	leaf := p.Leaf
	derChain := make([][]byte, 0, len(p.Chain))
	for _, c := range p.Chain {
		derChain = append(derChain, c.Raw)
	}
	sum := sha256.Sum256(leaf.Raw)
	return CertMetadata{
		SubjectCN:    leaf.Subject.CommonName,
		OAB:          extractOAB(leaf),
		Issuer:       leaf.Issuer.CommonName,
		Serial:       leaf.SerialNumber.String(),
		NotBefore:    leaf.NotBefore.UTC(),
		NotAfter:     leaf.NotAfter.UTC(),
		Fingerprint:  hex.EncodeToString(sum[:]),
		CertChainDER: derChain,
	}
}

// checkPFX computes the boolean checks the FE wizard shows.
func checkPFX(m CertMetadata) PreviewChecks {
	now := time.Now().UTC()
	return PreviewChecks{
		NaoExpirado:    now.After(m.NotBefore) && now.Before(m.NotAfter),
		CadeiaOk:       len(m.CertChainDER) >= 2,
		TitularConfere: strings.TrimSpace(m.SubjectCN) != "",
	}
}

// oidICPBrasilOAB é o OtherName ICP-Brasil que carrega a OAB do titular quando
// é um advogado (subject alt name customizado). Formato: sequência
// {numero_oab, uf}. Ver ITI/Serpro spec.
var oidICPBrasilOAB = asn1.ObjectIdentifier{2, 16, 76, 1, 3, 4}

// extractOAB tenta ler a OAB do OtherName ICP-Brasil na leaf. Retorna "" se
// não encontrar ou não conseguir decodificar (best-effort: nem todo cert
// e-CPF tem — só o e-CPF de advogado).
func extractOAB(cert *x509.Certificate) string {
	for _, ext := range cert.Extensions {
		// SubjectAltName é 2.5.29.17
		if !ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 17}) {
			continue
		}
		// parse do SAN em ASN.1 raw — procurar OtherName com OID da OAB
		var seq asn1.RawValue
		if _, err := asn1.Unmarshal(ext.Value, &seq); err != nil {
			continue
		}
		rest := seq.Bytes
		for len(rest) > 0 {
			var v asn1.RawValue
			var err error
			rest, err = asn1.Unmarshal(rest, &v)
			if err != nil {
				break
			}
			// OtherName tem tag 0 (context-specific)
			if v.Class != asn1.ClassContextSpecific || v.Tag != 0 {
				continue
			}
			// dentro do OtherName: OID + value
			var oid asn1.ObjectIdentifier
			inner, err := asn1.Unmarshal(v.Bytes, &oid)
			if err != nil {
				continue
			}
			if !oid.Equal(oidICPBrasilOAB) {
				continue
			}
			// value = tag [0] SET OF { numero (PrintableString), uf (PrintableString) }
			// Ou string simples "NNNNNN/UF" — best-effort só extrai printable ASCII.
			return decodePrintable(inner)
		}
	}
	return ""
}

// decodePrintable joga fora bytes de framing ASN.1 e devolve a primeira
// substring que pareça "NNNNN/UF". Fallback silencioso: "" quando o formato
// diverge (a OAB fica opcional na CertMetadata).
func decodePrintable(b []byte) string {
	// tira caracteres de controle e formata como "num/UF" quando o pattern bater
	var buf strings.Builder
	for _, c := range b {
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || c == '/' {
			buf.WriteByte(c)
		}
	}
	s := buf.String()
	// se veio "NNNNNNUF" sem barra, tenta separar (últimos 2 = UF)
	if len(s) >= 3 && !strings.Contains(s, "/") {
		return fmt.Sprintf("%s/%s", s[:len(s)-2], s[len(s)-2:])
	}
	return s
}

// signSHA256 assina um digest de 32 bytes usando a chave do PFX. Só RSA
// (99% dos e-CPF/e-CNPJ ICP-Brasil). ECDSA fica pra fatia 2 se aparecer.
func signSHA256(p *parsedPFX, digest []byte) ([]byte, error) {
	rsaKey, ok := p.Key.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrPKCS12Parse
	}
	// Assinatura RSA PKCS#1 v1.5 SHA-256 (compatível com PAdES básico e a
	// maioria dos verificadores brasileiros).
	sig, err := rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA256, digest)
	if err != nil {
		return nil, ErrPKCS12Parse
	}
	return sig, nil
}
