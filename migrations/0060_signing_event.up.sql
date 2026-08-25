-- 0060_signing_event.up.sql — audit trail for server-side signatures produced by
-- POST /v1/certificates/:id/sign. Each row records that a certificate was used to
-- sign a specific digest, WITHOUT ever storing the signature, the private key, or
-- the .pfx password (all of which live in memory only for the duration of the
-- request and are zeroed after use). The digest is the SHA-256 that was signed —
-- it is not secret (it is a hash of the caller's document/peça) and it lets an
-- auditor prove exactly what was signed, when, and by whom.
--
-- Faithful to the slice's security contract: no key material at rest, no password
-- ever persisted. This table is metadata only.

CREATE TABLE signing_event (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  certificate_id uuid NOT NULL REFERENCES certificate(id),
  signer_user_id uuid NOT NULL REFERENCES app_user(id),

  -- The SHA-256 digest that was signed (32 bytes). NOT the signature and NOT the
  -- document — only the hash, so the act is auditable without exposing content.
  digest_sha256  bytea NOT NULL,

  signed_at      timestamptz NOT NULL DEFAULT now()
);

-- Access paths: audit a tenant's signatures, and trace all uses of one certificate.
CREATE INDEX ON signing_event (tenant_id);
CREATE INDEX ON signing_event (certificate_id);

-- Row-Level Security — barrier 2 of tenant isolation (docs/erd-backend.md §4d.4),
-- same molde as certificate (0060) and the tables in 0001/0007.
ALTER TABLE signing_event ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON signing_event
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
