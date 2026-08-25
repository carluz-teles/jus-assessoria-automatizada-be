-- 0060_certificate.up.sql — a tenant's A1 digital certificate (.pfx/.p12),
-- stored with envelope encryption. Faithful to the slice's security contract:
-- the private key is NEVER at rest in plaintext.
--
-- ciphertext = AES-256-GCM of the raw .pfx bytes under a per-record data key (DEK);
-- nonce      = the GCM nonce for that ciphertext;
-- wrapped_dek= the DEK wrapped by the vault (KMS CMK in prod, local KEK in dev);
-- kek_ref    = opaque label of the wrapping key (for rotation) — NOT a secret.
-- The .pfx password is used only to parse at upload and is discarded — it is
-- deliberately absent from this table.
--
-- Enums/status follow the project convention (text; here a soft revoke via a
-- nullable revoked_at instead of a status column — a certificate is either active
-- or revoked, nothing more). One tenant may hold several certificates (multiple
-- lawyers), so no UNIQUE on tenant_id.

CREATE TABLE certificate (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenant(id),
  owner_user_id uuid NOT NULL REFERENCES app_user(id),

  -- Metadata parsed from the x509 certificate — safe to display.
  subject_cn   text NOT NULL,
  oab          text,                    -- NULL when the cert carries no OAB
  issuer       text NOT NULL,
  serial       text NOT NULL,
  not_before   timestamptz NOT NULL,
  not_after    timestamptz NOT NULL,
  fingerprint  text NOT NULL,           -- hex SHA-256 of the DER cert

  -- Encrypted key material (the envelope). bytea; never leaves the backend.
  ciphertext   bytea NOT NULL,
  nonce        bytea NOT NULL,
  wrapped_dek  bytea NOT NULL,
  kek_ref      text  NOT NULL,

  created_at   timestamptz NOT NULL DEFAULT now(),
  revoked_at   timestamptz              -- soft revoke; NULL = active
);

-- Access paths: list a tenant's certificates (barrier-1 filter) and, within that,
-- a lawyer's own. FK columns are not auto-indexed in Postgres, so index explicitly.
CREATE INDEX ON certificate (tenant_id);
CREATE INDEX ON certificate (owner_user_id);

-- Row-Level Security — barrier 2 of tenant isolation (docs/erd-backend.md §4d.4),
-- same molde as the tables in 0001/0007.
ALTER TABLE certificate ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON certificate
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
