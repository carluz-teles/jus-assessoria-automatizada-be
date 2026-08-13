package document

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- fakes ------------------------------------------------------------------

// fakeUOW runs fn with a nil tx (the mocked repo/storage never touch it) and records the RLS
// scope each Do asked for.
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

// fakeOutbox captures every published event so a test can assert the document.uploaded contract.
type fakeOutbox struct {
	published []events.Event
	err       error
}

func (o *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	o.published = append(o.published, ev)
	return o.err
}

// fakeStorage records the presign/exists calls and returns canned values.
type fakeStorage struct {
	putURL, getURL string
	putErr, getErr error
	exists         bool
	existsErr      error

	gotPutKey, gotPutCT, gotGetKey, gotExistsKey string
	putCalls, getCalls, existsCalls              int
}

func (s *fakeStorage) PresignedPut(_ context.Context, key, contentType string, _ time.Duration) (string, error) {
	s.putCalls++
	s.gotPutKey, s.gotPutCT = key, contentType
	return s.putURL, s.putErr
}

func (s *fakeStorage) PresignedGet(_ context.Context, key string, _ time.Duration) (string, error) {
	s.getCalls++
	s.gotGetKey = key
	return s.getURL, s.getErr
}

func (s *fakeStorage) Exists(_ context.Context, key string) (bool, error) {
	s.existsCalls++
	s.gotExistsKey = key
	return s.exists, s.existsErr
}

// fakeRepo primes each read/write of the upload path and records what it was asked.
type fakeRepo struct {
	ensureErr error
	gotEnsure [2]string

	inserted   *Document
	insertErr  error
	gotInsert  *Document
	insertCall int

	completeDoc *DocumentForComplete
	completeErr error

	uploaded    *Document
	uploadedErr error
	gotChecksum string

	deleteDoc *DocumentForDelete
	deleteErr error

	softErr     error
	softDeleted time.Time
	softCalls   int
}

func (r *fakeRepo) EnsureCourtRecordInTenant(_ context.Context, _ database.Tx, tenantID, courtRecordID string) error {
	r.gotEnsure = [2]string{tenantID, courtRecordID}
	return r.ensureErr
}

func (r *fakeRepo) InsertDocument(_ context.Context, _ database.Tx, d *Document) (*Document, error) {
	r.insertCall++
	r.gotInsert = d
	if r.insertErr != nil {
		return nil, r.insertErr
	}
	if r.inserted != nil {
		return r.inserted, nil
	}
	// Echo the entity with a fresh id, as the real repo would.
	saved := *d
	saved.ID = uuid.NewString()
	return &saved, nil
}

func (r *fakeRepo) GetDocumentForComplete(_ context.Context, _ database.Tx, _, _ string) (*DocumentForComplete, error) {
	return r.completeDoc, r.completeErr
}

func (r *fakeRepo) MarkUploaded(_ context.Context, _ database.Tx, _, _, checksum string) (*Document, error) {
	r.gotChecksum = checksum
	return r.uploaded, r.uploadedErr
}

func (r *fakeRepo) GetDocumentForDelete(_ context.Context, _ database.Tx, _, _ string) (*DocumentForDelete, error) {
	return r.deleteDoc, r.deleteErr
}

func (r *fakeRepo) SoftDelete(_ context.Context, _ database.Tx, _, _ string, deletedAt time.Time) error {
	r.softCalls++
	r.softDeleted = deletedAt
	return r.softErr
}

// --- Start ------------------------------------------------------------------

// TestStart_CreatesPendingAndPresigns is the happy path: Start verifies the court_record, mints a
// tenant-scoped key, inserts the PENDING/UPLOAD document with that key, and presigns a PUT for
// key + mime.
func TestStart_CreatesPendingAndPresigns(t *testing.T) {
	tenant := uuid.NewString()
	crid := uuid.NewString()
	repo := &fakeRepo{}
	store := &fakeStorage{putURL: "https://s3/put"}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, store, &fakeOutbox{}, uow,
		WithKeyGen(func(tid, prefix string) string { return tid + "/" + prefix + "/fixed" }))

	res, err := uc.Start(context.Background(), StartUploadCommand{
		TenantID:         tenant,
		CourtRecordID:    crid,
		DocumentType:     "PETICAO",
		OriginalFilename: "peca.pdf",
		MimeType:         "application/pdf",
		SizeBytes:        2048,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if len(uow.scopes) != 1 || uow.scopes[0] != tenant {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenant)
	}
	if repo.gotEnsure != [2]string{tenant, crid} {
		t.Errorf("ensure got = %v, want [%q %q]", repo.gotEnsure, tenant, crid)
	}
	if repo.gotInsert.Status != StatusPending || repo.gotInsert.Origin != OriginUpload {
		t.Errorf("inserted status/origin = %q/%q, want PENDING/UPLOAD", repo.gotInsert.Status, repo.gotInsert.Origin)
	}
	wantKey := tenant + "/documents/fixed"
	if repo.gotInsert.StorageKey != wantKey {
		t.Errorf("inserted key = %q, want %q", repo.gotInsert.StorageKey, wantKey)
	}
	// Title defaults to the original filename when absent.
	if repo.gotInsert.Title != "peca.pdf" {
		t.Errorf("inserted title = %q, want peca.pdf (default)", repo.gotInsert.Title)
	}
	if store.gotPutKey != wantKey || store.gotPutCT != "application/pdf" {
		t.Errorf("presign key/ct = %q/%q", store.gotPutKey, store.gotPutCT)
	}
	if res.UploadURL != "https://s3/put" || res.StorageKey != wantKey || res.ExpiresIn != 900 {
		t.Errorf("result = %+v", res)
	}
}

// TestStart_NoCourtRecordSkipsGuard proves an avulsa upload (no court_record_id) never calls the
// court_record guard.
func TestStart_NoCourtRecordSkipsGuard(t *testing.T) {
	repo := &fakeRepo{}
	uc := NewUseCase(repo, &fakeStorage{putURL: "u"}, &fakeOutbox{}, &fakeUOW{})

	if _, err := uc.Start(context.Background(), StartUploadCommand{
		TenantID: uuid.NewString(), DocumentType: "PETICAO", OriginalFilename: "a.pdf", MimeType: "application/pdf", SizeBytes: 1,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if repo.gotEnsure != [2]string{} {
		t.Errorf("court_record guard was called for an avulsa upload: %v", repo.gotEnsure)
	}
}

// TestStart_CourtRecordMiss propagates ErrCourtRecordNotFound and never inserts.
func TestStart_CourtRecordMiss(t *testing.T) {
	repo := &fakeRepo{ensureErr: ErrCourtRecordNotFound}
	uc := NewUseCase(repo, &fakeStorage{}, &fakeOutbox{}, &fakeUOW{})

	_, err := uc.Start(context.Background(), StartUploadCommand{
		TenantID: uuid.NewString(), CourtRecordID: uuid.NewString(),
		DocumentType: "PETICAO", OriginalFilename: "a.pdf", MimeType: "application/pdf", SizeBytes: 1,
	})
	if !errors.Is(err, ErrCourtRecordNotFound) {
		t.Fatalf("error = %v, want ErrCourtRecordNotFound", err)
	}
	if repo.insertCall != 0 {
		t.Errorf("inserted despite a court_record miss")
	}
}

// --- Complete ---------------------------------------------------------------

// TestComplete_FlipsUploadedAndEmits is the happy path: a PENDING document whose bytes exist
// flips UPLOADED and emits document.uploaded with the right payload, in the tenant's tx.
func TestComplete_FlipsUploadedAndEmits(t *testing.T) {
	tenant := uuid.NewString()
	crid := uuid.NewString()
	docID := uuid.NewString()
	repo := &fakeRepo{
		completeDoc: &DocumentForComplete{ID: docID, TenantID: tenant, CourtRecordID: crid, Status: StatusPending, StorageKey: "k", MimeType: "application/pdf"},
		uploaded:    &Document{ID: docID, TenantID: tenant, CourtRecordID: crid, StorageKey: "k", MimeType: "application/pdf", Status: StatusUploaded, Origin: OriginUpload},
	}
	store := &fakeStorage{exists: true}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, store, outbox, uow)

	view, err := uc.Complete(context.Background(), CompleteCommand{TenantID: tenant, DocumentID: docID, Checksum: "sha"})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if store.existsCalls != 1 || store.gotExistsKey != "k" {
		t.Errorf("exists calls/key = %d/%q", store.existsCalls, store.gotExistsKey)
	}
	if repo.gotChecksum != "sha" {
		t.Errorf("checksum = %q, want sha", repo.gotChecksum)
	}
	if view.Status != "UPLOADED" || view.ID != docID {
		t.Errorf("view = %+v", view)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenant {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenant)
	}
	if len(outbox.published) != 1 {
		t.Fatalf("published %d events, want 1", len(outbox.published))
	}
	ev, ok := outbox.published[0].(DocumentUploaded)
	if !ok {
		t.Fatalf("event type = %T, want DocumentUploaded", outbox.published[0])
	}
	if ev.DocumentID != docID || ev.TenantID != tenant || ev.CourtRecordID != crid || ev.StorageKey != "k" || ev.MimeType != "application/pdf" {
		t.Errorf("event = %+v", ev)
	}
	if ev.Type() != TypeDocumentUploaded || ev.AggregateType() != aggregateTypeDocument || ev.AggregateID() != docID {
		t.Errorf("event contract = %q/%q/%q", ev.Type(), ev.AggregateType(), ev.AggregateID())
	}
	if _, perr := uuid.Parse(ev.IdempotencyKey()); perr != nil {
		t.Errorf("event id not a uuid: %q", ev.IdempotencyKey())
	}
}

// TestComplete_NotPending refuses a non-PENDING document (409) and never checks storage/emits.
func TestComplete_NotPending(t *testing.T) {
	repo := &fakeRepo{completeDoc: &DocumentForComplete{Status: StatusUploaded, StorageKey: "k"}}
	store := &fakeStorage{}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, store, outbox, &fakeUOW{})

	_, err := uc.Complete(context.Background(), CompleteCommand{TenantID: uuid.NewString(), DocumentID: uuid.NewString()})
	if !errors.Is(err, ErrDocumentNotUploadable) {
		t.Fatalf("error = %v, want ErrDocumentNotUploadable", err)
	}
	if store.existsCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("touched storage/outbox on a non-PENDING document")
	}
}

// TestComplete_BytesMissing refuses when the object never landed (409) and never emits.
func TestComplete_BytesMissing(t *testing.T) {
	repo := &fakeRepo{completeDoc: &DocumentForComplete{Status: StatusPending, StorageKey: "k"}}
	store := &fakeStorage{exists: false}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, store, outbox, &fakeUOW{})

	_, err := uc.Complete(context.Background(), CompleteCommand{TenantID: uuid.NewString(), DocumentID: uuid.NewString()})
	if !errors.Is(err, ErrDocumentBytesMissing) {
		t.Fatalf("error = %v, want ErrDocumentBytesMissing", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("emitted despite missing bytes")
	}
}

// TestComplete_NotFound propagates ErrDocumentNotFound from the load.
func TestComplete_NotFound(t *testing.T) {
	repo := &fakeRepo{completeErr: ErrDocumentNotFound}
	uc := NewUseCase(repo, &fakeStorage{}, &fakeOutbox{}, &fakeUOW{})

	if _, err := uc.Complete(context.Background(), CompleteCommand{TenantID: uuid.NewString(), DocumentID: uuid.NewString()}); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("error = %v, want ErrDocumentNotFound", err)
	}
}

// --- Download ---------------------------------------------------------------

// TestDownload_PresignsGet is the happy path: a document with a storage_key presigns a GET.
func TestDownload_PresignsGet(t *testing.T) {
	repo := &fakeRepo{completeDoc: &DocumentForComplete{Status: StatusUploaded, StorageKey: "tenant/documents/k"}}
	store := &fakeStorage{getURL: "https://s3/get"}
	uc := NewUseCase(repo, store, &fakeOutbox{}, &fakeUOW{})

	res, err := uc.Download(context.Background(), uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if store.gotGetKey != "tenant/documents/k" || res.URL != "https://s3/get" || res.ExpiresIn != 300 {
		t.Errorf("result = %+v, getKey = %q", res, store.gotGetKey)
	}
}

// TestDownload_NoStorageKey refuses a document with no key (409).
func TestDownload_NoStorageKey(t *testing.T) {
	repo := &fakeRepo{completeDoc: &DocumentForComplete{Status: StatusPending, StorageKey: ""}}
	store := &fakeStorage{}
	uc := NewUseCase(repo, store, &fakeOutbox{}, &fakeUOW{})

	if _, err := uc.Download(context.Background(), uuid.NewString(), uuid.NewString()); !errors.Is(err, ErrDocumentNoStorageKey) {
		t.Fatalf("error = %v, want ErrDocumentNoStorageKey", err)
	}
	if store.getCalls != 0 {
		t.Errorf("presigned despite no storage key")
	}
}

// --- Delete -----------------------------------------------------------------

// TestDelete_SoftDeletesUpload is the happy path: an UPLOAD document is soft-deleted with the
// clock's now().
func TestDelete_SoftDeletesUpload(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	repo := &fakeRepo{deleteDoc: &DocumentForDelete{ID: "doc-1", Origin: OriginUpload}}
	uc := NewUseCase(repo, &fakeStorage{}, &fakeOutbox{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	if err := uc.Delete(context.Background(), uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if repo.softCalls != 1 || !repo.softDeleted.Equal(now) {
		t.Errorf("soft delete calls/at = %d/%v, want 1/%v", repo.softCalls, repo.softDeleted, now)
	}
}

// TestDelete_RefusesCourt refuses an origin=COURT document (409) and never soft-deletes.
func TestDelete_RefusesCourt(t *testing.T) {
	repo := &fakeRepo{deleteDoc: &DocumentForDelete{ID: "doc-1", Origin: OriginCourt}}
	uc := NewUseCase(repo, &fakeStorage{}, &fakeOutbox{}, &fakeUOW{})

	if err := uc.Delete(context.Background(), uuid.NewString(), uuid.NewString()); !errors.Is(err, ErrDocumentNotDeletable) {
		t.Fatalf("error = %v, want ErrDocumentNotDeletable", err)
	}
	if repo.softCalls != 0 {
		t.Errorf("soft-deleted a COURT document")
	}
}
