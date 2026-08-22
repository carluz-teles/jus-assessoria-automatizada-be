package certificate

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"

	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/vault"
)

// signResult is the crypto output of a server-side signature: the raw signature
// bytes over the caller's SHA-256 digest, plus the certificate chain (leaf first,
// then any CA certs) in DER form so the caller can embed/verify it. Neither the
// private key nor the .pfx password appear here.
type signResult struct {
	Signature []byte
	ChainDER  [][]byte
}

// signDigest reverses the envelope, extracts the private key, and signs a caller-
// supplied SHA-256 digest with RSA PKCS#1 v1.5. Steps:
//  1. open() the envelope via the vault → the raw .pfx bytes (the read half the
//     crypto slice already owns; the plaintext DEK is zeroed inside open).
//  2. DecodeChain the .pfx with the session password → private key + leaf + CAs.
//     A wrong password is a typed KindInvalid (ErrInvalidPassword), never a 500.
//  3. sign the 32-byte digest with RSA-PKCS1v15/SHA-256 and return the signature
//     plus the DER chain (leaf first).
//
// SECURITY: the password is used ONLY here to decrypt the .pfx and is never stored
// or logged. The decoded .pfx bytes and the private key are zeroed / dropped before
// returning — the key never leaves this function. digest MUST be exactly 32 bytes
// (a SHA-256 sum); anything else is rejected as invalid so we never sign attacker-
// chosen structured data.
func signDigest(ctx context.Context, v vault.SecretVault, e envelope, password string, digest []byte) (signResult, error) {
	if len(digest) != len(([32]byte{})) {
		return signResult{}, ErrInvalidDigest
	}

	pfxData, err := open(ctx, v, e)
	if err != nil {
		return signResult{}, err
	}
	defer zero(pfxData)

	priv, leaf, caCerts, err := pkcs12.DecodeChain(pfxData, password)
	if err != nil {
		return signResult{}, classifyPKCS12Error(err)
	}
	if leaf == nil {
		return signResult{}, ErrMalformedPFX
	}

	signer, ok := priv.(crypto.Signer)
	if !ok {
		return signResult{}, apperr.NewInvalid("certificate private key cannot sign")
	}
	// ICP-Brasil A1 lawyer certs are RSA; guard so a non-RSA key does not silently
	// produce a signature the FE/tribunal cannot verify with the PKCS1v15 scheme.
	if _, isRSA := signer.Public().(*rsa.PublicKey); !isRSA {
		return signResult{}, apperr.NewInvalid("certificate key type is not supported for signing")
	}

	// SignPKCS1v15 with crypto.SHA256 signs the digest directly (the caller already
	// hashed the document); rand is used for blinding, not for the signature value.
	signature, err := signer.Sign(rand.Reader, digest, crypto.SHA256)
	if err != nil {
		return signResult{}, apperr.NewInfra("certificate: sign digest", err)
	}

	return signResult{
		Signature: signature,
		ChainDER:  chainDER(leaf, caCerts),
	}, nil
}

// chainDER returns the DER bytes of the certificate chain, leaf first then any CA
// certs, so the caller can build a full chain (e.g. for a future PAdES container).
func chainDER(leaf *x509.Certificate, caCerts []*x509.Certificate) [][]byte {
	chain := make([][]byte, 0, 1+len(caCerts))
	chain = append(chain, leaf.Raw)
	for _, ca := range caCerts {
		chain = append(chain, ca.Raw)
	}
	return chain
}
