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
	ID               string
	TenantID         string
	DraftID          string
	DocumentID       string
	Name             string // document.title, falling back to original_filename
	Category         AttachmentCategory
	MimeType         string
	SizeBytes        int64
	Status           string // mirrors document.status (always UPLOADED in the read model)
	Position         int
	CreatedAt        time.Time
}

// AttachmentCategory is the closed set of labels the advogado can assign to an
// attachment. The same set is enforced by the DB CHECK constraint.
type AttachmentCategory string

const (
	CategoryProcuracao              AttachmentCategory = "Procuração"
	CategoryComprovante             AttachmentCategory = "Comprovante de endereço"
	CategoryContrato                AttachmentCategory = "Contrato"
	CategoryProvasDocumentais       AttachmentCategory = "Provas documentais"
	CategoryDeclaracaoHipossuficiencia AttachmentCategory = "Declaração de hipossuficiência"
	CategoryOutro                   AttachmentCategory = "Outro"
)

// validAttachmentCategories is the lookup for validation (mirrors the DB CHECK).
var validAttachmentCategories = map[AttachmentCategory]bool{
	CategoryProcuracao:              true,
	CategoryComprovante:             true,
	CategoryContrato:                true,
	CategoryProvasDocumentais:       true,
	CategoryDeclaracaoHipossuficiencia: true,
	CategoryOutro:                   true,
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
