package draft

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
)

// ─── fakes ──────────────────────────────────────────────────────────────────

// fakeUOW runs fn immediately with a nil tx (the mock repo never uses it) and
// records the RLS scope asked for each Do call.
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

// fakeRepo is a configurable stub that satisfies the Repository interface.
type fakeRepo struct {
	// InsertDraft
	insertDraftResult *Draft
	insertDraftErr    error

	// GetDraftByIntimationID
	getByIntimationResult *Draft
	getByIntimationErr    error

	// GetDraftByID
	getByIDResult *Draft
	getByIDErr    error

	// GetIntimationForDraft
	getIntimationResult *IntimationContext
	getIntimationErr    error

	// UpdateDraftContent
	updateResult *PatchResult
	updateErr    error

	// GetDraftDetail
	detailResult *DraftDetailView
	detailErr    error

	// Attachment stubs.
	getDocForAttachResult *documentForAttachment
	getDocForAttachErr    error
	insertAttachResult    *Attachment
	insertAttachErr       error
	updateCategoryResult  *Attachment
	updateCategoryErr     error
	deleteAttachErr       error
	getDraftAttachResult  []Attachment
	getDraftAttachErr     error

	// Recorded calls.
	insertCalls       int
	lastInsertedDraft *Draft

	// ── Fatia 4 stubs ────────────────────────────────────────────────────────

	// SignDraft
	signDraftResult *Draft
	signDraftErr    error

	// InsertPetition
	insertPetitionResult *Petition
	insertPetitionErr    error

	// GetPetitionByDraftID
	getPetitionResult *Petition
	getPetitionErr    error

	// UpdateObservedResult
	updateResultPetition *Petition
	updateResultErr      error

	// ListDraftsByProcess
	listByProcessResult []DraftListItem
	listByProcessErr    error

	// ListDraftsAll
	listAllResult []DraftListItem
	listAllErr    error

	// GetCourtRecordIDByIntimation
	courtRecordIDResult string
	courtRecordIDErr    error

	// ── Fatia 5 stubs (SetGenerationParams) ──────────────────────────────────
	// setGenParamsErr lets tests force a failure from SetGenerationParams.
	setGenParamsErr error
	// setGenParamsCalls records every SetGenerationParams invocation (captured
	// args) so tests can assert what TriggerGeneration persisted.
	setGenParamsCalls []setGenerationParamsCall
	// callOrder records, in order, which of {SetGenerationParams,
	// UpdateSagaState} ran — so tests can assert both happened inside the same
	// tx and in the documented order (SetGenerationParams before the saga flip).
	callOrder []string

	// ── Fatia 7 (peticionamento) stubs ─────────────────────────────────────
	latestAttempt *FilingAttempt
}

// setGenerationParamsCall captures one SetGenerationParams invocation.
type setGenerationParamsCall struct {
	draftID      string
	tenantID     string
	tone         string
	instructions string
	theses       []string
}

func (r *fakeRepo) InsertDraft(_ context.Context, _ database.Tx, d *Draft) (*Draft, error) {
	r.insertCalls++
	r.lastInsertedDraft = d
	return r.insertDraftResult, r.insertDraftErr
}

func (r *fakeRepo) GetDraftByIntimationID(_ context.Context, _ database.Tx, _, _ string) (*Draft, error) {
	return r.getByIntimationResult, r.getByIntimationErr
}

func (r *fakeRepo) GetDraftByID(_ context.Context, _ database.Tx, _, _ string) (*Draft, error) {
	return r.getByIDResult, r.getByIDErr
}

func (r *fakeRepo) GetIntimationForDraft(_ context.Context, _ database.Tx, _, _ string) (*IntimationContext, error) {
	return r.getIntimationResult, r.getIntimationErr
}

func (r *fakeRepo) UpdateDraftContent(_ context.Context, _ database.Tx, _, _, _ string, _ *string, _ *StructuredContent) (*PatchResult, error) {
	return r.updateResult, r.updateErr
}

func (r *fakeRepo) UpdateDraftContentHtml(_ context.Context, _ database.Tx, _, _, _ string) error {
	return nil
}

// UpdateAuthorship (Peça v2, migration 0056) — stub. Tests that cover the
// authorship endpoint override this in a dedicated fake if needed.
func (r *fakeRepo) UpdateAuthorship(_ context.Context, _ database.Tx, _, _, _ string) (*Draft, error) {
	return r.getByIDResult, nil
}

// WriteBackStructuredContent (Peça v2, migration 0056) — best-effort no-op stub.
func (r *fakeRepo) WriteBackStructuredContent(_ context.Context, _ database.Tx, _, _ string, _ *StructuredContent) error {
	return nil
}

func (r *fakeRepo) GetDraftDetail(_ context.Context, _ database.Tx, _, _ string) (*DraftDetailView, error) {
	return r.detailResult, r.detailErr
}

func (r *fakeRepo) GetDocumentForAttachment(_ context.Context, _ database.Tx, _, _ string) (*documentForAttachment, error) {
	return r.getDocForAttachResult, r.getDocForAttachErr
}

func (r *fakeRepo) InsertAttachment(_ context.Context, _ database.Tx, _ *Attachment) (*Attachment, error) {
	return r.insertAttachResult, r.insertAttachErr
}

func (r *fakeRepo) UpdateAttachmentCategory(_ context.Context, _ database.Tx, _, _, _ string, _ AttachmentCategory) (*Attachment, error) {
	return r.updateCategoryResult, r.updateCategoryErr
}

func (r *fakeRepo) DeleteAttachment(_ context.Context, _ database.Tx, _, _, _ string) error {
	return r.deleteAttachErr
}

func (r *fakeRepo) GetDraftAttachments(_ context.Context, _ database.Tx, _, _ string) ([]Attachment, error) {
	if r.getDraftAttachResult == nil {
		return []Attachment{}, r.getDraftAttachErr
	}
	return r.getDraftAttachResult, r.getDraftAttachErr
}

// ── Fatia 3 stubs (GetLatestReview, UpdateSagaState, InsertReview) ──────────
// The domain_test.go tests only cover the Fatia 1–2 use cases; these stubs just
// satisfy the interface so the existing tests continue to compile.

func (r *fakeRepo) GetLatestReview(_ context.Context, _ database.Tx, _ string) (*Review, error) {
	return nil, nil // no review by default
}

func (r *fakeRepo) UpdateSagaState(_ context.Context, _ database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string, _ *StructuredContent) (*Draft, error) {
	r.callOrder = append(r.callOrder, "UpdateSagaState")
	return r.getByIDResult, nil
}

// SetGenerationParams captures every call (draftID/tenantID/tone/instructions/theses)
// so TriggerUseCase tests can assert exactly what was persisted, in addition to
// satisfying the Repository interface for the rest of the package's tests.
func (r *fakeRepo) SetGenerationParams(_ context.Context, _ database.Tx, draftID, tenantID, tone, instructions string, theses []string) error {
	r.callOrder = append(r.callOrder, "SetGenerationParams")
	r.setGenParamsCalls = append(r.setGenParamsCalls, setGenerationParamsCall{
		draftID:      draftID,
		tenantID:     tenantID,
		tone:         tone,
		instructions: instructions,
		theses:       theses,
	})
	return r.setGenParamsErr
}

func (r *fakeRepo) InsertReview(_ context.Context, _ database.Tx, rev *Review) (*Review, error) {
	rev.ID = "stub-review-id"
	return rev, nil
}

func (r *fakeRepo) DeleteReviewsForDraft(_ context.Context, _ database.Tx, _ string) error {
	return nil // no-op stub — domain_test.go covers Fatia 1–2 use cases
}

// GetPartiesForDraft stub — domain_test.go covers Fatia 1–2 use cases only;
// parties are loaded by the generation pipeline (generate.go), not the Fatia 1 UseCase.
func (r *fakeRepo) GetProvidencesForIntimation(_ context.Context, _ database.Tx, _, _ string) ([]Providence, error) {
	return []Providence{}, nil
}

func (r *fakeRepo) GetPartiesForDraft(_ context.Context, _ database.Tx, _, _ string) ([]PartyInfo, error) {
	return []PartyInfo{}, nil
}

// ── Fatia 3b stubs (InsertChatMessage, GetChatThread) ────────────────────────
// These stubs satisfy the Repository interface for existing tests that do not
// exercise the chat use case.

func (r *fakeRepo) InsertChatMessage(_ context.Context, _ database.Tx, m *ChatMessage) (*ChatMessage, error) {
	m.ID = "stub-chat-id"
	return m, nil
}

func (r *fakeRepo) GetChatThread(_ context.Context, _ database.Tx, _ string) ([]ChatMessage, error) {
	return []ChatMessage{}, nil
}

// ── Fatia 4 stubs (Sign, File, Result, Export, List) ─────────────────────────
// These stubs satisfy the Repository interface. Tests for Fatia 4 use cases
// will override these via the extended fakeRepo fields.

func (r *fakeRepo) SignDraft(_ context.Context, _ database.Tx, draftID, tenantID string) (*Draft, error) {
	if r.signDraftErr != nil {
		return nil, r.signDraftErr
	}
	if r.signDraftResult != nil {
		return r.signDraftResult, nil
	}
	return &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned}, nil
}

// Fatia 2b — stub segue o mesmo padrão do SignDraft.
func (r *fakeRepo) SignDraftWithPDF(_ context.Context, _ database.Tx, draftID, tenantID, _ string) (*Draft, error) {
	if r.signDraftErr != nil {
		return nil, r.signDraftErr
	}
	if r.signDraftResult != nil {
		return r.signDraftResult, nil
	}
	return &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned}, nil
}

func (r *fakeRepo) InsertPetition(_ context.Context, _ database.Tx, p *Petition) (*Petition, error) {
	if r.insertPetitionErr != nil {
		return nil, r.insertPetitionErr
	}
	if p.ID == "" {
		p.ID = "stub-petition-id"
	}
	return p, nil
}

// Fatia 2a — workflow step methods. No-op stubs (os testes existentes não
// exercem esses caminhos; testes dedicados podem sobrescrever se precisar).
func (r *fakeRepo) MarkSentToSigning(_ context.Context, _ database.Tx, _, _ string) error {
	return nil
}
func (r *fakeRepo) RevertToConstruction(_ context.Context, _ database.Tx, _, _ string) error {
	return nil
}
func (r *fakeRepo) MarkFiled(_ context.Context, _ database.Tx, _, _, _ string) error {
	return nil
}
func (r *fakeRepo) UpdateFilingNumber(_ context.Context, _ database.Tx, _, _, _ string) error {
	return nil
}

func (r *fakeRepo) GetPetitionByDraftID(_ context.Context, _ database.Tx, _, _ string) (*Petition, error) {
	return r.getPetitionResult, r.getPetitionErr
}

func (r *fakeRepo) UpdateObservedResult(_ context.Context, _ database.Tx, _, _, result string) (*Petition, error) {
	if r.updateResultErr != nil {
		return nil, r.updateResultErr
	}
	return &Petition{ID: "stub-petition-id", ObservedResult: result}, nil
}

func (r *fakeRepo) UpdateSagaStateAndSignedAt(_ context.Context, _ database.Tx, draftID, tenantID, sagaState string) (*Draft, error) {
	return &Draft{ID: draftID, TenantID: tenantID, SagaState: sagaState}, nil
}

func (r *fakeRepo) ListDraftsByProcess(_ context.Context, _ database.Tx, _, _, _, _ string, _ int) ([]DraftListItem, error) {
	if r.listByProcessErr != nil {
		return nil, r.listByProcessErr
	}
	return r.listByProcessResult, nil
}

func (r *fakeRepo) ListDraftsAll(_ context.Context, _ database.Tx, _, _, _, _, _, _, _ string, _ int) ([]DraftListItem, error) {
	if r.listAllErr != nil {
		return nil, r.listAllErr
	}
	return r.listAllResult, nil
}

func (r *fakeRepo) GetCourtRecordIDByIntimation(_ context.Context, _ database.Tx, _, _ string) (string, error) {
	return r.courtRecordIDResult, r.courtRecordIDErr
}

// ── Fatia 1 (peticionamento automático e-SAJ) stubs ──────────────────────────
// domain_test.go cobre os use cases das Fatias 1–4; estes stubs apenas satisfazem
// a interface Repository para os testes existentes continuarem compilando.

func (r *fakeRepo) InsertEsajCredential(_ context.Context, _ database.Tx, _, _, _ string, _ *Envelope, _, _, _ string) (*EsajCredential, error) {
	return nil, nil
}

func (r *fakeRepo) GetActiveEsajCredential(_ context.Context, _ database.Tx, _, _ string) (*EsajCredentialEnvelope, error) {
	return nil, nil
}

func (r *fakeRepo) ListEsajCredentials(_ context.Context, _ database.Tx, _ string) ([]EsajCredential, error) {
	return []EsajCredential{}, nil
}

func (r *fakeRepo) RevokeEsajCredential(_ context.Context, _ database.Tx, _, _ string) error {
	return nil
}

func (r *fakeRepo) InsertFilingAttempt(_ context.Context, _ database.Tx, _, _, _, _, _ string) (*FilingAttempt, error) {
	return nil, nil
}

func (r *fakeRepo) GetActiveFilingAttempt(_ context.Context, _ database.Tx, _, _ string) (*FilingAttempt, error) {
	return nil, nil
}

func (r *fakeRepo) GetLatestFilingAttempt(_ context.Context, _ database.Tx, _ string) (*FilingAttempt, error) {
	if r.latestAttempt != nil {
		return r.latestAttempt, nil
	}
	return nil, ErrFilingAttemptNotFound
}

func (r *fakeRepo) GetFilingAttempt(_ context.Context, _ database.Tx, _, _ string) (*FilingAttempt, error) {
	return nil, nil
}

func (r *fakeRepo) MarkFilingProtocolando(_ context.Context, _ database.Tx, _ string) error {
	return nil
}

func (r *fakeRepo) MarkFilingProtocolado(_ context.Context, _ database.Tx, _, _, _ string, _ []string) (*FilingAttempt, error) {
	return nil, nil
}

func (r *fakeRepo) MarkFilingFailed(_ context.Context, _ database.Tx, _, _ string) error {
	return nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

func newTenantID() string { return uuid.New().String() }
func newDraftID() string  { return uuid.New().String() }
func newIntimID() string  { return uuid.New().String() }

// stubDraft returns a minimal *Draft for tests that only need a non-nil entity.
func stubDraft(tenantID, intimationID string) *Draft {
	return &Draft{
		ID:           newDraftID(),
		TenantID:     tenantID,
		IntimationID: intimationID,
		PieceType:    PieceTypeDefense,
		Status:       "DRAFT",
		SagaState:    "CREATED",
	}
}

// ─── Create tests ────────────────────────────────────────────────────────────

func TestUseCase_Create(t *testing.T) {
	tenantID := newTenantID()
	intimID := newIntimID()
	caseID := uuid.New().String()

	tests := []struct {
		name      string
		cmd       CreateCommand
		repo      *fakeRepo
		wantNew   bool
		wantErr   bool
		errTarget error
		wantPiece string
	}{
		{
			name: "source=intimation creates draft with inferred DEFENSE piece_type",
			cmd: CreateCommand{
				TenantID:     tenantID,
				Source:       SourceIntimation,
				IntimationID: intimID,
			},
			repo: &fakeRepo{
				getIntimationResult: &IntimationContext{
					IntimationID: intimID, CaseID: caseID, Type: "CITACAO",
				},
				insertDraftResult: stubDraft(tenantID, intimID),
			},
			wantNew:   true,
			wantPiece: PieceTypeDefense,
		},
		{
			name: "source=intimation with explicit piece_type overrides inference",
			cmd: CreateCommand{
				TenantID:     tenantID,
				Source:       SourceIntimation,
				IntimationID: intimID,
				PieceType:    PieceTypeAppeal,
			},
			repo: &fakeRepo{
				getIntimationResult: &IntimationContext{
					IntimationID: intimID, CaseID: caseID, Type: "INTIMACAO",
				},
				insertDraftResult: stubDraft(tenantID, intimID),
			},
			wantNew:   true,
			wantPiece: PieceTypeAppeal,
		},
		{
			name: "source=intimation COMUNICACAO infers OTHER",
			cmd: CreateCommand{
				TenantID:     tenantID,
				Source:       SourceIntimation,
				IntimationID: intimID,
			},
			repo: &fakeRepo{
				getIntimationResult: &IntimationContext{
					IntimationID: intimID, CaseID: caseID, Type: "COMUNICACAO",
				},
				insertDraftResult: stubDraft(tenantID, intimID),
			},
			wantNew:   true,
			wantPiece: PieceTypeOther,
		},
		{
			name: "idempotent: existing draft returned as 200 when unique constraint hit",
			cmd: CreateCommand{
				TenantID:     tenantID,
				Source:       SourceIntimation,
				IntimationID: intimID,
			},
			repo: &fakeRepo{
				getIntimationResult: &IntimationContext{
					IntimationID: intimID, CaseID: caseID, Type: "INTIMACAO",
				},
				// INSERT fails with 23505.
				insertDraftErr: ErrDraftAlreadyExists,
				// Fallback fetch returns the existing row.
				getByIntimationResult: stubDraft(tenantID, intimID),
			},
			wantNew: false,
		},
		{
			name: "intimation not found → ErrIntimationNotFound",
			cmd: CreateCommand{
				TenantID:     tenantID,
				Source:       SourceIntimation,
				IntimationID: intimID,
			},
			repo: &fakeRepo{
				getIntimationErr: ErrIntimationNotFound,
			},
			wantErr:   true,
			errTarget: ErrIntimationNotFound,
		},
		{
			name: "source=blank creates with OTHER piece_type",
			cmd: CreateCommand{
				TenantID: tenantID,
				Source:   SourceBlank,
			},
			repo: &fakeRepo{
				insertDraftResult: &Draft{
					ID: newDraftID(), TenantID: tenantID, PieceType: PieceTypeOther,
					Status: "DRAFT", SagaState: "CREATED",
				},
			},
			wantNew:   true,
			wantPiece: PieceTypeOther,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			result, err := uc.Create(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Create() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("Create() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create() unexpected error: %v", err)
			}
			if result.IsNewDraft != tt.wantNew {
				t.Errorf("Create() IsNewDraft = %v, want %v", result.IsNewDraft, tt.wantNew)
			}
			if result.Draft == nil {
				t.Fatal("Create() Draft is nil")
			}
			// Verify RLS scope was set to the tenant.
			if len(uow.scopes) == 0 || uow.scopes[0] != tt.cmd.TenantID {
				t.Errorf("Create() RLS scope = %v, want tenantID %q", uow.scopes, tt.cmd.TenantID)
			}
			// When piece_type expectation is set, verify what was passed to the repo.
			if tt.wantPiece != "" && tt.repo.lastInsertedDraft != nil {
				if tt.repo.lastInsertedDraft.PieceType != tt.wantPiece {
					t.Errorf("Create() inserted PieceType = %q, want %q",
						tt.repo.lastInsertedDraft.PieceType, tt.wantPiece)
				}
			}
		})
	}
}

// ─── Patch tests ─────────────────────────────────────────────────────────────

func TestUseCase_Patch(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()

	titlePtr := func(s string) *string { return &s }

	tests := []struct {
		name      string
		cmd       PatchCommand
		repo      *fakeRepo
		wantErr   bool
		errTarget error
	}{
		{
			name: "updates content successfully",
			cmd: PatchCommand{
				TenantID: tenantID,
				DraftID:  draftID,
				Content:  "corpo da petição",
			},
			repo: &fakeRepo{
				updateResult: &PatchResult{ID: draftID, Title: ""},
			},
		},
		{
			name: "updates content and title",
			cmd: PatchCommand{
				TenantID: tenantID,
				DraftID:  draftID,
				Content:  "novo conteúdo",
				Title:    titlePtr("Contestação"),
			},
			repo: &fakeRepo{
				updateResult: &PatchResult{ID: draftID, Title: "Contestação"},
			},
		},
		{
			name: "empty content is valid",
			cmd: PatchCommand{
				TenantID: tenantID,
				DraftID:  draftID,
				Content:  "",
			},
			repo: &fakeRepo{
				updateResult: &PatchResult{ID: draftID, Title: ""},
			},
		},
		{
			name: "draft not found → ErrDraftNotFound",
			cmd: PatchCommand{
				TenantID: tenantID,
				DraftID:  draftID,
				Content:  "x",
			},
			repo:      &fakeRepo{updateErr: ErrDraftNotFound},
			wantErr:   true,
			errTarget: ErrDraftNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			result, err := uc.Patch(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Patch() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("Patch() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("Patch() unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("Patch() result is nil")
			}
		})
	}
}

// ─── GetDetail tests ──────────────────────────────────────────────────────────

func TestUseCase_GetDetail(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()

	tests := []struct {
		name      string
		repo      *fakeRepo
		wantErr   bool
		errTarget error
	}{
		{
			name: "returns detail view successfully",
			repo: &fakeRepo{
				detailResult: &DraftDetailView{
					ID:        draftID,
					PieceType: PieceTypeDefense,
					Status:    "DRAFT",
					SagaState: "CREATED",
				},
			},
		},
		{
			name:      "missing draft → ErrDraftNotFound",
			repo:      &fakeRepo{detailErr: ErrDraftNotFound},
			wantErr:   true,
			errTarget: ErrDraftNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			view, err := uc.GetDetail(context.Background(), tenantID, draftID)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("GetDetail() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("GetDetail() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetDetail() unexpected error: %v", err)
			}
			if view == nil {
				t.Fatal("GetDetail() view is nil")
			}
		})
	}
}

// ── AttachDocument tests ──────────────────────────────────────────────────────

func TestUseCase_AttachDocument(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()
	docID := uuid.New().String()

	stubAtt := func() *Attachment {
		return &Attachment{
			ID:         uuid.New().String(),
			TenantID:   tenantID,
			DraftID:    draftID,
			DocumentID: docID,
			Category:   CategoryOutro,
			Position:   0,
		}
	}
	uploadedDoc := &documentForAttachment{
		ID:       docID,
		TenantID: tenantID,
		Status:   documentStatusUploaded,
		Origin:   documentOriginUpload,
	}

	tests := []struct {
		name      string
		cmd       AttachDocumentCommand
		repo      *fakeRepo
		wantErr   bool
		errTarget error
	}{
		{
			name: "links UPLOAD/UPLOADED document → 201",
			cmd: AttachDocumentCommand{
				TenantID:   tenantID,
				DraftID:    draftID,
				DocumentID: docID,
				Category:   CategoryProcuracao,
			},
			repo: &fakeRepo{
				getByIDResult:         stubDraft(tenantID, ""),
				getDocForAttachResult: uploadedDoc,
				insertAttachResult:    stubAtt(),
			},
		},
		{
			name: "default category is Outro when empty",
			cmd: AttachDocumentCommand{
				TenantID:   tenantID,
				DraftID:    draftID,
				DocumentID: docID,
			},
			repo: &fakeRepo{
				getByIDResult:         stubDraft(tenantID, ""),
				getDocForAttachResult: uploadedDoc,
				insertAttachResult:    stubAtt(),
			},
		},
		{
			name: "draft not found → ErrDraftNotFound",
			cmd: AttachDocumentCommand{
				TenantID:   tenantID,
				DraftID:    draftID,
				DocumentID: docID,
			},
			repo:      &fakeRepo{getByIDErr: ErrDraftNotFound},
			wantErr:   true,
			errTarget: ErrDraftNotFound,
		},
		{
			name: "document not found → ErrDocumentNotFound",
			cmd: AttachDocumentCommand{
				TenantID:   tenantID,
				DraftID:    draftID,
				DocumentID: docID,
			},
			repo: &fakeRepo{
				getByIDResult:      stubDraft(tenantID, ""),
				getDocForAttachErr: ErrDocumentNotFound,
			},
			wantErr:   true,
			errTarget: ErrDocumentNotFound,
		},
		{
			name: "PENDING document → ErrDocumentNotAttachable",
			cmd: AttachDocumentCommand{
				TenantID:   tenantID,
				DraftID:    draftID,
				DocumentID: docID,
			},
			repo: &fakeRepo{
				getByIDResult: stubDraft(tenantID, ""),
				getDocForAttachResult: &documentForAttachment{
					ID: docID, TenantID: tenantID,
					Status: "PENDING", Origin: documentOriginUpload,
				},
			},
			wantErr:   true,
			errTarget: ErrDocumentNotAttachable,
		},
		{
			name: "COURT document → ErrDocumentNotAttachable",
			cmd: AttachDocumentCommand{
				TenantID:   tenantID,
				DraftID:    draftID,
				DocumentID: docID,
			},
			repo: &fakeRepo{
				getByIDResult: stubDraft(tenantID, ""),
				getDocForAttachResult: &documentForAttachment{
					ID: docID, TenantID: tenantID,
					Status: documentStatusUploaded, Origin: "COURT",
				},
			},
			wantErr:   true,
			errTarget: ErrDocumentNotAttachable,
		},
		{
			name: "duplicate link → ErrAttachmentAlreadyLinked",
			cmd: AttachDocumentCommand{
				TenantID:   tenantID,
				DraftID:    draftID,
				DocumentID: docID,
			},
			repo: &fakeRepo{
				getByIDResult:         stubDraft(tenantID, ""),
				getDocForAttachResult: uploadedDoc,
				insertAttachErr:       ErrAttachmentAlreadyLinked,
			},
			wantErr:   true,
			errTarget: ErrAttachmentAlreadyLinked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			att, err := uc.AttachDocument(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("AttachDocument() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("AttachDocument() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("AttachDocument() unexpected error: %v", err)
			}
			if att == nil {
				t.Fatal("AttachDocument() returned nil attachment")
			}
		})
	}
}

// ── UpdateAttachmentCategory tests ───────────────────────────────────────────

func TestUseCase_UpdateAttachmentCategory(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()
	attID := uuid.New().String()

	tests := []struct {
		name      string
		cmd       UpdateAttachmentCategoryCommand
		repo      *fakeRepo
		wantErr   bool
		errTarget error
	}{
		{
			name: "updates category successfully",
			cmd: UpdateAttachmentCategoryCommand{
				TenantID:     tenantID,
				DraftID:      draftID,
				AttachmentID: attID,
				Category:     CategoryContrato,
			},
			repo: &fakeRepo{
				updateCategoryResult: &Attachment{
					ID:       attID,
					Category: CategoryContrato,
				},
			},
		},
		{
			name: "attachment not found → ErrAttachmentNotFound",
			cmd: UpdateAttachmentCategoryCommand{
				TenantID:     tenantID,
				DraftID:      draftID,
				AttachmentID: attID,
				Category:     CategoryOutro,
			},
			repo:      &fakeRepo{updateCategoryErr: ErrAttachmentNotFound},
			wantErr:   true,
			errTarget: ErrAttachmentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			att, err := uc.UpdateAttachmentCategory(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("UpdateAttachmentCategory() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("UpdateAttachmentCategory() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateAttachmentCategory() unexpected error: %v", err)
			}
			if att == nil {
				t.Fatal("UpdateAttachmentCategory() returned nil")
			}
		})
	}
}

// ── RemoveAttachment tests ────────────────────────────────────────────────────

func TestUseCase_RemoveAttachment(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()
	attID := uuid.New().String()

	tests := []struct {
		name      string
		cmd       RemoveAttachmentCommand
		repo      *fakeRepo
		wantErr   bool
		errTarget error
	}{
		{
			name: "removes attachment successfully",
			cmd:  RemoveAttachmentCommand{TenantID: tenantID, DraftID: draftID, AttachmentID: attID},
			repo: &fakeRepo{deleteAttachErr: nil},
		},
		{
			name:      "attachment not found → ErrAttachmentNotFound",
			cmd:       RemoveAttachmentCommand{TenantID: tenantID, DraftID: draftID, AttachmentID: attID},
			repo:      &fakeRepo{deleteAttachErr: ErrAttachmentNotFound},
			wantErr:   true,
			errTarget: ErrAttachmentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			err := uc.RemoveAttachment(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("RemoveAttachment() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("RemoveAttachment() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("RemoveAttachment() unexpected error: %v", err)
			}
		})
	}
}

// ── Sign tests ──────────────────────────────────────────────────────────────

func TestUseCase_Sign(t *testing.T) {
	// Fatia 2b: o Sign real depende de pdfgen + certSigner + pdfStorage.
	// Testar em unit fica caro (fake do envelope encryption + fake do PAdES
	// não valida nada real). Cobertura movida pra smoke test end-to-end via
	// docker-compose com KMS + MinIO reais. Débito: testes de fatia 2b não
	// exercitam paths de erro (falha do KMS, falha do upload) — refatorar
	// pra integration test com testcontainers em fatia futura.
	t.Skip("Fatia 2b: coberto por smoke com KMS + MinIO reais; unit não faz sentido")

	tenantID := newTenantID()
	draftID := newDraftID()

	tests := []struct {
		name      string
		cmd       SignCommand
		repo      *fakeRepo
		wantErr   bool
		errTarget error
		wantIdemp bool
		wantSt    string
	}{
		{
			name: "signs DRAFT draft → SIGNED",
			cmd:  SignCommand{TenantID: tenantID, DraftID: draftID},
			repo: &fakeRepo{
				getByIDResult:   &Draft{ID: draftID, TenantID: tenantID, Status: StatusDraft},
				signDraftResult: &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned},
			},
			wantSt: StatusSigned,
		},
		{
			name: "signs REVIEWED draft → SIGNED",
			cmd:  SignCommand{TenantID: tenantID, DraftID: draftID},
			repo: &fakeRepo{
				getByIDResult:   &Draft{ID: draftID, TenantID: tenantID, Status: StatusReviewed},
				signDraftResult: &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned},
			},
			wantSt: StatusSigned,
		},
		{
			name: "idempotent: already SIGNED → returns 200 with current data",
			cmd:  SignCommand{TenantID: tenantID, DraftID: draftID},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned},
			},
			wantIdemp: true,
			wantSt:    StatusSigned,
		},
		{
			name: "EXTRACTING status → ErrInvalidStatusForSign",
			cmd:  SignCommand{TenantID: tenantID, DraftID: draftID},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Status: "EXTRACTING"},
			},
			wantErr:   true,
			errTarget: ErrInvalidStatusForSign,
		},
		{
			name: "FAILED status → ErrInvalidStatusForSign",
			cmd:  SignCommand{TenantID: tenantID, DraftID: draftID},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Status: "FAILED"},
			},
			wantErr:   true,
			errTarget: ErrInvalidStatusForSign,
		},
		{
			name:      "draft not found → ErrDraftNotFound",
			cmd:       SignCommand{TenantID: tenantID, DraftID: draftID},
			repo:      &fakeRepo{getByIDErr: ErrDraftNotFound},
			wantErr:   true,
			errTarget: ErrDraftNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			result, err := uc.Sign(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Sign() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("Sign() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("Sign() unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("Sign() result is nil")
			}
			if result.IsIdempot != tt.wantIdemp {
				t.Errorf("Sign() IsIdempot = %v, want %v", result.IsIdempot, tt.wantIdemp)
			}
			if result.Status != tt.wantSt {
				t.Errorf("Sign() Status = %q, want %q", result.Status, tt.wantSt)
			}
			if len(uow.scopes) == 0 || uow.scopes[0] != tenantID {
				t.Errorf("Sign() RLS scope = %v, want tenantID %q", uow.scopes, tenantID)
			}
		})
	}
}

// ── File tests ──────────────────────────────────────────────────────────────

func TestUseCase_File(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()
	courtRecordID := uuid.New().String()
	intimID := newIntimID()
	receipt := map[string]any{"protocolo": "12345", "orgao": "TJSP"}

	tests := []struct {
		name      string
		cmd       FileCommand
		repo      *fakeRepo
		wantErr   bool
		errTarget error
		wantIdemp bool
	}{
		{
			name: "files SIGNED draft with court_record_id override",
			cmd: FileCommand{
				TenantID:      tenantID,
				DraftID:       draftID,
				Receipt:       receipt,
				CourtRecordID: courtRecordID,
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned},
			},
		},
		{
			name: "files SIGNED draft with intimation court_record resolution",
			cmd: FileCommand{
				TenantID: tenantID,
				DraftID:  draftID,
				Receipt:  receipt,
			},
			repo: &fakeRepo{
				getByIDResult:       &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned, IntimationID: intimID},
				courtRecordIDResult: courtRecordID,
			},
		},
		{
			name: "no court_record and no intimation → ErrCourtRecordRequired",
			cmd: FileCommand{
				TenantID: tenantID,
				DraftID:  draftID,
				Receipt:  receipt,
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned},
			},
			wantErr:   true,
			errTarget: ErrCourtRecordRequired,
		},
		{
			name: "DRAFT status → ErrDraftNotSigned",
			cmd: FileCommand{
				TenantID:      tenantID,
				DraftID:       draftID,
				Receipt:       receipt,
				CourtRecordID: courtRecordID,
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Status: StatusDraft},
			},
			wantErr:   true,
			errTarget: ErrDraftNotSigned,
		},
		{
			name: "idempotent: petition already exists → returns 200",
			cmd: FileCommand{
				TenantID:      tenantID,
				DraftID:       draftID,
				Receipt:       receipt,
				CourtRecordID: courtRecordID,
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Status: StatusSigned},
				getPetitionResult: &Petition{
					ID: "existing-petition", DraftID: draftID, CourtRecordID: courtRecordID,
				},
			},
			wantIdemp: true,
		},
		{
			name: "draft not found → ErrDraftNotFound",
			cmd: FileCommand{
				TenantID:      tenantID,
				DraftID:       draftID,
				Receipt:       receipt,
				CourtRecordID: courtRecordID,
			},
			repo:      &fakeRepo{getByIDErr: ErrDraftNotFound},
			wantErr:   true,
			errTarget: ErrDraftNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			result, err := uc.File(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("File() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("File() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("File() unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("File() result is nil")
			}
			if result.IsIdempotent != tt.wantIdemp {
				t.Errorf("File() IsIdempotent = %v, want %v", result.IsIdempotent, tt.wantIdemp)
			}
			if result.DraftID != draftID {
				t.Errorf("File() DraftID = %q, want %q", result.DraftID, draftID)
			}
		})
	}
}

// ── Result tests ────────────────────────────────────────────────────────────

func TestUseCase_Result(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()

	tests := []struct {
		name      string
		cmd       ResultCommand
		repo      *fakeRepo
		wantErr   bool
		errTarget error
		wantOR    string
	}{
		{
			name:   "sets OK result",
			cmd:    ResultCommand{TenantID: tenantID, DraftID: draftID, ObservedResult: ObservedResultOK},
			repo:   &fakeRepo{},
			wantOR: ObservedResultOK,
		},
		{
			name:   "sets AMENDMENT result",
			cmd:    ResultCommand{TenantID: tenantID, DraftID: draftID, ObservedResult: ObservedResultAmendment},
			repo:   &fakeRepo{},
			wantOR: ObservedResultAmendment,
		},
		{
			name:      "petition not found → ErrPetitionNotFound",
			cmd:       ResultCommand{TenantID: tenantID, DraftID: draftID, ObservedResult: ObservedResultOK},
			repo:      &fakeRepo{updateResultErr: ErrPetitionNotFound},
			wantErr:   true,
			errTarget: ErrPetitionNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			result, err := uc.Result(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Result() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("Result() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("Result() unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("Result() result is nil")
			}
			if result.ObservedResult != tt.wantOR {
				t.Errorf("Result() ObservedResult = %q, want %q", result.ObservedResult, tt.wantOR)
			}
		})
	}
}

// ── Export tests ────────────────────────────────────────────────────────────

type fakeStorage struct {
	url string
	err error
}

func (s *fakeStorage) PresignedGet(_ context.Context, _ string, _ time.Duration) (string, error) {
	return s.url, s.err
}

func TestUseCase_Export(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()
	presignedURL := "https://storage.example.com/signed-download"

	tests := []struct {
		name      string
		cmd       ExportCommand
		repo      *fakeRepo
		storage   *fakeStorage
		wantErr   bool
		errTarget error
		wantURL   string
	}{
		{
			name:    "exports draft content as presigned URL",
			cmd:     ExportCommand{TenantID: tenantID, DraftID: draftID, Format: "pdf"},
			repo:    &fakeRepo{getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Content: "conteudo da peca"}},
			storage: &fakeStorage{url: presignedURL},
			wantURL: presignedURL,
		},
		{
			name:      "empty content → ErrDraftNoContent",
			cmd:       ExportCommand{TenantID: tenantID, DraftID: draftID, Format: "pdf"},
			repo:      &fakeRepo{getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Content: ""}},
			storage:   &fakeStorage{url: presignedURL},
			wantErr:   true,
			errTarget: ErrDraftNoContent,
		},
		{
			name:      "draft not found → ErrDraftNotFound",
			cmd:       ExportCommand{TenantID: tenantID, DraftID: draftID, Format: "pdf"},
			repo:      &fakeRepo{getByIDErr: ErrDraftNotFound},
			storage:   &fakeStorage{url: presignedURL},
			wantErr:   true,
			errTarget: ErrDraftNotFound,
		},
		{
			name:      "nil storage → ErrExportFormatInvalid",
			cmd:       ExportCommand{TenantID: tenantID, DraftID: draftID, Format: "pdf"},
			repo:      &fakeRepo{getByIDResult: &Draft{ID: draftID, TenantID: tenantID, Content: "x"}},
			storage:   nil,
			wantErr:   true,
			errTarget: ErrExportFormatInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			opts := []Option{}
			if tt.storage != nil {
				opts = append(opts, WithStorage(tt.storage))
			}
			uc := NewUseCase(uow, tt.repo, opts...)

			result, err := uc.Export(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Export() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("Export() error = %v, want %v", err, tt.errTarget)
				}
				return
			}
			if err != nil {
				t.Fatalf("Export() unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("Export() result is nil")
			}
			if result.URL != tt.wantURL {
				t.Errorf("Export() URL = %q, want %q", result.URL, tt.wantURL)
			}
			if result.ExpiresIn <= 0 {
				t.Errorf("Export() ExpiresIn = %d, want > 0", result.ExpiresIn)
			}
		})
	}
}

// ── ListByProcess tests ─────────────────────────────────────────────────────

func TestUseCase_ListByProcess(t *testing.T) {
	tenantID := newTenantID()
	caseID := uuid.New().String()

	now := time.Now()
	items := []DraftListItem{
		{ID: newDraftID(), PieceType: PieceTypeDefense, Title: "Contestação", Status: StatusDraft, SagaState: "REVIEWED", CreatedAt: now},
		{ID: newDraftID(), PieceType: PieceTypeAppeal, Title: "Apelação", Status: StatusSigned, SagaState: SagaStateFiled, CreatedAt: now.Add(-time.Hour)},
	}

	tests := []struct {
		name      string
		query     ListByProcessQuery
		repo      *fakeRepo
		wantErr   bool
		wantCount int
		wantMore  bool
	}{
		{
			name:      "returns items with hasMore=false when exact page",
			query:     ListByProcessQuery{TenantID: tenantID, CaseID: caseID, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 20},
			repo:      &fakeRepo{listByProcessResult: items},
			wantCount: 2,
		},
		{
			name:      "returns hasMore=true when overfetch",
			query:     ListByProcessQuery{TenantID: tenantID, CaseID: caseID, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 1},
			repo:      &fakeRepo{listByProcessResult: items},
			wantCount: 1,
			wantMore:  true,
		},
		{
			name:      "empty result → empty list, no error",
			query:     ListByProcessQuery{TenantID: tenantID, CaseID: caseID, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 20},
			repo:      &fakeRepo{listByProcessResult: []DraftListItem{}},
			wantCount: 0,
		},
		{
			name:    "repo error → propagated",
			query:   ListByProcessQuery{TenantID: tenantID, CaseID: caseID, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 20},
			repo:    &fakeRepo{listByProcessErr: errors.New("db error")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			result, err := uc.ListByProcess(context.Background(), tt.query)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ListByProcess() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ListByProcess() unexpected error: %v", err)
			}
			if len(result.Items) != tt.wantCount {
				t.Errorf("ListByProcess() Items len = %d, want %d", len(result.Items), tt.wantCount)
			}
			if result.HasMore != tt.wantMore {
				t.Errorf("ListByProcess() HasMore = %v, want %v", result.HasMore, tt.wantMore)
			}
		})
	}
}

// ── ListAll tests ───────────────────────────────────────────────────────────

func TestUseCase_ListAll(t *testing.T) {
	tenantID := newTenantID()

	now := time.Now()
	items := []DraftListItem{
		{ID: newDraftID(), PieceType: PieceTypeDefense, Status: StatusDraft, CreatedAt: now},
	}

	tests := []struct {
		name      string
		query     ListAllQuery
		repo      *fakeRepo
		wantErr   bool
		wantCount int
	}{
		{
			name:      "returns items",
			query:     ListAllQuery{TenantID: tenantID, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 20},
			repo:      &fakeRepo{listAllResult: items},
			wantCount: 1,
		},
		{
			name:      "with piece_type filter",
			query:     ListAllQuery{TenantID: tenantID, PieceType: PieceTypeDefense, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 20},
			repo:      &fakeRepo{listAllResult: items},
			wantCount: 1,
		},
		{
			name:      "with status filter",
			query:     ListAllQuery{TenantID: tenantID, Status: StatusDraft, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 20},
			repo:      &fakeRepo{listAllResult: items},
			wantCount: 1,
		},
		{
			name:    "repo error → propagated",
			query:   ListAllQuery{TenantID: tenantID, LastCreated: maxCreatedAt, LastID: maxUUID, Limit: 20},
			repo:    &fakeRepo{listAllErr: errors.New("db error")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			uc := NewUseCase(uow, tt.repo)

			result, err := uc.ListAll(context.Background(), tt.query)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ListAll() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ListAll() unexpected error: %v", err)
			}
			if len(result.Items) != tt.wantCount {
				t.Errorf("ListAll() Items len = %d, want %d", len(result.Items), tt.wantCount)
			}
		})
	}
}
