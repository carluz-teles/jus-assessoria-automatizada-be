-- action_item queries (docs/erd-costura-providencia-tarefa-peca.md §2/§3). Every :one query
-- below projects the FULL, same-order action_item column list so sqlc reuses one row shape
-- (actionitemdb.ActionItem) across Insert/Get/Confirm/Discard — mapper.go's fromRow covers
-- them all.

-- name: InsertActionItem :one
-- Materializes one providência candidate (the listener's OnIntimationAnalyzed, one row per
-- candidate in the event payload). task_id is never set here — a future fatia's deadline
-- listener binds it once the item is confiável and turned into work.
INSERT INTO action_item (
    id, tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
    tipo_origem, tipo_status, deadline_id, confianca, status, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
)
RETURNING id, tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
    tipo_origem, tipo_status, deadline_id, confianca, status, task_id, created_at, updated_at;

-- name: GetActionItem :one
-- One action_item by id, scoped to tenantID (barrier 1). A miss/foreign row → pgx.ErrNoRows
-- → the repo's typed ErrActionItemNotFound, never (nil, nil). Backs both confirmar/descartar's
-- pre-read (to classify idempotent-no-op vs terminal-conflict vs proceed) and any future
-- direct lookup.
SELECT id, tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
    tipo_origem, tipo_status, deadline_id, confianca, status, task_id, created_at, updated_at
FROM action_item
WHERE id = $1 AND tenant_id = $2;

-- name: ConfirmActionItem :one
-- The guarded UPDATE behind POST /v1/action-items/:id/confirmar: tipo_status a_confirmar →
-- confiável. Guarded by `tipo_status = 'a_confirmar' AND status <> 'DISCARDED'` so a racing
-- descartar (or a second confirmar) cannot double-apply the transition — the concurrency
-- floor, mirroring deadline's guarded MarkTaskStatus. Zero rows means the use case's pre-read
-- raced with a concurrent write; the use case classifies that as ErrActionItemConflict.
UPDATE action_item
SET tipo_status = 'confiavel', updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND tipo_status = 'a_confirmar' AND status <> 'DISCARDED'
RETURNING id, tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
    tipo_origem, tipo_status, deadline_id, confianca, status, task_id, created_at, updated_at;

-- name: DiscardActionItem :one
-- The guarded UPDATE behind POST /v1/action-items/:id/descartar: status → DISCARDED.
-- Guarded by `status <> 'DISCARDED'` — the concurrency floor, same shape as ConfirmActionItem.
UPDATE action_item
SET status = 'DISCARDED', updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status <> 'DISCARDED'
RETURNING id, tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
    tipo_origem, tipo_status, deadline_id, confianca, status, task_id, created_at, updated_at;

-- name: DeleteReplaceableActionItems :exec
-- The listener's guard aditivo (OnIntimationAnalyzed, docs handoff §"Guard obrigatório"):
-- clears ONLY the items a re-analysis is allowed to replace — SUGGESTED, a_confirmar, and
-- not yet bound to a task. A confiável (declarado/manual) or already-task-bound item is
-- NEVER touched here; the fresh candidates are inserted right after in the same tx.
DELETE FROM action_item
WHERE tenant_id = $1 AND intimation_id = $2
  AND task_id IS NULL AND status = 'SUGGESTED' AND tipo_status = 'a_confirmar';

-- name: LinkActionItemTask :one
-- The reverse pointer of the providência→tarefa loop (docs §2/§6, fatia 3): once deadline's
-- listener creates the task for a confiável action_item and announces task.created (carrying
-- action_item_id), this UPDATE writes task_id + flips status → CONFIRMED. Guarded by
-- task_id IS NULL: a redelivered task.created (past the consumer dedup, belt-and-suspenders)
-- or a missing/foreign id both yield ZERO rows here, mapped by the repo to the SAME
-- ErrActionItemNotFound GetActionItem uses — the use case treats either as a safe no-op,
-- never overwriting an already-bound task_id.
UPDATE action_item
SET task_id = $3, status = 'CONFIRMED', updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND task_id IS NULL
RETURNING id, tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
    tipo_origem, tipo_status, deadline_id, confianca, status, task_id, created_at, updated_at;

-- name: ExistsActionItemByTipo :one
-- Dedup guard for the confiável (declarado/manual) candidates the delete above never
-- clears: without it, re-running "Analisar" on an intimação whose teor keeps declaring the
-- same providência would insert a duplicate confiável row every time. Scoped by
-- (tenant, intimation, tipo, tipo_origem) — the listener skips inserting a candidate that
-- already has a committed match on all four.
SELECT EXISTS (
    SELECT 1 FROM action_item
    WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3 AND tipo_origem = $4
);

-- name: HasFiledDraftForActionItem :one
-- Fatia 5 (docs §7 questão 4) guard for POST /v1/action-items/:id/reclassificar: once the
-- providência's peça has been FILED (protocolada), the providência is frozen — reclassifying
-- now would orphan a filed document with no paper trail of why the tipo changed. Cross-slice
-- JOIN (task/draft are owned by internal/deadline and internal/draft respectively) — never a
-- Go import, same pattern internal/draft's GetActionItemForTask already uses in reverse.
-- superseded_at IS NULL scopes to the VIGENTE draft only: a task can carry a superseded +
-- filed draft from a PRIOR reclassify round, which must never block a new one.
SELECT EXISTS (
    SELECT 1
    FROM task t
    JOIN draft d ON d.task_id = t.id AND d.superseded_at IS NULL
    WHERE t.action_item_id = $1 AND t.tenant_id = $2 AND d.filed_at IS NOT NULL
);

-- name: ReclassifyActionItem :one
-- The guarded UPDATE behind POST /v1/action-items/:id/reclassificar (fatia 5, docs §7
-- questão 4): overrides tipo/piece_profile_key with tipo_origem='manual'/
-- tipo_status='confiavel' (the same override precedence §3's motor already uses) and FORCES
-- gera_peca=true — this endpoint only covers "ainda gera peça, tipo diferente"; converting to
-- ciência is a future fatia's scope. confianca is reset to NULL: migration 0086's CHECK
-- (tipo_origem = 'ia' OR confianca IS NULL) would otherwise reject overriding an
-- ia-classified item's tipo_origem away from 'ia' while its old confidence score lingers.
-- Guarded by `status <> 'DISCARDED'` — mirrors ConfirmActionItem/DiscardActionItem's
-- concurrency floor: zero rows means a concurrent descartar won the race between the use
-- case's pre-read and this UPDATE.
UPDATE action_item
SET piece_profile_key = $3,
    tipo               = $4,
    tipo_origem        = 'manual',
    tipo_status        = 'confiavel',
    gera_peca          = true,
    confianca          = NULL,
    updated_at         = now()
WHERE id = $1 AND tenant_id = $2 AND status <> 'DISCARDED'
RETURNING id, tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
    tipo_origem, tipo_status, deadline_id, confianca, status, task_id, created_at, updated_at;
