-- name: InsertCourtConnection :one
INSERT INTO court_connection (
  tenant_id, app_user_id, court, system, authentication_method,
  credential_ref, certificate_ref, status
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING id, created_at;

-- name: GetCourtConnectionByID :one
SELECT * FROM court_connection
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateCourtConnectionStatus :one
UPDATE court_connection
SET status = $3,
    last_authenticated_at = $4,
    error = $5
WHERE id = $1 AND tenant_id = $2
RETURNING id;

-- name: UpdateCourtConnectionMFASeedRef :one
UPDATE court_connection
SET mfa_seed_ref = $3
WHERE id = $1 AND tenant_id = $2
RETURNING id;

-- name: ListCourtConnectionsByTenant :many
SELECT * FROM court_connection
WHERE tenant_id = $1
ORDER BY created_at;

-- name: InsertTenantSecret :one
INSERT INTO tenant_secret (tenant_id, ciphertext, nonce, wrapped_dek, dek_nonce)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: GetTenantSecretByID :one
SELECT * FROM tenant_secret
WHERE id = $1 AND tenant_id = $2;
