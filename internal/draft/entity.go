// Package draft is the peticionamento Fatia 1 slice: creating and autosaving a
// peça (rascunho/draft). The advogado clicks "Peticionar" on an intimação (or
// opens a blank/processo draft), and this slice persists the initial context so
// the editor can open with pre-filled metadata and autosave.
//
// Scope: POST /v1/pecas (create), GET /v1/pecas/:id (read model), PATCH /v1/pecas/:id
// (autosave). Attachments (F2), AI review (F3), and signing/protocol (F4) are out of
// scope for this fatia.
//
// entity.go holds only the aggregate and its value types — it imports no repository,
// handler, or lib (the slice's inward dependency rule).
package draft

import "time"

// Draft is a peça (minuta) in its DRAFT lifecycle state. It is 1:1 with an intimação
// when source=intimation (enforced by the partial unique index); drafts from
// source=processo or source=blank have no intimação.
type Draft struct {
	ID           string
	TenantID     string
	CaseID       string // empty when source=blank and no case resolved yet
	IntimationID string // empty for blank/processo drafts
	PieceType    string
	Title        string
	Content      string // empty until autosave
	Status       string
	SagaState    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PatchResult is the thin response the autosave PATCH returns: only the fields that
// changed, so the client does not have to reload the full draft.
type PatchResult struct {
	ID        string
	Title     string
	UpdatedAt time.Time
}

// IntimationContext is the data the use case reads from the intimation row when
// source=intimation, to infer piece_type and resolve case_id/court_record_id.
type IntimationContext struct {
	IntimationID  string
	CaseID        string
	CourtRecordID string
	// Type is the raw DJEN type: CITACAO, INTIMACAO, COMUNICACAO, etc.
	Type string
}

// PieceType closed set — the only values the edge accepts and the DB stores.
const (
	PieceTypeDefense   = "DEFENSE"
	PieceTypeComplaint = "COMPLAINT"
	PieceTypeAppeal    = "APPEAL"
	PieceTypeMotion    = "MOTION"
	PieceTypeOther     = "OTHER"
)

// validPieceTypes is the lookup used by validation.
var validPieceTypes = map[string]bool{
	PieceTypeDefense:   true,
	PieceTypeComplaint: true,
	PieceTypeAppeal:    true,
	PieceTypeMotion:    true,
	PieceTypeOther:     true,
}

// ── Attachment (Fatia 2) ──────────────────────────────────────────────────────

// Attachment is the join between a draft and an uploaded document. It represents
// one exhibit the advogado has attached to the peça. The document itself lives in
// the document slice; this entity owns only the link metadata (category, position).
type Attachment struct {
	ID         string
	TenantID   string
	DraftID    string
	DocumentID string
	Name       string // document.title, falling back to original_filename
	Category   AttachmentCategory
	MimeType   string
	SizeBytes  int64
	Status     string // mirrors document.status (always UPLOADED in the read model)
	Position   int
	CreatedAt  time.Time
}

// AttachmentCategory is the closed set of labels the advogado can assign to an
// attachment. The same set is enforced by the DB CHECK constraint.
type AttachmentCategory string

const (
	CategoryProcuracao                 AttachmentCategory = "Procuração"
	CategoryComprovante                AttachmentCategory = "Comprovante de endereço"
	CategoryContrato                   AttachmentCategory = "Contrato"
	CategoryProvasDocumentais          AttachmentCategory = "Provas documentais"
	CategoryDeclaracaoHipossuficiencia AttachmentCategory = "Declaração de hipossuficiência"
	CategoryOutro                      AttachmentCategory = "Outro"
)

// validAttachmentCategories is the lookup for validation (mirrors the DB CHECK).
var validAttachmentCategories = map[AttachmentCategory]bool{
	CategoryProcuracao:                 true,
	CategoryComprovante:                true,
	CategoryContrato:                   true,
	CategoryProvasDocumentais:          true,
	CategoryDeclaracaoHipossuficiencia: true,
	CategoryOutro:                      true,
}

// IsValidCategory reports whether cat is a member of the closed set.
func IsValidCategory(cat AttachmentCategory) bool {
	return validAttachmentCategories[cat]
}

// documentForAttachment is the minimal document state the attachment use case loads
// before linking. It carries only the fields needed for the guard checks; the full
// document entity lives in the document slice (imported by the read query via JOIN,
// never via Go import). This is an internal type — not exported.
type documentForAttachment struct {
	ID       string
	TenantID string
	Status   string
	Origin   string
}

// documentStatus mirrors the document status constants used in validation —
// kept as local constants so the draft slice never imports the document slice.
const (
	documentStatusUploaded = "UPLOADED"
	documentOriginUpload   = "UPLOAD"
)

// ── Saga state constants (Fatia 3) ───────────────────────────────────────────

// The saga_state column on draft tracks the AI generation lifecycle. No DB CHECK is
// added (the init migration has no CHECK on saga_state), so the enforcement is in the
// application layer (use case guard + handler guard).
const (
	SagaStateCreated    = "CREATED"
	SagaStateExtracting = "EXTRACTING"
	SagaStateReviewed   = "REVIEWED"
	SagaStateFailed     = "FAILED"
)

// ── Review (AI parecer) ────────────────────────────────────────────────────

// Review is one AI-generated review for a draft. Multiple reviews may exist (one per
// generation attempt). The read model exposes only the LATEST (by generated_at DESC).
type Review struct {
	ID           string
	DraftID      string
	Findings     []Finding
	Coverage     Coverage
	ModelVersion string
	RulesVersion string
	Status       string // COMPLETED | FAILED
	GeneratedAt  time.Time
	CreatedAt    time.Time
}

// ReviewStatus closed set.
const (
	ReviewStatusCompleted = "COMPLETED"
	ReviewStatusFailed    = "FAILED"
)

// Finding is one AI suggestion mapped to an exact substring of draft.content.
// category drives citation requirements (Argumento/Coerência → citation required).
type Finding struct {
	N           int       `json:"n"`
	Category    string    `json:"category"`
	Original    string    `json:"original"`
	Replacement string    `json:"replacement"`
	Problem     string    `json:"problem"`
	Description string    `json:"description"`
	Citation    *Citation `json:"citation,omitempty"`
}

// Citation anchors a finding to a specific chunk from the case corpus.
type Citation struct {
	DocumentID string `json:"document_id"`
	Page       int    `json:"page"`
	Quote      string `json:"quote"`
}

// Coverage summarizes the grounding and filtering results of one generation run.
type Coverage struct {
	Grounded           bool     `json:"grounded"`
	ChunksUsed         int      `json:"chunks_used"`
	SuggestionsTotal   int      `json:"suggestions_total"`
	SuggestionsDropped int      `json:"suggestions_dropped"`
	DocumentsCited     []string `json:"documents_cited"`
	// Error is non-empty only for FAILED reviews — the human-readable reason.
	Error string `json:"error,omitempty"`
}

// FindingCategory is the closed set of suggestion categories.
const (
	CategoryClareza   = "Clareza"
	CategoryArgumento = "Argumento"
	CategoryCoerencia = "Coerência"
	CategoryEstilo    = "Estilo"
)

// citationRequired reports whether a finding of the given category must include a citation
// (true for Argumento and Coerência — the factual categories that require grounding in the
// case corpus). Clareza and Estilo are stylistic; citation is optional.
func citationRequired(category string) bool {
	return category == CategoryArgumento || category == CategoryCoerencia
}

// inferPieceType maps an intimation type (DJEN tipoComunicacao) to a PieceType
// when the client omits piece_type. This is the SINGLE source of truth for the
// inference — referenced by the domain use case and tested directly.
//
// Inference rules (docs/erd-backend.md peticionamento §4):
//   - CITACAO  → DEFENSE
//   - INTIMACAO → DEFENSE
//   - anything else (COMUNICACAO, unknown) → OTHER
func inferPieceType(intimationType string) string {
	switch intimationType {
	case "CITACAO", "INTIMACAO":
		return PieceTypeDefense
	default:
		return PieceTypeOther
	}
}
