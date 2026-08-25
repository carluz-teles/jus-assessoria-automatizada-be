package draft

import (
	"context"
	"errors"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// UseCase owns the domain logic for the peticionamento Fatia 1 write path.
// It opens the UoW transaction; the repo participates. Reads (GetDraftDetail) run
// in a lightweight, pool-backed read tx — no outbox, no UoW overhead.
//
// Event note: draft.created is intentionally NOT emitted in this fatia.
// The queueFor function in lib/events/relay.go routes "draft.*" → "default", and
// no worker drains the "default" queue — events would accumulate silently.
// TODO(F2): emit draft.created once a listener is registered on the correct queue.
type UseCase struct {
	repo    database.UnitOfWork
	rw      Repository
	now     func() time.Time
	outbox  OutboxPublisher
	storage StoragePresigner
}

// OutboxPublisher is the narrow port for event publishing within a tx.
type OutboxPublisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// StoragePresigner is the narrow port for presigned URL generation.
type StoragePresigner interface {
	PresignedGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// NewUseCase wires the use case to its dependencies. uow owns the transaction
// boundary for all three endpoints (GetDetail runs in a read-scoped Do, same RLS
// barrier as the writes). now defaults to time.Now and is overridable in tests.
func NewUseCase(uow database.UnitOfWork, repo Repository, opts ...Option) *UseCase {
	uc := &UseCase{repo: uow, rw: repo, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// Option configures a UseCase at construction.
type Option func(*UseCase)

// WithClock overrides the reference clock. Production leaves the default (time.Now);
// tests pin it for deterministic assertions.
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// WithOutbox attaches the outbox publisher. Called by cmd/api composition.
func WithOutbox(ob OutboxPublisher) Option {
	return func(uc *UseCase) { uc.outbox = ob }
}

// WithStorage attaches the presigned URL generator. Called by cmd/api composition.
func WithStorage(s StoragePresigner) Option {
	return func(uc *UseCase) { uc.storage = s }
}

// CreateCommand is the input the handler builds from the request + the verified
// principal. TenantID comes from the principal, never the body.
type CreateCommand struct {
	TenantID     string
	Source       string
	IntimationID string
	CaseID       string
	PieceType    string
	Title        string
}

// CreateResult carries the response body for a POST /v1/pecas: the created or found
// draft, plus a flag indicating whether this was a first creation (201) or idempotent
// (200).
type CreateResult struct {
	Draft      *Draft
	IsNewDraft bool
}

// Create implements POST /v1/pecas. It runs in a single tenant-scoped transaction:
//  1. When source=intimation, reads the intimation context (case_id, court_record_id,
//     type) to infer piece_type and populate CaseID.
//  2. Inserts the draft. On a 23505 unique violation (same tenant + intimation_id),
//     fetches the existing row and returns IsNewDraft=false (200 idempotent).
//
// tenant_id always comes from the verified principal — never the body.
func (uc *UseCase) Create(ctx context.Context, cmd CreateCommand) (CreateResult, error) {
	var result CreateResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d := &Draft{
			TenantID:  cmd.TenantID,
			PieceType: cmd.PieceType,
			Title:     cmd.Title,
		}

		switch cmd.Source {
		case SourceIntimation:
			intimation, err := uc.rw.GetIntimationForDraft(ctx, tx, cmd.TenantID, cmd.IntimationID)
			if err != nil {
				return err
			}
			d.IntimationID = intimation.IntimationID
			d.CaseID = intimation.CaseID
			// Infer piece_type from the intimation (content-first) when not supplied.
			if d.PieceType == "" {
				d.PieceType = inferPieceType(intimation)
			}

		case SourceProcesso:
			d.CaseID = cmd.CaseID
			if d.PieceType == "" {
				d.PieceType = PieceTypeOther
			}

		default: // SourceBlank
			if d.PieceType == "" {
				d.PieceType = PieceTypeOther
			}
		}

		created, err := uc.rw.InsertDraft(ctx, tx, d)
		if err != nil {
			if errors.Is(err, ErrDraftAlreadyExists) {
				// Idempotent: fetch and return the existing row (→ 200 at the edge).
				existing, fetchErr := uc.rw.GetDraftByIntimationID(ctx, tx, cmd.TenantID, cmd.IntimationID)
				if fetchErr != nil {
					return fetchErr
				}
				result = CreateResult{Draft: existing, IsNewDraft: false}
				return nil
			}
			return err
		}

		// TODO(F2): emit draft.created event here once a consumer queue is registered.
		// The relay routes "draft.*" → "default" (no worker drains it), so emitting
		// in Fatia 1 would cause silent queue buildup. Wire the event after the F2
		// listener is in place and lib/events/relay.go's queueFor routes "draft" to
		// the correct queue.

		result = CreateResult{Draft: created, IsNewDraft: true}
		return nil
	})
	if err != nil {
		return CreateResult{}, err
	}
	return result, nil
}

// PatchCommand is the input for PATCH /v1/pecas/:id.
type PatchCommand struct {
	TenantID string
	DraftID  string
	Content  string
	Title    *string
}

// Patch implements PATCH /v1/pecas/:id (autosave). Runs in a single tenant-scoped tx:
// updates content + optionally title + bumps updated_at. A missing or foreign-tenant
// draft is ErrDraftNotFound (→ 404). content empty is valid (editor cleared it).
// No event is emitted — autosave is not a domain event.
func (uc *UseCase) Patch(ctx context.Context, cmd PatchCommand) (*PatchResult, error) {
	var result *PatchResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		r, err := uc.rw.UpdateDraftContent(ctx, tx, cmd.DraftID, cmd.TenantID, cmd.Content, cmd.Title)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetDetail implements GET /v1/pecas/:id. Runs in a read-only tenant-scoped tx
// (the pool-backed Do — same RLS barrier as the writes). A missing or foreign-tenant
// draft is ErrDraftNotFound (→ 404). The attachments list is loaded by a separate
// dedicated query (2 queries per the Architect's decision — no JOIN monstrosity on
// GetDraftDetail).
func (uc *UseCase) GetDetail(ctx context.Context, tenantID, draftID string) (*DraftDetailView, error) {
	var view *DraftDetailView

	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		v, err := uc.rw.GetDraftDetail(ctx, tx, tenantID, draftID)
		if err != nil {
			return err
		}
		attachments, err := uc.rw.GetDraftAttachments(ctx, tx, tenantID, draftID)
		if err != nil {
			return err
		}
		v.Attachments = attachments

		// Latest AI review (nil when no generation has been run).
		rev, err := uc.rw.GetLatestReview(ctx, tx, v.ID)
		if err != nil {
			return err
		}
		v.Review = rev

		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

// ── Attachment use cases (Fatia 2) ────────────────────────────────────────────

// AttachDocumentCommand is the input for POST /v1/pecas/:id/anexos.
type AttachDocumentCommand struct {
	TenantID   string
	DraftID    string
	DocumentID string
	Category   AttachmentCategory
}

// AttachDocument implements POST /v1/pecas/:id/anexos. In ONE tenant-scoped tx it:
//  1. Verifies the draft exists in the tenant (ErrDraftNotFound → 404).
//  2. Loads the document (ErrDocumentNotFound → 404 if unknown/foreign/soft-deleted).
//  3. Guards: origin must be UPLOAD (not COURT) and status must be UPLOADED (not PENDING).
//     Either violation → ErrDocumentNotAttachable → 422.
//  4. Inserts the join row. A UNIQUE conflict → ErrAttachmentAlreadyLinked → 409.
//
// tenant_id comes from the principal, never the body or path.
func (uc *UseCase) AttachDocument(ctx context.Context, cmd AttachDocumentCommand) (*Attachment, error) {
	category := cmd.Category
	if category == "" {
		category = CategoryOutro
	}

	var result *Attachment
	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		// 1. Confirm draft belongs to tenant.
		if _, err := uc.rw.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID); err != nil {
			return err
		}

		// 2. Load document (guards tenant scope, deleted_at IS NULL).
		doc, err := uc.rw.GetDocumentForAttachment(ctx, tx, cmd.TenantID, cmd.DocumentID)
		if err != nil {
			return err
		}

		// 3. Guard: origin=UPLOAD and status=UPLOADED.
		isUpload := doc.Origin == documentOriginUpload
		isUploaded := doc.Status == documentStatusUploaded
		if !isUpload || !isUploaded {
			return ErrDocumentNotAttachable
		}

		// 4. Insert (UNIQUE conflict → ErrAttachmentAlreadyLinked).
		att, err := uc.rw.InsertAttachment(ctx, tx, &Attachment{
			TenantID:   cmd.TenantID,
			DraftID:    cmd.DraftID,
			DocumentID: cmd.DocumentID,
			Category:   category,
			Position:   0,
		})
		if err != nil {
			return err
		}
		result = att
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateAttachmentCategoryCommand is the input for PATCH /v1/pecas/:id/anexos/:attachmentId.
type UpdateAttachmentCategoryCommand struct {
	TenantID     string
	DraftID      string
	AttachmentID string
	Category     AttachmentCategory
}

// UpdateAttachmentCategory implements PATCH /v1/pecas/:id/anexos/:attachmentId. In ONE
// tenant-scoped tx it validates the category and updates the row. A miss (wrong id, draft,
// or tenant) is ErrAttachmentNotFound → 404. An invalid category is ErrDocumentNotAttachable
// returned before the tx — actually we use a validation error, see validation.go.
func (uc *UseCase) UpdateAttachmentCategory(ctx context.Context, cmd UpdateAttachmentCategoryCommand) (*Attachment, error) {
	var result *Attachment
	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		att, err := uc.rw.UpdateAttachmentCategory(ctx, tx, cmd.TenantID, cmd.DraftID, cmd.AttachmentID, cmd.Category)
		if err != nil {
			return err
		}
		result = att
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RemoveAttachmentCommand is the input for DELETE /v1/pecas/:id/anexos/:attachmentId.
type RemoveAttachmentCommand struct {
	TenantID     string
	DraftID      string
	AttachmentID string
}

// RemoveAttachment implements DELETE /v1/pecas/:id/anexos/:attachmentId (hard-delete of
// the join row). The document subjacente is NEVER deleted. A miss is ErrAttachmentNotFound
// → 404.
func (uc *UseCase) RemoveAttachment(ctx context.Context, cmd RemoveAttachmentCommand) error {
	return uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		return uc.rw.DeleteAttachment(ctx, tx, cmd.TenantID, cmd.DraftID, cmd.AttachmentID)
	})
}

// ── Peticionamento use cases (Fatia 4) ──────────────────────────────────────

// SignCommand is the input for POST /v1/pecas/:id/sign.
type SignCommand struct {
	TenantID string
	DraftID  string
}

// SignResult carries the response for a successful sign.
type SignResult struct {
	ID        string
	Status    string
	SignedAt  time.Time
	IsIdempot bool // true when the draft was already SIGNED (200, not fresh)
}

// Sign implements POST /v1/pecas/:id/sign. In ONE tenant-scoped tx:
//  1. GetDraftByID (tenant guard → 404 if not found).
//  2. Guard: status must be DRAFT or REVIEWED → ErrInvalidStatusForSign.
//  3. If already SIGNED → return current data (idempotent, no error).
//  4. SignDraft → status=SIGNED, signed_at=now().
//  5. outbox.Publish(draft.signed) — same tx.
//  6. Return {id, status, signed_at}.
func (uc *UseCase) Sign(ctx context.Context, cmd SignCommand) (*SignResult, error) {
	var result *SignResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d, err := uc.rw.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}

		// Idempotent: already signed → return current data.
		if d.Status == StatusSigned {
			result = &SignResult{
				ID:        d.ID,
				Status:    d.Status,
				SignedAt:  d.UpdatedAt, // best approximation without signed_at on entity
				IsIdempot: true,
			}
			return nil
		}

		// Guard: must be DRAFT or REVIEWED.
		if d.Status != StatusDraft && d.Status != StatusReviewed {
			return ErrInvalidStatusForSign
		}

		signed, err := uc.rw.SignDraft(ctx, tx, cmd.DraftID, cmd.TenantID)
		if err != nil {
			return err
		}

		// Emit draft.signed event (outbox).
		if uc.outbox != nil {
			ev := newDraftSigned(signed)
			if err := uc.outbox.Publish(ctx, tx, ev); err != nil {
				return err
			}
		}

		result = &SignResult{
			ID:        signed.ID,
			Status:    signed.Status,
			SignedAt:  signed.UpdatedAt, // mapper sets updated_at = now() on sign
			IsIdempot: false,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// FileCommand is the input for POST /v1/pecas/:id/file.
type FileCommand struct {
	TenantID      string
	DraftID       string
	Receipt       map[string]any
	CourtRecordID string // optional override
	FiledAt       *time.Time
}

// FileResult carries the response for a successful file.
type FileResult struct {
	PetitionID   string
	DraftID      string
	FiledAt      time.Time
	Receipt      map[string]any
	IsIdempotent bool // true when petition already existed (200, not 201)
}

// File implements POST /v1/pecas/:id/file. In ONE tenant-scoped tx:
//  1. GetDraftByID (tenant guard → 404).
//  2. Guard: status must be SIGNED.
//  3. Check for existing petition → idempotent (return 200).
//  4. Resolve court_record_id: body override > intimation → ErrCourtRecordRequired.
//  5. InsertPetition + UpdateSagaStateAndSignedAt(saga_state=FILED).
//  6. outbox.Publish(petition.filed) — same tx.
func (uc *UseCase) File(ctx context.Context, cmd FileCommand) (*FileResult, error) {
	var result *FileResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d, err := uc.rw.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}

		// Guard: must be SIGNED.
		if d.Status != StatusSigned {
			return ErrDraftNotSigned
		}

		// Check for existing petition (idempotent).
		existing, err := uc.rw.GetPetitionByDraftID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}
		if existing != nil {
			result = &FileResult{
				PetitionID:   existing.ID,
				DraftID:      existing.DraftID,
				FiledAt:      existing.FiledAt,
				Receipt:      existing.Receipt,
				IsIdempotent: true,
			}
			return nil
		}

		// Resolve court_record_id.
		courtRecordID := cmd.CourtRecordID
		if courtRecordID == "" && d.IntimationID != "" {
			crid, crErr := uc.rw.GetCourtRecordIDByIntimation(ctx, tx, cmd.TenantID, d.IntimationID)
			if crErr != nil {
				return crErr
			}
			courtRecordID = crid
		}
		if courtRecordID == "" {
			return ErrCourtRecordRequired
		}

		filedAt := uc.now()
		if cmd.FiledAt != nil {
			filedAt = *cmd.FiledAt
		}

		petition, err := uc.rw.InsertPetition(ctx, tx, &Petition{
			DraftID:       cmd.DraftID,
			CourtRecordID: courtRecordID,
			FiledAt:       filedAt,
			Receipt:       cmd.Receipt,
		})
		if err != nil {
			return err
		}

		// Flip saga_state to FILED.
		if _, err := uc.rw.UpdateSagaState(ctx, tx, cmd.DraftID, cmd.TenantID, SagaStateFiled, false, ""); err != nil {
			return err
		}

		// Emit petition.filed event (outbox).
		if uc.outbox != nil {
			ev := newPetitionFiled(petition, cmd.TenantID)
			if err := uc.outbox.Publish(ctx, tx, ev); err != nil {
				return err
			}
		}

		result = &FileResult{
			PetitionID: petition.ID,
			DraftID:    petition.DraftID,
			FiledAt:    petition.FiledAt,
			Receipt:    petition.Receipt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ResultCommand is the input for PATCH /v1/pecas/:id/result.
type ResultCommand struct {
	TenantID       string
	DraftID        string
	ObservedResult string
}

// ResultResult carries the response for a successful result update.
type ResultResult struct {
	PetitionID     string
	ObservedResult string
}

// Result implements PATCH /v1/pecas/:id/result. In ONE tenant-scoped tx:
//  1. Guard: petition must exist for this draft → ErrPetitionNotFound.
//  2. UpdateObservedResult.
func (uc *UseCase) Result(ctx context.Context, cmd ResultCommand) (*ResultResult, error) {
	var result *ResultResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		p, err := uc.rw.UpdateObservedResult(ctx, tx, cmd.TenantID, cmd.DraftID, cmd.ObservedResult)
		if err != nil {
			return err
		}
		result = &ResultResult{
			PetitionID:     p.ID,
			ObservedResult: p.ObservedResult,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ExportCommand is the input for GET /v1/pecas/:id/export.
type ExportCommand struct {
	TenantID string
	DraftID  string
	Format   string // "docx" or "pdf"
}

// ExportResult carries the presigned URL for download.
type ExportResult struct {
	URL       string
	ExpiresIn int // seconds
}

// Export implements GET /v1/pecas/:id/export. It runs in a read-only tx to fetch
// the draft content, then generates a presigned GET URL. In v0, content is served
// as text/plain.
func (uc *UseCase) Export(ctx context.Context, cmd ExportCommand) (*ExportResult, error) {
	if uc.storage == nil {
		return nil, ErrExportFormatInvalid
	}

	var result *ExportResult
	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d, err := uc.rw.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}

		if d.Content == "" {
			return ErrDraftNoContent
		}

		// v0: presign the draft content as text/plain. The storage key is derived
		// from the tenant + draft id. In production, content would be in S3 and
		// the key would come from the draft row; for v0 we use the content column.
		key := cmd.TenantID + "/drafts/" + d.ID + "/content.txt"
		ttl := 15 * time.Minute
		url, err := uc.storage.PresignedGet(ctx, key, ttl)
		if err != nil {
			return err
		}

		result = &ExportResult{
			URL:       url,
			ExpiresIn: int(ttl.Seconds()),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListByProcessQuery is the input for GET /v1/processos/:id/pecas.
type ListByProcessQuery struct {
	TenantID    string
	CaseID      string
	LastCreated string // RFC3339 sort value; max sentinel for first page
	LastID      string
	Limit       int
}

// ListAllQuery is the input for GET /v1/pecas (tenant library).
type ListAllQuery struct {
	TenantID    string
	PieceType   string // optional filter
	Status      string // optional filter
	LastCreated string
	LastID      string
	Limit       int
}

// ListByProcess implements GET /v1/processos/:id/pecas. Runs in a read-only tx.
func (uc *UseCase) ListByProcess(ctx context.Context, q ListByProcessQuery) (DraftListResult, error) {
	var result DraftListResult
	err := uc.repo.Do(ctx, q.TenantID, func(tx database.Tx) error {
		rows, err := uc.rw.ListDraftsByProcess(ctx, tx, q.TenantID, q.CaseID, q.LastCreated, q.LastID, q.Limit+1)
		if err != nil {
			return err
		}
		hasMore := false
		if len(rows) > q.Limit {
			rows, hasMore = rows[:q.Limit], true
		}
		result = DraftListResult{Items: rows, HasMore: hasMore}
		return nil
	})
	if err != nil {
		return DraftListResult{}, err
	}
	return result, nil
}

// ListAll implements GET /v1/pecas (tenant library). Runs in a read-only tx.
func (uc *UseCase) ListAll(ctx context.Context, q ListAllQuery) (DraftListResult, error) {
	var result DraftListResult
	err := uc.repo.Do(ctx, q.TenantID, func(tx database.Tx) error {
		rows, err := uc.rw.ListDraftsAll(ctx, tx, q.TenantID, q.PieceType, q.Status, q.LastCreated, q.LastID, q.Limit+1)
		if err != nil {
			return err
		}
		hasMore := false
		if len(rows) > q.Limit {
			rows, hasMore = rows[:q.Limit], true
		}
		result = DraftListResult{Items: rows, HasMore: hasMore}
		return nil
	})
	if err != nil {
		return DraftListResult{}, err
	}
	return result, nil
}
