//go:build integration

// Draft slice integration tests — prove the Fatia 1 write+read path against a
// REAL Postgres with every migration applied (including 0042_draft_peca_schema).
//
// What the unit tests with a mocked repo CANNOT prove:
//   - the SQL INSERT actually lands in the database with the correct defaults;
//   - the partial unique index draft_intimation_id_uidx enforces the idempotency
//     guarantee at the DB level (23505 → ErrDraftAlreadyExists → 200);
//   - the GetDraftDetail JOIN query stitches intimation + court_record correctly;
//   - the PATCH bumps updated_at beyond the original value;
//   - the tenant-isolation barrier (WHERE tenant_id) returns ErrDraftNotFound for
//     a foreign tenant, never the other tenant's data.
//
// Parent rows (tenant → court_case → court_record → intimation) are COMMITTED
// before the use case runs so its own transaction can reference them via FK.
// Each test uses a fresh uuid tenantID so counts are isolated on the shared DB.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/lib/database"
)

// newDraftUC wires the real draft use case over the pool — real sqlc repo,
// real unit-of-work (tenant-scoped tx + RLS). Mirrors cmd/api composition.
func newDraftUC(pool *pgxpool.Pool) *draft.UseCase {
	return draft.NewUseCase(
		database.NewUnitOfWork(pool),
		draft.NewRepository(),
	)
}

// seedIntimationTyped seeds one intimation with an explicit type (e.g. "CITACAO")
// and returns its id. The existing seedIntimationReturningID omits the type column
// (NULL), which is valid for list tests but causes the piece_type inference to fall
// through to OTHER. This variant populates type so the CITACAO→DEFENSE path is
// exercised end-to-end against the real DB.
func seedIntimationTyped(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, recordID, intimationType string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source, type)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN', $5)
		 RETURNING id::text`,
		tenantID, caseID, recordID, uuid.NewString(), intimationType).Scan(&id); err != nil {
		t.Fatalf("seed intimation with type=%s: %v", intimationType, err)
	}
	return id
}

// countDrafts counts draft rows for (tenant, intimation) as the owner (RLS bypassed).
func countDrafts(t *testing.T, pool *pgxpool.Pool, tenantID, intimationID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM draft WHERE tenant_id = $1 AND intimation_id = $2`,
		tenantID, intimationID).Scan(&n); err != nil {
		t.Fatalf("countDrafts: %v", err)
	}
	return n
}

// TestDraft_CreateFromIntimation_RoundTrip seeds prerequisites (tenant → court_case →
// court_record → intimation CITACAO) and proves that CreateDraft source=intimation:
//   - lands a row with status=DRAFT, saga_state=CREATED;
//   - infers piece_type=DEFENSE from the CITACAO intimation type;
//   - correctly inherits case_id from the court_record via the intimation FK chain.
func TestDraft_CreateFromIntimation_RoundTrip(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-draft-roundtrip", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0001111-22.2024.8.26.0099")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")

	result, err := uc.Create(ctx, draft.CreateCommand{
		TenantID:     tenantID,
		Source:       draft.SourceIntimation,
		IntimationID: intimationID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !result.IsNewDraft {
		t.Error("IsNewDraft = false, want true for a first creation")
	}
	d := result.Draft
	if d.ID == "" {
		t.Fatal("Draft.ID is empty — INSERT did not return a uuid")
	}
	if d.Status != "DRAFT" {
		t.Errorf("Status = %q, want %q", d.Status, "DRAFT")
	}
	if d.SagaState != "CREATED" {
		t.Errorf("SagaState = %q, want %q", d.SagaState, "CREATED")
	}
	if d.PieceType != draft.PieceTypeDefense {
		t.Errorf("PieceType = %q, want %q (CITACAO→DEFENSE inference)", d.PieceType, draft.PieceTypeDefense)
	}
	if d.IntimationID != intimationID {
		t.Errorf("IntimationID = %q, want %q", d.IntimationID, intimationID)
	}
	if d.CaseID != caseID {
		t.Errorf("CaseID = %q, want %q (inherited from court_record via intimation)", d.CaseID, caseID)
	}
	if d.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero — DB default now() was not applied")
	}
	if d.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero — DB default now() was not applied")
	}
}

// TestDraft_Idempotency_SameIntimation exercises the partial unique index
// draft_intimation_id_uidx: a second CreateDraft for the SAME intimation_id
// must return the EXISTING draft (IsNewDraft=false, same ID) and must NOT
// produce a second row (COUNT stays 1). This is the 23505 → ErrDraftAlreadyExists
// → GetDraftByIntimationID path in the use case.
func TestDraft_Idempotency_SameIntimation(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-draft-idempotent", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0002222-33.2024.8.26.0099")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "INTIMACAO")

	cmd := draft.CreateCommand{
		TenantID:     tenantID,
		Source:       draft.SourceIntimation,
		IntimationID: intimationID,
	}

	first, err := uc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if !first.IsNewDraft {
		t.Fatal("first Create: IsNewDraft = false, want true")
	}

	second, err := uc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("second Create (idempotent): %v", err)
	}
	if second.IsNewDraft {
		t.Error("second Create: IsNewDraft = true, want false (idempotent path)")
	}
	if second.Draft.ID != first.Draft.ID {
		t.Errorf("second Create returned id %q, want the same id %q", second.Draft.ID, first.Draft.ID)
	}

	if n := countDrafts(t, pool, tenantID, intimationID); n != 1 {
		t.Errorf("draft count = %d after two Creates for the same intimation, want 1 (no duplicate)", n)
	}
}

// TestDraft_GetDetail_JoinsPopulated proves the GetDraftDetail JOIN query stitches
// draft + intimation + court_record correctly. The Intimation and Process nested
// structs must be non-nil and carry the seeded data.
func TestDraft_GetDetail_JoinsPopulated(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-draft-detail", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0003333-44.2024.8.26.0099")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")

	created, err := uc.Create(ctx, draft.CreateCommand{
		TenantID:     tenantID,
		Source:       draft.SourceIntimation,
		IntimationID: intimationID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	view, err := uc.GetDetail(ctx, tenantID, created.Draft.ID)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}
	if view.ID != created.Draft.ID {
		t.Errorf("view.ID = %q, want %q", view.ID, created.Draft.ID)
	}
	if view.Status != "DRAFT" {
		t.Errorf("view.Status = %q, want DRAFT", view.Status)
	}
	if view.SagaState != "CREATED" {
		t.Errorf("view.SagaState = %q, want CREATED", view.SagaState)
	}

	if view.Intimation == nil {
		t.Fatal("view.Intimation is nil — LEFT JOIN on intimation failed")
	}
	if view.Intimation.ID != intimationID {
		t.Errorf("view.Intimation.ID = %q, want %q", view.Intimation.ID, intimationID)
	}
	if view.Intimation.Type != "CITACAO" {
		t.Errorf("view.Intimation.Type = %q, want CITACAO", view.Intimation.Type)
	}

	if view.Process == nil {
		t.Fatal("view.Process is nil — LEFT JOIN on court_record failed")
	}
	if view.Process.CaseID != caseID {
		t.Errorf("view.Process.CaseID = %q, want %q", view.Process.CaseID, caseID)
	}
	if view.Process.CourtRecordID != recordID {
		t.Errorf("view.Process.CourtRecordID = %q, want %q", view.Process.CourtRecordID, recordID)
	}
	if view.Process.CNJNumber != "0003333-44.2024.8.26.0099" {
		t.Errorf("view.Process.CNJNumber = %q, want the seeded cnj", view.Process.CNJNumber)
	}
	if view.Process.Court != "TJSP" {
		t.Errorf("view.Process.Court = %q, want TJSP", view.Process.Court)
	}

	// Deadline is nil — none has been derived for this intimation yet (no deadline
	// slice ran). This asserts the LEFT JOIN correctly yields nil rather than a
	// zero-value struct when the deadline row is absent.
	if view.Deadline != nil {
		t.Errorf("view.Deadline is non-nil, want nil (no deadline derived yet)")
	}
}

// TestDraft_Patch_UpdatesContent proves PATCH /v1/pecas/:id (autosave):
//   - updated content is reflected in a subsequent GetDetail;
//   - updated_at advances beyond the created_at value.
func TestDraft_Patch_UpdatesContent(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-draft-patch", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0004444-55.2024.8.26.0099")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "INTIMACAO")

	created, err := uc.Create(ctx, draft.CreateCommand{
		TenantID:     tenantID,
		Source:       draft.SourceIntimation,
		IntimationID: intimationID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	draftID := created.Draft.ID

	newContent := "Excelentíssimo Senhor Juiz, vem o réu, por seu advogado..."
	_, err = uc.Patch(ctx, draft.PatchCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Content:  newContent,
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}

	view, err := uc.GetDetail(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetDetail after Patch: %v", err)
	}
	if view.Content != newContent {
		t.Errorf("Content after Patch = %q, want %q", view.Content, newContent)
	}
	if !view.UpdatedAt.After(created.Draft.CreatedAt) {
		t.Errorf("UpdatedAt (%v) is not after CreatedAt (%v) — updated_at was not bumped by PATCH",
			view.UpdatedAt, created.Draft.CreatedAt)
	}
}

// TestDraft_GetDetail_CrossTenant_NotFound proves the tenant isolation barrier:
// GetDetail with the correct draft id but a DIFFERENT tenantID must return
// ErrDraftNotFound — never the other tenant's data.
func TestDraft_GetDetail_CrossTenant_NotFound(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	ownerTenantID := uuid.NewString()
	seedTenant(t, pool, ownerTenantID, "org-draft-owner-iso", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, ownerTenantID, "0005555-66.2024.8.26.0099")
	intimationID := seedIntimationTyped(t, pool, ownerTenantID, caseID, recordID, "CITACAO")

	created, err := uc.Create(ctx, draft.CreateCommand{
		TenantID:     ownerTenantID,
		Source:       draft.SourceIntimation,
		IntimationID: intimationID,
	})
	if err != nil {
		t.Fatalf("Create as owner: %v", err)
	}

	// A different tenant reads the same draft id → must get ErrDraftNotFound.
	foreignTenantID := uuid.NewString()
	seedTenant(t, pool, foreignTenantID, "org-draft-foreign-iso", 0)

	_, err = uc.GetDetail(ctx, foreignTenantID, created.Draft.ID)
	if !errors.Is(err, draft.ErrDraftNotFound) {
		t.Errorf("cross-tenant GetDetail err = %v, want ErrDraftNotFound", err)
	}
}
