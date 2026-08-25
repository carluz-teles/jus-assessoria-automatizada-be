//go:build integration

// certificate integration tests — prove the outbox events (certificate.added,
// certificate.revoked) and the signing_event audit trail actually land in a REAL
// Postgres, ported onto the KMS envelope-encryption design in this merge-fallout
// fix. The unit tests (fakeRepo/fakeUOW) cannot prove: real FK/RLS constraints
// hold, the outbox row commits atomically with the certificate row, and
// InsertSigningEvent round-trips through the real sqlc query.
//
// The KMS Cipher is faked here (no network call to GCP) — this suite proves the
// SQL/outbox layer, not the KMS integration itself.
package integration_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/jusassessoria/platform/internal/certificate"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// firstAppUserID returns the id of one app_user seeded for tenantID (RLS bypassed).
func firstAppUserID(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM app_user WHERE tenant_id = $1 LIMIT 1`, tenantID).Scan(&id); err != nil {
		t.Fatalf("firstAppUserID(%s): %v", tenantID, err)
	}
	return id
}

// generateTestPFXForIntegration builds a synthetic self-signed .pfx in memory —
// enough for the certificate slice's parse → envelope-encrypt → sign round-trip.
func generateTestPFXForIntegration(t *testing.T, cn, password string, ttl time.Duration) []byte {
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pfx, err := pkcs12.Modern2023.Encode(key, cert, nil, password)
	if err != nil {
		t.Fatalf("encode pfx: %v", err)
	}
	return pfx
}

// fakeCertCipher is an in-memory certificate.Cipher — real AES-GCM locally, DEK
// "wrapped" via XOR against a random master key. No network call, unlike the
// GCP KMS-backed EnvelopeCipher used in production.
type fakeCertCipher struct{ master []byte }

func newFakeCertCipher(t *testing.T) *fakeCertCipher {
	t.Helper()
	m := make([]byte, 32)
	if _, err := rand.Read(m); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return &fakeCertCipher{master: m}
}

func (c *fakeCertCipher) Seal(_ context.Context, plaintext []byte) (*certificate.Envelope, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(dek)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	wrapped := xorCert(dek, c.master)
	return &certificate.Envelope{Ciphertext: ct, Nonce: nonce, WrappedDEK: wrapped, KEKRef: "fake"}, nil
}

func (c *fakeCertCipher) Open(_ context.Context, env *certificate.Envelope) ([]byte, error) {
	dek := xorCert(env.WrappedDEK, c.master)
	block, _ := aes.NewCipher(dek)
	gcm, _ := cipher.NewGCM(block)
	return gcm.Open(nil, env.Nonce, env.Ciphertext, nil)
}

func (c *fakeCertCipher) Close() error { return nil }

func xorCert(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i%len(b)]
	}
	return out
}

// newCertificateUseCase wires a real Repository + real UnitOfWork + real Outbox
// against the shared test Postgres, with a fake Cipher (no GCP KMS network call).
func newCertificateUseCase(t *testing.T, pool *pgxpool.Pool) *certificate.UseCase {
	t.Helper()
	return certificate.NewUseCase(
		certificate.NewRepository(),
		database.NewUnitOfWork(pool),
		newFakeCertCipher(t),
		events.NewOutbox(),
	)
}

func TestCertificate_Upload_PublishesAddedInSameTx(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-cert-upload", 1)
	ownerID := firstAppUserID(t, pool, tenantID)

	uc := newCertificateUseCase(t, pool)
	pfx := generateTestPFXForIntegration(t, "ADV TESTE", "senha123", time.Hour)

	cert, err := uc.Upload(context.Background(), tenantID, ownerID, pfx, "senha123")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if cert.ID == "" {
		t.Fatal("Upload: empty certificate id")
	}

	// The ciphertext must NOT be the raw .pfx — envelope encryption happened.
	if string(cert.Envelope.Ciphertext) == string(pfx) {
		t.Fatal("stored ciphertext equals plaintext .pfx")
	}

	if n := countOutboxRows(t, pool, "certificate.added", cert.ID); n != 1 {
		t.Errorf("outbox rows for certificate.added = %d, want 1", n)
	}

	// The row is really there, tenant-scoped.
	var subjectCN string
	if err := pool.QueryRow(context.Background(),
		`SELECT subject_cn FROM certificate WHERE id = $1 AND tenant_id = $2`,
		cert.ID, tenantID).Scan(&subjectCN); err != nil {
		t.Fatalf("querying inserted certificate: %v", err)
	}
	if subjectCN != "ADV TESTE" {
		t.Errorf("subject_cn = %q, want %q", subjectCN, "ADV TESTE")
	}
}

func TestCertificate_Sign_RecordsAuditRow(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-cert-sign", 1)
	ownerID := firstAppUserID(t, pool, tenantID)

	uc := newCertificateUseCase(t, pool)
	pfx := generateTestPFXForIntegration(t, "ADV SIGNER", "senha123", time.Hour)

	cert, err := uc.Upload(context.Background(), tenantID, ownerID, pfx, "senha123")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	sum := sha256.Sum256([]byte("peça de teste — integration"))
	res, err := uc.Sign(context.Background(), tenantID, cert.ID, ownerID, "senha123", sum[:])
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(res.Signature) == 0 {
		t.Fatal("Sign: empty signature")
	}
	if len(res.Chain) == 0 {
		t.Fatal("Sign: empty chain")
	}

	// The audit row (signing_event) must exist with the exact digest signed.
	var (
		signerUserID string
		digest       []byte
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT signer_user_id, digest_sha256 FROM signing_event
		 WHERE tenant_id = $1 AND certificate_id = $2`,
		tenantID, cert.ID).Scan(&signerUserID, &digest); err != nil {
		t.Fatalf("querying signing_event: %v", err)
	}
	if signerUserID != ownerID {
		t.Errorf("signing_event.signer_user_id = %q, want %q", signerUserID, ownerID)
	}
	if string(digest) != string(sum[:]) {
		t.Error("signing_event.digest_sha256 does not match the signed digest")
	}
}

func TestCertificate_Revoke_PublishesRevokedAndStampsRow(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-cert-revoke", 1)
	ownerID := firstAppUserID(t, pool, tenantID)

	uc := newCertificateUseCase(t, pool)
	pfx := generateTestPFXForIntegration(t, "ADV REVOKE", "senha123", time.Hour)

	cert, err := uc.Upload(context.Background(), tenantID, ownerID, pfx, "senha123")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if err := uc.Revoke(context.Background(), tenantID, cert.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if n := countOutboxRows(t, pool, "certificate.revoked", cert.ID); n != 1 {
		t.Errorf("outbox rows for certificate.revoked = %d, want 1", n)
	}

	var revokedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT revoked_at FROM certificate WHERE id = $1 AND tenant_id = $2`,
		cert.ID, tenantID).Scan(&revokedAt); err != nil {
		t.Fatalf("querying revoked certificate: %v", err)
	}
	if revokedAt == nil {
		t.Error("revoked_at is NULL after Revoke")
	}

	// Idempotency: revoking again must return ErrCertificateNotFound (already
	// revoked) and must NOT publish a second event.
	err = uc.Revoke(context.Background(), tenantID, cert.ID)
	if err != certificate.ErrCertificateNotFound {
		t.Errorf("second Revoke error = %v, want ErrCertificateNotFound", err)
	}
	if n := countOutboxRows(t, pool, "certificate.revoked", cert.ID); n != 1 {
		t.Errorf("outbox rows for certificate.revoked after double revoke = %d, want 1 (no duplicate)", n)
	}
}
