package draft

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port the use case depends on (never the concrete
// impl). Every method takes the caller's tx so it participates in the use case's
// unit of work; RLS scopes the reads/writes to the principal's tenant.
type Repository interface {
	// InsertDraft persists a new peça. On success it returns the full *Draft so the
	// handler renders the 201 response without a follow-up read. When the partial
	// unique index draft_intimation_id_uidx fires (ON CONFLICT DO NOTHING), it
	// returns ErrDraftAlreadyExists so the use case can fetch the existing row for 200.
	InsertDraft(ctx context.Context, tx database.Tx, d *Draft) (*Draft, error)
	// GetDraftByIntimationID returns the existing draft for the (tenant, intimation)
	// pair — the idempotent path after a 23505. A miss is ErrDraftNotFound.
	GetDraftByIntimationID(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*Draft, error)
	// GetDraftByID returns the full draft by id, scoped to tenantID. A miss or
	// foreign-tenant id is ErrDraftNotFound (→ 404).
	GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error)
	// GetIntimationForDraft loads the intimation context (case_id, court_record_id,
	// type, process metadata, deadline, and recipients jsonb for signing-lawyer
	// resolution) needed by the generation pipeline. A miss or foreign id is
	// ErrIntimationNotFound (→ 404).
	GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error)

	// GetPartiesForDraft loads the parties (PLAINTIFF/DEFENDANT/THIRD_PARTY) and
	// their aggregated counsels for a given case, tenant-scoped (barrier 1). Used
	// by the generation pipeline to inject structured party data into the AI prompt.
	// An empty case or one with no parties returns an empty slice, never nil.
	GetPartiesForDraft(ctx context.Context, tx database.Tx, tenantID, caseID string) ([]PartyInfo, error)
	// UpdateDraftContent patches content (+optional title) and bumps updated_at. A
	// no-match (wrong id or foreign tenant) is ErrDraftNotFound.
	UpdateDraftContent(ctx context.Context, tx database.Tx, draftID, tenantID, content string, title *string) (*PatchResult, error)
	// GetDraftDetail runs the JOIN read model for GET /v1/pecas/:id. A miss is
	// ErrDraftNotFound.
	GetDraftDetail(ctx context.Context, tx database.Tx, tenantID, draftID string) (*DraftDetailView, error)

	// ── Attachment methods (Fatia 2) ─────────────────────────────────────────

	// GetDocumentForAttachment loads the minimal document fields (id, tenant_id,
	// status, origin) to validate before linking. A miss (unknown, foreign, or
	// soft-deleted) is ErrDocumentNotFound (→ 404).
	GetDocumentForAttachment(ctx context.Context, tx database.Tx, tenantID, documentID string) (*documentForAttachment, error)

	// InsertAttachment inserts the join row. On a UNIQUE (draft_id, document_id)
	// conflict it returns ErrAttachmentAlreadyLinked (→ 409) — never a 23505 abort.
	InsertAttachment(ctx context.Context, tx database.Tx, a *Attachment) (*Attachment, error)

	// UpdateAttachmentCategory changes the category of an existing attachment. A miss
	// (wrong id, draft, or tenant) is ErrAttachmentNotFound (→ 404).
	UpdateAttachmentCategory(ctx context.Context, tx database.Tx, tenantID, draftID, attachmentID string, category AttachmentCategory) (*Attachment, error)

	// DeleteAttachment hard-deletes the join row. A miss is ErrAttachmentNotFound (→ 404).
	DeleteAttachment(ctx context.Context, tx database.Tx, tenantID, draftID, attachmentID string) error

	// GetDraftAttachments returns the ordered attachment list for a draft (only
	// UPLOADED documents, ordered position ASC, created_at ASC). An empty draft
	// returns an empty slice, never nil.
	GetDraftAttachments(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]Attachment, error)

	// ── AI generation methods (Fatia 3) ──────────────────────────────────────

	// UpdateSagaState transitions the draft's saga_state column and optionally
	// overwrites content (when updateContent=true). Scoped to (draftID, tenantID).
	// A miss is ErrDraftNotFound.
	UpdateSagaState(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string) (*Draft, error)

	// InsertReview persists one AI review row. No tenant guard here — the caller
	// already tenant-guarded the draft before entering the tx. Returns the persisted
	// Review entity.
	InsertReview(ctx context.Context, tx database.Tx, r *Review) (*Review, error)

	// DeleteReviewsForDraft removes all review rows for a draft. Called by Gerar
	// before persisting DRAFTED state so that Revisar always operates on a clean
	// slate (no stale suggestions from a prior generation attempt).
	DeleteReviewsForDraft(ctx context.Context, tx database.Tx, draftID string) error

	// GetLatestReview returns the most recent review for a draft (generated_at DESC).
	// A draft with no reviews returns (nil, nil) — not an error.
	GetLatestReview(ctx context.Context, tx database.Tx, draftID string) (*Review, error)

	// ── Chat methods (Fatia 3b) ───────────────────────────────────────────────

	// InsertChatMessage appends one turn to the thread. No tenant guard in the query —
	// the caller must have already tenant-guarded the draft (barrier 1). Returns the
	// persisted ChatMessage entity.
	InsertChatMessage(ctx context.Context, tx database.Tx, m *ChatMessage) (*ChatMessage, error)

	// GetChatThread returns the last 50 messages for a draft ordered chronologically
	// (oldest first). No tenant guard in the query — the caller tenant-guards via the
	// draft load. An empty thread returns an empty slice, never nil.
	GetChatThread(ctx context.Context, tx database.Tx, draftID string) ([]ChatMessage, error)
}

// ErrDraftAlreadyExists is a sentinel the repository returns when InsertDraft hits
// the partial unique index (23505). The use case treats this as idempotence and
// fetches the existing row for a 200 response — it is NOT an AppError because it
// never escapes to the client.
var ErrDraftAlreadyExists = errors.New("draft already exists for this intimation")

// pgRepository is the sqlc-backed Repository. Every method binds draftdb to the
// caller's tx; the repo is stateless (nothing to inject at construction).
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the stateless Repository.
func NewRepository() Repository { return &pgRepository{} }

func (r *pgRepository) InsertDraft(ctx context.Context, tx database.Tx, d *Draft) (*Draft, error) {
	tenantID, err := parseUUID(d.TenantID)
	if err != nil {
		return nil, err
	}

	// ON CONFLICT DO NOTHING: when the partial unique index fires (same tenant +
	// intimation_id), the INSERT is silently skipped and RETURNING yields no rows
	// (pgx.ErrNoRows). We map that to ErrDraftAlreadyExists so the use case can
	// fetch the existing row — the transaction stays healthy (no 23505 abort).
	row, err := draftdb.New(tx).InsertDraft(ctx, draftdb.InsertDraftParams{
		TenantID:     tenantID,
		CaseID:       optUUID(d.CaseID),
		IntimationID: optUUID(d.IntimationID),
		PieceType:    d.PieceType,
		Title:        d.Title,
		Content:      textToNull(d.Content),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftAlreadyExists
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromInsertRow(row), nil
}

func (r *pgRepository) GetDraftByIntimationID(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*Draft, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	iid := optUUID(intimationID)

	row, err := draftdb.New(tx).GetDraftByIntimationID(ctx, draftdb.GetDraftByIntimationIDParams{
		TenantID:     tid,
		IntimationID: iid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromGetByIntimationRow(row), nil
}

func (r *pgRepository) GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetDraftByID(ctx, draftdb.GetDraftByIDParams{
		ID:       did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromGetByIDRow(row), nil
}

func (r *pgRepository) GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	iid, err := parseUUID(intimationID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetIntimationForDraft(ctx, draftdb.GetIntimationForDraftParams{
		ID:       iid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIntimationNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	// Format deadline end_date as DateOnly string; empty when NULL (no deadline yet).
	deadlineDate := ""
	if row.DeadlineEndDate.Valid {
		deadlineDate = row.DeadlineEndDate.Time.Format("2006-01-02")
	}
	return &IntimationContext{
		IntimationID:    row.IntimationID.String(),
		CaseID:          row.CaseID.String(),
		CourtRecordID:   row.CourtRecordID.String(),
		Type:            derefString(row.IntimationType),
		Content:         row.IntimationContent,
		Recipients:      row.Recipients,
		CNJNumber:       row.CnjNumber,
		Court:           row.Court,
		Degree:          row.Degree,
		Class:           derefString(row.Class),
		Subject:         derefString(row.Subject),
		JudgingBody:     derefString(row.JudgingBody),
		DeadlineEndDate: deadlineDate,
	}, nil
}

func (r *pgRepository) GetPartiesForDraft(ctx context.Context, tx database.Tx, tenantID, caseID string) ([]PartyInfo, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	cid, err := parseUUID(caseID)
	if err != nil {
		return nil, err
	}

	rows, err := draftdb.New(tx).GetPartiesForDraft(ctx, draftdb.GetPartiesForDraftParams{
		TenantID: tid,
		CaseID:   cid,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return partiesFromRows(rows), nil
}

func (r *pgRepository) UpdateDraftContent(ctx context.Context, tx database.Tx, draftID, tenantID, content string, title *string) (*PatchResult, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	updateTitle := title != nil
	titleVal := ""
	if updateTitle {
		titleVal = *title
	}

	row, err := draftdb.New(tx).UpdateDraftContent(ctx, draftdb.UpdateDraftContentParams{
		ID:       did,
		TenantID: tid,
		Content:  textToNull(content),
		Column4:  updateTitle,
		Title:    titleVal,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &PatchResult{
		ID:        row.ID.String(),
		Title:     row.Title,
		UpdatedAt: timestamptzToTime(row.UpdatedAt),
	}, nil
}

func (r *pgRepository) GetDraftDetail(ctx context.Context, tx database.Tx, tenantID, draftID string) (*DraftDetailView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetDraftDetail(ctx, draftdb.GetDraftDetailParams{
		ID:       did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return detailViewFromRow(row), nil
}

// ── Attachment repository methods (Fatia 2) ──────────────────────────────────

func (r *pgRepository) GetDocumentForAttachment(ctx context.Context, tx database.Tx, tenantID, documentID string) (*documentForAttachment, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(documentID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetDocumentForAttachment(ctx, draftdb.GetDocumentForAttachmentParams{
		ID:       did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &documentForAttachment{
		ID:       row.ID.String(),
		TenantID: row.TenantID.String(),
		Status:   row.Status,
		Origin:   row.Origin,
	}, nil
}

func (r *pgRepository) InsertAttachment(ctx context.Context, tx database.Tx, a *Attachment) (*Attachment, error) {
	tid, err := parseUUID(a.TenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(a.DraftID)
	if err != nil {
		return nil, err
	}
	docID, err := parseUUID(a.DocumentID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).InsertDraftAttachment(ctx, draftdb.InsertDraftAttachmentParams{
		TenantID:   tid,
		DraftID:    did,
		DocumentID: docID,
		Category:   string(a.Category),
		Position:   int32(a.Position),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAttachmentAlreadyLinked
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return attachmentFromRow(row), nil
}

func (r *pgRepository) UpdateAttachmentCategory(ctx context.Context, tx database.Tx, tenantID, draftID, attachmentID string, category AttachmentCategory) (*Attachment, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	aid, err := parseUUID(attachmentID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).UpdateAttachmentCategory(ctx, draftdb.UpdateAttachmentCategoryParams{
		ID:       aid,
		DraftID:  did,
		TenantID: tid,
		Category: string(category),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAttachmentNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return attachmentFromRow(row), nil
}

func (r *pgRepository) DeleteAttachment(ctx context.Context, tx database.Tx, tenantID, draftID, attachmentID string) error {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	aid, err := parseUUID(attachmentID)
	if err != nil {
		return err
	}

	// Confirm existence before deleting so a miss is always a typed ErrAttachmentNotFound
	// (the :exec query returns only error, with no RowsAffected). A separate SELECT +
	// DELETE in the same tx is safe under RLS — the policy prevents cross-tenant rows.
	_, err = draftdb.New(tx).GetAttachmentForUpdate(ctx, draftdb.GetAttachmentForUpdateParams{
		ID:       aid,
		DraftID:  did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAttachmentNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}

	if err := draftdb.New(tx).DeleteDraftAttachment(ctx, draftdb.DeleteDraftAttachmentParams{
		ID:       aid,
		DraftID:  did,
		TenantID: tid,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) GetDraftAttachments(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]Attachment, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	rows, err := draftdb.New(tx).GetDraftAttachments(ctx, draftdb.GetDraftAttachmentsParams{
		DraftID:  did,
		TenantID: tid,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return attachmentsFromRows(rows), nil
}

// ── AI generation repository methods (Fatia 3) ───────────────────────────────

func (r *pgRepository) UpdateSagaState(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string) (*Draft, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	var contentPtr *string
	if updateContent {
		contentPtr = &content
	}

	row, err := draftdb.New(tx).UpdateSagaState(ctx, draftdb.UpdateSagaStateParams{
		ID:        did,
		TenantID:  tid,
		SagaState: sagaState,
		Column4:   updateContent,
		Content:   contentPtr,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromUpdateSagaStateRow(row), nil
}

func (r *pgRepository) InsertReview(ctx context.Context, tx database.Tx, rev *Review) (*Review, error) {
	did, err := parseUUID(rev.DraftID)
	if err != nil {
		return nil, err
	}

	findingsJSON, err := marshalJSON(rev.Findings)
	if err != nil {
		return nil, err
	}
	coverageJSON, err := marshalJSON(rev.Coverage)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).InsertReview(ctx, draftdb.InsertReviewParams{
		DraftID:      did,
		Findings:     findingsJSON,
		Coverage:     coverageJSON,
		ModelVersion: rev.ModelVersion,
		RulesVersion: rev.RulesVersion,
		Status:       rev.Status,
		GeneratedAt:  timeToTimestamptz(rev.GeneratedAt),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return reviewFromInsertRow(row), nil
}

func (r *pgRepository) DeleteReviewsForDraft(ctx context.Context, tx database.Tx, draftID string) error {
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	if err := draftdb.New(tx).DeleteReviewsForDraft(ctx, did); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) GetLatestReview(ctx context.Context, tx database.Tx, draftID string) (*Review, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetLatestReview(ctx, did)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no review yet — not an error
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return reviewFromGetLatestRow(row), nil
}

// ── Chat repository methods (Fatia 3b) ───────────────────────────────────────

func (r *pgRepository) InsertChatMessage(ctx context.Context, tx database.Tx, m *ChatMessage) (*ChatMessage, error) {
	did, err := parseUUID(m.DraftID)
	if err != nil {
		return nil, err
	}

	citationsJSON, err := marshalJSON(m.Citations)
	if err != nil {
		return nil, err
	}

	var modelVersion *string
	if m.ModelVersion != "" {
		modelVersion = &m.ModelVersion
	}

	row, err := draftdb.New(tx).InsertChatMessage(ctx, draftdb.InsertChatMessageParams{
		DraftID:      did,
		Role:         m.Role,
		Content:      m.Content,
		Citations:    citationsJSON,
		Grounded:     m.Grounded,
		ModelVersion: modelVersion,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return chatMessageFromRow(row), nil
}

func (r *pgRepository) GetChatThread(ctx context.Context, tx database.Tx, draftID string) ([]ChatMessage, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	rows, err := draftdb.New(tx).GetChatThread(ctx, did)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return chatMessagesFromRows(rows), nil
}
