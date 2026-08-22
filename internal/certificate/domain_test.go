package certificate

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/vault"
)

// --- fakes ------------------------------------------------------------------

// fakeUOW runs fn with a nil tx and records the RLS scope each Do asked for.
type fakeUOW struct {
	scopes []string
	err    error
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, tenantID)
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, "system")
	return fn(nil)
}

// fakeOutbox captures published events for contract assertions.
type fakeOutbox struct {
	published []events.Event
	err       error
}

func (o *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	o.published = append(o.published, ev)
	return o.err
}

// fakeRepo records what it was asked to persist and returns canned values. It
// captures the InsertParams so a test can assert the envelope + metadata threaded
// through — and, crucially, that the ciphertext is NOT the plaintext .pfx.
type fakeRepo struct {
	inserted   *InsertParams
	insertErr  error
	listViews  []CertificateView
	listErr    error
	revokeErr  error
	revokedID  string
	revokedTID string

	// signing path
	loadRes      signable
	loadErr      error
	recordErr    error
	recordedDig  []byte
	recordedCID  string
	recordedTID  string
	recordedUser string
}

func (r *fakeRepo) Insert(_ context.Context, _ database.Tx, p InsertParams) (*Certificate, error) {
	if r.insertErr != nil {
		return nil, r.insertErr
	}
	r.inserted = &p
	return &Certificate{
		ID:          "cert-1",
		TenantID:    p.TenantID,
		OwnerUserID: p.OwnerUserID,
		SubjectCN:   p.Meta.SubjectCN,
		OAB:         p.Meta.OAB,
		Issuer:      p.Meta.Issuer,
		Serial:      p.Meta.Serial,
		NotBefore:   p.Meta.NotBefore,
		NotAfter:    p.Meta.NotAfter,
		Fingerprint: p.Meta.Fingerprint,
		CreatedAt:   time.Now(),
	}, nil
}

func (r *fakeRepo) List(_ context.Context, _ database.Tx, _ string) ([]CertificateView, error) {
	return r.listViews, r.listErr
}

func (r *fakeRepo) LoadEnvelope(_ context.Context, _ database.Tx, _, _ string) (signable, error) {
	return r.loadRes, r.loadErr
}

func (r *fakeRepo) RecordSigning(_ context.Context, _ database.Tx, tenantID, certificateID, signerUserID string, digest []byte) error {
	r.recordedTID, r.recordedCID, r.recordedUser = tenantID, certificateID, signerUserID
	r.recordedDig = digest
	return r.recordErr
}

func (r *fakeRepo) Revoke(_ context.Context, _ database.Tx, tenantID, id string, _ time.Time) error {
	r.revokedTID, r.revokedID = tenantID, id
	return r.revokeErr
}

// --- Create -----------------------------------------------------------------

func TestCreate_StoresEncryptedEnvelope_NeverPlaintextKey(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "ADV TESTE:1", "pw", now.Add(-time.Hour), now.Add(24*time.Hour))

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	v := testVault(t)
	uc := NewUseCase(repo, v, outbox, uow, WithClock(func() time.Time { return now }))

	view, err := uc.Create(context.Background(), CreateCommand{
		TenantID:    uuid.NewString(),
		OwnerUserID: uuid.NewString(),
		PFXData:     pfx,
		Password:    "pw",
	})
	require.NoError(t, err)

	// The persisted ciphertext must NOT be the raw .pfx, and the envelope must be
	// fully populated (no plaintext key stored).
	require.NotNil(t, repo.inserted)
	env := repo.inserted.Envelope
	is.NotEmpty(env.Ciphertext)
	is.NotEmpty(env.Nonce)
	is.NotEmpty(env.WrappedDEK)
	is.NotEqual(pfx, env.Ciphertext, "ciphertext must not equal the plaintext .pfx")
	is.Equal("local", env.KEKRef)

	// The view carries metadata only (no envelope fields exist on it).
	is.Equal("ADV TESTE:1", view.SubjectCN)
	is.Len(view.Fingerprint, 64)

	// The write ran in the tenant scope and published certificate.added atomically.
	is.Equal([]string{repo.inserted.TenantID}, uow.scopes)
	require.Len(t, outbox.published, 1)
	is.Equal(typeCertificateAdded, outbox.published[0].Type())
}

func TestCreate_WrongPassword_NoWriteNoEvent(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "CN", "right", now.Add(-time.Hour), now.Add(24*time.Hour))

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, testVault(t), outbox, uow, WithClock(func() time.Time { return now }))

	_, err := uc.Create(context.Background(), CreateCommand{
		TenantID: "t", OwnerUserID: "u", PFXData: pfx, Password: "wrong",
	})
	is.ErrorIs(err, ErrInvalidPassword)
	is.Nil(repo.inserted, "no row on a wrong password")
	is.Empty(outbox.published, "no event on a wrong password")
	is.Empty(uow.scopes, "no tx opened on a parse failure")
}

func TestCreate_Expired_Rejected(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "CN", "pw", now.Add(-48*time.Hour), now.Add(-time.Hour))

	uc := NewUseCase(&fakeRepo{}, testVault(t), &fakeOutbox{}, &fakeUOW{}, WithClock(func() time.Time { return now }))
	_, err := uc.Create(context.Background(), CreateCommand{TenantID: "t", OwnerUserID: "u", PFXData: pfx, Password: "pw"})
	is.ErrorIs(err, ErrCertificateExpired)
}

// --- Preview ----------------------------------------------------------------

func TestPreview_ReportsMetadataAndChecks_NoWrite(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "MARIA:1", "pw", now.Add(-time.Hour), now.Add(24*time.Hour))

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, testVault(t), outbox, uow, WithClock(func() time.Time { return now }))

	res, err := uc.Preview(context.Background(), PreviewCommand{PFXData: pfx, Password: "pw"})
	require.NoError(t, err)

	is.Equal("MARIA:1", res.SubjectCN)
	is.Equal("12345/SP", res.OAB)
	is.Len(res.Fingerprint, 64)
	is.True(res.Checks.NaoExpirado)
	is.True(res.Checks.TitularConfere)
	is.False(res.Checks.CadeiaOk, "self-signed fixture ships no CA chain")

	// Read-only: nothing persisted, no tx, no event.
	is.Nil(repo.inserted)
	is.Empty(uow.scopes)
	is.Empty(outbox.published)
}

func TestPreview_Expired_StillSucceeds_ReportsNaoExpiradoFalse(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "CN", "pw", now.Add(-48*time.Hour), now.Add(-time.Hour))

	uc := NewUseCase(&fakeRepo{}, testVault(t), &fakeOutbox{}, &fakeUOW{}, WithClock(func() time.Time { return now }))
	res, err := uc.Preview(context.Background(), PreviewCommand{PFXData: pfx, Password: "pw"})
	require.NoError(t, err, "preview reports rather than rejecting an expired cert")
	is.False(res.Checks.NaoExpirado)
}

func TestPreview_WrongPassword_TypedError(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "CN", "right", now.Add(-time.Hour), now.Add(24*time.Hour))

	uc := NewUseCase(&fakeRepo{}, testVault(t), &fakeOutbox{}, &fakeUOW{})
	_, err := uc.Preview(context.Background(), PreviewCommand{PFXData: pfx, Password: "wrong"})
	is.ErrorIs(err, ErrInvalidPassword)
}

// --- List / Revoke ----------------------------------------------------------

func TestList_ScopedToTenant_MetadataOnly(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{listViews: []CertificateView{{ID: "c1", SubjectCN: "A"}}}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, testVault(t), &fakeOutbox{}, uow)

	views, err := uc.List(context.Background(), "tenant-x")
	require.NoError(t, err)
	is.Len(views, 1)
	is.Equal([]string{"tenant-x"}, uow.scopes)
}

func TestRevoke_StampsAndPublishesInTx(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, testVault(t), outbox, uow)

	err := uc.Revoke(context.Background(), "tenant-x", "cert-9")
	require.NoError(t, err)
	is.Equal("tenant-x", repo.revokedTID)
	is.Equal("cert-9", repo.revokedID)
	is.Equal([]string{"tenant-x"}, uow.scopes)
	require.Len(t, outbox.published, 1)
	is.Equal(typeCertRevoked, outbox.published[0].Type())
}

func TestRevoke_NotFound_NoEvent(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{revokeErr: ErrCertificateNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, testVault(t), outbox, &fakeUOW{})

	err := uc.Revoke(context.Background(), "t", "missing")
	is.ErrorIs(err, ErrCertificateNotFound)
	is.Empty(outbox.published, "no event when the revoke found nothing")
}

// --- Sign -------------------------------------------------------------------

// sealedSignable builds a real envelope from a test .pfx (via the real vault +
// seal) plus the leaf's public key, so a sign test can round-trip: sign a digest
// through the use case, then verify the signature against this public key. This is
// the acceptance-criterion fixture — it proves the stored, encrypted key really
// signs and the result verifies.
func sealedSignable(t *testing.T, v vault.SecretVault, cn, password string, notBefore, notAfter time.Time) (signable, *rsa.PublicKey) {
	t.Helper()

	pfx := makePFX(t, cn, password, notBefore, notAfter)
	env, err := seal(context.Background(), v, pfx)
	require.NoError(t, err)

	// Recover the leaf's public key to verify the signature later (the private key
	// itself is never exposed by the slice; the test grabs the leaf directly from
	// the fixture .pfx via the pkcs12 package).
	_, leaf, _, err := pkcs12.DecodeChain(pfx, password)
	require.NoError(t, err)
	pub, ok := leaf.PublicKey.(*rsa.PublicKey)
	require.True(t, ok, "test fixture must be RSA")

	return signable{NotAfter: notAfter, Envelope: env}, pub
}

func TestSign_RoundTrip_SignatureVerifies(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	v := testVault(t)
	sgn, pub := sealedSignable(t, v, "ADV SIGNER:1", "pw", now.Add(-time.Hour), now.Add(24*time.Hour))

	repo := &fakeRepo{loadRes: sgn}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, v, &fakeOutbox{}, uow, WithClock(func() time.Time { return now }))

	sum := sha256.Sum256([]byte("a document to sign"))
	res, err := uc.Sign(context.Background(), SignCommand{
		TenantID:      "tenant-x",
		SignerUserID:  "user-1",
		CertificateID: "cert-1",
		Password:      "pw",
		Digest:        sum[:],
	})
	require.NoError(t, err)

	// The signature must verify against the certificate's public key with the same
	// scheme (RSA-PKCS1v15/SHA-256) — this is the core acceptance criterion.
	err = rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], res.Signature)
	require.NoError(t, err, "server-side signature must verify with the cert public key")

	// The chain is returned leaf-first and the audit row was written in the tenant tx.
	require.NotEmpty(t, res.ChainDER)
	is.Equal([]string{"tenant-x"}, uow.scopes)
	is.Equal(sum[:], repo.recordedDig, "the audited digest is exactly what was signed")
	is.Equal("cert-1", repo.recordedCID)
	is.Equal("tenant-x", repo.recordedTID)
	is.Equal("user-1", repo.recordedUser)
}

func TestSign_WrongPassword_TypedError_NoAudit(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	v := testVault(t)
	sgn, _ := sealedSignable(t, v, "CN", "right", now.Add(-time.Hour), now.Add(24*time.Hour))

	repo := &fakeRepo{loadRes: sgn}
	uc := NewUseCase(repo, v, &fakeOutbox{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	sum := sha256.Sum256([]byte("x"))
	_, err := uc.Sign(context.Background(), SignCommand{
		TenantID: "t", SignerUserID: "u", CertificateID: "c", Password: "wrong", Digest: sum[:],
	})
	is.ErrorIs(err, ErrInvalidPassword)
	is.Nil(repo.recordedDig, "no audit row on a wrong password")
}

func TestSign_Revoked_Rejected_BeforeDecrypt(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	v := testVault(t)
	sgn, _ := sealedSignable(t, v, "CN", "pw", now.Add(-time.Hour), now.Add(24*time.Hour))
	revoked := now.Add(-time.Minute)
	sgn.RevokedAt = &revoked

	repo := &fakeRepo{loadRes: sgn}
	uc := NewUseCase(repo, v, &fakeOutbox{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	sum := sha256.Sum256([]byte("x"))
	_, err := uc.Sign(context.Background(), SignCommand{
		TenantID: "t", SignerUserID: "u", CertificateID: "c", Password: "pw", Digest: sum[:],
	})
	is.ErrorIs(err, ErrCertificateRevoked)
	is.Nil(repo.recordedDig)
}

func TestSign_Expired_Rejected(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	v := testVault(t)
	// Sealed with a validity window already in the past.
	sgn, _ := sealedSignable(t, v, "CN", "pw", now.Add(-48*time.Hour), now.Add(-time.Hour))

	uc := NewUseCase(&fakeRepo{loadRes: sgn}, v, &fakeOutbox{}, &fakeUOW{}, WithClock(func() time.Time { return now }))
	sum := sha256.Sum256([]byte("x"))
	_, err := uc.Sign(context.Background(), SignCommand{
		TenantID: "t", SignerUserID: "u", CertificateID: "c", Password: "pw", Digest: sum[:],
	})
	is.ErrorIs(err, ErrCertificateExpired)
}

func TestSign_NotFound_Propagates(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{loadErr: ErrCertificateNotFound}
	uc := NewUseCase(repo, testVault(t), &fakeOutbox{}, &fakeUOW{})

	sum := sha256.Sum256([]byte("x"))
	_, err := uc.Sign(context.Background(), SignCommand{
		TenantID: "t", SignerUserID: "u", CertificateID: "missing", Password: "pw", Digest: sum[:],
	})
	is.ErrorIs(err, ErrCertificateNotFound)
}

func TestSign_BadDigestLength_Rejected(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	uc := NewUseCase(&fakeRepo{}, testVault(t), &fakeOutbox{}, &fakeUOW{})
	_, err := uc.Sign(context.Background(), SignCommand{
		TenantID: "t", SignerUserID: "u", CertificateID: "c", Password: "pw", Digest: []byte("too short"),
	})
	is.ErrorIs(err, ErrInvalidDigest)
}

// Guard: a repo insert error propagates and is not swallowed.
func TestCreate_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	now := time.Now()
	pfx := makePFX(t, "CN", "pw", now.Add(-time.Hour), now.Add(24*time.Hour))
	repo := &fakeRepo{insertErr: errors.New("db down")}
	uc := NewUseCase(repo, testVault(t), &fakeOutbox{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	_, err := uc.Create(context.Background(), CreateCommand{TenantID: "t", OwnerUserID: "u", PFXData: pfx, Password: "pw"})
	is.Error(err)
}
