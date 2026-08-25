-- certificate slice queries. Every statement runs inside the use case's tx so RLS
-- scopes it to the principal's tenant (barrier 2) on top of the explicit tenant_id
-- filter (barrier 1). Absence is a typed error at the mapper, never (nil, nil).
--
-- SECURITY: the SELECT for the list read (ListCertificatesByTenant) deliberately
-- does NOT project ciphertext/nonce/wrapped_dek — a screen read never carries key
-- material. Only GetCertificateByID (the sign path) projects the envelope.

-- name: InsertCertificate :one
-- Persist a parsed + envelope-encrypted certificate (POST /v1/certificates). All
-- metadata columns come from the parsed x509; the four envelope columns come from
-- seal(). tenant_id and owner_user_id come from the trusted principal, never the
-- body. Returns the whole row so the handler renders the CertificateView.
INSERT INTO certificate (
    tenant_id, owner_user_id,
    subject_cn, oab, issuer, serial, not_before, not_after, fingerprint,
    ciphertext, nonce, wrapped_dek, kek_ref
) VALUES (
    $1, $2,
    $3, $4, $5, $6, $7, $8, $9,
    $10, $11, $12, $13
)
RETURNING id, tenant_id, owner_user_id, subject_cn, oab, issuer, serial,
          not_before, not_after, fingerprint, created_at, revoked_at;

-- name: InsertSigningEvent :one
-- Record that a certificate signed a digest (audit trail, committed in the SAME tx
-- as the sign so the act and its record land together). Stores the SHA-256 digest
-- only — never the signature, the key, or the password. tenant_id + signer_user_id
-- come from the trusted principal. $1 = tenant_id, $2 = certificate_id,
-- $3 = signer_user_id, $4 = digest_sha256.
INSERT INTO signing_event (tenant_id, certificate_id, signer_user_id, digest_sha256)
VALUES ($1, $2, $3, $4)
RETURNING id, signed_at;

-- name: RevokeCertificate :one
-- Soft-revoke a certificate (DELETE /v1/certificates/:id): stamp revoked_at = $3
-- when it is still active (revoked_at IS NULL), scoped to tenant_id. A no-row result
-- (missing, foreign, or already revoked) → pgx.ErrNoRows → ErrCertificateNotFound at
-- the mapper. The row stays for audit. $1 = id, $2 = tenant_id, $3 = revoked_at.
UPDATE certificate
SET revoked_at = $3
WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL
RETURNING id, tenant_id, owner_user_id, subject_cn, oab, issuer, serial,
          not_before, not_after, fingerprint, created_at, revoked_at;

-- name: GetCertificateByID :one
-- Um certificado por id, tenant-scoped. Inclui o envelope (uso interno pra o
-- Sign decifrar o .pfx). Devolve linhas revogadas também — o domain decide.
SELECT id, tenant_id, owner_user_id,
       subject_cn, oab, issuer, serial,
       not_before, not_after, fingerprint,
       ciphertext, nonce, wrapped_dek, kek_ref,
       created_at, revoked_at
FROM certificate
WHERE id = $1 AND tenant_id = $2;

-- name: ListCertificatesByTenant :many
-- Lista os ATIVOS do tenant (revoked_at IS NULL), ordenados por criação DESC.
-- Junção LEFT com app_user pra devolver o nome do owner. tenant-scoped.
SELECT c.id, c.tenant_id, c.owner_user_id,
       c.subject_cn, c.oab, c.issuer, c.serial,
       c.not_before, c.not_after, c.fingerprint,
       c.created_at, c.revoked_at,
       u.name AS owner_user_name
FROM certificate c
LEFT JOIN app_user u ON u.id = c.owner_user_id
WHERE c.tenant_id = $1
  AND c.revoked_at IS NULL
ORDER BY c.created_at DESC;
