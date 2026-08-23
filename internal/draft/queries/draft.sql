-- draft slice queries (peticionamento Fatia 1). Every write runs inside the use
-- case's transaction so RLS scopes it to the principal's tenant (barrier 2) on top
-- of the explicit tenant filter (barrier 1). Absence is a typed error at the mapper,
-- never (nil, nil).

-- name: InsertDraft :one
-- Persist a new peça (DRAFT status, CREATED saga_state). Returns all columns so the
-- handler renders the 201 response without a follow-up read. storage_key is NULL for
-- Fatia 1 (content lives in the column, not in S3).
--
-- ON CONFLICT DO NOTHING targets the partial unique index
-- (tenant_id, intimation_id WHERE intimation_id IS NOT NULL). When the row already
-- exists the RETURNING clause yields zero rows (pgx.ErrNoRows), which the repository
-- maps to ErrDraftAlreadyExists so the use case can fetch the existing row for 200.
-- This avoids a 23505 error that would abort the current transaction (25P02), making
-- subsequent queries in the same tx impossible without a SAVEPOINT.
INSERT INTO draft (
    tenant_id, case_id, intimation_id,
    piece_type, title, content,
    status, saga_state,
    created_at, updated_at
) VALUES (
    $1, $2, $3,
    $4, $5, $6,
    'DRAFT', 'CREATED',
    now(), now()
)
ON CONFLICT (tenant_id, intimation_id) WHERE intimation_id IS NOT NULL DO NOTHING
RETURNING id, tenant_id, case_id, intimation_id,
          piece_type, title, content,
          status, saga_state,
          created_at, updated_at,
          structured_content, authorship;

-- name: GetDraftByIntimationID :one
-- Fetch the draft that already exists for the (tenant_id, intimation_id) pair —
-- used by the idempotent POST path when the INSERT fails with 23505. Filters by
-- tenant (barrier 1).
SELECT id, tenant_id, case_id, intimation_id,
       piece_type, title, content,
       status, saga_state,
       created_at, updated_at,
       structured_content, authorship
FROM draft
WHERE tenant_id = $1 AND intimation_id = $2;

-- name: GetDraftByID :one
-- Load the full peça aggregate by id, filtered by tenant (barrier 1). A miss or
-- foreign-tenant id yields pgx.ErrNoRows → ErrDraftNotFound (→ 404). Includes the
-- Gerar-time generation params (tone/instructions/selected_theses, Fatia 5) so
-- OnGenerationRequested (the async worker, which reloads the draft) has them
-- without the event payload carrying them.
SELECT id, tenant_id, case_id, intimation_id,
       piece_type, title, content,
       status, saga_state,
       created_at, updated_at,
       tone, instructions, selected_theses,
       structured_content, authorship
FROM draft
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateDraftContent :one
-- Autosave: update content (and optionally title + structured_content) + bump
-- updated_at, scoped to (id, tenant_id). Returns the minimal patch response fields.
-- A no-match (wrong id or foreign tenant) yields pgx.ErrNoRows → ErrDraftNotFound.
--
-- structured_content is dual-written: when $6::boolean is true, $7 (jsonb) is
-- persisted; otherwise the existing structured_content is left untouched. The FE
-- always sends both (Peça v2), older PATCH callers can send just content.
UPDATE draft
SET content            = $3,
    title              = CASE WHEN $4::boolean THEN $5 ELSE title END,
    structured_content = CASE WHEN $6::boolean THEN $7 ELSE structured_content END,
    updated_at         = now()
WHERE id = $1 AND tenant_id = $2
RETURNING id, title, updated_at;

-- name: GetDraftDetail :one
-- Read model for GET /v1/pecas/:id: a JOIN over draft, intimation (optional),
-- court_record (via intimation), and deadline (via intimation 1:1 UNIQUE). All
-- intimation/process/deadline columns are NULLable (a draft without an intimation
-- is valid — source=blank or source=processo). Filtered by (draft.id, draft.tenant_id)
-- (barrier 1); RLS on draft is barrier 2. The court_record columns are read here
-- without importing the acquisition slice (decisão: read the table directly).
SELECT
    d.id,
    d.piece_type,
    d.title,
    d.content,
    d.status,
    d.saga_state,
    d.created_at,
    d.updated_at,
    d.structured_content,
    d.authorship,

    -- workflow timestamps (0060) — a UI deriva o step atual a partir deles.
    d.sent_to_signing_at,
    d.signed_at,
    d.filed_at,
    d.filing_number,
    -- storage key do PDF assinado (Fatia 2b — 0061). NULL antes de assinar.
    -- O handler transforma em presigned URL antes de devolver ao cliente.
    d.signed_pdf_key,

    -- intimation fields (NULL when draft has no intimation_id)
    i.id            AS intimation_id,
    i.type          AS intimation_type,
    i.content       AS intimation_content,
    i.made_available_at AS intimation_made_available_at,
    i.deadline_start_at AS intimation_deadline_start_at,

    -- process fields (via court_record, NULL when no intimation)
    cr.case_id      AS process_case_id,
    cr.id           AS process_court_record_id,
    cr.cnj_number   AS process_cnj_number,
    cr.court        AS process_court,
    cr.degree       AS process_degree,
    cr.class        AS process_class,
    cr.subject      AS process_subject,
    cr.judging_body AS process_judging_body,

    -- deadline fields (NULL when no intimation or no deadline derived yet)
    dl.id           AS deadline_id,
    dl.end_date     AS deadline_end_date,
    dl.status       AS deadline_status
FROM draft d
LEFT JOIN intimation  i  ON i.id = d.intimation_id
LEFT JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline    dl  ON dl.notification_id = i.id
WHERE d.id = $1 AND d.tenant_id = $2;

-- ── draft_attachment queries (Peticionamento Fatia 2) ────────────────────────
-- All writes run inside the use case's transaction (RLS barrier 2 + explicit
-- tenant filter barrier 1). Absence is a typed error at the mapper, never (nil,nil).

-- name: InsertDraftAttachment :one
-- Link a document (origin=UPLOAD, status=UPLOADED) to a draft. The UNIQUE constraint
-- (draft_id, document_id) guards against duplicates at the DB level; ON CONFLICT DO
-- NOTHING maps it to pgx.ErrNoRows so the repo can return ErrAttachmentAlreadyLinked
-- without a 23505 transaction abort.
INSERT INTO draft_attachment (tenant_id, draft_id, document_id, category, position)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (draft_id, document_id) DO NOTHING
RETURNING id, tenant_id, draft_id, document_id, category, position, created_at;

-- name: UpdateAttachmentCategory :one
-- Change the category of an existing attachment, scoped to (id, draft_id, tenant_id).
-- A no-match (wrong id, wrong draft, or foreign tenant) → pgx.ErrNoRows →
-- ErrAttachmentNotFound (→ 404).
UPDATE draft_attachment
SET category = $4
WHERE id = $1 AND draft_id = $2 AND tenant_id = $3
RETURNING id, tenant_id, draft_id, document_id, category, position, created_at;

-- name: DeleteDraftAttachment :exec
-- Hard-delete the join row. The document itself is NOT touched (ON DELETE RESTRICT
-- enforces that in the FK). Scoped to (id, draft_id, tenant_id) — a miss is silently
-- ignored (the caller checks rows-affected via the :exec tag; our mapper maps 0 to
-- ErrAttachmentNotFound).
DELETE FROM draft_attachment
WHERE id = $1 AND draft_id = $2 AND tenant_id = $3;

-- name: GetDraftAttachments :many
-- Read model for the attachments list embedded in GET /v1/pecas/:id. Returns only
-- documents with status='UPLOADED' (a PENDING attachment is invisible — its bytes
-- have not landed yet). Ordered by (position ASC, created_at ASC) for stable display.
-- Scoped to (draft_id, tenant_id) — barrier 1; RLS on draft_attachment is barrier 2.
SELECT
    da.id,
    da.document_id,
    d.title         AS name,
    d.original_filename,
    da.category,
    d.mime_type,
    d.size_bytes,
    d.status,
    da.position,
    da.created_at
FROM draft_attachment da
JOIN document d ON d.id = da.document_id
WHERE da.draft_id = $1
  AND da.tenant_id = $2
  AND d.status = 'UPLOADED'
  AND d.deleted_at IS NULL
ORDER BY da.position ASC, da.created_at ASC;

-- name: GetAttachmentForUpdate :one
-- Load an attachment for update/delete guard: resolves (id, draft_id, tenant_id) to
-- confirm it belongs to the right draft and tenant. A miss → pgx.ErrNoRows →
-- ErrAttachmentNotFound (→ 404).
SELECT id, tenant_id, draft_id, document_id, category, position, created_at
FROM draft_attachment
WHERE id = $1 AND draft_id = $2 AND tenant_id = $3;

-- name: GetDocumentForAttachment :one
-- Load the minimal document fields the POST /v1/pecas/:id/anexos use case needs to
-- validate before linking: status (must be UPLOADED) and origin (must be UPLOAD, never
-- COURT). Scoped to (id, tenant_id) — a foreign or missing document → pgx.ErrNoRows →
-- ErrDocumentNotFound (→ 404). Filters deleted_at IS NULL (a soft-deleted document
-- cannot be attached).
SELECT id, tenant_id, status, origin
FROM document
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL;

-- name: GetIntimationForDraft :one
-- Load the intimation context needed to build a draft from source=intimation:
-- the case_id (via court_record), the court_record_id, the type (for piece_type
-- inference), the rich context fields (content, process metadata, deadline)
-- used to compose the AI generation prompt, and the recipients jsonb (for
-- signing-lawyer resolution — matched=true recipient is our advogado). Filtered
-- by intimation.id and tenant_id (barrier 1 via court_record).
-- A miss → pgx.ErrNoRows → ErrIntimationNotFound (→ 404).
SELECT
    i.id                AS intimation_id,
    cr.case_id          AS case_id,
    cr.id               AS court_record_id,
    i.type              AS intimation_type,
    i.content           AS intimation_content,
    i.recipients        AS recipients,
    cr.cnj_number       AS cnj_number,
    cr.court            AS court,
    cr.degree           AS degree,
    cr.class            AS class,
    cr.subject          AS subject,
    cr.judging_body     AS judging_body,
    dl.end_date         AS deadline_end_date
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline dl ON dl.notification_id = i.id
WHERE i.id = $1 AND cr.tenant_id = $2;

-- name: GetProvidencesForIntimation :many
-- Read model helper (Peça v2): providences shown on the FE sidebar are the
-- tasks linked to the draft's intimation. Tenant-scoped (barrier 1), OPEN +
-- DONE only (DISMISSED tasks disappear from the peça sidebar — the advogado
-- discarded them). Ordered by (status ASC — OPEN first, DONE below, then
-- created_at ASC for stable display).
--
-- Read cross-slice directly (same pattern as GetDraftDetail reading court_record
-- and party without importing acquisition — see docs §5b.2).
SELECT id, title, kind, source, status
FROM task
WHERE tenant_id = $1 AND intimation_id = $2 AND status IN ('OPEN', 'DONE')
ORDER BY
    CASE status WHEN 'OPEN' THEN 0 WHEN 'DONE' THEN 1 ELSE 2 END,
    created_at ASC;

-- name: GetPartiesForDraft :many
-- Load the parties (autor/réu/terceiro) and their advogados for a given case,
-- tenant-scoped (barrier 1). Used by the draft generation pipeline to inject
-- structured party names and counsel info into the AI prompt — the draft slice
-- reads party/party_counsel directly without importing the acquisition slice
-- (same pattern as GetDraftDetail for court_record). counsels defaults to an
-- empty jsonb array (never NULL) when a party has no advogado. Ordered by
-- role then name for deterministic iteration.
SELECT p.id, p.role, p.name,
       COALESCE(
         (SELECT jsonb_agg(
                   jsonb_build_object('name', pc.name, 'oab', pc.oab, 'uf', pc.uf)
                   ORDER BY pc.name
                 )
          FROM party_counsel pc
          WHERE pc.party_id = p.id AND pc.tenant_id = p.tenant_id),
         '[]'::jsonb
       )::text AS counsels
FROM party p
WHERE p.tenant_id = $1 AND p.case_id = $2
ORDER BY p.role, p.name;

-- ── AI generation queries (Peticionamento Fatia 3) ───────────────────────────
-- These are the three new queries the async generation saga needs.

-- name: SetGenerationParams :exec
-- Persist the Gerar-time generation params (tone/instructions/selected_theses,
-- Fatia 5) chosen on POST /v1/pecas/:id/generate. Called by TriggerGeneration in
-- the SAME tx as UpdateSagaState → EXTRACTING; the draft.generation_requested event
-- payload does NOT carry these — the async worker rereads the draft row instead.
-- Scoped to (id, tenant_id) — barrier 1; RLS is barrier 2. The caller already
-- tenant-guarded (id, tenant_id) via GetDraftByID earlier in the same tx, so a
-- no-match here is not expected in practice; the repo does not re-check
-- RowsAffected (mirrors DeleteReviewsForDraft's :exec, which is also fire-and-forget
-- within an already-guarded tx).
UPDATE draft
SET tone            = $3,
    instructions    = $4,
    selected_theses = $5,
    updated_at      = now()
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateSagaState :one
-- Transition the draft's saga_state (EXTRACTING when generation is triggered, REVIEWED
-- on success, FAILED on LLM error). Also updates content when the generator returns new
-- text ($4=true → $5 overwrites; false → leaves content unchanged — used for the FAILED
-- path). Same for structured_content ($6=true → $7 jsonb overwrites) — the DRAFTED path
-- writes BOTH content + structured_content in one tx (dual write for Peça v2).
-- Scoped to (id, tenant_id) — barrier 1; RLS is barrier 2. No-match → ErrDraftNotFound.
UPDATE draft
SET saga_state         = $3,
    content            = CASE WHEN $4::boolean THEN $5 ELSE content END,
    structured_content = CASE WHEN $6::boolean THEN $7 ELSE structured_content END,
    updated_at         = now()
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, case_id, intimation_id,
          piece_type, title, content,
          status, saga_state,
          created_at, updated_at;

-- name: UpdateDraftAuthorship :one
-- Flip the peça's authorship marker. Called by POST /v1/pecas/:id/assume-authorship
-- when the advogado clicks "Assumir autoria" — from that moment the FE hides the
-- Iterar tab and shows Revisão. Idempotent: a repeat call is a no-op at the DB level
-- (same UPDATE). Scoped to (id, tenant_id). A no-match → pgx.ErrNoRows → ErrDraftNotFound.
UPDATE draft
SET authorship = $3,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, case_id, intimation_id,
          piece_type, title, content,
          status, saga_state,
          created_at, updated_at,
          structured_content, authorship;

-- name: WriteBackStructuredContent :exec
-- Best-effort lazy backfill: when GET /v1/pecas/:id parses a plain-text `content`
-- into a StructuredContent on the fly (structured_content IS NULL for drafts
-- created before migration 0056 / Fatia B), this UPDATE persists the parsed shape
-- so subsequent reads skip the parser. Fire-and-forget within the same tx — the
-- caller does NOT check RowsAffected (a race where another writer already
-- populated it is harmless — the last writer wins). Scoped to (id, tenant_id).
UPDATE draft
SET structured_content = $3
WHERE id = $1 AND tenant_id = $2
  AND structured_content IS NULL;

-- name: InsertReview :one
-- Persist one AI review (findings + coverage as jsonb, model_version, rules_version,
-- status, generated_at). No tenant_id — isolation is via JOIN on draft.tenant_id (the
-- caller already tenant-guarded the draft before calling this). ON CONFLICT is absent
-- (multiple reviews per draft are allowed; only the LATEST is exposed by the read model).
INSERT INTO review (draft_id, findings, coverage, model_version, rules_version, status, generated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, draft_id, findings, coverage, model_version, rules_version, status, generated_at, created_at;

-- name: DeleteReviewsForDraft :exec
-- Remove all review rows for a draft. Called by Gerar before persisting DRAFTED so
-- that subsequent Revisar calls always operate on a clean slate (no stale suggestions
-- from a prior generation attempt are mixed with a new minuta).
-- tenant isolation: review has no tenant_id (child of draft); the caller guards the
-- tenant via GetDraftByID(tenantID, draftID) earlier in the same tx. Do NOT "fix"
-- this with a JOIN — the app-layer barrier is intentional (see 0044_review_status).
DELETE FROM review WHERE draft_id = $1;

-- name: GetLatestReview :one
-- Read model: the most recent review for a draft, ordered by generated_at DESC LIMIT 1.
-- Used by GET /v1/pecas/:id to surface the latest AI result alongside the draft content.
-- A draft with no reviews → pgx.ErrNoRows (the read model maps this to nil, not an error).
SELECT id, draft_id, findings, coverage, model_version, rules_version, status, generated_at, created_at
FROM review
WHERE draft_id = $1
ORDER BY generated_at DESC
LIMIT 1;

-- ── Chat queries (Peticionamento Fatia 3b) ───────────────────────────────────
-- Isolation: no tenant_id on chat_message — barrier 1 is enforced by the caller
-- first tenant-guarding the draft (same pattern as review, documented in 0044 and 0045).

-- name: InsertChatMessage :one
-- Append one turn (user or assistant) to the thread. No tenant guard here — the caller
-- must have already verified tenant ownership of draft_id before calling this.
-- citations is a jsonb array of {document_id, page, quote}; empty [] for user turns.
INSERT INTO chat_message (draft_id, role, content, citations, grounded, model_version)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, draft_id, role, content, citations, grounded, model_version, created_at;

-- name: GetChatThread :many
-- Load the last 50 messages for a draft, ordered chronologically (oldest first).
-- Returns at most 50 rows (LIMIT 50 on a subquery ordered DESC so the result is
-- the most-recent 50 turned back into ASC order for display).
-- No tenant guard here — the caller must have verified draft ownership.
SELECT id, draft_id, role, content, citations, grounded, model_version, created_at
FROM (
    SELECT id, draft_id, role, content, citations, grounded, model_version, created_at
    FROM chat_message
    WHERE draft_id = $1
    ORDER BY created_at DESC
    LIMIT 50
) sub
ORDER BY created_at ASC;

-- ── Peticionamento queries (Fatia 4) ────────────────────────────────────────

-- name: MarkSentToSigning :one
-- Marca o gesto "usuário clicou Enviar para assinatura" (0060). Só setta se
-- ainda não foi setado (idempotente sem sobrescrever timestamp original).
-- Zero linhas afetadas quando (a) draft não existe, (b) tenant errado, OU
-- (c) já estava setado — o caso (c) surface na app como "no-op" (não erro).
UPDATE draft
SET sent_to_signing_at = now(),
    updated_at         = now()
WHERE id = $1 AND tenant_id = $2 AND sent_to_signing_at IS NULL
RETURNING id, sent_to_signing_at, updated_at;

-- name: RevertToConstruction :one
-- Nulla sent_to_signing_at (usuário voltou pra Construção). Só permite quando
-- a peça AINDA não foi assinada (signed_at IS NULL) — depois de assinada, o
-- workflow não volta pra atrás sem invalidar a assinatura.
UPDATE draft
SET sent_to_signing_at = NULL,
    updated_at         = now()
WHERE id = $1 AND tenant_id = $2 AND signed_at IS NULL
RETURNING id, updated_at;

-- name: MarkFiled :one
-- Marca a peça como protocolada (Fatia 2a v0 — manual). Copia filed_at
-- opcionalmente informado pelo cliente (senão, agora). filing_number é opcional
-- (número do protocolo no tribunal — string livre). Requer status=SIGNED.
UPDATE draft
SET filed_at       = COALESCE(sqlc.narg('filed_at')::timestamptz, now()),
    filing_number  = sqlc.narg('filing_number')::text,
    updated_at     = now()
WHERE id = $1 AND tenant_id = $2 AND status = 'SIGNED' AND filed_at IS NULL
RETURNING id, filed_at, filing_number, updated_at;

-- name: SignDraftWithPDF :one
-- Fatia 2b: assina + grava a chave do PDF assinado no storage. Difere de
-- SignDraft porque também popula signed_pdf_key. Idempotente: re-assinar
-- devolve nil (a UI trata via Idempot flag).
UPDATE draft
SET status         = 'SIGNED',
    signed_at      = now(),
    signed_pdf_key = $3,
    updated_at     = now()
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, case_id, intimation_id,
          piece_type, title, content,
          status, saga_state,
          created_at, updated_at, signed_at;

-- name: SignDraft :one
-- Transition draft.status to SIGNED and set signed_at = now(). Scoped to
-- (id, tenant_id). A no-match → pgx.ErrNoRows → ErrDraftNotFound.
UPDATE draft
SET status     = 'SIGNED',
    signed_at  = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, case_id, intimation_id,
          piece_type, title, content,
          status, saga_state,
          created_at, updated_at, signed_at;

-- name: InsertPetition :one
-- Persist a filed petition (immutable). No tenant_id on petition — isolation
-- is via the draft FK (JOIN draft.tenant_id). Returns all columns.
INSERT INTO petition (draft_id, court_record_id, filed_at, receipt)
VALUES ($1, $2, $3, $4)
RETURNING id, draft_id, court_record_id, filed_at, receipt, observed_result;

-- name: GetPetitionByDraftID :one
-- Load the existing petition for a draft, scoped to tenant via JOIN. Returns
-- pgx.ErrNoRows when no petition exists (the caller treats nil as "not filed").
SELECT p.id, p.draft_id, p.court_record_id, p.filed_at, p.receipt, p.observed_result
FROM petition p
JOIN draft d ON d.id = p.draft_id
WHERE p.draft_id = $1 AND d.tenant_id = $2;

-- name: UpdateObservedResult :one
-- Patch the observed_result on a petition, scoped to tenant via JOIN. A miss
-- (no petition or wrong tenant) → pgx.ErrNoRows → ErrPetitionNotFound.
UPDATE petition
SET observed_result = $3
FROM draft d
WHERE petition.draft_id = $1
  AND d.id = petition.draft_id
  AND d.tenant_id = $2
RETURNING petition.id, petition.draft_id, petition.observed_result;

-- name: GetCourtRecordIDByIntimation :one
-- Resolve the court_record_id for an intimation, scoped to tenant via
-- court_record.tenant_id. Returns pgx.ErrNoRows when the intimation has no
-- linked court record (the caller treats empty string as "not found").
SELECT cr.id AS court_record_id
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.id = $1 AND cr.tenant_id = $2;

-- name: ListDraftsByProcess :many
-- Paginated list of peças for a given court_record_id, ordered by (created_at DESC,
-- id DESC). The :id param is court_record.id; we resolve case_id via JOIN.
-- Coverage summary is resolved from the latest review via LEFT JOIN LATERAL.
-- Over-fetch by 1 for hasMore detection.
SELECT
    d.id,
    d.piece_type,
    d.title,
    d.status,
    d.saga_state,
    d.created_at,
    p.filed_at,
    p.observed_result,
    r.coverage AS review_coverage
FROM draft d
JOIN court_record cr ON cr.case_id = d.case_id AND cr.tenant_id = d.tenant_id
LEFT JOIN petition p ON p.draft_id = d.id
LEFT JOIN LATERAL (
    SELECT rv.coverage
    FROM review rv
    WHERE rv.draft_id = d.id
    ORDER BY rv.generated_at DESC
    LIMIT 1
) r ON true
WHERE d.tenant_id = $1
  AND cr.id = $2
  AND (d.created_at, d.id) < ($3::timestamptz, $4::uuid)
ORDER BY d.created_at DESC, d.id DESC
LIMIT $5;

-- name: ListDraftsAll :many
-- Paginated list of all peças for a tenant, ordered by (created_at DESC,
-- id DESC). Optional piece_type and status filters. Coverage summary from
-- latest review via LEFT JOIN LATERAL. Over-fetch by 1 for hasMore detection.
SELECT
    d.id,
    d.piece_type,
    d.title,
    d.status,
    d.saga_state,
    d.created_at,
    p.filed_at,
    p.observed_result,
    r.coverage AS review_coverage
FROM draft d
LEFT JOIN petition p ON p.draft_id = d.id
LEFT JOIN LATERAL (
    SELECT rv.coverage
    FROM review rv
    WHERE rv.draft_id = d.id
    ORDER BY rv.generated_at DESC
    LIMIT 1
) r ON true
WHERE d.tenant_id = $1
  AND ($2::text = '' OR d.piece_type = $2)
  AND ($3::text = '' OR d.status = $3)
  AND (d.created_at, d.id) < ($4::timestamptz, $5::uuid)
ORDER BY d.created_at DESC, d.id DESC
LIMIT $6;
