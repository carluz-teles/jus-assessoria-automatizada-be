package certificate

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// parsedCertificate is the metadata extracted from a decoded .pfx — everything
// the read model needs, and nothing secret. The private key is deliberately NOT
// carried here: it is decoded during parse to validate the password, then dropped
// on the floor. Only the metadata survives this struct.
type parsedCertificate struct {
	SubjectCN   string
	OAB         string
	Issuer      string
	Serial      string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string
}

// oabRegex matches a Brazilian OAB registration embedded in a certificate's
// subject/SAN (ICP-Brasil A1 lawyer certs carry it). It is best-effort: an
// absent OAB is not an error (empty string), only a display hint.
var oabRegex = regexp.MustCompile(`(?i)OAB[^0-9A-Z]*([0-9]{2,6})[/\s-]*([A-Z]{2})`)

// decodePFX decodes a DER-encoded PKCS#12 blob with the given password and returns
// the leaf certificate's metadata plus whether a CA chain was present. It validates
// ONLY the password (a wrong one is ErrInvalidPassword) and the file format
// (ErrMalformedPFX). It does NOT gate on the validity window — that is the caller's
// choice: Create rejects an expired cert (parsePFX), preview REPORTS it (nao_expirado).
//
// SECURITY: the private key returned by DecodeChain is intentionally discarded —
// it is only decoded to prove the password is correct; it is never returned,
// stored, or logged. The password argument is never logged either.
func decodePFX(pfxData []byte, password string) (meta parsedCertificate, hasChain bool, err error) {
	// DecodeChain returns (privateKey, leaf, caCerts, err). We keep only the leaf
	// cert's metadata + whether a CA chain came along; the private key is dropped
	// (never bound to a variable that leaves this function).
	_, leaf, caCerts, err := pkcs12.DecodeChain(pfxData, password)
	if err != nil {
		return parsedCertificate{}, false, classifyPKCS12Error(err)
	}
	if leaf == nil {
		return parsedCertificate{}, false, ErrMalformedPFX
	}

	sum := sha256.Sum256(leaf.Raw)

	return parsedCertificate{
		SubjectCN:   leaf.Subject.CommonName,
		OAB:         extractOAB(leaf),
		Issuer:      leaf.Issuer.CommonName,
		Serial:      leaf.SerialNumber.String(),
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Fingerprint: hex.EncodeToString(sum[:]),
	}, len(caCerts) > 0, nil
}

// parsePFX is the STORE path's parse: decodePFX plus the validity-window gate. An
// expired (or not-yet-valid) A1 cannot sign anything, so storing it is refused
// (ErrCertificateExpired). Preview does not call this — it wants to report, not gate.
func parsePFX(pfxData []byte, password string, now time.Time) (parsedCertificate, error) {
	meta, _, err := decodePFX(pfxData, password)
	if err != nil {
		return parsedCertificate{}, err
	}
	if now.After(meta.NotAfter) || now.Before(meta.NotBefore) {
		return parsedCertificate{}, ErrCertificateExpired
	}
	return meta, nil
}

// classifyPKCS12Error maps the pkcs12 package's sentinel errors onto the slice's
// typed errors. A wrong password (ErrIncorrectPassword) and a decryption failure
// (ErrDecryption, the usual symptom of a wrong password) both surface as
// ErrInvalidPassword; anything else is a malformed file. The raw error is never
// exposed to the client.
func classifyPKCS12Error(err error) error {
	if errors.Is(err, pkcs12.ErrIncorrectPassword) || errors.Is(err, pkcs12.ErrDecryption) {
		return ErrInvalidPassword
	}
	return ErrMalformedPFX
}

// extractOAB best-effort scans the certificate's common name and SANs for a
// Brazilian OAB number, returning a normalized "NNNNN/UF" or "" when none is
// found. An absent OAB is never an error.
func extractOAB(cert *x509.Certificate) string {
	haystack := cert.Subject.CommonName
	if len(cert.Subject.OrganizationalUnit) > 0 {
		haystack += " " + strings.Join(cert.Subject.OrganizationalUnit, " ")
	}
	haystack += " " + strings.Join(cert.DNSNames, " ")

	if m := oabRegex.FindStringSubmatch(haystack); m != nil {
		return fmt.Sprintf("%s/%s", m[1], strings.ToUpper(m[2]))
	}
	return ""
}
