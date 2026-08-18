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
          created_at, updated_at;

-- name: GetDraftByIntimationID :one
-- Fetch the draft that already exists for the (tenant_id, intimation_id) pair —
-- used by the idempotent POST path when the INSERT fails with 23505. Filters by
-- tenant (barrier 1).
SELECT id, tenant_id, case_id, intimation_id,
       piece_type, title, content,
       status, saga_state,
       created_at, updated_at
FROM draft
WHERE tenant_id = $1 AND intimation_id = $2;

-- name: GetDraftByID :one
-- Load the full peça aggregate by id, filtered by tenant (barrier 1). A miss or
-- foreign-tenant id yields pgx.ErrNoRows → ErrDraftNotFound (→ 404).
SELECT id, tenant_id, case_id, intimation_id,
       piece_type, title, content,
       status, saga_state,
       created_at, updated_at
FROM draft
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateDraftContent :one
-- Autosave: update content (and optionally title) + bump updated_at, scoped to
-- (id, tenant_id). Returns the minimal patch response fields. A no-match (wrong id
-- or foreign tenant) yields pgx.ErrNoRows → ErrDraftNotFound (→ 404).
UPDATE draft
SET content    = $3,
    title      = CASE WHEN $4::boolean THEN $5 ELSE title END,
    updated_at = now()
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
-- inference), and the rich context fields (content, process metadata, deadline)
-- used to compose the AI generation prompt. Filtered by intimation.id and
-- tenant_id (barrier 1 via court_record).
-- A miss → pgx.ErrNoRows → ErrIntimationNotFound (→ 404).
SELECT
    i.id                AS intimation_id,
    cr.case_id          AS case_id,
    cr.id               AS court_record_id,
    i.type              AS intimation_type,
    i.content           AS intimation_content,
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

-- ── AI generation queries (Peticionamento Fatia 3) ───────────────────────────
-- These are the three new queries the async generation saga needs.

-- name: UpdateSagaState :one
-- Transition the draft's saga_state (EXTRACTING when generation is triggered, REVIEWED
-- on success, FAILED on LLM error). Also updates content when the generator returns new
-- text ($3 non-NULL overwrites; NULL leaves content unchanged — used for the FAILED path
-- which does NOT touch content). Scoped to (id, tenant_id) — barrier 1; RLS is barrier 2.
-- A no-match → pgx.ErrNoRows → ErrDraftNotFound.
UPDATE draft
SET saga_state = $3,
    content    = CASE WHEN $4::boolean THEN $5 ELSE content END,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, case_id, intimation_id,
          piece_type, title, content,
          status, saga_state,
          created_at, updated_at;

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
