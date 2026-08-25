package draft

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. Absence is always a typed error from the
// repository, never (nil, nil). The Kind drives the HTTP status at the edge
// (lib/httpx.statusByKind).
var (
	// errGenerationInProgress is returned when POST /v1/pecas/:id/generate is called
	// while the draft is already in EXTRACTING state (generation is already running).
	// CONFLICT (→ 409). Internal sentinel; exported as ErrGenerationInProgress via the
	// generate_trigger.go alias for package-external code that needs to errors.Is it.
	errGenerationInProgress = apperr.NewConflict("generation is already in progress for this draft")

	// ErrAttachmentAlreadyLinked — a POST /v1/pecas/:id/anexos tried to link a
	// document that is already attached to this draft. The UNIQUE (draft_id, document_id)
	// constraint fires. CONFLICT (→ 409).
	ErrAttachmentAlreadyLinked = apperr.NewConflict("document is already attached to this draft")

	// ErrAttachmentNotFound — the requested attachment id resolves to no row in the
	// (draft_id, tenant_id) scope. Typed not-found (→ 404).
	ErrAttachmentNotFound = apperr.NewNotFound("attachment not found")

	// ErrDocumentNotAttachable — a POST /v1/pecas/:id/anexos referred to a document
	// that cannot be attached: either its status is not UPLOADED (still PENDING) or its
	// origin is COURT (dos autos, nunca vinculável a uma peça). INVALID (→ 422).
	ErrDocumentNotAttachable = apperr.NewInvalid("document cannot be attached: it must be origin=UPLOAD and status=UPLOADED")

	// ErrDocumentNotFound is the sentinel for when the document id in the attachment
	// request resolves to no live row in the tenant (wrong tenant, unknown id, or
	// soft-deleted). Typed not-found (→ 404), never (nil, nil).
	ErrDocumentNotFound = apperr.NewNotFound("document not found")

	// ErrDraftNotFound — the requested draft id resolves to no row in the tenant
	// (GET /v1/pecas/:id, PATCH /v1/pecas/:id). Typed not-found (→ 404), never
	// (nil, nil): a foreign or unknown id is a client-facing miss.
	ErrDraftNotFound = apperr.NewNotFound("draft not found")

	// ErrIntimationNotFound — the source=intimation POST supplied an intimation_id
	// that resolves to no row in the tenant (no intimation, or wrong tenant). Typed
	// not-found (→ 404), never (nil, nil).
	ErrIntimationNotFound = apperr.NewNotFound("intimation not found")

	// ── Fatia 4 — peticionamento errors ─────────────────────────────────────

	// ErrInvalidStatusForSign — POST /v1/pecas/:id/sign called when draft.status is
	// not DRAFT or REVIEWED (e.g. CREATED, EXTRACTING, FAILED, or already SIGNED).
	// INVALID (→ 422).
	ErrInvalidStatusForSign = apperr.NewInvalid("draft status does not allow signing: must be DRAFT or REVIEWED")

	// ErrAlreadySigned — POST /v1/pecas/:id/sign called on an already SIGNED draft.
	// Idempotent: returns 200 with current data (not an error in the handler path),
	// but the use case returns this sentinel so the handler can distinguish the
	// idempotent path from the fresh-sign path.
	ErrAlreadySigned = apperr.NewConflict("draft is already signed")

	// ErrAlreadyFiled — POST /v1/pecas/:id/file called when a petition already exists
	// for this draft. CONFLICT (→ 409).
	ErrAlreadyFiled = apperr.NewConflict("petition already exists for this draft")

	// ErrPetitionNotFound — PATCH /v1/pecas/:id/result called when no petition exists
	// for the draft. NOT_FOUND (→ 404).
	ErrPetitionNotFound = apperr.NewNotFound("petition not found: draft has not been filed yet")

	// ErrCourtRecordRequired — POST /v1/pecas/:id/file could not resolve a
	// court_record_id (no intimation and no override in the body). INVALID (→ 422).
	ErrCourtRecordRequired = apperr.NewInvalid("court_record_id is required: no intimation linked and none provided in the request")

	// ErrExportFormatInvalid — GET /v1/pecas/:id/export with an unsupported format.
	// INVALID (→ 422).
	ErrExportFormatInvalid = apperr.NewInvalid("export format must be 'docx' or 'pdf'")

	// ErrDraftNoContent — GET /v1/pecas/:id/export on a draft with empty content.
	// INVALID (→ 422).
	ErrDraftNoContent = apperr.NewInvalid("draft has no content to export")

	// ErrDraftNotSigned — POST /v1/pecas/:id/file called when draft.status is not
	// SIGNED. The piece must be signed before filing. INVALID (→ 422).
	ErrDraftNotSigned = apperr.NewInvalid("draft must be signed before filing")

	// ── Fatia 1 — peticionamento automático (e-SAJ) ──────────────────────────

	// ErrSecretVaultUnavailable — o slice de certificados (KMS envelope) não está
	// montado (GCP_KMS_KEY_NAME vazio). Bloqueia o CRUD de credenciais e-SAJ.
	// CONFLICT (→ 409) — o cliente deve provisionar o KMS antes.
	ErrSecretVaultUnavailable = apperr.NewConflict("secret vault (KMS) indisponível: credenciais e-SAJ requerem envelope criptográfico")

	// ErrEsajCredentialNotFound — não há credencial e-SAJ ativa para o usuário.
	// NOT_FOUND (→ 404).
	ErrEsajCredentialNotFound = apperr.NewNotFound("credencial e-SAJ não encontrada para este usuário")

	// ErrFilingAlreadyEnqueued — POST /v1/pecas/:id/filing/approve chamado com uma
	// tentativa já ativa (ENFILEIRADO/PROTOCOLANDO). O handler devolve a tentativa
	// existente (200 idempotente), não um erro.
	ErrFilingAlreadyEnqueued = apperr.NewConflict("já existe um protocolo automático em andamento para esta peça")

	// ErrFilingNotSigned — POST /v1/pecas/:id/filing/approve chamado quando o draft
	// não está SIGNED. INVALID (→ 422).
	ErrFilingNotSigned = apperr.NewInvalid("a peça deve estar assinada antes de protocolar")

	// ErrFilingConsentRequired — falta a credencial e-SAJ ativa (ou o consentimento
	// dos termos) para protocolar automaticamente. INVALID (→ 422).
	ErrFilingConsentRequired = apperr.NewInvalid("credencial e-SAJ ativa obrigatória para protocolo automático")

	// ErrEsajCredentialConflict — já existe uma credencial e-SAJ ativa para o
	// usuário (unique parcial). O FE deve revogar a anterior antes de cadastrar.
	// CONFLICT (→ 409).
	ErrEsajCredentialConflict = apperr.NewConflict("já existe uma credencial e-SAJ ativa para este usuário")

	// ErrFilingAttemptConflict — o unique parcial (draft_id ativo) impediu a 2ª
	// tentativa ativa. O handler trata como idempotência (devolve a existente).
	ErrFilingAttemptConflict = apperr.NewConflict("já existe um protocolo automático em andamento para esta peça")

	// ErrFilingAttemptNotFound — GET /v1/pecas/:id/filing com id inexistente.
	// NOT_FOUND (→ 404).
	ErrFilingAttemptNotFound = apperr.NewNotFound("tentativa de protocolo não encontrada")
)
