package draft

import (
	"errors"
	"strings"
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/google/uuid"
)

// Source closed set — the only values the POST /v1/pecas body accepts.
const (
	SourceIntimation = "intimation"
	SourceProcesso   = "processo"
	SourceBlank      = "blank"
)

// CreateRequest is the POST /v1/pecas body. tenant_id/user come from the verified
// principal, never the body (tenant isolation cannot be spoofed).
type CreateRequest struct {
	// Source is required and must be one of the closed set.
	Source string `json:"source"`
	// IntimationID is required when Source=intimation; the BE resolves case_id and
	// court_record_id from the intimation row (never from the body).
	IntimationID string `json:"intimation_id"`
	// CaseID is used when Source=processo to inherit the process context.
	CaseID string `json:"case_id"`
	// PieceType is optional; when absent the BE infers it from the intimation type.
	PieceType string `json:"piece_type"`
	// Title is optional; defaults to "" (the editor sets it on first autosave).
	Title string `json:"title"`
}

// Validate enforces the edge boundary rules via ozzo (method-based, not struct tags):
// source is required and must be in the closed set; when source=intimation,
// intimation_id must be a valid UUID; piece_type, when present, must be in the
// closed set.
func (r CreateRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Source,
			validation.Required,
			validation.In(SourceIntimation, SourceProcesso, SourceBlank),
		),
		validation.Field(&r.IntimationID,
			validation.When(r.Source == SourceIntimation,
				validation.Required, validation.By(isUUID)),
		),
		validation.Field(&r.CaseID,
			validation.When(r.CaseID != "", validation.By(isUUID)),
		),
		validation.Field(&r.PieceType,
			validation.When(r.PieceType != "",
				validation.In(
					PieceTypeDefense,
					PieceTypeComplaint,
					PieceTypeAppeal,
					PieceTypeMotion,
					PieceTypeOther,
				)),
		),
	)
}

// PatchRequest is the PATCH /v1/pecas/:id body. content is the autosave payload
// (empty string is valid — the editor cleared it). title is optional (only updated
// when the pointer is non-nil).
type PatchRequest struct {
	Content string  `json:"content"`
	Title   *string `json:"title"`
}

// Validate has no rules for PATCH: content empty is valid (clearing is allowed),
// and title is optional. The method exists so httpx.WriteValidationError can be
// called uniformly at the edge.
func (r PatchRequest) Validate() error { return nil }

// ── Attachment request types (Fatia 2) ───────────────────────────────────────

// AttachDocumentRequest is the POST /v1/pecas/:id/anexos body.
type AttachDocumentRequest struct {
	DocumentID string `json:"document_id"`
	Category   string `json:"category"`
}

// Validate enforces edge rules: document_id is required and must be a valid UUID;
// category, when present, must be in the closed set. An absent category defaults to
// "Outro" in the use case.
func (r AttachDocumentRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.DocumentID, validation.Required, validation.By(isUUID)),
		validation.Field(&r.Category,
			validation.When(r.Category != "",
				validation.By(isAttachmentCategory)),
		),
	)
}

// UpdateAttachmentCategoryRequest is the PATCH /v1/pecas/:id/anexos/:attachmentId body.
type UpdateAttachmentCategoryRequest struct {
	Category string `json:"category"`
}

// Validate enforces edge rules: category is required and must be in the closed set.
func (r UpdateAttachmentCategoryRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Category,
			validation.Required,
			validation.By(isAttachmentCategory)),
	)
}

// ── Chat request types (Fatia 3b) ─────────────────────────────────────────────

// ChatRequest is the POST /v1/pecas/:id/chat body.
type ChatRequest struct {
	Question string `json:"question"`
}

// Validate enforces edge rules: question is required, must be non-empty after trim,
// and must be at most 2000 rune characters. The Question field is trimmed in-place
// so the handler receives the sanitised value (no leading/trailing whitespace).
func (r *ChatRequest) Validate() error {
	r.Question = strings.TrimSpace(r.Question)
	return validation.ValidateStruct(r,
		validation.Field(&r.Question,
			validation.Required,
			validation.RuneLength(1, 2000),
		),
	)
}

// ── Generation request type (Fatia 5 — teses/tom/instruções) ────────────────

// GenerateRequest is the POST /v1/pecas/:id/generate body. Every field is
// OPTIONAL — an empty body `{}` (or no body at all) must remain valid, matching
// the pre-Fatia-5 contract exactly (backward-compat).
type GenerateRequest struct {
	Tone         string   `json:"tone"`
	Instructions string   `json:"instructions"`
	Theses       []string `json:"theses"`
}

// Validate enforces edge rules: tone, when present, must be in the closed set
// (an invalid tone is a 400, never a silent default); instructions, when
// present, is trimmed then capped at 2000 runes — same trim-then-validate
// pattern as ChatRequest.Validate(). Tone left empty is valid: the use case
// defaults it server-side to ToneTecnicoFormal.
func (r *GenerateRequest) Validate() error {
	r.Instructions = strings.TrimSpace(r.Instructions)
	return validation.ValidateStruct(r,
		validation.Field(&r.Tone,
			validation.When(r.Tone != "",
				validation.In(ToneTecnicoFormal, ToneDiretoAssertivo, ToneConciliadorInstitucional)),
		),
		validation.Field(&r.Instructions,
			validation.RuneLength(0, 2000),
		),
	)
}

// isAttachmentCategory rejects a string that is not a valid AttachmentCategory.
func isAttachmentCategory(value any) error {
	s, _ := value.(string)
	if !IsValidCategory(AttachmentCategory(s)) {
		return errors.New("must be a valid attachment category")
	}
	return nil
}

// isUUID is an ozzo custom rule that rejects non-UUID strings.
func isUUID(value any) error {
	s, _ := value.(string)
	if _, err := uuid.Parse(s); err != nil {
		return errors.New("must be a valid UUID")
	}
	return nil
}

// ── Fatia 4 — peticionamento request types ──────────────────────────────────

// FileRequest is the POST /v1/pecas/:id/file body.
type FileRequest struct {
	Receipt       map[string]any `json:"receipt"`
	CourtRecordID string         `json:"court_record_id"`
	FiledAt       string         `json:"filed_at"`
}

// Validate enforces edge rules: receipt must be a non-empty object (at least 1
// key). court_record_id, when present, must be a valid UUID. filed_at, when
// present, must be a valid RFC3339 timestamp.
func (r *FileRequest) Validate() error {
	return validation.ValidateStruct(r,
		validation.Field(&r.Receipt, validation.Required, validation.By(isNonEmptyObject)),
		validation.Field(&r.CourtRecordID,
			validation.When(r.CourtRecordID != "", validation.By(isUUID)),
		),
		validation.Field(&r.FiledAt,
			validation.When(r.FiledAt != "", validation.By(isRFC3339)),
		),
	)
}

// ResultRequest is the PATCH /v1/pecas/:id/result body.
type ResultRequest struct {
	ObservedResult string `json:"observed_result"`
}

// Validate enforces edge rules: observed_result is required and must be in the
// closed set.
func (r ResultRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.ObservedResult,
			validation.Required,
			validation.In(
				ObservedResultOK,
				ObservedResultAmendment,
				ObservedResultNotAdmitted,
				ObservedResultUntimely,
			),
		),
	)
}

// isNonEmptyObject rejects a nil or empty map (the receipt must have at least
// one field).
func isNonEmptyObject(value any) error {
	m, ok := value.(map[string]any)
	if !ok || len(m) == 0 {
		return errors.New("must be a non-empty object")
	}
	return nil
}

// isRFC3339 rejects a string that is not a valid RFC3339 timestamp.
func isRFC3339(value any) error {
	s, _ := value.(string)
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return errors.New("must be a valid RFC3339 timestamp")
	}
	return nil
}
