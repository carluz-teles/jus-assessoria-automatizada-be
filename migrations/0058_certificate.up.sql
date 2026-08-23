-- 0058_certificate — cadastro dos certificados digitais A1 (ICP-Brasil, .pfx)
-- por advogado. Cada linha é a METADATA extraída do PFX no upload; o binário
-- cifrado vive no object storage (storage_key), fora do banco. A senha do PFX
-- NUNCA é persistida: sempre pedida na hora de assinar, então mesmo dump do DB +
-- do bucket não permite forjar assinatura sem a senha do titular.
--
-- Dupla proteção at-rest:
--   1) o .pfx já é cifrado por senha (proteção nativa PKCS#12);
--   2) o BE cifra o blob com uma master key (env var CERT_MASTER_KEY, AES-GCM
--      app-level) antes de subir; atacante que pegue só o bucket não abre o .pfx.
--
-- Assinar = FE envia (senha da sessão, digest_sha256); BE baixa o blob do bucket,
-- decifra com master key, decifra o PKCS#12 com a senha, assina em memória,
-- descarta a chave. A senha nunca toca o disco.
--
-- Cardinalidade: um usuário pode ter VÁRIOS certificados ativos (e-CPF + e-CNPJ,
-- ou renovação sobrepondo o antigo por um período). UNIQUE (tenant, fingerprint)
-- WHERE revoked_at IS NULL impede re-upload do mesmo cert; permite re-cadastro
-- após revogação (rotação com mesma OAB).
CREATE TABLE certificate (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  owner_user_id  uuid NOT NULL REFERENCES app_user(id),

  -- metadata parseada do PKCS#12 no upload (não sensível — é public info do cert)
  subject_cn     text NOT NULL,           -- Common Name do titular ("LUAN GOMES")
  oab            text,                    -- "347019/SP" (quando o cert traz OAB no OtherName)
  issuer         text NOT NULL,           -- Autoridade Certificadora emissora
  serial         text NOT NULL,           -- número de série do X.509
  not_before     timestamptz NOT NULL,    -- início de validade
  not_after      timestamptz NOT NULL,    -- fim de validade
  fingerprint    text NOT NULL,           -- SHA-256 do DER do cert, hex sem separador

  -- ponteiro pro blob .pfx cifrado no object storage (S3/R2/MinIO). O key é
  -- tenant-scoped ({tenant}/certificates/{uuid}), então isolamento tb no storage.
  storage_key    text NOT NULL,

  created_at     timestamptz NOT NULL DEFAULT now(),
  revoked_at     timestamptz              -- soft delete; NULL = ativo
);
-- dedup: mesmo cert (fingerprint = mesmo par pubkey+serial+issuer) não pode
-- estar duplicado ATIVO no tenant. Após revogar, pode ser re-adicionado.
CREATE UNIQUE INDEX certificate_active_fingerprint_idx
  ON certificate (tenant_id, fingerprint) WHERE revoked_at IS NULL;
CREATE INDEX ON certificate (tenant_id, owner_user_id) WHERE revoked_at IS NULL;

-- RLS: mesma barreira dupla (app filter por tenant_id + policy).
ALTER TABLE certificate ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON certificate
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
