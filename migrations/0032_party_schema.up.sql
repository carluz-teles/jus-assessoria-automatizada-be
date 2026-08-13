-- 0032_party_schema — materialize the PARTES do processo (Autor/Réu + advogados)
-- so the cockpit can render the AUTOR/RÉU cards. Reference: docs/erd-modelo-de-dados.md
-- (§ court_case / party). v0 is DERIVED from the DJEN `destinatarios` array (nome +
-- polo A/P); manual entry (the `document` column, CPF/CNPJ) is a later slice.
--
-- A party belongs to a CASE (court_case), not to a single intimation/court_record:
-- the process's parties are stable across graus and across N intimações, so the
-- natural key is (tenant, case, role, name) — the DJEN gives us no party id, so we
-- dedup by name. DATAJUD never carries parties (LGPD), so this is the only source.

-- ── 1. party — one party of a case (Autor / Réu / Terceiro) ─────────────────
-- tenant_id: the CLAUDE.md inegociável — every user table carries tenant_id and is
-- isolated by 2 barriers (app filter + RLS). role is the CHECK-on-app enum polo→role
-- (A→PLAINTIFF, P→DEFENDANT, outro→THIRD_PARTY). document (CPF/CNPJ) is NULL in v0 —
-- the DJEN never discloses it and we NEVER invent one; it is the manual-entry seam.
-- source records where the row came from ('DJEN' in v0). The UNIQUE (tenant, case,
-- role, name) allows more than one autor/réu (a litisconsórcio) while deduping the
-- SAME name under the SAME role on re-observation.
CREATE TABLE party (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenant(id),
  case_id     uuid NOT NULL REFERENCES court_case(id),
  role        text NOT NULL,                         -- PLAINTIFF | DEFENDANT | THIRD_PARTY (validated on app)
  name        text NOT NULL,
  document    text,                                  -- CPF/CNPJ — NULL in v0 (manual/future), never invented
  source      text NOT NULL DEFAULT 'DJEN',          -- DJEN | MANUAL (where the party came from)
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, case_id, role, name)
);
CREATE INDEX ON party (tenant_id, case_id);

-- party is tenant-scoped → the same tenant_isolation policy as every user table.
ALTER TABLE party ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON party
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── 2. party_counsel — the advogados representing a party ────────────────────
-- The DJEN `destinatarioadvogados` carry {nome, numero_oab, uf_oab}; they attach to
-- a party (see the ingestion note in sync.go — the DJEN does not link advogado→parte,
-- so we associate a communication's advogados to the polo of that communication's
-- destinatários, defaulting to the adverse/DEFENDANT when the polo is not inferible).
-- ON DELETE CASCADE: a counsel has no meaning without its party. UNIQUE (tenant,
-- party, oab, uf) dedups the same advogado on re-observation.
CREATE TABLE party_counsel (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenant(id),
  party_id    uuid NOT NULL REFERENCES party(id) ON DELETE CASCADE,
  name        text NOT NULL,
  oab         text NOT NULL,
  uf          text NOT NULL,
  source      text NOT NULL DEFAULT 'DJEN',
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, party_id, oab, uf)
);
CREATE INDEX ON party_counsel (tenant_id, party_id);

-- party_counsel is tenant-scoped → same tenant_isolation policy.
ALTER TABLE party_counsel ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON party_counsel
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
