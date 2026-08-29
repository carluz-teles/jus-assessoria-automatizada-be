package court

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/vault"
)

// --- fakes -------------------------------------------------------------------

type fakeUOW struct{}

func (fakeUOW) Do(_ context.Context, _ string, fn func(tx database.Tx) error) error {
	return fn(nil)
}
func (fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error { return fn(nil) }

type fakeOutbox struct {
	published []events.Event
}

func (o *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	o.published = append(o.published, ev)
	return nil
}

// fakeRepo is an in-memory Repository — enough to exercise Connect's orchestration
// without a database. secrets is keyed by the fake id InsertSecret hands out.
type fakeRepo struct {
	conn *CourtConnection

	statusUpdates []Status
	mfaSeedRef    string

	secrets   map[string]vault.Sealed
	secretSeq int
}

func newFakeRepo(conn *CourtConnection) *fakeRepo {
	return &fakeRepo{conn: conn, secrets: map[string]vault.Sealed{}}
}

func (r *fakeRepo) Insert(_ context.Context, _ database.Tx, conn *CourtConnection) (string, time.Time, error) {
	panic("not used by these tests")
}

func (r *fakeRepo) GetByID(_ context.Context, _ database.Tx, _, _ string) (*CourtConnection, error) {
	c := *r.conn
	c.MFASeedRef = r.mfaSeedRef
	return &c, nil
}

func (r *fakeRepo) List(_ context.Context, _ database.Tx, _ string) ([]CourtConnection, error) {
	return []CourtConnection{*r.conn}, nil
}

func (r *fakeRepo) UpdateStatus(_ context.Context, _ database.Tx, _, _ string, status Status, _ *time.Time, _ string) error {
	r.statusUpdates = append(r.statusUpdates, status)
	r.conn.Status = status
	return nil
}

func (r *fakeRepo) UpdateMFASeedRef(_ context.Context, _ database.Tx, _, _, mfaSeedRef string) error {
	r.mfaSeedRef = mfaSeedRef
	r.conn.MFASeedRef = mfaSeedRef
	return nil
}

func (r *fakeRepo) InsertSecret(_ context.Context, _ database.Tx, _ string, sealed vault.Sealed) (string, error) {
	r.secretSeq++
	id := "secret-" + itoa(r.secretSeq)
	r.secrets[id] = sealed
	return id, nil
}

func (r *fakeRepo) GetSecret(_ context.Context, _ database.Tx, _, id string) (vault.Sealed, error) {
	s, ok := r.secrets[id]
	if !ok {
		return vault.Sealed{}, errors.New("secret not found")
	}
	return s, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// fakeProvider records every Connect call (with the seed it was given) and answers
// with the next canned result — enough to script the "first call: no seed, MFA
// required; second call: seed present, succeeds" scenario Connect's retry exists for.
type fakeProvider struct {
	connectCalls []string // seed argument of each call, in order
	connectErrs  []error  // one error per call, by index; last one repeats if exhausted

	enroll    func(ctx context.Context, conn *CourtConnection) (string, error)
	enrollErr error
}

func (p *fakeProvider) Connect(_ context.Context, _ *CourtConnection, seed string) error {
	p.connectCalls = append(p.connectCalls, seed)
	idx := len(p.connectCalls) - 1
	if idx < len(p.connectErrs) {
		return p.connectErrs[idx]
	}
	if len(p.connectErrs) > 0 {
		return p.connectErrs[len(p.connectErrs)-1]
	}
	return nil
}

// fakeEnrollingProvider embeds fakeProvider and additionally implements MFAEnroller —
// a SEPARATE type (not a flag on fakeProvider) so tests can assert the
// "provider doesn't support MFAEnroller at all" path via a plain *fakeProvider.
type fakeEnrollingProvider struct {
	*fakeProvider
	seed string
	err  error
}

func (p *fakeEnrollingProvider) EnrollMFA(_ context.Context, _ *CourtConnection) (string, error) {
	return p.seed, p.err
}

func testVault(t *testing.T) *vault.Vault {
	t.Helper()
	kek, err := vault.GenerateKEK()
	require.NoError(t, err)
	v, err := vault.New(kek)
	require.NoError(t, err)
	return v
}

func newConn() *CourtConnection {
	return &CourtConnection{
		ID:                   "conn-1",
		TenantID:             "tenant-1",
		AppUserID:            "user-1",
		Court:                "TJSP",
		System:               "EPROC",
		AuthenticationMethod: AuthenticationMethodCertificateA1,
		CertificateRef:       "cert-1",
		Status:               StatusDisconnected,
	}
}

// --- Connect -------------------------------------------------------------------

func TestConnect_Success_NoMFANeeded(t *testing.T) {
	is := assert.New(t)
	repo := newFakeRepo(newConn())
	outbox := &fakeOutbox{}
	provider := &fakeProvider{} // Connect returns nil on the first call

	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	uc.RegisterProvider("EPROC", provider)

	conn, err := uc.Connect(context.Background(), "tenant-1", "conn-1")

	require.NoError(t, err)
	is.Equal(StatusConnected, conn.Status)
	is.Len(provider.connectCalls, 1)
	is.Equal("", provider.connectCalls[0], "first Connect call should carry no seed (none on file)")
	is.Contains(repo.statusUpdates, StatusConnected)
	is.Len(outbox.published, 1)
}

func TestConnect_MFAEnrollmentRequired_AutoEnrollsAndRetries(t *testing.T) {
	is := assert.New(t)
	repo := newFakeRepo(newConn())
	outbox := &fakeOutbox{}
	base := &fakeProvider{
		// First call (no seed): portal demands MFA. Second call (post-enrollment,
		// with the freshly captured seed): succeeds.
		connectErrs: []error{ErrMFAEnrollmentRequired, nil},
	}
	provider := &fakeEnrollingProvider{fakeProvider: base, seed: "JBSWY3DPEHPK3PXP"}

	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	uc.RegisterProvider("EPROC", provider)

	conn, err := uc.Connect(context.Background(), "tenant-1", "conn-1")

	require.NoError(t, err)
	is.Equal(StatusConnected, conn.Status)
	is.Len(base.connectCalls, 2, "Connect must be retried exactly once after enrollment")
	is.Equal("", base.connectCalls[0], "first attempt has no seed yet")
	is.Equal("JBSWY3DPEHPK3PXP", base.connectCalls[1], "retry must use the freshly enrolled seed")
	is.NotEmpty(conn.MFASeedRef, "the enrolled seed must be sealed and referenced on the connection")
	is.Contains(repo.statusUpdates, StatusMFAEnrollmentRequired, "the interim enrollment attempt must be visible")
	is.Equal(StatusConnected, repo.statusUpdates[len(repo.statusUpdates)-1], "final status must be CONNECTED, not stuck on the interim one")
}

func TestConnect_ProviderWithoutMFAEnroller_FailsWithoutLooping(t *testing.T) {
	is := assert.New(t)
	repo := newFakeRepo(newConn())
	outbox := &fakeOutbox{}
	// Plain *fakeProvider does NOT implement MFAEnroller.
	provider := &fakeProvider{connectErrs: []error{ErrMFAEnrollmentRequired}}

	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	uc.RegisterProvider("EPROC", provider)

	conn, err := uc.Connect(context.Background(), "tenant-1", "conn-1")

	require.ErrorIs(t, err, ErrMFAEnrollmentFailed)
	is.Equal(StatusMFAEnrollmentRequired, conn.Status)
	is.Len(provider.connectCalls, 1, "no retry when the provider can't enroll at all")
}

func TestConnect_EnrollMFAFails_SurfacesError(t *testing.T) {
	is := assert.New(t)
	repo := newFakeRepo(newConn())
	outbox := &fakeOutbox{}
	base := &fakeProvider{connectErrs: []error{ErrMFAEnrollmentRequired}}
	enrollErr := errors.New("enroll: portal page shape changed")
	provider := &fakeEnrollingProvider{fakeProvider: base, err: enrollErr}

	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	uc.RegisterProvider("EPROC", provider)

	conn, err := uc.Connect(context.Background(), "tenant-1", "conn-1")

	require.ErrorIs(t, err, enrollErr)
	is.Len(base.connectCalls, 1, "Connect is not retried when enrollment itself fails")
	is.Empty(conn.MFASeedRef)
}

func TestConnect_ProviderNotRegistered(t *testing.T) {
	repo := newFakeRepo(newConn())
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	// No RegisterProvider call — "EPROC" has no adapter wired.

	_, err := uc.Connect(context.Background(), "tenant-1", "conn-1")

	require.ErrorIs(t, err, ErrProviderNotRegistered)
}

func TestConnect_GenericProviderError_MarksStatusError(t *testing.T) {
	is := assert.New(t)
	repo := newFakeRepo(newConn())
	provider := &fakeProvider{connectErrs: []error{errors.New("eproc: portal unavailable")}}

	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	conn, err := uc.Connect(context.Background(), "tenant-1", "conn-1")

	require.Error(t, err)
	is.Equal(StatusError, conn.Status)
}

// --- SubmitMFASeed (human-assisted enrollment) ---------------------------------

func TestSubmitMFASeed_SealsAndConnects(t *testing.T) {
	is := assert.New(t)
	repo := newFakeRepo(newConn())
	outbox := &fakeOutbox{}
	provider := &fakeProvider{} // Connect succeeds once the seed is presented

	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	uc.RegisterProvider("EPROC", provider)

	conn, err := uc.SubmitMFASeed(context.Background(), "tenant-1", "conn-1", "JBSWY3DPEHPK3PXP")

	require.NoError(t, err)
	is.Equal(StatusConnected, conn.Status)
	is.NotEmpty(conn.MFASeedRef, "the submitted seed must be sealed and referenced")
	is.Len(provider.connectCalls, 1)
	is.Equal("JBSWY3DPEHPK3PXP", provider.connectCalls[0], "Connect must be called with the just-submitted seed")
	is.Len(outbox.published, 1)
}

func TestSubmitMFASeed_ProviderStillRejects_MarksError(t *testing.T) {
	is := assert.New(t)
	repo := newFakeRepo(newConn())
	// The submitted seed doesn't fix things (e.g. the lawyer copied the wrong key).
	provider := &fakeProvider{connectErrs: []error{errors.New("eproc: certificate accepted, MFA/TOTP challenge required")}}

	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	conn, err := uc.SubmitMFASeed(context.Background(), "tenant-1", "conn-1", "WRONGSEED")

	require.Error(t, err)
	is.Equal(StatusError, conn.Status)
	is.NotEmpty(conn.MFASeedRef, "the (wrong) seed is still stored — resubmitting overwrites it, not silently dropped")
}

func TestSubmitMFASeed_ProviderNotRegistered(t *testing.T) {
	repo := newFakeRepo(newConn())
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})

	_, err := uc.SubmitMFASeed(context.Background(), "tenant-1", "conn-1", "JBSWY3DPEHPK3PXP")

	require.ErrorIs(t, err, ErrProviderNotRegistered)
}
