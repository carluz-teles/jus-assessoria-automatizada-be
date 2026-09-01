package court

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/apperr"
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

// fakeRepo is an in-memory Repository — enough to exercise Connect's and
// FetchAutosBatch's orchestration without a database. secrets is keyed by the fake
// id InsertSecret hands out; fetchState/syncRuns back the FetchAutosBatch fakes.
type fakeRepo struct {
	conn *CourtConnection

	statusUpdates []Status
	mfaSeedRef    string
	sessionRef    string

	secrets   map[string]vault.Sealed
	secretSeq int

	lockAcquired int

	fetchState map[string]FetchStateItem // key: court_record_id
	fetchOrder []string                  // insertion order, for deterministic ListDueFetchState

	syncRuns map[string]*fakeSyncRun
	syncSeq  int
}

type fakeSyncRun struct {
	status                 string
	itemsNew, itemsDeduped int
	finishedAt             time.Time
	errMsg                 string
	closed                 bool
}

func newFakeRepo(conn *CourtConnection) *fakeRepo {
	return &fakeRepo{
		conn:       conn,
		secrets:    map[string]vault.Sealed{},
		fetchState: map[string]FetchStateItem{},
		syncRuns:   map[string]*fakeSyncRun{},
	}
}

func (r *fakeRepo) Insert(_ context.Context, _ database.Tx, conn *CourtConnection) (string, time.Time, error) {
	panic("not used by these tests")
}

func (r *fakeRepo) GetByID(_ context.Context, _ database.Tx, _, _ string) (*CourtConnection, error) {
	c := *r.conn
	c.MFASeedRef = r.mfaSeedRef
	c.SessionRef = r.sessionRef
	return &c, nil
}

func (r *fakeRepo) List(_ context.Context, _ database.Tx, _ string) ([]CourtConnection, error) {
	return []CourtConnection{*r.conn}, nil
}

func (r *fakeRepo) FindByCourtSystem(_ context.Context, _ database.Tx, tenantID, court, system string) (*CourtConnection, error) {
	if r.conn.TenantID != tenantID || r.conn.Court != court || r.conn.System != system {
		return nil, ErrConnectionNotFound
	}
	c := *r.conn
	c.MFASeedRef = r.mfaSeedRef
	c.SessionRef = r.sessionRef
	return &c, nil
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

func (r *fakeRepo) UpdateSessionRef(_ context.Context, _ database.Tx, _, _, sessionRef string, lastAuthenticatedAt time.Time) error {
	r.sessionRef = sessionRef
	r.conn.SessionRef = sessionRef
	r.conn.LastAuthenticatedAt = &lastAuthenticatedAt
	return nil
}

func (r *fakeRepo) AcquireTenantWriteLock(_ context.Context, _ database.Tx, _ string) error {
	r.lockAcquired++
	return nil
}

func (r *fakeRepo) UpsertFetchStateObserved(_ context.Context, _ database.Tx, _, _, recordID, cnjNumber string, observedAt time.Time) error {
	existing, ok := r.fetchState[recordID]
	if !ok {
		r.fetchOrder = append(r.fetchOrder, recordID)
		existing = FetchStateItem{CourtRecordID: recordID, CNJNumber: cnjNumber}
	}
	if observedAt.After(existing.ObservedAt) {
		existing.ObservedAt = observedAt
	}
	r.fetchState[recordID] = existing
	return nil
}

func (r *fakeRepo) ListDueFetchState(_ context.Context, _ database.Tx, _ string, limit int32) ([]FetchStateItem, error) {
	due := make([]FetchStateItem, 0, limit)
	for _, id := range r.fetchOrder {
		item := r.fetchState[id]
		isDue := item.LastFetchedAt == nil || item.LastFetchedAt.Before(item.ObservedAt)
		if !isDue {
			continue
		}
		due = append(due, item)
		if int32(len(due)) >= limit {
			break
		}
	}
	return due, nil
}

func (r *fakeRepo) GetFetchState(_ context.Context, _ database.Tx, _, recordID string) (FetchStateItem, error) {
	item, ok := r.fetchState[recordID]
	if !ok {
		return FetchStateItem{}, apperr.NewNotFound("estado de busca não encontrado")
	}
	return item, nil
}

func (r *fakeRepo) MarkFetchStateFetched(_ context.Context, _ database.Tx, _, recordID string, fetchedAt time.Time, docketCursor *time.Time) error {
	item := r.fetchState[recordID]
	item.LastFetchedAt = &fetchedAt
	item.DocketCursor = docketCursor
	r.fetchState[recordID] = item
	return nil
}

func (r *fakeRepo) OpenSyncRun(_ context.Context, _ database.Tx, _ string, _ time.Time) (string, error) {
	r.syncSeq++
	id := "run-" + itoa(r.syncSeq)
	r.syncRuns[id] = &fakeSyncRun{status: "RUNNING"}
	return id, nil
}

func (r *fakeRepo) CloseSyncRun(_ context.Context, _ database.Tx, runID, status string, itemsNew, itemsDeduped int, finishedAt time.Time, errMsg string) error {
	run, ok := r.syncRuns[runID]
	if !ok || run.closed {
		return nil
	}
	run.status = status
	run.itemsNew = itemsNew
	run.itemsDeduped = itemsDeduped
	run.finishedAt = finishedAt
	run.errMsg = errMsg
	run.closed = true
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
// It also records every FetchAutos call and answers with the next canned
// (result, session, error) triple — enough to script FetchAutosUseCase's per-item
// classification (a credential-broken error vs. a transient one) without needing to
// simulate doAuthed's own re-auth-on-rejection loop, which lives entirely inside the
// real lib/eproc.Client, not the CourtProvider contract this fake stands in for.
type fakeProvider struct {
	connectCalls []string // seed argument of each call, in order
	connectErrs  []error  // one error per call, by index; last one repeats if exhausted

	enroll    func(ctx context.Context, conn *CourtConnection) (string, error)
	enrollErr error

	fetchCalls   []fetchAutosCall
	fetchResults []AutosResult // one per call, by index; zero value if exhausted
	fetchErrs    []error       // one per call, by index; nil (success) if exhausted
	fetchSession Session       // sessionOut returned on every FetchAutos call
}

type fetchAutosCall struct {
	seed          string
	sessionIn     Session
	courtRecordID string
	cnjNumber     string
	docketCursor  time.Time
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

func (p *fakeProvider) FetchAutos(_ context.Context, _ *CourtConnection, seed string, sessionIn Session, courtRecordID, cnjNumber string, docketCursor time.Time) (AutosResult, Session, error) {
	p.fetchCalls = append(p.fetchCalls, fetchAutosCall{
		seed:          seed,
		sessionIn:     sessionIn,
		courtRecordID: courtRecordID,
		cnjNumber:     cnjNumber,
		docketCursor:  docketCursor,
	})
	idx := len(p.fetchCalls) - 1

	var result AutosResult
	if idx < len(p.fetchResults) {
		result = p.fetchResults[idx]
	}
	var err error
	if idx < len(p.fetchErrs) {
		err = p.fetchErrs[idx]
	}
	return result, p.fetchSession, err
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

// --- FetchAutosBatch / FetchAutosItem ----------------------------------------

func connectedConn() *CourtConnection {
	c := newConn()
	c.Status = StatusConnected
	return c
}

func TestFetchAutosBatch_NotConnected_NoOp(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := newConn() // default Status is DISCONNECTED
	repo := newFakeRepo(conn)
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	result, err := uc.FetchAutosBatch(context.Background(), conn.TenantID, conn.ID)

	require.NoError(t, err)
	is.False(result.HasMore)
	is.Empty(provider.fetchCalls, "must never call the provider against a connection that isn't CONNECTED")
}

func TestFetchAutosBatch_NoDueItems_NoOp(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	result, err := uc.FetchAutosBatch(context.Background(), conn.TenantID, conn.ID)

	require.NoError(t, err)
	is.False(result.HasMore)
	is.Empty(provider.fetchCalls)
}

func TestFetchAutosBatch_FetchesAllDueItems_ThreadsSessionBetweenCalls(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", now))
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-2", "cnj-2", now))

	provider := &fakeProvider{
		fetchResults: []AutosResult{{}, {}},
		fetchSession: Session("renewed-session"),
	}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	result, err := uc.FetchAutosBatch(ctx, conn.TenantID, conn.ID)

	require.NoError(t, err)
	is.False(result.HasMore)
	is.Empty(result.RetryItems)
	require.Len(t, provider.fetchCalls, 2)
	is.Equal("cnj-1", provider.fetchCalls[0].cnjNumber)
	is.Empty(provider.fetchCalls[0].sessionIn, "no session persisted yet — first call starts from scratch")
	is.Equal("cnj-2", provider.fetchCalls[1].cnjNumber)
	is.Equal(Session("renewed-session"), provider.fetchCalls[1].sessionIn, "call 2 must reuse call 1's returned session, not open a new one")

	due, err := repo.ListDueFetchState(ctx, nil, conn.ID, 10)
	require.NoError(t, err)
	is.Empty(due, "both items should now be marked fetched")
}

func TestFetchAutosBatch_RespectsSizeLimit_SetsHasMore(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range fetchAutosBatchSize + 5 {
		id := fmt.Sprintf("rec-%d", i)
		require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, id, id, now))
	}

	provider := &fakeProvider{
		fetchResults: make([]AutosResult, fetchAutosBatchSize),
		fetchSession: Session("s"),
	}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	result, err := uc.FetchAutosBatch(ctx, conn.TenantID, conn.ID)

	require.NoError(t, err)
	is.True(result.HasMore)
	is.Len(provider.fetchCalls, fetchAutosBatchSize)
}

func TestFetchAutosBatch_CredentialBroken_AbortsBatchAndTransitionsStatus(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", now))
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-2", "cnj-2", now))

	provider := &fakeProvider{
		fetchErrs:    []error{apperr.NewUnauthorized("credencial inválida")},
		fetchSession: Session("s"),
	}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	_, err := uc.FetchAutosBatch(ctx, conn.TenantID, conn.ID)

	require.Error(t, err)
	is.Len(provider.fetchCalls, 1, "must not attempt the second item after a credential-broken failure")
	is.Equal(StatusError, conn.Status)
}

func TestFetchAutosBatch_TransientFailure_SkipsItemButContinuesBatch(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", now))
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-2", "cnj-2", now))

	provider := &fakeProvider{
		fetchErrs:    []error{apperr.NewUnavailable("timeout", nil)},
		fetchResults: []AutosResult{{}, {}},
		fetchSession: Session("s"),
	}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	result, err := uc.FetchAutosBatch(ctx, conn.TenantID, conn.ID)

	require.NoError(t, err)
	is.Len(provider.fetchCalls, 2, "the second item must still be attempted")
	require.Len(t, result.RetryItems, 1)
	is.Equal("rec-1", result.RetryItems[0].CourtRecordID)
	is.Equal(StatusConnected, conn.Status, "a transient item failure must not touch connection status")

	due, err := repo.ListDueFetchState(ctx, nil, conn.ID, 10)
	require.NoError(t, err)
	require.Len(t, due, 1, "the failed item must remain due — this IS how it 'volta pra fila'")
	is.Equal("rec-1", due[0].CourtRecordID)
}

func TestFetchAutosItem_Success_MarksFetched(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", time.Now().UTC()))

	provider := &fakeProvider{fetchResults: []AutosResult{{}}, fetchSession: Session("s")}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	err := uc.FetchAutosItem(ctx, conn.TenantID, conn.ID, "rec-1", "cnj-1")

	require.NoError(t, err)
	due, err := repo.ListDueFetchState(ctx, nil, conn.ID, 10)
	require.NoError(t, err)
	is.Empty(due)
}

func TestFetchAutosItem_TransientFailure_ReturnsErrorForAsynqRetry(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", time.Now().UTC()))

	provider := &fakeProvider{fetchErrs: []error{apperr.NewUnavailable("timeout", nil)}, fetchSession: Session("s")}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	err := uc.FetchAutosItem(ctx, conn.TenantID, conn.ID, "rec-1", "cnj-1")

	require.Error(t, err)
	is.Equal(StatusConnected, conn.Status)
	due, err := repo.ListDueFetchState(ctx, nil, conn.ID, 10)
	require.NoError(t, err)
	is.Len(due, 1, "must remain due so the next asynq attempt picks it up again")
}

func TestFetchAutosItem_CredentialBroken_TransitionsStatus(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", time.Now().UTC()))

	provider := &fakeProvider{fetchErrs: []error{apperr.NewUnauthorized("credencial inválida")}, fetchSession: Session("s")}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	err := uc.FetchAutosItem(ctx, conn.TenantID, conn.ID, "rec-1", "cnj-1")

	require.Error(t, err)
	is.Equal(StatusError, conn.Status)
}

// --- OnCourtRecordObserved / OnFetchAutosRequested / OnFetchAutosItemRequested ---

func observedEvent(conn *CourtConnection, recordID, cnjNumber string) courtRecordObserved {
	return courtRecordObserved{
		TenantID:      conn.TenantID,
		CourtRecordID: recordID,
		CNJNumber:     cnjNumber,
		Court:         conn.Court,
	}
}

func TestOnCourtRecordObserved_NoMatchingConnection_NoOp(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)

	// A different tribunal — FindByCourtSystem won't match r.conn.
	ev := observedEvent(conn, "rec-1", "cnj-1")
	ev.Court = "TJRJ"

	err := uc.OnCourtRecordObserved(context.Background(), ev)

	require.NoError(t, err)
	is.Empty(outbox.published)
}

func TestOnCourtRecordObserved_ConnectionNotConnected_NoOp(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := newConn() // default Status is DISCONNECTED
	repo := newFakeRepo(conn)
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)

	err := uc.OnCourtRecordObserved(context.Background(), observedEvent(conn, "rec-1", "cnj-1"))

	require.NoError(t, err)
	is.Empty(outbox.published, "must not schedule work against a broken/pending connection")
}

func TestOnCourtRecordObserved_MatchingConnected_UpsertsAndPublishesStepZero(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)

	err := uc.OnCourtRecordObserved(context.Background(), observedEvent(conn, "rec-1", "cnj-1"))

	require.NoError(t, err)
	require.Len(t, outbox.published, 1)
	got, ok := outbox.published[0].(fetchAutosRequested)
	require.True(t, ok, "published event should be fetchAutosRequested, got %T", outbox.published[0])
	is.Equal(0, got.Step)
	is.Equal(conn.ID, got.ConnectionID)

	due, err := repo.ListDueFetchState(context.Background(), nil, conn.ID, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	is.Equal("cnj-1", due[0].CNJNumber)
}

func TestOnCourtRecordObserved_Burst_CollapsesToStableEventID(t *testing.T) {
	// Two observations for the SAME connection must publish events sharing the same
	// EventID (step 0 is stable) — the relay's own dedup collapses the burst into
	// one pending task. This test only proves OUR half: the id we hand it is stable.
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	ctx := context.Background()

	require.NoError(t, uc.OnCourtRecordObserved(ctx, observedEvent(conn, "rec-1", "cnj-1")))
	require.NoError(t, uc.OnCourtRecordObserved(ctx, observedEvent(conn, "rec-2", "cnj-2")))

	require.Len(t, outbox.published, 2)
	first := outbox.published[0].(fetchAutosRequested)
	second := outbox.published[1].(fetchAutosRequested)
	is.Equal(first.IdempotencyKey(), second.IdempotencyKey())
}

func TestOnFetchAutosRequested_HasMore_PublishesFreshContinuationStep(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range fetchAutosBatchSize + 3 {
		id := fmt.Sprintf("rec-%d", i)
		require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, id, id, now))
	}

	provider := &fakeProvider{
		fetchResults: make([]AutosResult, fetchAutosBatchSize),
		fetchSession: Session("s"),
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	uc.RegisterProvider("EPROC", provider)

	err := uc.OnFetchAutosRequested(ctx, newFetchAutosRequested(conn.TenantID, conn.ID))

	require.NoError(t, err)
	require.Len(t, outbox.published, 1)
	got, ok := outbox.published[0].(fetchAutosRequested)
	require.True(t, ok)
	is.Equal(1, got.Step, "continuation must be step+1")

	first := newFetchAutosRequested(conn.TenantID, conn.ID)
	is.NotEqual(first.IdempotencyKey(), got.IdempotencyKey(), "continuation must use a FRESH id, never collide with the just-processed step")
}

func TestOnFetchAutosRequested_RetryItems_PublishesItemRequestedEvents(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", now))

	provider := &fakeProvider{
		fetchErrs:    []error{apperr.NewUnavailable("timeout", nil)},
		fetchSession: Session("s"),
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), outbox)
	uc.RegisterProvider("EPROC", provider)

	err := uc.OnFetchAutosRequested(ctx, newFetchAutosRequested(conn.TenantID, conn.ID))

	require.NoError(t, err)
	require.Len(t, outbox.published, 1)
	got, ok := outbox.published[0].(fetchAutosItemRequested)
	require.True(t, ok, "published event should be fetchAutosItemRequested, got %T", outbox.published[0])
	is.Equal("rec-1", got.CourtRecordID)
	is.Equal("cnj-1", got.CNJNumber)
}

func TestOnFetchAutosItemRequested_DelegatesToFetchAutosItem(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	conn := connectedConn()
	repo := newFakeRepo(conn)
	ctx := context.Background()
	require.NoError(t, repo.UpsertFetchStateObserved(ctx, nil, conn.TenantID, conn.ID, "rec-1", "cnj-1", time.Now().UTC()))

	provider := &fakeProvider{fetchResults: []AutosResult{{}}, fetchSession: Session("s")}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)

	err := uc.OnFetchAutosItemRequested(ctx, fetchAutosItemRequested{
		TenantID: conn.TenantID, ConnectionID: conn.ID, CourtRecordID: "rec-1", CNJNumber: "cnj-1",
	})

	require.NoError(t, err)
	due, err := repo.ListDueFetchState(ctx, nil, conn.ID, 10)
	require.NoError(t, err)
	is.Empty(due)
}
