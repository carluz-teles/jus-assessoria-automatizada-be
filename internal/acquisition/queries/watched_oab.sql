-- watched_oab queries (acquisition slice). The per-tenant index of watched OABs the
-- national match joins against. Populated from the integration_activated event: the
-- use case replaces an integration's set (delete + insert) so a scope change is
-- reflected. Writes run per-tenant (RLS by app.tenant_id); the match reads system-wide.

-- name: DeleteWatchedOABsByIntegration :exec
-- Clear an integration's watched OABs before re-populating, so a removed OAB stops
-- matching. Scoped to the integration (the tenant is implied by the RLS tx).
DELETE FROM watched_oab WHERE integration_id = $1;

-- name: InsertWatchedOAB :exec
-- Add one watched OAB for an integration, idempotent on (integration_id, oab_key):
-- a re-activation with the same scope is a no-op. oab_key is the normalized "NUMBER|UF".
INSERT INTO watched_oab (tenant_id, integration_id, oab_key)
VALUES ($1, $2, $3)
ON CONFLICT (integration_id, oab_key) DO NOTHING;

-- name: ListWatchedOABsWithName :many
-- Termos monitorados com nome derivado: as OABs monitoradas pelo tenant via DJEN,
-- cada uma com o nome mais frequente encontrado em party_counsel (mode() within group).
-- oab_key é "NUMBER|UF"; devolvemos "UFNUMBER" (canônico do FE) via uf||split_part.
-- O LEFT JOIN garante que OABs novas (sem captura ainda) retornam com name = NULL.
-- Ordenado por oab_key para estabilidade (sem offset, lista pequena).
SELECT
    (split_part(w.oab_key, '|', 2) || split_part(w.oab_key, '|', 1))::text AS oab,
    (mode() WITHIN GROUP (ORDER BY pc.name))::text AS name
FROM watched_oab w
JOIN integration i ON i.id = w.integration_id AND i.source = 'DJEN'
LEFT JOIN party_counsel pc
    ON pc.oab  = split_part(w.oab_key, '|', 1)
   AND pc.uf   = split_part(w.oab_key, '|', 2)
   AND pc.tenant_id = w.tenant_id
WHERE w.tenant_id = $1
GROUP BY w.oab_key
ORDER BY w.oab_key;
