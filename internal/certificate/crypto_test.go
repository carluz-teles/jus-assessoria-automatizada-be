package certificate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/vault"
)

// makePFX builds a self-signed cert + key encoded as a password-protected .pfx,
// for tests. cn is the subject common name; validity is [notBefore, notAfter].
func makePFX(t *testing.T, cn, password string, notBefore, notAfter time.Time) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1234567890),
		Subject: pkix.Name{
			CommonName:         cn,
			OrganizationalUnit: []string{"OAB 12345/SP"},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,
		KeyUsage:  x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	pfx, err := pkcs12.Modern.Encode(key, cert, nil, password)
	require.NoError(t, err)
	return pfx
}

func testVault(t *testing.T) vault.SecretVault {
	t.Helper()
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	v, err := vault.NewLocalVault(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	return v
}

func TestParsePFX_ValidCertificate(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "MARIA ADVOGADA:12345678900", "s3cr3t", now.Add(-time.Hour), now.Add(365*24*time.Hour))

	meta, err := parsePFX(pfx, "s3cr3t", now)
	require.NoError(t, err)

	is.Equal("MARIA ADVOGADA:12345678900", meta.SubjectCN)
	is.Equal("12345/SP", meta.OAB)
	// Self-signed fixture: CreateCertificate copies the issuer from the parent's
	// subject (itself), so Issuer CN == Subject CN here. A real CA-signed A1 carries
	// the AC's CN; the extraction path (leaf.Issuer.CommonName) is identical.
	is.Equal("MARIA ADVOGADA:12345678900", meta.Issuer)
	is.Equal("1234567890", meta.Serial)
	is.NotEmpty(meta.Fingerprint)
	is.Len(meta.Fingerprint, 64) // hex SHA-256
}

func TestParsePFX_WrongPassword(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "CN", "right", now.Add(-time.Hour), now.Add(24*time.Hour))

	_, err := parsePFX(pfx, "wrong", now)
	is.ErrorIs(err, ErrInvalidPassword)
}

func TestParsePFX_Expired(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "CN", "pw", now.Add(-48*time.Hour), now.Add(-24*time.Hour))

	_, err := parsePFX(pfx, "pw", now)
	is.ErrorIs(err, ErrCertificateExpired)
}

func TestParsePFX_Malformed(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	_, err := parsePFX([]byte("not a pfx at all"), "pw", time.Now())
	// A non-PKCS12 blob decodes as malformed (or a decryption error → invalid
	// password); either way it is a typed KindInvalid, never a panic or 500.
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInvalid, ae.Kind)
}

func TestSealOpen_RoundTrip(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v := testVault(t)
	plaintext := bytes.Repeat([]byte{0x42}, 4096) // stand-in for the .pfx bytes

	env, err := seal(context.Background(), v, plaintext)
	require.NoError(t, err)
	is.False(bytes.Equal(plaintext, env.Ciphertext), "ciphertext must differ from plaintext")
	is.NotEmpty(env.Nonce)
	is.NotEmpty(env.WrappedDEK)
	is.Equal(vault.KekRefLocal, env.KEKRef)

	got, err := open(context.Background(), v, env)
	require.NoError(t, err)
	is.True(bytes.Equal(plaintext, got), "round-trip must recover the original blob")
}

func TestSeal_FreshDEKAndNoncePerCall(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v := testVault(t)
	plaintext := []byte("same blob")

	a, err := seal(context.Background(), v, plaintext)
	require.NoError(t, err)
	b, err := seal(context.Background(), v, plaintext)
	require.NoError(t, err)

	is.False(bytes.Equal(a.Nonce, b.Nonce), "nonce must be fresh per call")
	is.False(bytes.Equal(a.Ciphertext, b.Ciphertext), "ciphertext must differ per call")
}

func TestOpen_TamperedCiphertext_Fails(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v := testVault(t)
	env, err := seal(context.Background(), v, []byte("secret pfx"))
	require.NoError(t, err)

	env.Ciphertext[0] ^= 0xFF // flip a bit
	_, err = open(context.Background(), v, env)
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInfra, ae.Kind)
}
