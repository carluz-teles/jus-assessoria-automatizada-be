-- Certificados A1 (ICP-Brasil) por advogado. Tenant-scoped em toda linha
-- (barrier 1 sempre no WHERE + RLS como barrier 2). ListCertificatesByTenant
-- OMITE ciphertext/wrapped_dek/nonce (lista pública não precisa do binário —
-- só o Get, chamado pelo Sign, os traz).

-- name: InsertCertificate :one
-- Cadastra um certificado após o BE parsear o .pfx, extrair metadata e cifrar
-- o binário com envelope (DEK+KMS). Dedup ativo é garantido pelo unique index
-- parcial (tenant, fingerprint) WHERE revoked_at IS NULL — se colidir, sqlc
-- devolve ErrUniqueViolation → o domain mapeia pra ErrCertificateAlreadyExists.
INSERT INTO certificate (
  id, tenant_id, owner_user_id,
  subject_cn, oab, issuer, serial,
  not_before, not_after, fingerprint,
  ciphertext, nonce, wrapped_dek, kek_ref
) VALUES (
  gen_random_uuid(), $1, $2,
  $3, $4, $5, $6,
  $7, $8, $9,
  $10, $11, $12, $13
)
RETURNING id, created_at;

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

-- name: RevokeCertificate :one
-- Soft-delete: marca revoked_at=now(). Idempotente (re-executar em linha
-- já revogada mantém a data original — o WHERE bloqueia). Zero linhas
-- afetadas quando a linha não existe ou já está revogada → ErrCertificateNotFound
-- no domain.
UPDATE certificate
   SET revoked_at = now()
 WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL
RETURNING id;
