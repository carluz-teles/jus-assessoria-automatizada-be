package draft

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
	"github.com/jusassessoria/platform/lib/apperr"
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
	// GetProvidencesForIntimation loads the OPEN/DONE tasks linked to the draft's
	// intimation. Surfaced in the FE sidebar (Peça v2). Empty slice for drafts
	// without an intimation or with no linked tasks.
	GetProvidencesForIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) ([]Providence, error)
	// UpdateDraftContent patches content (+optional title, +optional structured
	// content) and bumps updated_at. When structured is non-nil it is dual-written
	// to structured_content jsonb; nil leaves that column untouched. A no-match
	// (wrong id or foreign tenant) is ErrDraftNotFound.
	UpdateDraftContent(ctx context.Context, tx database.Tx, draftID, tenantID, content string, title *string, structured *StructuredContent) (*PatchResult, error)
	// UpdateDraftContentHtml persiste o HTML rico do editor Tiptap
	// (Fase B). A partir desse save, content_html vira source-of-truth
	// pro renderer PDF; structured_content fica congelado. Escopo
	// (draftID, tenantID). No-match → ErrDraftNotFound.
	UpdateDraftContentHtml(ctx context.Context, tx database.Tx, draftID, tenantID, html string) error
	// GetDraftDetail runs the JOIN read model for GET /v1/pecas/:id. A miss is
	// ErrDraftNotFound.
	GetDraftDetail(ctx context.Context, tx database.Tx, tenantID, draftID string) (*DraftDetailView, error)

	// UpdateAuthorship flips draft.authorship (assistant → human_taken). Idempotent
	// at the DB level. A no-match is ErrDraftNotFound. Returns the updated Draft
	// (Peça v2, migration 0056).
	UpdateAuthorship(ctx context.Context, tx database.Tx, draftID, tenantID, authorship string) (*Draft, error)

	// WriteBackStructuredContent persists a lazily-parsed StructuredContent to
	// draft.structured_content — only when the column is still NULL (a WHERE guard
	// makes it a no-op if a concurrent writer already populated it). Best-effort;
	// the caller does not check RowsAffected. (Peça v2, migration 0056).
	WriteBackStructuredContent(ctx context.Context, tx database.Tx, draftID, tenantID string, sc *StructuredContent) error

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

	// SetGenerationParams persists the Gerar-time generation params
	// (tone/instructions/selected_theses, Fatia 5) chosen on
	// POST /v1/pecas/:id/generate. Called in the SAME tx as UpdateSagaState by
	// TriggerGeneration; the draft.generation_requested event payload does not
	// carry these — OnGenerationRequested rereads the draft row instead.
	SetGenerationParams(ctx context.Context, tx database.Tx, draftID, tenantID, tone, instructions string, theses []string) error

	// UpdateSagaState transitions the draft's saga_state column and optionally
	// overwrites content (when updateContent=true) and structured_content (when
	// structured is non-nil — dual-write for the DRAFTED path). Scoped to
	// (draftID, tenantID). A miss is ErrDraftNotFound.
	UpdateSagaState(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string, structured *StructuredContent) (*Draft, error)

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

	// ── Peticionamento methods (Fatia 4) ────────────────────────────────────

	// SignDraft transitions draft.status to SIGNED and sets signed_at. Guarded by
	// the caller to status ∈ {DRAFT, REVIEWED}. Idempotent: if already SIGNED,
	// returns the current row (no error). A miss is ErrDraftNotFound.
	SignDraft(ctx context.Context, tx database.Tx, draftID, tenantID string) (*Draft, error)

	// SignDraftWithPDF (Fatia 2b) transitions status=SIGNED + signed_at=now() +
	// signed_pdf_key=<storage key do PDF assinado>. Miss = ErrDraftNotFound.
	SignDraftWithPDF(ctx context.Context, tx database.Tx, draftID, tenantID, signedPDFKey string) (*Draft, error)

	// MarkSentToSigning marca sent_to_signing_at=now() (o gesto "usuário clicou
	// Enviar para assinatura", 0060). Idempotente: retorna nil sem erro quando
	// já estava setado (a query só afeta linhas com sent_to_signing_at IS NULL).
	// Miss real (draft não existe / tenant errado) → ErrDraftNotFound.
	MarkSentToSigning(ctx context.Context, tx database.Tx, draftID, tenantID string) error

	// RevertToConstruction nulla sent_to_signing_at (usuário voltou pra Construção).
	// Só permite quando signed_at IS NULL — depois de assinada, não volta. Miss ou
	// já-assinada → ErrDraftNotFound (a UI trata ambos como "não posso reverter").
	RevertToConstruction(ctx context.Context, tx database.Tx, draftID, tenantID string) error

	// MarkFiled marca filed_at + filing_number opcional (0060). Requer status=SIGNED
	// e filed_at IS NULL (idempotência dura — não re-protocola). Miss ou pré-condição
	// falha → ErrDraftNotFound.
	MarkFiled(ctx context.Context, tx database.Tx, draftID, tenantID, filingNumber string) error

	// UpdateFilingNumber atualiza APENAS o filing_number quando o advogado
	// esqueceu de digitar na 1ª vez OU digitou errado. Guard SQL: só grava
	// quando filing_number atual IS NULL — nunca zera um valor existente.
	// No-op silencioso quando não bate (WHERE não encontra). Usado no branch
	// idempotente do File.
	UpdateFilingNumber(ctx context.Context, tx database.Tx, draftID, tenantID, filingNumber string) error

	// InsertPetition persists the filed petition. Returns the persisted entity.
	InsertPetition(ctx context.Context, tx database.Tx, p *Petition) (*Petition, error)

	// GetPetitionByDraftID returns the existing petition for a draft, or nil if none.
	// No error when nil — the caller checks for double-filing.
	GetPetitionByDraftID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Petition, error)

	// UpdateObservedResult patches the observed_result on a petition. A miss
	// (wrong id or foreign tenant) is ErrPetitionNotFound.
	UpdateObservedResult(ctx context.Context, tx database.Tx, tenantID, draftID, result string) (*Petition, error)

	// UpdateSagaStateAndSignedAt transitions saga_state AND sets signed_at in one
	// call. Used by the File use case to set FILED + signed_at atomically.
	UpdateSagaStateAndSignedAt(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string) (*Draft, error)

	// ListDraftsByProcess returns draft list items for a given case_id, keyset
	// paginated (created_at DESC, id DESC). Over-fetches by 1 for hasMore.
	ListDraftsByProcess(ctx context.Context, tx database.Tx, tenantID, caseID string, lastCreated string, lastID string, limit int) ([]DraftListItem, error)

	// CountDraftsByProcess returns the total peças matching ListDraftsByProcess's
	// WHERE (same court_record_id + tenant_id), no pagination.
	CountDraftsByProcess(ctx context.Context, tx database.Tx, tenantID, caseID string) (int64, error)

	// ListDraftsAll returns draft list items across all processes for a tenant,
	// keyset paginated (created_at DESC, id DESC). Filtros opcionais (vazio = ignorado):
	//   pieceType, status  — colunas próprias do draft
	//   workflowState      — "aguardando_assinatura" | "aguardando_protocolo"
	//   urgencia           — "atraso" | "hoje" (contra deadline.end_date da intimation)
	//   assignee           — d.created_by (o chip "Minhas"), já resolvido pelo handler
	// Over-fetches por 1 pra hasMore.
	ListDraftsAll(ctx context.Context, tx database.Tx, tenantID, pieceType, status, workflowState, urgencia, assignee, lastCreated, lastID string, limit int) ([]DraftListItem, error)

	// CountDraftsAll returns the total peças matching ListDraftsAll's WHERE (same
	// pieceType/status/workflowState/urgencia/assignee filters), no pagination.
	CountDraftsAll(ctx context.Context, tx database.Tx, tenantID, pieceType, status, workflowState, urgencia, assignee string) (int64, error)

	// GetCourtRecordIDByIntimation returns the court_record_id for an intimation,
	// or empty string if the intimation has no linked court record.
	GetCourtRecordIDByIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) (string, error)

	// ── e-SAJ credential methods (Fatia 1 — peticionamento automático) ──────

	// InsertEsajCredential persiste login + envelope KMS + consentimento. A miss é
	// apperr (infra); o unique parcial já-ativa vira ErrEsajCredentialConflict.
	InsertEsajCredential(ctx context.Context, tx database.Tx, tenantID, ownerUserID, login string, env *Envelope, termsVersion, termsAcceptedAt, termsAcceptedBy string) (*EsajCredential, error)
	// ListEsajCredentials devolve a metadata pública das credenciais ativas do tenant.
	ListEsajCredentials(ctx context.Context, tx database.Tx, tenantID string) ([]EsajCredential, error)
	// GetActiveEsajCredential carrega a credencial ativa do usuário INCLUINDO o
	// envelope (pra decifrar a senha no worker). Miss → pgx.ErrNoRows.
	GetActiveEsajCredential(ctx context.Context, tx database.Tx, tenantID, ownerUserID string) (*EsajCredentialEnvelope, error)
	// RevokeEsajCredential faz soft-delete idempotente (só marca revoked_at quando ativa).
	RevokeEsajCredential(ctx context.Context, tx database.Tx, tenantID, id string) error

	// ── filing_attempt methods (Fatia 1 — peticionamento automático) ────────

	// InsertFilingAttempt congela o snapshot do PDF (key + sha256) e cria a
	// tentativa ENFILEIRADO. O unique parcial (draft_id ativo) é a barreira de
	// idempotência contra duplo-clique.
	InsertFilingAttempt(ctx context.Context, tx database.Tx, tenantID, draftID, pdfStorageKey, pdfSha256, requestedBy string) (*FilingAttempt, error)
	// GetActiveFilingAttempt devolve a tentativa ativa (ENFILEIRADO/PROTOCOLANDO)
	// do draft, ou nil se não houver.
	GetActiveFilingAttempt(ctx context.Context, tx database.Tx, tenantID, draftID string) (*FilingAttempt, error)
	// GetLatestFilingAttempt devolve a ÚLTIMA tentativa do draft (qualquer status,
	// inclusive PROTOCOLADO/FALHOU) — usada pelo endpoint de status para refletir
	// o estado terminal. Miss (sem nenhuma) → apperr not-found.
	GetLatestFilingAttempt(ctx context.Context, tx database.Tx, draftID string) (*FilingAttempt, error)
	// GetFilingAttempt devolve a tentativa por id. Miss → apperr not-found.
	GetFilingAttempt(ctx context.Context, tx database.Tx, tenantID, id string) (*FilingAttempt, error)
	// MarkFilingProtocolando transiciona ENFILEIRADO→PROTOCOLANDO. Guarda status:
	// 0 linhas afetadas = outro worker já ganhou (idempotência de retry).
	MarkFilingProtocolando(ctx context.Context, tx database.Tx, id string) error
	// MarkFilingProtocolado finaliza com sucesso (petition_id + filing_number + screenshots).
	MarkFilingProtocolado(ctx context.Context, tx database.Tx, id, petitionID, filingNumber string, screenshotKeys []string) (*FilingAttempt, error)
	// MarkFilingFailed finaliza com falha (reason).
	MarkFilingFailed(ctx context.Context, tx database.Tx, id, reason string) error
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
	// created_by opcional — vazio vira UUID zero e o NULLIF no SQL grava NULL,
	// mantendo compat com callers que ainda não passam o principal.
	createdByRaw, _ := parseUUID(d.CreatedBy)
	row, err := draftdb.New(tx).InsertDraft(ctx, draftdb.InsertDraftParams{
		TenantID:     tenantID,
		CaseID:       optUUID(d.CaseID),
		IntimationID: optUUID(d.IntimationID),
		PieceType:    d.PieceType,
		Title:        d.Title,
		Content:      textToNull(d.Content),
		Column7:      createdByRaw,
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

func (r *pgRepository) GetProvidencesForIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) ([]Providence, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	iid, err := parseUUID(intimationID)
	if err != nil {
		return nil, err
	}

	rows, err := draftdb.New(tx).GetProvidencesForIntimation(ctx, draftdb.GetProvidencesForIntimationParams{
		TenantID:     tid,
		IntimationID: pgtype.UUID{Bytes: iid, Valid: true},
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]Providence, 0, len(rows))
	for _, r := range rows {
		out = append(out, Providence{
			ID:     r.ID.String(),
			Title:  r.Title,
			Kind:   derefString(r.Kind),
			Source: r.Source,
			Status: r.Status,
		})
	}
	return out, nil
}

func (r *pgRepository) UpdateDraftContentHtml(ctx context.Context, tx database.Tx, draftID, tenantID, html string) error {
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	_, err = draftdb.New(tx).UpdateDraftContentHtml(ctx, draftdb.UpdateDraftContentHtmlParams{
		ID:          did,
		TenantID:    tid,
		ContentHtml: &html,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDraftNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) UpdateDraftContent(ctx context.Context, tx database.Tx, draftID, tenantID, content string, title *string, structured *StructuredContent) (*PatchResult, error) {
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

	updateStructured := structured != nil
	structuredJSON := structuredContentToJSON(structured)

	row, err := draftdb.New(tx).UpdateDraftContent(ctx, draftdb.UpdateDraftContentParams{
		ID:                did,
		TenantID:          tid,
		Content:           textToNull(content),
		Column4:           updateTitle,
		Title:             titleVal,
		Column6:           updateStructured,
		StructuredContent: structuredJSON,
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

// UpdateAuthorship flips the peça's authorship marker (assistant ↔ human_taken).
// Reused by POST /v1/pecas/:id/assume-authorship (Peça v2).
func (r *pgRepository) UpdateAuthorship(ctx context.Context, tx database.Tx, draftID, tenantID, authorship string) (*Draft, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).UpdateDraftAuthorship(ctx, draftdb.UpdateDraftAuthorshipParams{
		ID:         did,
		TenantID:   tid,
		Authorship: authorship,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromAuthorshipRow(row), nil
}

// WriteBackStructuredContent lazily persists a parsed StructuredContent to a
// draft that still has structured_content = NULL. The SQL guards on IS NULL so
// concurrent writes are safe. Fire-and-forget (no RowsAffected check).
func (r *pgRepository) WriteBackStructuredContent(ctx context.Context, tx database.Tx, draftID, tenantID string, sc *StructuredContent) error {
	if sc == nil {
		return nil
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	if err := draftdb.New(tx).WriteBackStructuredContent(ctx, draftdb.WriteBackStructuredContentParams{
		ID:                did,
		TenantID:          tid,
		StructuredContent: structuredContentToJSON(sc),
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
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

func (r *pgRepository) SetGenerationParams(ctx context.Context, tx database.Tx, draftID, tenantID, tone, instructions string, theses []string) error {
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	if theses == nil {
		theses = []string{}
	}

	if err := draftdb.New(tx).SetGenerationParams(ctx, draftdb.SetGenerationParamsParams{
		ID:             did,
		TenantID:       tid,
		Tone:           tone,
		Instructions:   textToNull(instructions),
		SelectedTheses: theses,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) UpdateSagaState(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string, structured *StructuredContent) (*Draft, error) {
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

	updateStructured := structured != nil
	structuredJSON := structuredContentToJSON(structured)

	row, err := draftdb.New(tx).UpdateSagaState(ctx, draftdb.UpdateSagaStateParams{
		ID:                did,
		TenantID:          tid,
		SagaState:         sagaState,
		Column4:           updateContent,
		Content:           contentPtr,
		Column6:           updateStructured,
		StructuredContent: structuredJSON,
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

// ── Peticionamento repository methods (Fatia 4) ─────────────────────────────

func (r *pgRepository) SignDraftWithPDF(ctx context.Context, tx database.Tx, draftID, tenantID, signedPDFKey string) (*Draft, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	row, err := draftdb.New(tx).SignDraftWithPDF(ctx, draftdb.SignDraftWithPDFParams{
		ID:           did,
		TenantID:     tid,
		SignedPdfKey: &signedPDFKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromSignRow(signRowFromSignWithPDF(row)), nil
}

// signRowFromSignWithPDF adapta SignDraftWithPDFRow → SignDraftRow (mesmo shape).
func signRowFromSignWithPDF(r draftdb.SignDraftWithPDFRow) draftdb.SignDraftRow {
	return draftdb.SignDraftRow{
		ID:           r.ID,
		TenantID:     r.TenantID,
		CaseID:       r.CaseID,
		IntimationID: r.IntimationID,
		PieceType:    r.PieceType,
		Title:        r.Title,
		Content:      r.Content,
		Status:       r.Status,
		SagaState:    r.SagaState,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		SignedAt:     r.SignedAt,
	}
}

func (r *pgRepository) SignDraft(ctx context.Context, tx database.Tx, draftID, tenantID string) (*Draft, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).SignDraft(ctx, draftdb.SignDraftParams{
		ID:       did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromSignRow(row), nil
}

func (r *pgRepository) MarkSentToSigning(ctx context.Context, tx database.Tx, draftID, tenantID string) error {
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	// A query só afeta linhas com sent_to_signing_at IS NULL — 0 rows = já
	// setado (idempotente, sem erro) OU draft não existe. Diferenciar exigiria
	// SELECT extra; pragmaticamente aceitamos: se draft não existe, o próximo
	// GetDraftDetail vai retornar NotFound e a UI trata.
	_, err = draftdb.New(tx).MarkSentToSigning(ctx, draftdb.MarkSentToSigningParams{
		ID: did, TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // idempotente
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) RevertToConstruction(ctx context.Context, tx database.Tx, draftID, tenantID string) error {
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	// 0 rows = draft não existe OU já foi assinado (a query exige signed_at IS
	// NULL). Ambos os casos surface como ErrDraftNotFound: a UI trata como "não
	// posso reverter" e refetch mostra o estado real.
	_, err = draftdb.New(tx).RevertToConstruction(ctx, draftdb.RevertToConstructionParams{
		ID: did, TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDraftNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) MarkFiled(ctx context.Context, tx database.Tx, draftID, tenantID, filingNumber string) error {
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	var fn *string
	if filingNumber != "" {
		fn = &filingNumber
	}
	_, err = draftdb.New(tx).MarkFiled(ctx, draftdb.MarkFiledParams{
		ID:           did,
		TenantID:     tid,
		FilingNumber: fn,
		FiledAt:      pgtype.Timestamptz{}, // NULL → COALESCE(now())
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDraftNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) UpdateFilingNumber(ctx context.Context, tx database.Tx, draftID, tenantID, filingNumber string) error {
	did, err := parseUUID(draftID)
	if err != nil {
		return err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	var fn *string
	if filingNumber != "" {
		fn = &filingNumber
	}
	// :exec — não retorna row. WHERE filing_number IS NULL no SQL garante
	// que só grava quando ainda não tem valor; caso contrário no-op.
	return draftdb.New(tx).UpdateFilingNumber(ctx, draftdb.UpdateFilingNumberParams{
		ID:           did,
		TenantID:     tid,
		FilingNumber: fn,
	})
}

func (r *pgRepository) InsertPetition(ctx context.Context, tx database.Tx, p *Petition) (*Petition, error) {
	did, err := parseUUID(p.DraftID)
	if err != nil {
		return nil, err
	}
	crid, err := parseUUID(p.CourtRecordID)
	if err != nil {
		return nil, err
	}

	receiptJSON, err := marshalJSON(p.Receipt)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).InsertPetition(ctx, draftdb.InsertPetitionParams{
		DraftID:       did,
		CourtRecordID: crid,
		FiledAt:       timeToTimestamptz(p.FiledAt),
		Receipt:       receiptJSON,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return petitionFromRow(row), nil
}

func (r *pgRepository) GetPetitionByDraftID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Petition, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetPetitionByDraftID(ctx, draftdb.GetPetitionByDraftIDParams{
		DraftID:  did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no petition yet — not an error
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	p := petitionFromRow(row)
	return p, nil
}

func (r *pgRepository) UpdateObservedResult(ctx context.Context, tx database.Tx, tenantID, draftID, result string) (*Petition, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).UpdateObservedResult(ctx, draftdb.UpdateObservedResultParams{
		DraftID:        did,
		TenantID:       tid,
		ObservedResult: &result,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPetitionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &Petition{
		ID:             row.ID.String(),
		DraftID:        row.DraftID.String(),
		ObservedResult: derefString(row.ObservedResult),
	}, nil
}

func (r *pgRepository) UpdateSagaStateAndSignedAt(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string) (*Draft, error) {
	// For now, reuse UpdateSagaState (signed_at is set by SignDraft).
	// This method exists for the File use case to update saga_state atomically.
	return r.UpdateSagaState(ctx, tx, draftID, tenantID, sagaState, false, "", nil)
}

func (r *pgRepository) ListDraftsByProcess(ctx context.Context, tx database.Tx, tenantID, caseID, lastCreated, lastID string, limit int) ([]DraftListItem, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	cid, err := parseUUID(caseID)
	if err != nil {
		return nil, err
	}

	created, err := parseCursorTime(lastCreated)
	if err != nil {
		return nil, err
	}
	lid, err := parseUUID(lastID)
	if err != nil {
		return nil, err
	}

	rows, err := draftdb.New(tx).ListDraftsByProcess(ctx, draftdb.ListDraftsByProcessParams{
		TenantID: tid,
		ID:       cid,
		Column3:  timeToTimestamptz(created),
		Column4:  lid,
		Limit:    int32(limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	items := make([]DraftListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, draftListItemFromProcessRow(row))
	}
	return items, nil
}

func (r *pgRepository) CountDraftsByProcess(ctx context.Context, tx database.Tx, tenantID, caseID string) (int64, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return 0, err
	}
	cid, err := parseUUID(caseID)
	if err != nil {
		return 0, err
	}

	total, err := draftdb.New(tx).CountDraftsByProcess(ctx, draftdb.CountDraftsByProcessParams{
		TenantID: tid,
		ID:       cid,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

func (r *pgRepository) ListDraftsAll(ctx context.Context, tx database.Tx, tenantID, pieceType, status, workflowState, urgencia, assignee, lastCreated, lastID string, limit int) ([]DraftListItem, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	created, err := parseCursorTime(lastCreated)
	if err != nil {
		return nil, err
	}
	lid, err := parseUUID(lastID)
	if err != nil {
		return nil, err
	}

	rows, err := draftdb.New(tx).ListDraftsAll(ctx, draftdb.ListDraftsAllParams{
		TenantID:   tid,
		Column2:    pieceType,
		Column3:    status,
		Column4:    timeToTimestamptz(created),
		Column5:    lid,
		Column6:    workflowState,
		Column7:    urgencia,
		Limit:      int32(limit),
		AssigneeID: optUUID(assignee),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	items := make([]DraftListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, draftListItemFromAllRow(row))
	}
	return items, nil
}

func (r *pgRepository) CountDraftsAll(ctx context.Context, tx database.Tx, tenantID, pieceType, status, workflowState, urgencia, assignee string) (int64, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return 0, err
	}

	total, err := draftdb.New(tx).CountDraftsAll(ctx, draftdb.CountDraftsAllParams{
		TenantID:   tid,
		Column2:    pieceType,
		Column3:    status,
		Column4:    workflowState,
		Column5:    urgencia,
		AssigneeID: optUUID(assignee),
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

func (r *pgRepository) GetCourtRecordIDByIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) (string, error) {
	iid, err := parseUUID(intimationID)
	if err != nil {
		return "", err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	row, err := draftdb.New(tx).GetCourtRecordIDByIntimation(ctx, draftdb.GetCourtRecordIDByIntimationParams{
		ID:       iid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // no court record linked — not an error
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return row.String(), nil
}

// parseCursorTime parses the RFC3339Nano sort value from the cursor. The max
// sentinel ("9999-12-31T23:59:59.999999Z") is valid RFC3339.
func parseCursorTime(s string) (time.Time, error) {
	if s == "" || s == maxCreatedAt {
		// max sentinel or empty → use max time for the first page.
		return time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, apperr.NewInvalid("invalid cursor timestamp: " + s)
	}
	return t, nil
}

// ── e-SAJ credential (Fatia 1 — peticionamento automático) ───────────────────

func (r *pgRepository) InsertEsajCredential(ctx context.Context, tx database.Tx, tenantID, ownerUserID, login string, env *Envelope, termsVersion, termsAcceptedAt, termsAcceptedBy string) (*EsajCredential, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	oid, err := parseUUID(ownerUserID)
	if err != nil {
		return nil, err
	}
	aid, err := parseUUID(termsAcceptedBy)
	if err != nil {
		return nil, err
	}
	acceptedAt, err := time.Parse(time.RFC3339, termsAcceptedAt)
	if err != nil {
		// caller validates; tolera empty caindo em now().
		acceptedAt = time.Now()
	}
	row, err := draftdb.New(tx).InsertEsajCredential(ctx, draftdb.InsertEsajCredentialParams{
		TenantID:        tid,
		OwnerUserID:     oid,
		Login:           login,
		Ciphertext:      env.Ciphertext,
		Nonce:           env.Nonce,
		WrappedDek:      env.WrappedDEK,
		KekRef:          env.KEKRef,
		TermsVersion:    termsVersion,
		TermsAcceptedAt: pgtype.Timestamptz{Time: acceptedAt, Valid: true},
		TermsAcceptedBy: aid,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEsajCredentialConflict
		}
		return nil, database.WrapInfra(err)
	}
	return &EsajCredential{
		ID:              row.ID.String(),
		TenantID:        row.TenantID.String(),
		OwnerUserID:     row.OwnerUserID.String(),
		Login:           row.Login,
		TermsVersion:    row.TermsVersion,
		TermsAcceptedAt: row.TermsAcceptedAt.Time,
		CreatedAt:       row.CreatedAt.Time,
	}, nil
}

func (r *pgRepository) ListEsajCredentials(ctx context.Context, tx database.Tx, tenantID string) ([]EsajCredential, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	rows, err := draftdb.New(tx).ListEsajCredentials(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]EsajCredential, 0, len(rows))
	for i := range rows {
		out = append(out, EsajCredential{
			ID:              rows[i].ID.String(),
			TenantID:        rows[i].TenantID.String(),
			OwnerUserID:     rows[i].OwnerUserID.String(),
			Login:           rows[i].Login,
			TermsVersion:    rows[i].TermsVersion,
			TermsAcceptedAt: rows[i].TermsAcceptedAt.Time,
			CreatedAt:       rows[i].CreatedAt.Time,
		})
	}
	return out, nil
}

func (r *pgRepository) GetActiveEsajCredential(ctx context.Context, tx database.Tx, tenantID, ownerUserID string) (*EsajCredentialEnvelope, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	oid, err := parseUUID(ownerUserID)
	if err != nil {
		return nil, err
	}
	row, err := draftdb.New(tx).GetActiveEsajCredential(ctx, draftdb.GetActiveEsajCredentialParams{
		TenantID:    tid,
		OwnerUserID: oid,
	})
	if err != nil {
		return nil, err // pgx.ErrNoRows propagado p/ o use case mapear
	}
	return &EsajCredentialEnvelope{
		ID:         row.ID.String(),
		Login:      row.Login,
		Ciphertext: row.Ciphertext,
		Nonce:      row.Nonce,
		WrappedDEK: row.WrappedDek,
		KEKRef:     row.KekRef,
	}, nil
}

func (r *pgRepository) RevokeEsajCredential(ctx context.Context, tx database.Tx, tenantID, id string) error {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	iid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := draftdb.New(tx).RevokeEsajCredential(ctx, draftdb.RevokeEsajCredentialParams{
		ID:       iid,
		TenantID: tid,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// ── filing_attempt (Fatia 1 — peticionamento automático) ─────────────────────

func (r *pgRepository) InsertFilingAttempt(ctx context.Context, tx database.Tx, tenantID, draftID, pdfStorageKey, pdfSha256, requestedBy string) (*FilingAttempt, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	rid, err := parseUUID(requestedBy)
	if err != nil {
		return nil, err
	}
	row, err := draftdb.New(tx).InsertFilingAttempt(ctx, draftdb.InsertFilingAttemptParams{
		TenantID:      tid,
		DraftID:       did,
		PdfStorageKey: pdfStorageKey,
		PdfSha256:     pdfSha256,
		RequestedBy:   rid,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrFilingAttemptConflict
		}
		return nil, database.WrapInfra(err)
	}
	return &FilingAttempt{
		ID:            row.ID.String(),
		DraftID:       row.DraftID.String(),
		Status:        row.Status,
		RequestedBy:   row.RequestedBy.String(),
		RequestedAt:   row.RequestedAt.Time,
		PdfStorageKey: row.PdfStorageKey,
		PdfSha256:     row.PdfSha256,
	}, nil
}

func (r *pgRepository) GetActiveFilingAttempt(ctx context.Context, tx database.Tx, tenantID, draftID string) (*FilingAttempt, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	row, err := draftdb.New(tx).GetActiveFilingAttempt(ctx, draftdb.GetActiveFilingAttemptParams{TenantID: tid, DraftID: did})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &FilingAttempt{
		ID:            row.ID.String(),
		DraftID:       row.DraftID.String(),
		Status:        row.Status,
		PdfStorageKey: row.PdfStorageKey,
		PdfSha256:     row.PdfSha256,
	}, nil
}

func (r *pgRepository) GetLatestFilingAttempt(ctx context.Context, tx database.Tx, draftID string) (*FilingAttempt, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	row, err := draftdb.New(tx).GetLatestFilingAttempt(ctx, did)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFilingAttemptNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return filingAttemptFromLatestRow(row), nil
}

func (r *pgRepository) GetFilingAttempt(ctx context.Context, tx database.Tx, tenantID, id string) (*FilingAttempt, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	iid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	row, err := draftdb.New(tx).GetFilingAttempt(ctx, draftdb.GetFilingAttemptParams{TenantID: tid, ID: iid})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFilingAttemptNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return filingAttemptFromRow(row), nil
}

func (r *pgRepository) MarkFilingProtocolando(ctx context.Context, tx database.Tx, id string) error {
	iid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := draftdb.New(tx).MarkFilingProtocolando(ctx, iid); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) MarkFilingProtocolado(ctx context.Context, tx database.Tx, id, petitionID, filingNumber string, screenshotKeys []string) (*FilingAttempt, error) {
	iid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	pid, err := parseUUID(petitionID)
	if err != nil {
		return nil, err
	}
	if screenshotKeys == nil {
		screenshotKeys = []string{}
	}
	row, err := draftdb.New(tx).MarkFilingProtocolado(ctx, draftdb.MarkFilingProtocoladoParams{
		ID:             iid,
		PetitionID:     pgtype.UUID{Bytes: pid, Valid: true},
		FilingNumber:   &filingNumber,
		ScreenshotKeys: screenshotKeys,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	fin := &FilingAttempt{ID: row.ID.String(), Status: row.Status, FinishedAt: row.FinishedAt.Time}
	if row.FilingNumber != nil {
		fin.FilingNumber = *row.FilingNumber
	}
	return fin, nil
}

func (r *pgRepository) MarkFilingFailed(ctx context.Context, tx database.Tx, id, reason string) error {
	iid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := draftdb.New(tx).MarkFilingFailed(ctx, draftdb.MarkFilingFailedParams{ID: iid, FailureReason: &reason}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func filingAttemptFromRow(row draftdb.GetFilingAttemptRow) *FilingAttempt {
	petitionID := ""
	if row.PetitionID.Valid {
		petitionID = uuid.UUID(row.PetitionID.Bytes).String()
	}
	failure := ""
	if row.FailureReason != nil {
		failure = *row.FailureReason
	}
	keys := row.ScreenshotKeys
	if keys == nil {
		keys = []string{}
	}
	return &FilingAttempt{
		ID:             row.ID.String(),
		DraftID:        row.DraftID.String(),
		PetitionID:     petitionID,
		Status:         row.Status,
		PdfStorageKey:  row.PdfStorageKey,
		PdfSha256:      row.PdfSha256,
		RequestedBy:    row.RequestedBy.String(),
		RequestedAt:    row.RequestedAt.Time,
		StartedAt:      timestamptzToTime(row.StartedAt),
		FinishedAt:     timestamptzToTime(row.FinishedAt),
		FailureReason:  failure,
		ScreenshotKeys: keys,
	}
}

// filingAttemptFromLatestRow mapeia a linha da GetLatestFilingAttempt (todos os
// status, inclusive terminal) — espelha filingAttemptFromRow mas para o row type
// retornado por aquela query.
func filingAttemptFromLatestRow(row draftdb.GetLatestFilingAttemptRow) *FilingAttempt {
	petitionID := ""
	if row.PetitionID.Valid {
		petitionID = uuid.UUID(row.PetitionID.Bytes).String()
	}
	failure := ""
	if row.FailureReason != nil {
		failure = *row.FailureReason
	}
	filingNumber := ""
	if row.FilingNumber != nil {
		filingNumber = *row.FilingNumber
	}
	keys := row.ScreenshotKeys
	if keys == nil {
		keys = []string{}
	}
	return &FilingAttempt{
		ID:             row.ID.String(),
		DraftID:        row.DraftID.String(),
		PetitionID:     petitionID,
		Status:         row.Status,
		PdfStorageKey:  row.PdfStorageKey,
		PdfSha256:      row.PdfSha256,
		RequestedBy:    row.RequestedBy.String(),
		RequestedAt:    row.RequestedAt.Time,
		StartedAt:      timestamptzToTime(row.StartedAt),
		FinishedAt:     timestamptzToTime(row.FinishedAt),
		FailureReason:  failure,
		FilingNumber:   filingNumber,
		ScreenshotKeys: keys,
	}
}
