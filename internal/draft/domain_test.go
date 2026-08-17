package draft

import (
	"context"
	"errors"
	"testing"

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

	// Recorded calls.
	insertCalls      int
	lastInsertedDraft *Draft
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

func (r *fakeRepo) UpdateDraftContent(_ context.Context, _ database.Tx, _, _, _ string, _ *string) (*PatchResult, error) {
	return r.updateResult, r.updateErr
}

func (r *fakeRepo) GetDraftDetail(_ context.Context, _ database.Tx, _, _ string) (*DraftDetailView, error) {
	return r.detailResult, r.detailErr
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
		name        string
		cmd         CreateCommand
		repo        *fakeRepo
		wantNew     bool
		wantErr     bool
		errTarget   error
		wantPiece   string
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

