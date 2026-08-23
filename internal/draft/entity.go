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

import "strings"

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

	// ── Generation params (Fatia 5 — teses/tom/instruções) ────────────────────
	// Chosen on POST /v1/pecas/:id/generate and persisted on the row (not on the
	// draft.generation_requested event) so the async worker rereads them here.

	// Tone is the closed-set writing register for Gerar. Defaults to
	// "tecnico-formal" (the DB column default — identical wording to the
	// pre-Fatia-5 prompt).
	Tone string
	// Instructions is free-text advogado guidance for Gerar, capped at 2000
	// runes at the edge. Empty when the advogado provided none.
	Instructions string
	// SelectedTheses are the tese labels (plain strings, not structured
	// citations) the advogado picked from /theses to steer Gerar. Empty when
	// none were selected.
	SelectedTheses []string
}

// PatchResult is the thin response the autosave PATCH returns: only the fields that
// changed, so the client does not have to reload the full draft.
type PatchResult struct {
	ID        string
	Title     string
	UpdatedAt time.Time
}

// IntimationContext is the data the use case reads from the intimation row when
// source=intimation, to infer piece_type, resolve case_id/court_record_id, and
// compose the AI generation prompt with real process metadata.
type IntimationContext struct {
	IntimationID  string
	CaseID        string
	CourtRecordID string
	// Type is the raw DJEN type: CITACAO, INTIMACAO, COMUNICACAO, etc.
	Type string

	// Fields below are used by the AI generation prompt (buildDraftContext).
	// They mirror the columns available via GetDraftDetail's JOIN on court_record
	// and deadline — reusing the same sources, loading them together for the
	// generation path so no second query is needed.

	// Content is the full text of the intimation (teor) — the richest context
	// signal for the AI prompt.
	Content string
	// CNJNumber is the process number in CNJ format (e.g. 0000001-23.2026.8.26.0001).
	CNJNumber string
	// Court is the tribunal sigla (e.g. TJSP, TRT2).
	Court string
	// Degree is the grau (G1, G2, JE, SUPERIOR…).
	Degree string
	// Class is the classe/rito processual.
	Class string
	// Subject is the assunto.
	Subject string
	// JudgingBody is the órgão julgador / vara.
	JudgingBody string
	// DeadlineEndDate is the prazo end date formatted as "2006-01-02" (DateOnly),
	// or empty string when no deadline has been derived for this intimation yet.
	DeadlineEndDate string
	// Recipients is the raw jsonb from intimation.recipients ([]djenRecipient shape).
	// Parsed by the mapper to resolve the signing lawyer (matched=true recipient).
	Recipients []byte
}

// PartyCounselInfo is one advogado aggregated under a party. OAB and UF are the
// stable identity (as stored by the DJEN parser); Name may be empty when absent.
type PartyCounselInfo struct {
	Name string
	OAB  string
	UF   string
}

// PartyInfo is one party of the process with its aggregated counsels.
// Role is the raw DB value (PLAINTIFF, DEFENDANT, THIRD_PARTY).
type PartyInfo struct {
	Role    string
	Name    string
	Counsel string // first counsel formatted as "Name (OAB/UF nº oab)", or "" when absent
}

// SigningLawyer is the OAB-matched advogado from intimation.recipients — the first
// recipient flagged matched=true (our OAB, the signing lawyer for the peça).
// All fields are empty strings when no matched recipient exists.
type SigningLawyer struct {
	Name string
	OAB  string
	UF   string
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

// ── Generation params (Fatia 5 — teses/tom/instruções) ──────────────────────

// Tone closed set — the only values POST /v1/pecas/:id/generate accepts for
// the `tone` field. Mirrors the DB CHECK (draft_tone_check). ToneTecnicoFormal
// is the default (server-side, when the caller omits or sends "").
const (
	ToneTecnicoFormal            = "tecnico-formal"
	ToneDiretoAssertivo          = "direto-assertivo"
	ToneConciliadorInstitucional = "conciliador-institucional"
)

// validTones is the lookup used by validation.
var validTones = map[string]bool{
	ToneTecnicoFormal:            true,
	ToneDiretoAssertivo:          true,
	ToneConciliadorInstitucional: true,
}

// Thesis is one AI-suggested legal thesis for POST /v1/pecas/:id/theses. Unlike
// a Finding/Citation, Reference is a plain string — jurisprudência or a legal
// dispositivo is not a chunk anchored in the case corpus, so it carries no
// document_id/page/quote.
type Thesis struct {
	Label      string `json:"label"`
	Confidence string `json:"confidence"` // alta|media|baixa
	Reference  string `json:"reference"`  // jurisprudência ou dispositivo legal, texto livre
	Foundation string `json:"foundation"`
}

// ThesisConfidence closed set.
const (
	ThesisConfidenceAlta  = "alta"
	ThesisConfidenceMedia = "media"
	ThesisConfidenceBaixa = "baixa"
)

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

// ── Status constants (Fatia 4 — peticionamento) ─────────────────────────────

// Status is the milestone lifecycle of the peça. It is distinct from saga_state
// (which tracks the async AI generation pipeline). The CHECK constraint on
// draft.status enforces the closed set at the DB level.
const (
	StatusDraft    = "DRAFT"
	StatusReviewed = "REVIEWED"
	StatusSigned   = "SIGNED"
)

// validDraftStatuses is the lookup used by validation.
var validDraftStatuses = map[string]bool{
	StatusDraft:    true,
	StatusReviewed: true,
	StatusSigned:   true,
}

// ── Saga state constants (Fatia 3) ───────────────────────────────────────────

// The saga_state column on draft tracks the AI generation lifecycle. No DB CHECK is
// added (the init migration has no CHECK on saga_state), so the enforcement is in the
// application layer (use case guard + handler guard).
const (
	SagaStateCreated    = "CREATED"
	SagaStateExtracting = "EXTRACTING"
	// SagaStateDrafted is set after Gerar completes: the draft has content but no
	// AI review yet. The advogado can then trigger Revisar to produce suggestions.
	SagaStateDrafted  = "DRAFTED"
	SagaStateReviewed = "REVIEWED"
	SagaStateFiled    = "FILED"
	SagaStateFailed   = "FAILED"
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

// ── Chat (Fatia 3b) ───────────────────────────────────────────────────────────

// ChatMessage is one turn in the multi-turn grounded chat thread for a draft. Each
// call to AnswerQuestion appends two rows: one role='user' and one role='assistant'.
// The thread is scoped to a draft; isolation is via JOIN draft.tenant_id (same
// pattern as review — no tenant_id column, no RLS on this table; barrier-1 is the
// enforced guard).
type ChatMessage struct {
	ID           string
	DraftID      string
	Role         string // "user" | "assistant"
	Content      string
	Citations    []Citation
	Grounded     bool
	ModelVersion string // non-empty only for role="assistant"
	CreatedAt    time.Time
}

// ChatRole closed set.
const (
	ChatRoleUser      = "user"
	ChatRoleAssistant = "assistant"
)

// inferPieceType classifies the peça type when the client omits piece_type. It is
// the SINGLE source of truth for creation-time inference and is CONTENT-FIRST: the
// real signal lives in the teor da intimação (the DJEN type alone is ambiguous — the
// same "INTIMACAO" covers a defense, a manifestation, or a recursal window). It uses
// the type + class + subject + the HTML-stripped teor, is high-precision (conservative
// keyword match), and falls back to OTHER rather than mislabel. The generation LLM
// still adapts the actual peça to the teor; this drives the UI label and prompt hint.
//
// Precedence (most specific first):
//   - APPEAL  — a decision was rendered and a recursal window opened
//   - DEFENSE — cited to defend, or embargos/impugnação (execução/cumprimento)
//   - MOTION  — ordered to manifest/say/indicate (manifestação incidental)
//   - DEFENSE — fallback: an INTIMACAO with no content signal (historical default)
//   - OTHER   — no signal and not an INTIMACAO/CITACAO
//
// Note: the class "Execução" alone does NOT imply DEFENSE — the recipient may be the
// exequente ordered to indicate assets (→ MOTION). Only the content disambiguates.
func inferPieceType(it *IntimationContext) string {
	if it == nil {
		return PieceTypeOther
	}
	blob := strings.ToLower(strings.Join(
		[]string{it.Type, it.Class, it.Subject, stripHTML(it.Content)}, " "))

	switch {
	case containsAny(blob, "sentença", "acórdão", "recurso inominado", "prazo recursal",
		"para recorrer", "interpor recurso", "apelaç", "julgo procedente", "julgo improcedente"):
		return PieceTypeAppeal
	case it.Type == "CITACAO",
		containsAny(blob, "conteste", "contestação", "apresentar defesa", "oferecer defesa",
			"embargos à execução", "embargos do executado", "impugnação ao cumprimento"):
		return PieceTypeDefense
	case containsAny(blob, "manifeste-se", "manifestar-se", "manifestação", "diga sobre",
		"indique bens", "indicar bens", "requeira o que"):
		return PieceTypeMotion
	case it.Type == "INTIMACAO":
		// Fallback: an INTIMACAO with no recognized content signal defaults to DEFENSE
		// (the historical default) — better a sane hint than OTHER when the teor is
		// thin/empty (scraping gap). Content signals above still win (APPEAL/MOTION).
		return PieceTypeDefense
	default:
		return PieceTypeOther
	}
}

// containsAny reports whether s contains any of subs (all expected pre-lowercased).
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ── Petition (Fatia 4 — peticionamento) ─────────────────────────────────────

// Petition is a signed, filed peça. It is immutable after creation. No tenant_id
// directly — isolation is via the draft FK (JOIN draft.tenant_id).
type Petition struct {
	ID             string
	DraftID        string
	CourtRecordID  string
	FiledAt        time.Time
	Receipt        map[string]any
	ObservedResult string // empty until observed
}

// ObservedResult closed set — the outcome the advogado records after the petition
// is filed.
const (
	ObservedResultOK           = "OK"
	ObservedResultAmendment    = "AMENDMENT"
	ObservedResultNotAdmitted  = "NOT_ADMITTED"
	ObservedResultUntimely     = "UNTIMELY"
)

// validObservedResults is the lookup used by validation.
var validObservedResults = map[string]bool{
	ObservedResultOK:          true,
	ObservedResultAmendment:   true,
	ObservedResultNotAdmitted: true,
	ObservedResultUntimely:    true,
}

// ── Coverage summary (Fatia 4 — list read model) ────────────────────────────

// CoverageSummary is the trimmed review snapshot exposed in list endpoints.
// It comes from the most recent review for a draft.
type CoverageSummary struct {
	Grounded         bool `json:"grounded"`
	ChunksUsed       int  `json:"chunks_used"`
	SuggestionsTotal int  `json:"suggestions_total"`
}

// ── List read models (Fatia 4) ──────────────────────────────────────────────

// DraftListItem is one row in the paginated list of peças. It carries the
// fields the client needs for the list card — not the full aggregate.
type DraftListItem struct {
	ID              string
	PieceType       string
	Title           string
	Status          string
	SagaState       string
	CoverageSummary *CoverageSummary
	FiledAt         *time.Time
	ObservedResult  *string
	CreatedAt       time.Time
}

// DraftListResult is a page of peças plus whether a further page exists.
type DraftListResult struct {
	Items   []DraftListItem
	HasMore bool
}
