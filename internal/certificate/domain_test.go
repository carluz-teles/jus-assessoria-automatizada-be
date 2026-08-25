package certificate

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
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

// fakeRepo records what it was asked to persist and returns canned values.
type fakeRepo struct {
	inserted  *Certificate
	insertErr error

	listViews []CertificateWithOwner
	listErr   error

	revokeErr  error
	revokedID  string
	revokedTID string

	getRes *Certificate
	getErr error

	recordErr    error
	recordedDig  []byte
	recordedCID  string
	recordedTID  string
	recordedUser string
}

func (r *fakeRepo) Insert(_ context.Context, _ database.Tx, c *Certificate) (string, time.Time, error) {
	if r.insertErr != nil {
		return "", time.Time{}, r.insertErr
	}
	r.inserted = c
	return "cert-1", time.Now(), nil
}

func (r *fakeRepo) GetByID(_ context.Context, _ database.Tx, _, _ string) (*Certificate, error) {
	return r.getRes, r.getErr
}

func (r *fakeRepo) ListActive(_ context.Context, _ database.Tx, _ string) ([]CertificateWithOwner, error) {
	return r.listViews, r.listErr
}

func (r *fakeRepo) Revoke(_ context.Context, _ database.Tx, tenantID, id string) error {
	r.revokedTID, r.revokedID = tenantID, id
	return r.revokeErr
}

func (r *fakeRepo) RecordSigning(_ context.Context, _ database.Tx, tenantID, certificateID, signerUserID string, digest []byte) error {
	r.recordedTID, r.recordedCID, r.recordedUser = tenantID, certificateID, signerUserID
	r.recordedDig = digest
	return r.recordErr
}

// --- Upload -------------------------------------------------------------------

func TestUpload_StoresEncryptedEnvelope_PublishesAdded(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	pfx := generateTestPFX(t, "ADV TESTE", "pw", time.Hour)

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, uow, newFakeCipher(t), outbox)

	tenantID, ownerID := uuid.NewString(), uuid.NewString()
	cert, err := uc.Upload(context.Background(), tenantID, ownerID, pfx, "pw")
	require.NoError(t, err)

	require.NotNil(t, repo.inserted)
	is.NotEmpty(repo.inserted.Envelope.Ciphertext)
	is.NotEmpty(repo.inserted.Envelope.WrappedDEK)
	is.Equal(tenantID, repo.inserted.TenantID)
	is.Equal(ownerID, repo.inserted.OwnerUserID)
	is.Equal("ADV TESTE", cert.SubjectCN)
	is.Len(cert.Fingerprint, 64)

	is.Equal([]string{tenantID}, uow.scopes)
	require.Len(t, outbox.published, 1)
	is.Equal(typeCertificateAdded, outbox.published[0].Type())
}

func TestUpload_WrongPassword_NoWriteNoEvent(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	pfx := generateTestPFX(t, "CN", "right", time.Hour)

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, uow, newFakeCipher(t), outbox)

	_, err := uc.Upload(context.Background(), "t", "u", pfx, "wrong")
	is.ErrorIs(err, ErrPKCS12BadPassword)
	is.Nil(repo.inserted, "no row on a wrong password")
	is.Empty(outbox.published, "no event on a wrong password")
	is.Empty(uow.scopes, "no tx opened on a parse failure")
}

func TestUpload_Expired_Rejected(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	pfx := generateTestPFX(t, "CN", "pw", -time.Hour) // NotAfter already in the past

	uc := NewUseCase(&fakeRepo{}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	_, err := uc.Upload(context.Background(), "t", "u", pfx, "pw")
	is.ErrorIs(err, ErrCertificateExpired)
}

func TestUpload_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	pfx := generateTestPFX(t, "CN", "pw", time.Hour)
	repo := &fakeRepo{insertErr: errors.New("db down")}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})

	_, err := uc.Upload(context.Background(), "t", "u", pfx, "pw")
	is.Error(err)
}

// --- Preview ------------------------------------------------------------------

func TestPreview_ReportsMetadataAndChecks_NoWrite(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	pfx := generateTestPFX(t, "MARIA", "pw", time.Hour)

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, uow, newFakeCipher(t), outbox)

	res, err := uc.Preview(context.Background(), pfx, "pw")
	require.NoError(t, err)

	is.Equal("MARIA", res.Meta.SubjectCN)
	is.Len(res.Meta.Fingerprint, 64)
	is.True(res.Checks.NaoExpirado)
	is.False(res.Checks.CadeiaOk, "self-signed fixture ships no CA chain")

	is.Nil(repo.inserted)
	is.Empty(uow.scopes)
	is.Empty(outbox.published)
}

func TestPreview_Expired_StillSucceeds_ReportsNaoExpiradoFalse(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	pfx := generateTestPFX(t, "CN", "pw", -time.Hour)

	uc := NewUseCase(&fakeRepo{}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	res, err := uc.Preview(context.Background(), pfx, "pw")
	require.NoError(t, err, "preview reports rather than rejecting an expired cert")
	is.False(res.Checks.NaoExpirado)
}

func TestPreview_WrongPassword_TypedError(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	pfx := generateTestPFX(t, "CN", "right", time.Hour)

	uc := NewUseCase(&fakeRepo{}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	_, err := uc.Preview(context.Background(), pfx, "wrong")
	is.ErrorIs(err, ErrPKCS12BadPassword)
}

// --- List / Revoke --------------------------------------------------------------

func TestList_ScopedToTenant_MetadataOnly(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{listViews: []CertificateWithOwner{{Certificate: Certificate{ID: "c1", SubjectCN: "A"}}}}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, uow, newFakeCipher(t), &fakeOutbox{})

	views, err := uc.List(context.Background(), "tenant-x")
	require.NoError(t, err)
	is.Len(views, 1)
	is.Equal([]string{"tenant-x"}, uow.scopes)
}

func TestRevoke_PublishesRevokedInTx(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, uow, newFakeCipher(t), outbox)

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
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), outbox)

	err := uc.Revoke(context.Background(), "t", "missing")
	is.ErrorIs(err, ErrCertificateNotFound)
	is.Empty(outbox.published, "no event when the revoke found nothing")
}

// --- Sign -------------------------------------------------------------------

// sealedCertificate uploads a real cert through the use case so its stored
// envelope round-trips through the (fake) cipher exactly like production, then
// returns the repo's view of it for a Sign test to feed back via fakeRepo.getRes.
func sealedCertificate(t *testing.T, uc *UseCase, repo *fakeRepo, cn, password string, ttl time.Duration) *Certificate {
	t.Helper()
	pfx := generateTestPFX(t, cn, password, ttl)
	_, err := uc.Upload(context.Background(), "tenant-x", "owner-1", pfx, password)
	require.NoError(t, err)
	return repo.inserted
}

func TestSign_RoundTrip_RecordsAudit(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	cert := sealedCertificate(t, uc, repo, "ADV SIGNER", "pw", time.Hour)
	repo.getRes = cert

	sum := sha256.Sum256([]byte("a document to sign"))
	res, err := uc.Sign(context.Background(), "tenant-x", "cert-1", "user-1", "pw", sum[:])
	require.NoError(t, err)

	is.NotEmpty(res.Signature)
	require.NotEmpty(t, res.Chain)

	// The audit row is written with the exact digest signed, the signer from the
	// principal (never the body), and the tenant scope.
	is.Equal(sum[:], repo.recordedDig)
	is.Equal("cert-1", repo.recordedCID)
	is.Equal("tenant-x", repo.recordedTID)
	is.Equal("user-1", repo.recordedUser)
}

func TestSign_Revoked_Rejected(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	cert := sealedCertificate(t, uc, repo, "CN", "pw", time.Hour)
	revoked := time.Now().Add(-time.Minute)
	cert.RevokedAt = &revoked
	repo.getRes = cert

	sum := sha256.Sum256([]byte("x"))
	_, err := uc.Sign(context.Background(), "t", "c", "u", "pw", sum[:])
	is.ErrorIs(err, ErrCertificateNotFound)
	is.Nil(repo.recordedDig, "no audit row when the cert is already revoked")
}

func TestSign_NotFound_Propagates(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{getErr: ErrCertificateNotFound}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})

	sum := sha256.Sum256([]byte("x"))
	_, err := uc.Sign(context.Background(), "t", "missing", "u", "pw", sum[:])
	is.ErrorIs(err, ErrCertificateNotFound)
}

func TestSign_RecordSigningError_Propagates(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{recordErr: errors.New("db down")}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	cert := sealedCertificate(t, uc, repo, "CN", "pw", time.Hour)
	repo.getRes = cert

	sum := sha256.Sum256([]byte("x"))
	_, err := uc.Sign(context.Background(), "t", "c", "u", "pw", sum[:])
	is.Error(err)
}
