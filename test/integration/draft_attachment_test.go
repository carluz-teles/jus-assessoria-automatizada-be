//go:build integration

// draft_attachment integration tests — prove the Fatia 2 attachment write+read path
// against a REAL Postgres with migrations applied (including 0043_draft_attachment).
//
// What unit tests with a mocked repo CANNOT prove:
//   - The SQL INSERT actually lands with the correct defaults.
//   - The UNIQUE (draft_id, document_id) constraint fires at the DB level.
//   - GetDraftAttachments JOINs document and filters status='UPLOADED' correctly.
//   - The PATCH category update is scoped to (id, draft_id, tenant_id).
//   - DELETE hard-deletes only the join row; the document row survives.
//   - RLS isolates one tenant from another on draft_attachment rows.
//
// Helpers reused from the existing test suite:
//   - newPool(t)           — setup_test.go
//   - seedTenant()         — rls_test.go
//   - seedCourtRecordCNJ() — list_search_test.go
//   - seedIntimationTyped()— draft_test.go
//
// A document UPLOAD/UPLOADED row is created directly via SQL (no presigned PUT
// or storage mock needed — the status is seeded directly in the DB).
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

// ── helpers ──────────────────────────────────────────────────────────────────

// seedUploadedDocument inserts a document row with origin=UPLOAD and
// status=UPLOADED so it is attachable. court_record_id is optional (empty → NULL).
// Returns the new document id.
func seedUploadedDocument(t *testing.T, pool *pgxpool.Pool, tenantID, courtRecordID string) string {
	t.Helper()
	var crID *string
	if courtRecordID != "" {
		crID = &courtRecordID
	}
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO document
		   (tenant_id, court_record_id, document_type, origin, storage_key, status,
		    mime_type, size_bytes, title, original_filename)
		 VALUES ($1, $2, 'PETIÇÃO', 'UPLOAD', 'docs/fake-key', 'UPLOADED',
		         'application/pdf', 102400, 'Procuração ad judicia', 'procuracao.pdf')
		 RETURNING id::text`,
		tenantID, crID).Scan(&id); err != nil {
		t.Fatalf("seedUploadedDocument: %v", err)
	}
	return id
}

// seedPendingDocument inserts a document with status=PENDING (not yet completed).
func seedPendingDocument(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO document
		   (tenant_id, document_type, origin, storage_key, status,
		    mime_type, size_bytes, title, original_filename)
		 VALUES ($1, 'PETIÇÃO', 'UPLOAD', 'docs/pending-key', 'PENDING',
		         'application/pdf', 512, 'Pending doc', 'pending.pdf')
		 RETURNING id::text`,
		tenantID).Scan(&id); err != nil {
		t.Fatalf("seedPendingDocument: %v", err)
	}
	return id
}

// seedCourtDocument inserts a document with origin=COURT.
func seedCourtDocument(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO document
		   (tenant_id, document_type, origin, storage_key, status,
		    mime_type, size_bytes, title, original_filename)
		 VALUES ($1, 'ACÓRDÃO', 'COURT', 'autos/court-key', 'UPLOADED',
		         'application/pdf', 8192, 'Acórdão', 'acordao.pdf')
		 RETURNING id::text`,
		tenantID).Scan(&id); err != nil {
		t.Fatalf("seedCourtDocument: %v", err)
	}
	return id
}

// countAttachments counts draft_attachment rows for (draft_id) as the owner (RLS
// bypassed).
func countAttachments(t *testing.T, pool *pgxpool.Pool, draftID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM draft_attachment WHERE draft_id = $1`, draftID).Scan(&n); err != nil {
		t.Fatalf("countAttachments: %v", err)
	}
	return n
}

// documentExists confirms the document row is NOT deleted (hard delete guard).
func documentExists(t *testing.T, pool *pgxpool.Pool, documentID string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM document WHERE id = $1`, documentID).Scan(&n); err != nil {
		t.Fatalf("documentExists: %v", err)
	}
	return n > 0
}

// newAttachmentDraftUC creates a draft use case wired to the real pool.
func newAttachmentDraftUC(pool *pgxpool.Pool) *draft.UseCase {
	return draft.NewUseCase(
		database.NewUnitOfWork(pool),
		draft.NewRepository(),
	)
}

// seedDraft creates a blank draft for the given tenant and returns its id.
func seedDraft(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	uc := newAttachmentDraftUC(pool)
	result, err := uc.Create(context.Background(), draft.CreateCommand{
		TenantID: tenantID,
		Source:   draft.SourceBlank,
	})
	if err != nil {
		t.Fatalf("seedDraft: %v", err)
	}
	return result.Draft.ID
}

// ── AC1: round-trip POST → GET /v1/pecas/:id → PATCH → DELETE ────────────────

// TestDraftAttachment_RoundTrip is the acceptance-criteria end-to-end:
//   - POST anexo returns 201 with the attachment item;
//   - GET /v1/pecas/:id includes the attachment in attachments[];
//   - PATCH /v1/pecas/:id/anexos/:id changes category;
//   - DELETE removes the join row (document survives).
func TestDraftAttachment_RoundTrip(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-attach-rt", 0)

	draftID := seedDraft(t, pool, tenantID)
	docID := seedUploadedDocument(t, pool, tenantID, "")

	// POST /v1/pecas/:id/anexos — link the document.
	att, err := uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantID,
		DraftID:    draftID,
		DocumentID: docID,
		Category:   draft.CategoryProcuracao,
	})
	if err != nil {
		t.Fatalf("AttachDocument: %v", err)
	}
	if att.ID == "" {
		t.Fatal("attachment.ID is empty — INSERT did not return a uuid")
	}
	if string(att.Category) != string(draft.CategoryProcuracao) {
		t.Errorf("attachment.Category = %q, want %q", att.Category, draft.CategoryProcuracao)
	}
	if att.DocumentID != docID {
		t.Errorf("attachment.DocumentID = %q, want %q", att.DocumentID, docID)
	}
	if n := countAttachments(t, pool, draftID); n != 1 {
		t.Fatalf("attachment count after POST = %d, want 1", n)
	}

	// GET /v1/pecas/:id — attachments must be non-empty.
	view, err := uc.GetDetail(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}
	if len(view.Attachments) != 1 {
		t.Fatalf("view.Attachments len = %d, want 1", len(view.Attachments))
	}
	got := view.Attachments[0]
	if got.DocumentID != docID {
		t.Errorf("view.Attachments[0].DocumentID = %q, want %q", got.DocumentID, docID)
	}
	if string(got.Category) != string(draft.CategoryProcuracao) {
		t.Errorf("view.Attachments[0].Category = %q, want %q", got.Category, draft.CategoryProcuracao)
	}
	if got.Name == "" {
		t.Error("view.Attachments[0].Name is empty — JOIN on document.title failed")
	}
	if got.MimeType == "" {
		t.Error("view.Attachments[0].MimeType is empty — JOIN on document failed")
	}
	if got.SizeBytes == 0 {
		t.Error("view.Attachments[0].SizeBytes is 0 — JOIN on document failed")
	}

	// PATCH /v1/pecas/:id/anexos/:id — change category.
	updated, err := uc.UpdateAttachmentCategory(ctx, draft.UpdateAttachmentCategoryCommand{
		TenantID:     tenantID,
		DraftID:      draftID,
		AttachmentID: att.ID,
		Category:     draft.CategoryContrato,
	})
	if err != nil {
		t.Fatalf("UpdateAttachmentCategory: %v", err)
	}
	if string(updated.Category) != string(draft.CategoryContrato) {
		t.Errorf("updated.Category = %q, want %q", updated.Category, draft.CategoryContrato)
	}

	// Verify category change is visible in GetDetail.
	view2, err := uc.GetDetail(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetDetail after PATCH: %v", err)
	}
	if len(view2.Attachments) != 1 {
		t.Fatalf("view.Attachments len after PATCH = %d, want 1", len(view2.Attachments))
	}
	if string(view2.Attachments[0].Category) != string(draft.CategoryContrato) {
		t.Errorf("view.Attachments[0].Category after PATCH = %q, want %q",
			view2.Attachments[0].Category, draft.CategoryContrato)
	}

	// DELETE /v1/pecas/:id/anexos/:id — removes join row.
	if err := uc.RemoveAttachment(ctx, draft.RemoveAttachmentCommand{
		TenantID:     tenantID,
		DraftID:      draftID,
		AttachmentID: att.ID,
	}); err != nil {
		t.Fatalf("RemoveAttachment: %v", err)
	}
	if n := countAttachments(t, pool, draftID); n != 0 {
		t.Fatalf("attachment count after DELETE = %d, want 0", n)
	}
	// The document itself must survive.
	if !documentExists(t, pool, docID) {
		t.Error("document was hard-deleted — only the join row should be removed")
	}

	// GET /v1/pecas/:id must now return empty attachments.
	view3, err := uc.GetDetail(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetDetail after DELETE: %v", err)
	}
	if len(view3.Attachments) != 0 {
		t.Errorf("view.Attachments after DELETE = %d, want 0", len(view3.Attachments))
	}
}

// AC2: UNIQUE constraint prevents linking the same document twice.
func TestDraftAttachment_Idempotency_UniqueConstraint(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-attach-unique", 0)
	draftID := seedDraft(t, pool, tenantID)
	docID := seedUploadedDocument(t, pool, tenantID, "")

	_, err := uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantID,
		DraftID:    draftID,
		DocumentID: docID,
	})
	if err != nil {
		t.Fatalf("first AttachDocument: %v", err)
	}

	// Second link of the same document → ErrAttachmentAlreadyLinked.
	_, err = uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantID,
		DraftID:    draftID,
		DocumentID: docID,
	})
	if !errors.Is(err, draft.ErrAttachmentAlreadyLinked) {
		t.Errorf("second AttachDocument err = %v, want ErrAttachmentAlreadyLinked", err)
	}
	if n := countAttachments(t, pool, draftID); n != 1 {
		t.Fatalf("attachment count after duplicate attempt = %d, want 1", n)
	}
}

// AC3: PENDING document cannot be attached.
func TestDraftAttachment_PendingDocument_Rejected(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-attach-pending", 0)
	draftID := seedDraft(t, pool, tenantID)
	pendingDocID := seedPendingDocument(t, pool, tenantID)

	_, err := uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantID,
		DraftID:    draftID,
		DocumentID: pendingDocID,
	})
	if !errors.Is(err, draft.ErrDocumentNotAttachable) {
		t.Errorf("AttachDocument(PENDING) err = %v, want ErrDocumentNotAttachable", err)
	}
}

// AC4: COURT document cannot be attached.
func TestDraftAttachment_CourtDocument_Rejected(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-attach-court", 0)
	draftID := seedDraft(t, pool, tenantID)
	courtDocID := seedCourtDocument(t, pool, tenantID)

	_, err := uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantID,
		DraftID:    draftID,
		DocumentID: courtDocID,
	})
	if !errors.Is(err, draft.ErrDocumentNotAttachable) {
		t.Errorf("AttachDocument(COURT) err = %v, want ErrDocumentNotAttachable", err)
	}
}

// AC5: Tenant isolation — a document owned by tenant A cannot be attached by tenant B.
func TestDraftAttachment_CrossTenant_DocumentNotFound(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-attach-owner", 0)
	seedTenant(t, pool, tenantB, "org-attach-intruder", 0)

	draftB := seedDraft(t, pool, tenantB)
	// Document belongs to tenantA.
	docA := seedUploadedDocument(t, pool, tenantA, "")

	// tenantB tries to attach tenantA's document.
	_, err := uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantB,
		DraftID:    draftB,
		DocumentID: docA,
	})
	if !errors.Is(err, draft.ErrDocumentNotFound) {
		t.Errorf("cross-tenant attach err = %v, want ErrDocumentNotFound", err)
	}
}

// AC6: Tenant isolation — attachment management is scoped; a foreign tenant cannot
// update or delete another tenant's attachment.
func TestDraftAttachment_CrossTenant_AttachmentNotFound(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-att-mgmt-owner", 0)
	seedTenant(t, pool, tenantB, "org-att-mgmt-intruder", 0)

	draftA := seedDraft(t, pool, tenantA)
	docA := seedUploadedDocument(t, pool, tenantA, "")

	att, err := uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantA,
		DraftID:    draftA,
		DocumentID: docA,
	})
	if err != nil {
		t.Fatalf("AttachDocument as owner: %v", err)
	}

	// tenantB tries to update the attachment owned by tenantA.
	_, err = uc.UpdateAttachmentCategory(ctx, draft.UpdateAttachmentCategoryCommand{
		TenantID:     tenantB,
		DraftID:      draftA,
		AttachmentID: att.ID,
		Category:     draft.CategoryOutro,
	})
	if !errors.Is(err, draft.ErrAttachmentNotFound) {
		t.Errorf("cross-tenant UpdateCategory err = %v, want ErrAttachmentNotFound", err)
	}

	// tenantB tries to delete it.
	err = uc.RemoveAttachment(ctx, draft.RemoveAttachmentCommand{
		TenantID:     tenantB,
		DraftID:      draftA,
		AttachmentID: att.ID,
	})
	if !errors.Is(err, draft.ErrAttachmentNotFound) {
		t.Errorf("cross-tenant RemoveAttachment err = %v, want ErrAttachmentNotFound", err)
	}
}

// AC7: GetDetail returns [] (empty slice, not null) when there are no attachments.
func TestDraftAttachment_GetDetail_EmptySlice(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-attach-empty", 0)
	draftID := seedDraft(t, pool, tenantID)

	view, err := uc.GetDetail(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}
	if view.Attachments == nil {
		t.Error("view.Attachments is nil, want empty slice (must serialize as [])")
	}
	if len(view.Attachments) != 0 {
		t.Errorf("view.Attachments len = %d, want 0", len(view.Attachments))
	}
}

// AC8: draft CASCADE — deleting the draft removes its attachments.
func TestDraftAttachment_DraftCascade_DeletesCascade(t *testing.T) {
	pool := newPool(t)
	uc := newAttachmentDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-attach-cascade", 0)
	draftID := seedDraft(t, pool, tenantID)
	docID := seedUploadedDocument(t, pool, tenantID, "")

	if _, err := uc.AttachDocument(ctx, draft.AttachDocumentCommand{
		TenantID:   tenantID,
		DraftID:    draftID,
		DocumentID: docID,
	}); err != nil {
		t.Fatalf("AttachDocument: %v", err)
	}
	if n := countAttachments(t, pool, draftID); n != 1 {
		t.Fatalf("attachment count before cascade = %d, want 1", n)
	}

	// Delete the draft directly via SQL (bypass use case — no delete endpoint in F1/F2).
	if _, err := pool.Exec(ctx, `DELETE FROM draft WHERE id = $1`, draftID); err != nil {
		t.Fatalf("delete draft: %v", err)
	}

	// Attachment rows must have cascaded.
	if n := countAttachments(t, pool, draftID); n != 0 {
		t.Fatalf("attachment count after draft delete = %d, want 0 (ON DELETE CASCADE)", n)
	}
	// Document must survive.
	if !documentExists(t, pool, docID) {
		t.Error("document was deleted — ON DELETE RESTRICT on document_id failed")
	}
}
