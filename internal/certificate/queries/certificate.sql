-- certificate slice queries. Every statement runs inside the use case's tx so RLS
-- scopes it to the principal's tenant (barrier 2) on top of the explicit tenant_id
-- filter (barrier 1). Absence is a typed error at the mapper, never (nil, nil).
--
-- SECURITY: the SELECT for the list read (ListCertificates) deliberately does NOT
-- project ciphertext/nonce/wrapped_dek — a screen read never carries key material.
-- The insert stores the envelope; there is no query in this fatia that reads the
-- ciphertext back out (no download-the-key endpoint exists).

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

-- name: ListCertificates :many
-- The GET /v1/certificates read model: a tenant's certificates, newest first, with
-- the owner lawyer's display name joined from app_user. METADATA ONLY — no envelope
-- columns. Scoped to tenant_id (barrier 1) on top of RLS (barrier 2). $1 = tenant_id.
SELECT c.id, c.tenant_id, c.owner_user_id, c.subject_cn, c.oab, c.issuer, c.serial,
       c.not_before, c.not_after, c.fingerprint, c.created_at, c.revoked_at,
       u.name AS owner_user_name
FROM certificate c
LEFT JOIN app_user u ON u.id = c.owner_user_id
WHERE c.tenant_id = $1
ORDER BY c.created_at DESC, c.id DESC;

-- name: GetCertificate :one
-- Load one certificate's metadata for the revoke path — scoped to tenant_id so a
-- foreign id never resolves. A miss → pgx.ErrNoRows → ErrCertificateNotFound at the
-- mapper. METADATA ONLY. $1 = id, $2 = tenant_id.
SELECT id, tenant_id, owner_user_id, subject_cn, oab, issuer, serial,
       not_before, not_after, fingerprint, created_at, revoked_at
FROM certificate
WHERE id = $1 AND tenant_id = $2;

-- name: GetCertificateEnvelope :one
-- Load one certificate's ENVELOPE (the encrypted key material) plus the fields
-- needed to gate signing (not_after, revoked_at), scoped to tenant_id. This is the
-- ONLY query that projects ciphertext/nonce/wrapped_dek — it exists solely for the
-- server-side signing path (POST /v1/certificates/:id/sign), which must decrypt the
-- .pfx to reach the private key. A miss → pgx.ErrNoRows → ErrCertificateNotFound at
-- the mapper. The bytes never leave the backend. $1 = id, $2 = tenant_id.
SELECT id, tenant_id, owner_user_id, not_after, revoked_at,
       ciphertext, nonce, wrapped_dek, kek_ref
FROM certificate
WHERE id = $1 AND tenant_id = $2;

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
