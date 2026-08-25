-- 0069_tenant_secret — generic envelope-encryption vault for per-tenant secrets
-- (lib/vault, docs/erd-execucao-judicial-tjsp.md §9). Each row holds a single
-- secret's AES-256-GCM ciphertext and its wrapped DEK; the KEK lives only in
-- process memory (VAULT_KEK_BASE64 env var), never in the database.
--
-- This table carries no notion of "what" the secret is — that belongs to the
-- owning table (e.g. court_connection.credential_ref points at a row here).
-- A future secret type (a second portal, an API key, a TOTP seed) reuses this
-- table without a migration.
--
-- RLS: the exact tenant_isolation policy every per-tenant table uses
-- (0001/0008/…) — SET LOCAL app.tenant_id per tx is barrier 2, the app's own
-- tenant_id filter (WHERE / repo signature) is barrier 1.

CREATE TABLE tenant_secret (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenant(id),
  -- ciphertext/nonce: the secret plaintext, AES-256-GCM sealed under a fresh
  -- random DEK generated for THIS secret (crypto/rand — never math/rand).
  ciphertext   bytea NOT NULL,
  nonce        bytea NOT NULL,
  -- wrapped_dek/dek_nonce: that DEK, itself AES-256-GCM sealed under the
  -- application's KEK (VAULT_KEK_BASE64) — the KEK never touches the database,
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
