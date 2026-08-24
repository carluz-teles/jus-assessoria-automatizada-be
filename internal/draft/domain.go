package draft

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pdfreader "github.com/digitorus/pdf"
	pdfsign "github.com/digitorus/pdfsign/sign"

	"github.com/jusassessoria/platform/internal/draft/pdfgen"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/storage"
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
	repo       database.UnitOfWork
	rw         Repository
	now        func() time.Time
	outbox     OutboxPublisher
	storage    StoragePresigner
	pdfStorage PDFStorage
	certSigner CertSigner
	// tsaURL: endpoint RFC 3161 pra carimbo de tempo (PAdES-T). Vazio =
	// PAdES-BASIC (sem carimbo). Injetado por WithTSAURL. Ver docs/erd-pecas.md.
	tsaURL string
}

// OutboxPublisher is the narrow port for event publishing within a tx.
type OutboxPublisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// StoragePresigner is the narrow port for presigned URL generation.
type StoragePresigner interface {
	PresignedGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// PDFStorage é a porta que o Sign usa pra subir o PDF assinado direto pelo
// server (bypass presigned — o binário passa pelo api porque a assinatura
// é montada aqui). Implementado por lib/storage.Client (PutBytes/GetBytes).
type PDFStorage interface {
	PutBytes(ctx context.Context, key, contentType string, data []byte) error
	GetBytes(ctx context.Context, key string) ([]byte, error)
}

// CertSigner é a porta que o Sign usa pra pegar um crypto.Signer KMS-backed
// pra um certificado do tenant. Implementado pelo internal/certificate.
// A leaf + chain acompanham (pdfsign precisa delas pra montar o CMS PAdES).
type CertSigner interface {
	NewSigner(ctx context.Context, tenantID, certificateID string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, error)
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

// WithPDFStorage attaches the server-side blob storage (put/get). Required
// for Sign (Fatia 2b): the signed PDF is uploaded here and its key persisted
// on draft.signed_pdf_key. Without this option, Sign returns ErrPDFStorageUnavailable.
func WithPDFStorage(s PDFStorage) Option {
	return func(uc *UseCase) { uc.pdfStorage = s }
}

// WithCertSigner attaches the certificate signer port. Required for Sign
// (Fatia 2b). Without this option, Sign returns ErrCertSignerUnavailable.
func WithCertSigner(c CertSigner) Option {
	return func(uc *UseCase) { uc.certSigner = c }
}

// WithTSAURL habilita PAdES-T (carimbo de tempo RFC 3161) na assinatura.
// URL vazia = PAdES-BASIC (comportamento default). Provedores conhecidos:
// http://freetsa.org/tsr (grátis, dev), http://timestamp.digicert.com (prod).
// Sem esta option, assinatura sai sem carimbo — verificadores mostram "data
// da assinatura: relógio do assinante" (não confiável).
func WithTSAURL(url string) Option {
	return func(uc *UseCase) { uc.tsaURL = url }
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
	// StructuredContent is Peça v2's block-structured version. Non-nil means
	// dual-write: the plain-text `Content` is persisted alongside the JSONB
	// StructuredContent (source of truth for the FE). Nil leaves the
	// structured_content column untouched — the legacy PATCH path (pre-Fatia B).
	StructuredContent *StructuredContent
}

// Patch implements PATCH /v1/pecas/:id (autosave). Runs in a single tenant-scoped tx:
// updates content + optionally title + bumps updated_at. A missing or foreign-tenant
// draft is ErrDraftNotFound (→ 404). content empty is valid (editor cleared it).
// No event is emitted — autosave is not a domain event.
func (uc *UseCase) Patch(ctx context.Context, cmd PatchCommand) (*PatchResult, error) {
	var result *PatchResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		r, err := uc.rw.UpdateDraftContent(ctx, tx, cmd.DraftID, cmd.TenantID, cmd.Content, cmd.Title, cmd.StructuredContent)
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

		// Providences: tasks linked to the intimation, shown on the FE sidebar
		// (Peça v2). Empty for drafts without an intimation. Non-fatal on error
		// (a task read failure shouldn't break the peça detail).
		if v.Intimation != nil {
			provs, e := uc.rw.GetProvidencesForIntimation(ctx, tx, tenantID, v.Intimation.ID)
			if e == nil {
				v.Providences = provs
			}
		}

		// Parties: autor/réu/terceiros of the peça's case, shown on the FE
		// sidebar (Peça v2 — bloco PARTES). Empty for drafts without a process
		// (blank drafts). Non-fatal on error (same rationale as providences).
		if v.Process != nil {
			parties, e := uc.rw.GetPartiesForDraft(ctx, tx, tenantID, v.Process.CaseID)
			if e == nil {
				v.Parties = parties
			}
		}

		// Peça v2 (migration 0056): lazy backfill of structured_content for
		// drafts that were persisted before the Fatia B pipeline (or by a
		// legacy PATCH path). When the column is NULL but there IS content,
		// parse it here and write back best-effort — subsequent reads skip the
		// parser. When both are empty the peça is truly empty (never
		// generated) and we leave nil so the FE renders the empty state.
		if v.StructuredContent == nil && v.Content != "" {
			parsed := ParseStructured(v.Content)
			if parsed != nil {
				v.StructuredContent = parsed
				// Fire-and-forget: WHERE structured_content IS NULL guards
				// against a race with a concurrent writer. An infra error
				// here does NOT fail the read — the FE already got the
				// parsed shape in this response.
				_ = uc.rw.WriteBackStructuredContent(ctx, tx, v.ID, tenantID, parsed)
			}
		}

		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

// AssumeAuthorship flips draft.authorship to "human_taken" (Peça v2). The
// advogado clicked "Assumir autoria"; from now on the FE hides the Iterar tab
// and shows Revisão. Idempotent — a repeat call is a harmless UPDATE.
func (uc *UseCase) AssumeAuthorship(ctx context.Context, tenantID, draftID string) (*Draft, error) {
	var draft *Draft
	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		d, err := uc.rw.UpdateAuthorship(ctx, tx, draftID, tenantID, AuthorshipHumanTaken)
		if err != nil {
			return err
		}
		draft = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return draft, nil
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
	TenantID      string
	DraftID       string
	CertificateID string // Fatia 2b — cert usado pra assinar (obrigatório)
}

// SignResult carries the response for a successful sign.
type SignResult struct {
	ID           string
	Status       string
	SignedAt     time.Time
	SignedPDFKey string // storage key do PDF assinado (Fatia 2b)
	IsIdempot    bool   // true when the draft was already SIGNED (200, not fresh)
}

// ErrPDFStorageUnavailable / ErrCertSignerUnavailable — Fatia 2b só sobe se
// wire trouxe as duas deps. Sem elas o handler cai em 503.
var (
	ErrPDFStorageUnavailable = apperr.NewInfra("PDF storage não configurado", nil)
	ErrCertSignerUnavailable = apperr.NewInfra("cert signer não configurado", nil)
)

// Sign implements POST /v1/pecas/:id/sign (Fatia 2b — assinatura real). Fluxo:
//  1. Tenta idempotência (draft já SIGNED → 200 com dados atuais);
//  2. Guarda status ∈ {DRAFT, REVIEWED};
//  3. Renderiza PDF via pdfgen a partir do structured_content;
//  4. Cria crypto.Signer KMS-backed via certSigner.NewSigner(cert_id);
//  5. Aplica PAdES via digitorus/pdfsign (signer + leaf + chain no SignData);
//  6. Upload do PDF assinado no storage em {tenant}/pecas/{draft_id}/signed.pdf;
//  7. UPDATE draft: status=SIGNED, signed_at=now(), signed_pdf_key=<key>.
//  8. Emite draft.signed no outbox.
//
// Roda tudo na mesma tx da tabela draft — se o KMS ou o upload falhar, nada é
// persistido. O único efeito colateral externo é o PutBytes no storage antes do
// commit da tx; num rollback, o blob fica órfão (aceitável — GC eventual).
func (uc *UseCase) Sign(ctx context.Context, cmd SignCommand) (*SignResult, error) {
	if uc.pdfStorage == nil {
		return nil, ErrPDFStorageUnavailable
	}
	if uc.certSigner == nil {
		return nil, ErrCertSignerUnavailable
	}
	if cmd.CertificateID == "" {
		return nil, apperr.NewInvalid("certificate_id é obrigatório")
	}

	// 1) Fetch draft + structured_content via read model (evita novo query).
	var view *DraftDetailView
	if err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		v, e := uc.rw.GetDraftDetail(ctx, tx, cmd.TenantID, cmd.DraftID)
		if e != nil {
			return e
		}
		view = v
		return nil
	}); err != nil {
		return nil, err
	}
	// Idempotente: já assinado → devolve dados atuais sem re-assinar.
	if view.Status == StatusSigned {
		var signedAt time.Time
		if view.SignedAt != nil {
			signedAt = *view.SignedAt
		}
		return &SignResult{
			ID:        view.ID,
			Status:    view.Status,
			SignedAt:  signedAt,
			IsIdempot: true,
		}, nil
	}
	if view.Status != StatusDraft && view.Status != StatusReviewed {
		return nil, ErrInvalidStatusForSign
	}
	if view.StructuredContent == nil {
		return nil, apperr.NewInvalid("peça sem conteúdo estruturado — não é possível gerar PDF")
	}

	// 2) Render PDF (determinístico — mesma peça, mesmos bytes).
	cnj := ""
	if view.Process != nil {
		cnj = view.Process.CNJNumber
	}
	pdfBytes, err := pdfgen.Render(pdfgen.Draft{
		Title:    view.Title,
		CNJ:      cnj,
		Preamble: view.StructuredContent.Preamble.Paragraphs,
		Sections: sectionsToPDF(view.StructuredContent.Sections),
	})
	if err != nil {
		return nil, apperr.NewInfra("render pdf", err)
	}

	// 3) crypto.Signer KMS-backed.
	signer, leaf, intermediates, err := uc.certSigner.NewSigner(ctx, cmd.TenantID, cmd.CertificateID)
	if err != nil {
		return nil, err
	}

	// 4) PAdES via digitorus/pdfsign (+TSA se tsaURL configurada = PAdES-T).
	signedPDF, err := signPDFPAdES(ctx, pdfBytes, signer, leaf, intermediates, uc.tsaURL)
	if err != nil {
		return nil, apperr.NewInfra("sign pdf", err)
	}

	// 5) Upload no storage (server-side put — o binário passa por aqui).
	key := storage.NewKey(cmd.TenantID, fmt.Sprintf("pecas/%s", cmd.DraftID))
	if err := uc.pdfStorage.PutBytes(ctx, key, "application/pdf", signedPDF); err != nil {
		return nil, err
	}

	// 6) UPDATE draft + outbox na mesma tx (as duas coisas são atômicas —
	// o blob no storage pode ficar órfão em rollback, aceitável).
	var result *SignResult
	err = uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		signed, e := uc.rw.SignDraftWithPDF(ctx, tx, cmd.DraftID, cmd.TenantID, key)
		if e != nil {
			return e
		}
		if uc.outbox != nil {
			ev := newDraftSigned(signed)
			if e := uc.outbox.Publish(ctx, tx, ev); e != nil {
				return e
			}
		}
		result = &SignResult{
			ID:           signed.ID,
			Status:       signed.Status,
			SignedAt:     signed.UpdatedAt,
			SignedPDFKey: key,
			IsIdempot:    false,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// sectionsToPDF converte []StructuredSection (do read model) em []pdfgen.Section.
func sectionsToPDF(in []StructuredSection) []pdfgen.Section {
	out := make([]pdfgen.Section, 0, len(in))
	for _, s := range in {
		out = append(out, pdfgen.Section{
			Roman:      s.Roman,
			Title:      s.Title,
			Paragraphs: s.Paragraphs,
		})
	}
	return out
}

// signPDFPAdES aplica PAdES ao PDF. Se tsaURL for vazia, gera PAdES-BASIC
// (só a assinatura CMS embedded); se preenchida, chama a TSA RFC 3161 e embute
// o TimeStampToken → PAdES-T. Detalhes:
//   - DigestAlgorithm = SHA-256 (padrão ICP-Brasil AD-RB/AD-RT);
//   - CertType = ApprovalSignature (não é certificação — permite assinaturas
//     adicionais e edição limitada);
//   - TSA sem auth (Username/Password vazios): serve pra freetsa.org / digicert.
//     Provedor com Basic Auth exigiria expor TSA_USER/TSA_PASS.
//
// Retry: só se a TSA estiver configurada. TSAs públicas gratuitas (digicert)
// aplicam rate limit; a digitorus/pdfsign propaga a resposta HTTP como
// "non success response (429): ...". Detectamos 429/502/503/504 + timeout
// como transitório e retentamos com backoff exponencial. Erros não-transitórios
// (chave inválida, cert expirado, PDF malformado) falham imediatamente.
// Cada tentativa loga estruturado — grep tsa_url + attempt no NR pra decidir
// migrar pra TSA paga (LSITEC, digicert enterprise, etc.).
// signAttemptTimeout limita cada tentativa de assinatura. Cobre PDF render,
// KMS Sign remoto e TSA round-trip. 15s é folgado pro caminho feliz (~500ms
// digicert, ~1s freetsa) e curto o bastante pra falhar rápido se a TSA travar
// (a digitorus/pdfsign usa http.Client sem timeout — a única forma de garantir
// wallclock bound é wrapar em goroutine + select).
const signAttemptTimeout = 15 * time.Second

func signPDFPAdES(ctx context.Context, pdfBytes []byte, signer crypto.Signer, cert *x509.Certificate, intermediates []*x509.Certificate, tsaURL string) ([]byte, error) {
	if tsaURL == "" {
		return signOnceBounded(ctx, pdfBytes, signer, cert, intermediates, "")
	}
	const maxAttempts = 3
	backoff := []time.Duration{0, 500 * time.Millisecond, 2 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff[attempt-1]):
			}
		}
		signed, err := signOnceBounded(ctx, pdfBytes, signer, cert, intermediates, tsaURL)
		if err == nil {
			if attempt > 1 {
				slog.WarnContext(ctx, "TSA recuperada após retry",
					slog.String("tsa_url", tsaURL),
					slog.Int("attempt", attempt))
			}
			return signed, nil
		}
		lastErr = err
		if !isTSATransient(err) {
			return nil, err
		}
		slog.WarnContext(ctx, "TSA erro transitório — retentando",
			slog.String("tsa_url", tsaURL),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", maxAttempts),
			slog.String("err", err.Error()))
	}
	slog.ErrorContext(ctx, "TSA falhou após retries — considere provedor pago (rate limit persistente = migrar)",
		slog.String("tsa_url", tsaURL),
		slog.Int("attempts", maxAttempts),
		slog.String("err", lastErr.Error()))
	return nil, fmt.Errorf("TSA %s falhou após %d tentativas: %w", tsaURL, maxAttempts, lastErr)
}

// signOnceBounded envolve signOnce em wallclock timeout. A goroutine "leaked"
// no timeout é bounded (max 1 por request) e limpa sozinha quando o TCP da TSA
// finalmente falha (OS timeout ~2-3min); seu output vai pro GC (bytes.Buffer
// local). Sem side effect no storage (upload é feito só depois de sign OK).
func signOnceBounded(ctx context.Context, pdfBytes []byte, signer crypto.Signer, cert *x509.Certificate, intermediates []*x509.Certificate, tsaURL string) ([]byte, error) {
	type result struct {
		pdf []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		pdf, err := signOnce(pdfBytes, signer, cert, intermediates, tsaURL)
		ch <- result{pdf, err}
	}()
	select {
	case r := <-ch:
		return r.pdf, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(signAttemptTimeout):
		return nil, fmt.Errorf("sign timeout após %v (tsa_url=%q)", signAttemptTimeout, tsaURL)
	}
}

func signOnce(pdfBytes []byte, signer crypto.Signer, cert *x509.Certificate, intermediates []*x509.Certificate, tsaURL string) ([]byte, error) {
	reader := bytes.NewReader(pdfBytes)
	pdfDoc, err := pdfreader.NewReader(reader, int64(len(pdfBytes)))
	if err != nil {
		return nil, fmt.Errorf("pdf reader: %w", err)
	}
	if _, err := reader.Seek(0, 0); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	chain := append([]*x509.Certificate{cert}, intermediates...)
	signData := pdfsign.SignData{
		Signer:            signer,
		DigestAlgorithm:   crypto.SHA256,
		Certificate:       cert,
		CertificateChains: [][]*x509.Certificate{chain},
		Signature: pdfsign.SignDataSignature{
			CertType:   pdfsign.ApprovalSignature,
			DocMDPPerm: pdfsign.AllowFillingExistingFormFieldsAndSignaturesPerms,
			Info: pdfsign.SignDataSignatureInfo{
				Name:   cert.Subject.CommonName,
				Reason: "Assinatura da peça",
				Date:   time.Now(),
			},
		},
	}
	if tsaURL != "" {
		signData.TSA = pdfsign.TSA{URL: tsaURL}
	}
	if err := pdfsign.Sign(reader, &out, pdfDoc, int64(len(pdfBytes)), signData); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// isTSATransient classifica erros vindos do GetTSA da digitorus/pdfsign como
// transitórios. Base: pdfsignature.go:415-421 formata como
// `non success response (<code>): <body>` e propaga erros de rede como-são.
func isTSATransient(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	transientMarkers := []string{
		"non success response (429",
		"non success response (502",
		"non success response (503",
		"non success response (504",
		"context deadline exceeded",
		"i/o timeout",
		"connection reset",
		"connection refused",
		"no such host",
		"EOF",
		"sign timeout após",
	}
	for _, m := range transientMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// SendToSigning marca sent_to_signing_at=now() no draft. Sinaliza que o
// usuário terminou a Construção e passou pra tela de Assinatura. Idempotente.
// tenantID vem do principal, nunca do body.
func (uc *UseCase) SendToSigning(ctx context.Context, tenantID, draftID string) error {
	return uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.rw.MarkSentToSigning(ctx, tx, draftID, tenantID)
	})
}

// RevertToConstruction nulla sent_to_signing_at (usuário voltou pra Construção).
// Só permite quando a peça ainda NÃO foi assinada (a query trata a guarda).
// Se já assinada, devolve ErrDraftNotFound — a UI trata como "não é possível".
func (uc *UseCase) RevertToConstruction(ctx context.Context, tenantID, draftID string) error {
	return uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.rw.RevertToConstruction(ctx, tx, draftID, tenantID)
	})
}

// FileCommand is the input for POST /v1/pecas/:id/file.
type FileCommand struct {
	TenantID      string
	DraftID       string
	Receipt       map[string]any
	CourtRecordID string // optional override
	FiledAt       *time.Time
	FilingNumber  string // opcional — número/protocolo do tribunal (Fatia 2a v0)
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
		if _, err := uc.rw.UpdateSagaState(ctx, tx, cmd.DraftID, cmd.TenantID, SagaStateFiled, false, "", nil); err != nil {
			return err
		}

		// Persiste filed_at + filing_number no draft (0060). Espelho conveniente
		// pra a UI derivar o step sem precisar fazer JOIN com petition.
		if err := uc.rw.MarkFiled(ctx, tx, cmd.DraftID, cmd.TenantID, cmd.FilingNumber); err != nil {
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
