-- 0042_portal_credential — the TJSP eproc login the lawyer configures so the
-- tribunal-scraping connector (docs/erd-tribunal-scraping.md §4/§6/§7) can read
-- the portal on their behalf. Two tables, split by responsibility:
--
--  * tenant_secret is the generic envelope-encryption vault (lib/vault, §6 Option
--    A): one row per encrypted secret, DEK-per-secret, the DEK itself wrapped by a
--    KEK the application holds only in memory (TENANT_SECRET_KEK env var, never in
--    the database). It carries no notion of "what" the secret is — that belongs to
--    the owning table (portal_credential.credential_ref points at a row here). A
--    future secret (a second portal, an API key) reuses this table without a
--    migration.
--  * portal_credential is the login itself: `login` is NOT secret (kept in the
--    clear for display/debugging), `credential_ref` POINTS at the tenant_secret row
--    holding the encrypted password — the password itself never touches this
--    table. `status` tracks the outcome of the last synchronous login test
--    (internal/portalcredential's PortalLoginTester), so the UI can show the lawyer
--    whether the credential actually works without them re-entering it.
--
-- RLS on both: the exact tenant_isolation molde every per-tenant table uses
-- (0001/0008/0037, …) — `SET LOCAL app.tenant_id` per tx is barrier 2, the app's
-- own tenant_id filter (WHERE / repo signature) is barrier 1.

CREATE TABLE tenant_secret (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenant(id),
  -- ciphertext/nonce: the secret plaintext, AES-256-GCM sealed under a fresh
  -- random DEK generated for THIS secret (crypto/rand — never math/rand).
  ciphertext   bytea NOT NULL,
  nonce        bytea NOT NULL,
  -- wrapped_dek/dek_nonce: that DEK, itself AES-256-GCM sealed under the
  -- application's KEK (TENANT_SECRET_KEK) — the KEK never touches the database,
  -- only the wrapped (encrypted) DEK does. GCM's auth tag makes a tampered
  -- ciphertext OR a wrong KEK fail decryption loudly (lib/vault tests this).
  wrapped_dek  bytea NOT NULL,
  dek_nonce    bytea NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON tenant_secret (tenant_id);

ALTER TABLE tenant_secret ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_secret
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE portal_credential (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenant(id),
  app_user_id       uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  portal            text NOT NULL,          -- 'TJSP_EPROC' only, v0
  login             text NOT NULL,          -- NOT secret
  credential_ref    uuid NOT NULL REFERENCES tenant_secret(id),
  status            text NOT NULL DEFAULT 'ACTIVE'
                       CHECK (status IN ('ACTIVE', 'AUTH_FAILED', 'CAPTCHA_BLOCKED', 'DISABLED')),
  last_error        text,
  last_verified_at  timestamptz,
  configured_by     uuid REFERENCES app_user(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, app_user_id, portal)
);
CREATE INDEX ON portal_credential (tenant_id);
CREATE INDEX ON portal_credential (app_user_id);
-- credential_ref FK column — PostgreSQL never auto-indexes FK columns.
CREATE INDEX ON portal_credential (credential_ref);

ALTER TABLE portal_credential ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON portal_credential
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
