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

-- name: GetIntimationForDraft :one
-- Load the intimation context needed to build a draft from source=intimation:
-- the case_id (via court_record), the court_record_id, and the type (for piece_type
-- inference). Filtered by intimation.id and tenant_id (barrier 1 via court_record).
-- A miss → pgx.ErrNoRows → ErrIntimationNotFound (→ 404).
SELECT
    i.id            AS intimation_id,
    cr.case_id      AS case_id,
    cr.id           AS court_record_id,
    i.type          AS intimation_type
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.id = $1 AND cr.tenant_id = $2;
