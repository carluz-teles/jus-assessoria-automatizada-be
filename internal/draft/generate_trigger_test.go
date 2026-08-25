package draft

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// triggerFakeOutbox records every published event so tests can assert Publish
// was (or was not) called, without touching a real tx/DB — TriggerUseCase now
// depends on the OutboxPublisher port (not the concrete *events.Outbox), which
// is what makes this fake possible.
type triggerFakeOutbox struct {
	published []events.Event
	err       error
}

func (o *triggerFakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	if o.err != nil {
		return o.err
	}
	o.published = append(o.published, ev)
	return nil
}

// ── TriggerGeneration tests ─────────────────────────────────────────────────
// These tests close the HIGH finding from the branch review: no unit test
// previously exercised TriggerUseCase.TriggerGeneration directly, and the
// fakeRepo.SetGenerationParams stub used to be a no-op that ignored its
// arguments. It now captures every call (see domain_test.go), so these tests
// assert the persisted tone/instructions/theses and the tx atomicity with
// UpdateSagaState + outbox.Publish.

func TestTriggerUseCase_TriggerGeneration(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()

	tests := []struct {
		name             string
		cmd              TriggerGenerationCommand
		repo             *fakeRepo
		wantErr          bool
		errTarget        error
		wantTone         string
		wantInstructions string
		wantTheses       []string
	}{
		{
			name: "empty tone defaults to tecnico server-side before persisting",
			cmd: TriggerGenerationCommand{
				TenantID: tenantID,
				DraftID:  draftID,
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, SagaState: "CREATED"},
			},
			wantTone: ToneTecnico,
		},
		{
			name: "non-empty tone/instructions/theses pass through unchanged",
			cmd: TriggerGenerationCommand{
				TenantID:     tenantID,
				DraftID:      draftID,
				Tone:         "coloquial",
				Instructions: "cite jurisprudência recente do STJ",
				Theses:       []string{"tese-prescricao", "tese-decadencia"},
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, SagaState: "DRAFTED"},
			},
			wantTone:         "coloquial",
			wantInstructions: "cite jurisprudência recente do STJ",
			wantTheses:       []string{"tese-prescricao", "tese-decadencia"},
		},
		{
			name: "regeneration from FAILED is allowed and re-persists params",
			cmd: TriggerGenerationCommand{
				TenantID: tenantID,
				DraftID:  draftID,
				Tone:     "narrativo",
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, SagaState: "FAILED"},
			},
			wantTone: "narrativo",
		},
		{
			name: "EXTRACTING saga_state → ErrGenerationInProgress, SetGenerationParams never called",
			cmd: TriggerGenerationCommand{
				TenantID: tenantID,
				DraftID:  draftID,
			},
			repo: &fakeRepo{
				getByIDResult: &Draft{ID: draftID, TenantID: tenantID, SagaState: SagaStateExtracting},
			},
			wantErr:   true,
			errTarget: ErrGenerationInProgress,
		},
		{
			name: "draft not found → propagates error, no writes",
			cmd: TriggerGenerationCommand{
				TenantID: tenantID,
				DraftID:  draftID,
			},
			repo: &fakeRepo{
				getByIDErr: ErrDraftNotFound,
			},
			wantErr:   true,
			errTarget: ErrDraftNotFound,
		},
		{
			name: "SetGenerationParams failure aborts before UpdateSagaState/outbox",
			cmd: TriggerGenerationCommand{
				TenantID: tenantID,
				DraftID:  draftID,
			},
			repo: &fakeRepo{
				getByIDResult:   &Draft{ID: draftID, TenantID: tenantID, SagaState: "CREATED"},
				setGenParamsErr: errors.New("db error"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			ob := &triggerFakeOutbox{}
			uc := NewTriggerUseCase(uow, tt.repo, ob)

			result, err := uc.TriggerGeneration(context.Background(), tt.cmd)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("TriggerGeneration() error = nil, want non-nil")
				}
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("TriggerGeneration() error = %v, want %v", err, tt.errTarget)
				}
				// On any error path, the transaction must not have flipped the
				// saga state nor published draft.generation_requested.
				if len(tt.repo.setGenParamsCalls) > 0 {
					for _, c := range tt.repo.callOrder {
						if c == "UpdateSagaState" {
							t.Error("UpdateSagaState ran despite an earlier error in the same tx")
						}
					}
				}
				if len(ob.published) != 0 {
					t.Errorf("outbox.published = %v, want none on error", ob.published)
				}
				return
			}
			if err != nil {
				t.Fatalf("TriggerGeneration() unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("TriggerGeneration() result is nil")
			}

			// ── assert what was persisted via SetGenerationParams ──────────────
			if len(tt.repo.setGenParamsCalls) != 1 {
				t.Fatalf("SetGenerationParams calls = %d, want 1", len(tt.repo.setGenParamsCalls))
			}
			got := tt.repo.setGenParamsCalls[0]
			if got.draftID != tt.cmd.DraftID {
				t.Errorf("SetGenerationParams draftID = %q, want %q", got.draftID, tt.cmd.DraftID)
			}
			if got.tenantID != tt.cmd.TenantID {
				t.Errorf("SetGenerationParams tenantID = %q, want %q", got.tenantID, tt.cmd.TenantID)
			}
			if got.tone != tt.wantTone {
				t.Errorf("SetGenerationParams tone = %q, want %q", got.tone, tt.wantTone)
			}
			if got.instructions != tt.wantInstructions {
				t.Errorf("SetGenerationParams instructions = %q, want %q", got.instructions, tt.wantInstructions)
			}
			if len(got.theses) != len(tt.wantTheses) {
				t.Fatalf("SetGenerationParams theses = %v, want %v", got.theses, tt.wantTheses)
			}
			for i, th := range tt.wantTheses {
				if got.theses[i] != th {
					t.Errorf("SetGenerationParams theses[%d] = %q, want %q", i, got.theses[i], th)
				}
			}

			// ── assert tx atomicity: SetGenerationParams ran before UpdateSagaState,
			// both inside the SAME uow.Do call (single tenant-scoped tx) ───────────
			if len(tt.repo.callOrder) != 2 || tt.repo.callOrder[0] != "SetGenerationParams" || tt.repo.callOrder[1] != "UpdateSagaState" {
				t.Errorf("call order = %v, want [SetGenerationParams UpdateSagaState]", tt.repo.callOrder)
			}
			if len(uow.scopes) != 1 || uow.scopes[0] != tt.cmd.TenantID {
				t.Errorf("uow.Do scopes = %v, want exactly one call scoped to tenantID %q (single tx)", uow.scopes, tt.cmd.TenantID)
			}

			// ── assert the outbox publish happened in the same tx (fake records
			// unconditionally, proving Publish was reached only on the success path) ──
			if len(ob.published) != 1 {
				t.Fatalf("outbox.published = %d events, want 1", len(ob.published))
			}
			if ob.published[0].Type() != "draft.generation_requested" {
				t.Errorf("published event type = %q, want draft.generation_requested", ob.published[0].Type())
			}
		})
	}
}

// TestTriggerUseCase_TriggerGeneration_OutboxFailureAborts verifies that an
// outbox.Publish failure surfaces as the TriggerGeneration error (the tx must
// roll back atomically — this fake doesn't literally roll back, but proves the
// use case propagates the failure instead of swallowing it).
func TestTriggerUseCase_TriggerGeneration_OutboxFailureAborts(t *testing.T) {
	tenantID := newTenantID()
	draftID := newDraftID()

	repo := &fakeRepo{
		getByIDResult: &Draft{ID: draftID, TenantID: tenantID, SagaState: "CREATED"},
	}
	ob := &triggerFakeOutbox{err: errors.New("outbox insert failed")}
	uow := &fakeUOW{}
	uc := NewTriggerUseCase(uow, repo, ob)

	_, err := uc.TriggerGeneration(context.Background(), TriggerGenerationCommand{
		TenantID: tenantID,
		DraftID:  draftID,
	})

	if err == nil {
		t.Fatal("TriggerGeneration() error = nil, want non-nil when outbox.Publish fails")
	}
	// SetGenerationParams and UpdateSagaState still ran (same tx, sequential
	// writes) — only the final outbox step failed, and the error propagated.
	if len(repo.setGenParamsCalls) != 1 {
		t.Errorf("SetGenerationParams calls = %d, want 1", len(repo.setGenParamsCalls))
	}
}
