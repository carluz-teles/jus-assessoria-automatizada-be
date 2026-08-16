-- name: InsertTenantSecret :one
-- Persists one sealed secret (lib/vault.Sealed already encrypted it — this
-- statement never sees plaintext). Called once per Configure — a reconfigure
-- (new password) inserts a NEW row and the caller repoints portal_credential's
-- credential_ref at it; the old row is deleted by the same tx (see
-- DeleteTenantSecret), so no orphan accumulates.
INSERT INTO tenant_secret (
    tenant_id, ciphertext, nonce, wrapped_dek, dek_nonce
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING *;

-- name: GetTenantSecret :one
-- Reads one sealed secret back for lib/vault.Open, scoped by tenant (RLS is the
-- second barrier; this WHERE is the first, app-level one).
SELECT * FROM tenant_secret
WHERE tenant_id = $1 AND id = $2;

-- name: DeleteTenantSecret :exec
-- Removes a sealed secret — called when a credential is reconfigured (drop the
-- OLD secret after the new one is written and portal_credential repointed) or
-- deleted outright (drop the secret alongside the portal_credential row).
DELETE FROM tenant_secret
WHERE tenant_id = $1 AND id = $2;

-- name: UpsertPortalCredential :one
-- One row per (tenant, app_user, portal). A reconfigure (same advogado, same
-- portal) overwrites login/credential_ref/status/last_error/last_verified_at —
-- the credential the advogado just tested is the one in effect, never merged
-- with the stale row.
INSERT INTO portal_credential (
    tenant_id, app_user_id, portal, login, credential_ref,
    status, last_error, last_verified_at, configured_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
ON CONFLICT (tenant_id, app_user_id, portal) DO UPDATE SET
    login             = EXCLUDED.login,
    credential_ref    = EXCLUDED.credential_ref,
    status            = EXCLUDED.status,
    last_error        = EXCLUDED.last_error,
    last_verified_at  = EXCLUDED.last_verified_at,
    configured_by     = EXCLUDED.configured_by,
    updated_at        = now()
RETURNING *;

-- name: GetPortalCredential :one
-- The caller's OWN credential for a portal — never another app_user's (tenant_id
-- + app_user_id + portal is the whole natural key). Used by GET and by the
-- reconfigure/delete flows to resolve the current credential_ref first.
SELECT * FROM portal_credential
WHERE tenant_id = $1 AND app_user_id = $2 AND portal = $3;

-- name: DeletePortalCredential :exec
-- Removes the caller's own credential row. The use case deletes the pointed-at
-- tenant_secret row in the SAME tx (via DeleteTenantSecret), so no secret
-- material outlives the credential that referenced it.
DELETE FROM portal_credential
WHERE tenant_id = $1 AND app_user_id = $2 AND portal = $3;
