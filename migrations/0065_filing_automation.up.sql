-- Fatia 1 (peticionamento automático TJSP e-SAJ): tabelas de apoio ao
-- protocolo automatizado (RPA via chromedp) de peças já assinadas.
--
-- filing_attempt é append-only e vive FORA de draft/petition: cada clique de
-- "Protocolar" gera uma tentativa. O unique parcial em (draft_id) garante que
-- nunca haja duas tentativas ativas (ENFILEIRADO/PROTOCOLANDO) para o mesmo
-- draft — é a barreira de idempotência contra duplo-clique/retry (critério de
-- aceite 3). O snapshot do PDF (pdf_storage_key + pdf_sha256) congela o bytes
-- protocolados no momento do clique, então edição pós-aprovação não altera o
-- PDF que vai pro tribunal (critério 7).
--
-- esaj_credential guarda login + senha do advogado no e-SAJ, cifrados com o
-- MESMO envelope KMS já usado em certificate (SecretVault). A senha NUNCA
-- persiste em claro; só o envelope (ciphertext+nonce+wrapped_dek+kek_ref) vai
-- pra linha. O consentimento dos termos é registrado (LGPD/auditoria).

CREATE TABLE filing_attempt (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL,
  draft_id          uuid NOT NULL REFERENCES draft(id),
  petition_id       uuid REFERENCES petition(id),
  status            text NOT NULL CHECK (status IN ('ENFILEIRADO','PROTOCOLANDO','PROTOCOLADO','FALHOU')),
  pdf_storage_key   text NOT NULL,
  pdf_sha256        text NOT NULL,
  requested_by      uuid NOT NULL REFERENCES app_user(id),
  requested_at      timestamptz NOT NULL DEFAULT now(),
  started_at        timestamptz,
  finished_at       timestamptz,
  failure_reason    text,
  filing_number     text,
  screenshot_keys   text[] NOT NULL DEFAULT '{}'
);

-- Tenant isolation: a tentativa carrega tenant_id explicitamente e a RLS filtra
-- por app.tenant_id (setado pelo UoW em toda tx). app.system='on' é o escape hatch
-- (populate/national reads). Falha-fechado quando nenhum dos dois está setado.
ALTER TABLE filing_attempt ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON filing_attempt
  USING (
    tenant_id = current_setting('app.tenant_id', true)::uuid
    OR current_setting('app.system', true) = 'on'
  );

CREATE UNIQUE INDEX ON filing_attempt (draft_id) WHERE status IN ('ENFILEIRADO','PROTOCOLANDO');
CREATE INDEX ON filing_attempt (draft_id);

CREATE TABLE esaj_credential (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL,
  owner_user_id     uuid NOT NULL REFERENCES app_user(id),
  login             text NOT NULL,
  ciphertext        bytea NOT NULL,
  nonce             bytea NOT NULL,
  wrapped_dek       bytea NOT NULL,
  kek_ref           text NOT NULL,
  terms_version     text NOT NULL,
  terms_accepted_at timestamptz NOT NULL,
  terms_accepted_by uuid NOT NULL REFERENCES app_user(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  revoked_at        timestamptz
);

-- Mesma isolação de tenant da filing_attempt (espelha watched_oab / draft).
ALTER TABLE esaj_credential ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON esaj_credential
  USING (
    tenant_id = current_setting('app.tenant_id', true)::uuid
    OR current_setting('app.system', true) = 'on'
  );

CREATE UNIQUE INDEX ON esaj_credential (tenant_id, owner_user_id) WHERE revoked_at IS NULL;
