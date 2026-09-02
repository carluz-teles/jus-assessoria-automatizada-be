-- suggested_thesis queries (Sugerir Teses persistido, C1). Draft-scoped. Every
-- write runs inside the use case's transaction so RLS scopes it to the principal's
-- tenant (barrier 2) on top of the explicit tenant_id filter (barrier 1).

-- name: InsertSuggestedThesis :one
-- Persist one generated thesis. label/confidence/reference/foundation/evidence and
-- the source_* fields mirror the in-memory Thesis produced by SuggestTheses (RAG+LLM);
-- state/position are assigned by the persistence use case (initial state = pre-seleção
-- do ancorado/alta confiança). C2: draft_id E intimation_id são nullable (o repo passa
-- um preenchido e o outro NULL); o CHECK suggested_thesis_scope_chk garante EXATAMENTE
-- UM não-nulo na borda do banco.
INSERT INTO suggested_thesis (
    tenant_id, draft_id, intimation_id,
    label, confidence, reference, foundation, evidence,
    source_ref, source_document_id, source_page, source_excerpt, source_label,
    grounded, state, position,
    created_at, updated_at
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13,
    $14, $15, $16,
    now(), now()
)
RETURNING *;

-- name: ListSuggestedThesesByDraft :many
-- The persisted list for GET /v1/pecas/:id/theses. Ordered deterministically by the
-- position assigned at generation (sortTheses), created_at as tiebreaker.
SELECT * FROM suggested_thesis
WHERE draft_id = $1 AND tenant_id = $2
ORDER BY position, created_at;

-- name: UpdateSuggestedThesisState :one
-- Flip the selection state (off|pending_add|included|pending_remove). Scoped by
-- (id, tenant_id) — RLS + explicit filter. Zero rows (unknown id / foreign tenant)
-- yields pgx.ErrNoRows, mapped to ErrThesisNotFound (→ 404).
UPDATE suggested_thesis
SET state = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: DeleteSuggestedThesesByDraft :exec
-- Wipe a draft's suggested theses before a regenerate (POST always regenerates:
-- delete + gera + persiste). Scoped by (draft_id, tenant_id).
DELETE FROM suggested_thesis
WHERE draft_id = $1 AND tenant_id = $2;

-- name: ListSuggestedThesesByIntimation :many
-- The persisted list for GET /v1/intimacoes/:id/theses (partida). Same shape/order as
-- the draft-scoped list; scoped by (intimation_id, tenant_id).
SELECT * FROM suggested_thesis
WHERE intimation_id = $1 AND tenant_id = $2
ORDER BY position, created_at;

-- name: DeleteSuggestedThesesByIntimation :exec
-- Wipe an intimation's suggested theses before a regenerate (POST /intimacoes/:id/theses
-- always regenerates). Scoped by (intimation_id, tenant_id).
DELETE FROM suggested_thesis
WHERE intimation_id = $1 AND tenant_id = $2;
