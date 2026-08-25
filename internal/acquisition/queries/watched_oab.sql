-- watched_oab queries (acquisition slice). The per-tenant index of watched OABs the
-- national match joins against. Populated from the integration_activated event (a
-- fresh activation or a NEW OAB added to an already-active scope) and from the
-- Termos toggle (disable/re-enable one OAB without touching the rest). Writes run
-- per-tenant (RLS by app.tenant_id); the match reads system-wide.

-- name: UpsertWatchedOAB :one
-- Adds (or re-enables) one watched OAB for an integration, and reports the PRIOR
-- state via was_enabled so the caller (backfill.go) can distinguish the three cases
-- a re-activation/toggle-on can land in:
--   was_enabled IS NULL  -> the row did not exist yet (brand-new OAB)   -> needs HISTORY
--   was_enabled = false  -> it existed but was disabled (a real re-enable) -> needs CATCH-UP
--   was_enabled = true   -> it already existed enabled (idempotent replay) -> no-op
-- The "prior_row" CTE reads the pre-upsert row (or no row) BEFORE the INSERT ... ON CONFLICT
-- mutates it. It is joined in (rather than a correlated scalar subquery in the RETURNING
-- list) so sqlc infers was_enabled as NULLABLE — a plain subquery-in-RETURNING is typed
-- NOT NULL by sqlc's analyzer (it only looks at the source column's constraint, not the
-- subquery's cardinality), which would panic scanning a real NULL on the insert path.
-- catch_up_since is preserved (COALESCEd) rather than overwritten so a second re-enable
-- before the pending catch-up is cleared does not lose the earlier disabled_at. The "wo"
-- alias (rather than an unqualified watched_oab reference) works around a sqlc analyzer
-- limitation: with a sibling writable CTE also touching watched_oab, an unqualified
-- column here is reported as ambiguous even though plain Postgres parses it fine.
-- last_action/last_action_at feed the Termos "última ação" label. The INSERT branch
-- (brand-new row) always stamps ADDED. The DO UPDATE branch only stamps REENABLED
-- when the row was actually disabled before (watched_oab.enabled, like
-- catch_up_since above, reads the PRE-update row inside DO UPDATE) — an idempotent
-- replay of an already-enabled row leaves the prior action/timestamp untouched.
WITH prior_row AS (
    SELECT wo.enabled
    FROM watched_oab wo
    WHERE wo.integration_id = $2 AND wo.oab_key = $3
), upsert AS (
    INSERT INTO watched_oab (tenant_id, integration_id, oab_key, enabled, last_action, last_action_at)
    VALUES ($1, $2, $3, true, 'ADDED', now())
    ON CONFLICT (integration_id, oab_key) DO UPDATE
       SET enabled        = true,
           catch_up_since = COALESCE(watched_oab.catch_up_since, watched_oab.disabled_at),
           last_action    = CASE WHEN watched_oab.enabled THEN watched_oab.last_action ELSE 'REENABLED' END,
           last_action_at = CASE WHEN watched_oab.enabled THEN watched_oab.last_action_at ELSE now() END
    RETURNING *
)
SELECT upsert.*, prior_row.enabled AS was_enabled
FROM upsert
LEFT JOIN prior_row ON true;

-- name: DisableWatchedOAB :one
-- Turns capture OFF for one watched OAB, stamping disabled_at (the catch-up horizon
-- a later re-enable will resume from). Guarded to the enabled->disabled transition so
-- a repeat disable is a no-op (0 rows) rather than clobbering an earlier disabled_at;
-- the repo treats a miss by re-reading the current row (SELECT), never an error.
UPDATE watched_oab
   SET enabled = false, disabled_at = now(), last_action = 'DISABLED', last_action_at = now()
 WHERE integration_id = $1 AND oab_key = $2 AND enabled = true
RETURNING *;

-- name: GetWatchedOAB :one
-- Reads the current row for (integration, oab_key) — used by DisableWatchedOAB's
-- no-op path (already-disabled) and by ToggleWatchedOAB's 404 guard (never existed).
SELECT * FROM watched_oab WHERE integration_id = $1 AND oab_key = $2;

-- name: ClearWatchedOABCatchUp :exec
-- Compare-and-clear: only wipes catch_up_since when it still equals the value the
-- caller dispatched the catch-up sync with. This guards against a LATE sync_completed
-- (from a stale, already-superseded catch-up) clobbering a NEWER catch_up_since set by
-- a subsequent disable/re-enable cycle.
UPDATE watched_oab
   SET catch_up_since = NULL
 WHERE integration_id = $1 AND oab_key = $2 AND catch_up_since = $3;

-- name: ListWatchedOABsWithName :many
-- Termos monitorados com nome derivado: as OABs monitoradas pelo tenant via DJEN,
-- cada uma com o nome mais frequente encontrado em party_counsel (mode() within group)
-- e o estado enabled (liga/desliga do card na tela de Termos). oab_key é "NUMBER|UF";
-- devolvemos "UFNUMBER" (canônico do FE) via uf||split_part. O LEFT JOIN garante que
-- OABs novas (sem captura ainda) casam zero linhas de party_counsel; mode() sobre um
-- grupo todo NULL também devolve NULL — o COALESCE pra '' evita isso (sqlc infere a
-- coluna como NOT NULL pelo ::text, e o scan do pgx falha contra um NULL de verdade;
-- repository.go já trata "" como "sem nome"). bool_and(w.enabled) é seguro porque o
-- GROUP BY é por oab_key e (integration_id, oab_key) é UNIQUE — sempre uma única linha
-- por grupo. Ordenado por oab_key para estabilidade (sem offset, lista pequena).
SELECT
    (split_part(w.oab_key, '|', 2) || split_part(w.oab_key, '|', 1))::text AS oab,
    COALESCE(mode() WITHIN GROUP (ORDER BY pc.name), '')::text AS name,
    bool_and(w.enabled)::bool AS enabled,
    -- COALESCE to '' (not a bare ::text cast) so sqlc infers a NOT NULL string
    -- without a scan panic on rows created before this column existed (NULL) —
    -- same fix as the `name` COALESCE above, for the same sqlc-nullability gotcha.
    COALESCE(max(w.last_action), '')::text AS last_action,
    max(w.last_action_at)::timestamptz AS last_action_at
FROM watched_oab w
JOIN integration i ON i.id = w.integration_id AND i.source = 'DJEN'
LEFT JOIN party_counsel pc
    ON pc.oab  = split_part(w.oab_key, '|', 1)
   AND pc.uf   = split_part(w.oab_key, '|', 2)
   AND pc.tenant_id = w.tenant_id
WHERE w.tenant_id = $1
GROUP BY w.oab_key
ORDER BY w.oab_key;
