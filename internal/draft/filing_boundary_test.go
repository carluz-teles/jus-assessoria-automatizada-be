package draft

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/lib/database"
)

// ── fakes for filing boundary tests ───────────────────────────────────────────

type fkRepo struct {
	*fakeRepo
	draftByID       *Draft
	draftErr        error
	esaj            *EsajCredentialEnvelope
	esajErr         error
	active          *FilingAttempt
	inserted        *FilingAttempt
	insertErr       error
	insertErrAfter  int // return insertErr after this many calls
	insertCalls     int
	detail          *DraftDetailView
	crID            string
	petition        *Petition
	markFiledCalls  int
	protoCalls      int
	failedCalls     int
	getAttempt      *FilingAttempt
	getAttemptCalls int
	latest          *FilingAttempt
	getLatestCalls  int
	lastSha256      string
	lastSnapKey     string
	lastRequestedBy string
}

func (r *fkRepo) GetDraftByID(_ context.Context, _ database.Tx, _, _ string) (*Draft, error) {
	return r.draftByID, r.draftErr
}
func (r *fkRepo) GetDraftDetail(_ context.Context, _ database.Tx, _, _ string) (*DraftDetailView, error) {
	return r.detail, nil
}
func (r *fkRepo) GetActiveEsajCredential(_ context.Context, _ database.Tx, _, _ string) (*EsajCredentialEnvelope, error) {
	return r.esaj, r.esajErr
}
func (r *fkRepo) GetActiveFilingAttempt(_ context.Context, _ database.Tx, _, _ string) (*FilingAttempt, error) {
	return r.active, nil
}
func (r *fkRepo) InsertFilingAttempt(_ context.Context, _ database.Tx, _, _, snapKey, sha, reqBy string) (*FilingAttempt, error) {
	r.insertCalls++
	r.lastSnapKey = snapKey
	r.lastSha256 = sha
	r.lastRequestedBy = reqBy
	if r.insertErr != nil && r.insertCalls > r.insertErrAfter {
		return nil, r.insertErr
	}
	return r.inserted, nil
}
func (r *fkRepo) GetFilingAttempt(_ context.Context, _ database.Tx, _, _ string) (*FilingAttempt, error) {
	r.getAttemptCalls++
	a := r.getAttempt
	if r.getAttemptCalls >= 2 {
		cp := *a
		cp.Status = StatusProtocolando
		return &cp, nil
	}
	return a, nil
}
func (r *fkRepo) GetLatestFilingAttempt(_ context.Context, _ database.Tx, _ string) (*FilingAttempt, error) {
	r.getLatestCalls++
	return r.latest, nil
}
func (r *fkRepo) GetCourtRecordIDByIntimation(_ context.Context, _ database.Tx, _, _ string) (string, error) {
	return r.crID, nil
}
func (r *fkRepo) InsertPetition(_ context.Context, _ database.Tx, p *Petition) (*Petition, error) {
	return r.petition, nil
}
func (r *fkRepo) MarkFiled(_ context.Context, _ database.Tx, _, _, _ string) error {
	r.markFiledCalls++
	return nil
}
func (r *fkRepo) MarkFilingProtocolando(_ context.Context, _ database.Tx, _ string) error {
	return nil
}
func (r *fkRepo) MarkFilingProtocolado(_ context.Context, _ database.Tx, _, _, _ string, _ []string) (*FilingAttempt, error) {
	r.protoCalls++
	return &FilingAttempt{ID: "a1", Status: StatusProtocolado}, nil
}
func (r *fkRepo) MarkFilingFailed(_ context.Context, _ database.Tx, _, _ string) error {
	r.failedCalls++
	return nil
}

type bndStorage struct{ got map[string][]byte }

func (s *bndStorage) PutBytes(_ context.Context, key, _ string, data []byte) error {
	s.got[key] = data
	return nil
}
func (s *bndStorage) GetBytes(_ context.Context, key string) ([]byte, error) {
	if b, ok := s.got[key]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("missing %s", key)
}

type fakeVault struct{}

func (fakeVault) Seal(_ context.Context, pt []byte) (*Envelope, error) {
	return &Envelope{Ciphertext: pt}, nil
}
func (fakeVault) Open(_ context.Context, env *Envelope) ([]byte, error) { return env.Ciphertext, nil }
func (fakeVault) Close() error                                          { return nil }

type fakeGateway struct {
	called bool
	num    string
	shots  [][]byte
	err    error
}

func (g *fakeGateway) Protocol(_ context.Context, _ FilingRequest) (*FilingResult, error) {
	g.called = true
	if g.err != nil {
		return nil, g.err
	}
	return &FilingResult{FilingNumber: g.num, Screenshots: g.shots}, nil
}

func newApproveUC(t *testing.T, r *fkRepo, st *bndStorage) *UseCase {
	t.Helper()
	uow := &fakeUOW{}
	uc := NewUseCase(uow, r, WithPDFStorage(st), WithSecretVault(fakeVault{}))
	return uc
}

// AC: draft not SIGNED → must reject.
func TestApproveFiling_NotSigned(t *testing.T) {
	r := &fkRepo{fakeRepo: &fakeRepo{}, draftByID: &Draft{Status: StatusDraft}}
	uc := newApproveUC(t, r, &bndStorage{got: map[string][]byte{}})
	_, err := uc.ApproveFiling(context.Background(), ApproveFilingCommand{TenantID: "t", DraftID: "d", UserID: "u"})
	if !errors.Is(err, ErrFilingNotSigned) {
		t.Fatalf("want ErrFilingNotSigned, got %v", err)
	}
	if r.insertCalls != 0 {
		t.Fatalf("must not insert attempt when not signed (calls=%d)", r.insertCalls)
	}
}

// AC: SIGNED but no e-SAJ credential → consent required, no insert.
func TestApproveFiling_ConsentRequired(t *testing.T) {
	r := &fkRepo{fakeRepo: &fakeRepo{}, draftByID: &Draft{Status: StatusSigned}, esajErr: pgx.ErrNoRows}
	uc := newApproveUC(t, r, &bndStorage{got: map[string][]byte{}})
	_, err := uc.ApproveFiling(context.Background(), ApproveFilingCommand{TenantID: "t", DraftID: "d", UserID: "u"})
	if !errors.Is(err, ErrFilingConsentRequired) {
		t.Fatalf("want ErrFilingConsentRequired, got %v", err)
	}
	if r.insertCalls != 0 {
		t.Fatalf("must not insert attempt without consent (calls=%d)", r.insertCalls)
	}
}

// AC: double-click (same draft) → second call is idempotent, no 2nd insert.
func TestApproveFiling_IdempotentDoubleClick(t *testing.T) {
	st := &bndStorage{got: map[string][]byte{"signed-key": []byte("PDFBYTES")}}
	r := &fkRepo{
		fakeRepo:  &fakeRepo{},
		draftByID: &Draft{Status: StatusSigned},
		esaj:      &EsajCredentialEnvelope{ID: "c1", Login: "lav"},
		detail:    &DraftDetailView{SignedPDFKey: "signed-key"},
		inserted:  &FilingAttempt{ID: "a1", Status: StatusEnfileirado},
	}
	uc := newApproveUC(t, r, st)

	res1, err := uc.ApproveFiling(context.Background(), ApproveFilingCommand{TenantID: "t", DraftID: "d", UserID: "u"})
	if err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if res1.IsIdempotent || r.insertCalls != 1 {
		t.Fatalf("first call should insert once (idempotent=%v calls=%d)", res1.IsIdempotent, r.insertCalls)
	}

	r.active = &FilingAttempt{ID: "a1", Status: StatusEnfileirado}
	res2, err := uc.ApproveFiling(context.Background(), ApproveFilingCommand{TenantID: "t", DraftID: "d", UserID: "u"})
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if !res2.IsIdempotent || r.insertCalls != 1 {
		t.Fatalf("second click must be idempotent without re-insert (idempotent=%v calls=%d)", res2.IsIdempotent, r.insertCalls)
	}
}

// AC: unique-violation race on insert → returns the existing active attempt.
func TestApproveFiling_InsertConflictReturnsActive(t *testing.T) {
	st := &bndStorage{got: map[string][]byte{"signed-key": []byte("PDFBYTES")}}
	r := &fkRepo{
		fakeRepo:  &fakeRepo{},
		draftByID: &Draft{Status: StatusSigned},
		esaj:      &EsajCredentialEnvelope{ID: "c1", Login: "lav"},
		detail:    &DraftDetailView{SignedPDFKey: "signed-key"},
		insertErr: ErrFilingAttemptConflict,
		active:    &FilingAttempt{ID: "existing", Status: StatusEnfileirado},
	}
	uc := newApproveUC(t, r, st)
	res, err := uc.ApproveFiling(context.Background(), ApproveFilingCommand{TenantID: "t", DraftID: "d", UserID: "u"})
	if err != nil {
		t.Fatalf("conflict path: %v", err)
	}
	if !res.IsIdempotent || res.FilingAttemptID != "existing" {
		t.Fatalf("should return existing attempt (id=%s idempotent=%v)", res.FilingAttemptID, res.IsIdempotent)
	}
}

// AC: snapshot freezes the SIGNED pdf bytes + sha256 at click time.
func TestApproveFiling_SnapshotFreezesSignedPDF(t *testing.T) {
	signed := []byte("SIGNED-PDF-BYTES")
	st := &bndStorage{got: map[string][]byte{"signed-key": signed}}
	r := &fkRepo{
		fakeRepo:  &fakeRepo{},
		draftByID: &Draft{Status: StatusSigned},
		esaj:      &EsajCredentialEnvelope{ID: "c1", Login: "lav"},
		detail:    &DraftDetailView{SignedPDFKey: "signed-key"},
		inserted:  &FilingAttempt{ID: "a1", Status: StatusEnfileirado},
	}
	uc := newApproveUC(t, r, st)
	if _, err := uc.ApproveFiling(context.Background(), ApproveFilingCommand{TenantID: "t", DraftID: "d", UserID: "u"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// snapshot key written
	var snap []byte
	for k, v := range st.got {
		if k != "signed-key" {
			snap = v
		}
	}
	if string(snap) != string(signed) {
		t.Fatalf("snapshot bytes mismatch: %q", snap)
	}
	want := sha256.Sum256(signed)
	if r.lastSha256 != hex.EncodeToString(want[:]) {
		t.Fatalf("sha256 mismatch: got %s want %s", r.lastSha256, hex.EncodeToString(want[:]))
	}
}

// AC: worker no-op when attempt is not ENFILEIRADO (idempotent under redelivery).
func TestOnFilingEnqueued_NotEnfileiradoNoOp(t *testing.T) {
	g := &fakeGateway{}
	r := &fkRepo{
		fakeRepo:   &fakeRepo{},
		getAttempt: &FilingAttempt{ID: "a1", Status: StatusProtocolado, DraftID: "d1", RequestedBy: "u1", PdfStorageKey: "snap"},
		esaj:       &EsajCredentialEnvelope{ID: "c1", Login: "lav", Ciphertext: []byte("pw")},
	}
	uc := NewFilingUseCase(&fakeUOW{}, r, WithFilingStorage(&bndStorage{got: map[string][]byte{"snap": []byte("p")}}), WithFilingVault(fakeVault{}), WithFilingGateway(g))
	err := uc.OnFilingEnqueued(context.Background(), FilingEnqueued{TenantID: "t", FilingAttemptID: "a1", DraftID: "d1"})
	if err != nil {
		t.Fatalf("no-op should not error: %v", err)
	}
	if g.called {
		t.Fatal("gateway must NOT be called when not ENFILEIRADO")
	}
}

// AC: worker happy path → RPA runs, petition created, PROTOCOLADO.
func TestOnFilingEnqueued_HappyPath(t *testing.T) {
	g := &fakeGateway{num: "202400112345"}
	r := &fkRepo{
		fakeRepo:   &fakeRepo{},
		getAttempt: &FilingAttempt{ID: "a1", Status: StatusEnfileirado, DraftID: "d1", RequestedBy: "u1", PdfStorageKey: "snap"},
		esaj:       &EsajCredentialEnvelope{ID: "c1", Login: "lav", Ciphertext: []byte("pw")},
		draftByID:  &Draft{ID: "d1", IntimationID: "i1", Status: StatusSigned},
		detail:     &DraftDetailView{Process: &ProcessView{CNJNumber: "x"}},
		crID:       "cr1",
		petition:   &Petition{ID: "p1"},
	}
	// second loadAttempt (after MarkFilingProtocolando) must report PROTOCOLANDO
	r.getAttempt.Status = StatusEnfileirado
	st := &bndStorage{got: map[string][]byte{"snap": []byte("pdf")}}
	uc := NewFilingUseCase(&fakeUOW{}, r, WithFilingStorage(st), WithFilingVault(fakeVault{}), WithFilingGateway(g))
	err := uc.OnFilingEnqueued(context.Background(), FilingEnqueued{TenantID: "t", FilingAttemptID: "a1", DraftID: "d1"})
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if !g.called {
		t.Fatal("gateway should have been called")
	}
	if r.protoCalls != 1 {
		t.Fatalf("should mark PROTOCOLADO once (calls=%d)", r.protoCalls)
	}
	if r.markFiledCalls != 1 {
		t.Fatalf("should mark filed once (calls=%d)", r.markFiledCalls)
	}
}

// AC: worker RPA failure → FALHOU, no petition, manual fallback stays available.
func TestOnFilingEnqueued_RPAFailureMarksFailed(t *testing.T) {
	g := &fakeGateway{err: errors.New("captcha")}
	r := &fkRepo{
		fakeRepo:   &fakeRepo{},
		getAttempt: &FilingAttempt{ID: "a1", Status: StatusEnfileirado, DraftID: "d1", RequestedBy: "u1", PdfStorageKey: "snap"},
		esaj:       &EsajCredentialEnvelope{ID: "c1", Login: "lav", Ciphertext: []byte("pw")},
		detail:     &DraftDetailView{},
	}
	st := &bndStorage{got: map[string][]byte{"snap": []byte("pdf")}}
	uc := NewFilingUseCase(&fakeUOW{}, r, WithFilingStorage(st), WithFilingVault(fakeVault{}), WithFilingGateway(g))
	if err := uc.OnFilingEnqueued(context.Background(), FilingEnqueued{TenantID: "t", FilingAttemptID: "a1", DraftID: "d1"}); err != nil {
		t.Fatalf("failure path should ack (nil): %v", err)
	}
	if r.failedCalls != 1 {
		t.Fatalf("should mark FALHOU once (calls=%d)", r.failedCalls)
	}
	if r.protoCalls != 0 {
		t.Fatalf("must not mark PROTOCOLADO on failure")
	}
}
