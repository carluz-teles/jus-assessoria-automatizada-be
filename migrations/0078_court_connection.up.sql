-- 0078_court_connection — estado de sessão autenticada por advogado × tribunal ×
-- sistema (docs/erd-execucao-judicial-tjsp.md §5). `integration` continua sendo a
-- assinatura de fonte por tenant (DATAJUD/DJEN); esta tabela é o acesso autenticado
-- ao portal, com estado de sessão/MFA/certificado que `integration` não modela.
--
-- credential_ref/mfa_seed_ref apontam pra tenant_secret (0069) — a mesma migration
-- já antecipava "uma seed TOTP reusa essa tabela sem migration". certificate_ref
-- aponta pra certificate (0058, certificado A1 já cadastrado do advogado) — nunca
-- duplicamos o certificado aqui.
--
-- MFA_ENROLLMENT_REQUIRED é um status além do que o ERD original listou: distingue
-- "nunca configuramos TOTP pra esse advogado" (precisa correr o enrollment
-- automatizado uma vez) de MFA_REQUIRED (já tem seed; gerar o código deveria ser
-- transparente e nunca parar nesse status em operação normal — ele existe pra
-- classificar uma falha pontual do provider, não o caminho feliz).

CREATE TABLE court_connection (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id              uuid NOT NULL REFERENCES tenant(id),
  app_user_id            uuid NOT NULL REFERENCES app_user(id),
  court                  text NOT NULL,
  system                 text NOT NULL,
  authentication_method  text NOT NULL CHECK (authentication_method IN ('PASSWORD', 'CERTIFICATE_A1')),
  credential_ref         uuid REFERENCES tenant_secret(id),
  certificate_ref        uuid REFERENCES certificate(id),
  mfa_seed_ref           uuid REFERENCES tenant_secret(id),
  session_ref            uuid REFERENCES tenant_secret(id),
  status                 text NOT NULL DEFAULT 'DISCONNECTED'
                         CHECK (status IN (
                           'DISCONNECTED', 'AUTHENTICATING', 'CONNECTED',
                           'MFA_ENROLLMENT_REQUIRED', 'MFA_REQUIRED',
                           'CERTIFICATE_REQUIRED', 'REAUTH_REQUIRED', 'ERROR'
                         )),
  last_authenticated_at  timestamptz,
  error                  jsonb,
  created_at             timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, app_user_id, court, system)
);
CREATE INDEX ON court_connection (tenant_id);

ALTER TABLE court_connection ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON court_connection
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
