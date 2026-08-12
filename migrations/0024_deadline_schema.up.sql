-- 0024_deadline_schema — the prazos (deadline) subsystem takes its schema shape.
-- Reference: docs/erd-modelo-de-dados.md (§ deadline, the catalog) and
-- docs/erd-prazos.md §4 (deltas de deadline + task DDL) and §8 (rules layer).
-- SCHEMA ONLY — zero slice logic (that is the 2c slice, internal/deadline).
--
-- Three moves:
--   1. deadline grows the audit/product deltas + a closed status set born PENDING;
--   2. deadline_rule — the versioned, seeded conservative rules layer (§8);
--   3. task — the actionable N-per-prazo work item (§4).
-- deadline has 0 rows today (the slice is future), so the NOT NULL adds are safe.

-- ── 1. deadline deltas ──────────────────────────────────────────────────────
-- tenant_id: the CLAUDE.md inegociável — every user table carries tenant_id and
-- is isolated by 2 barriers (app filter + RLS). deadline inherited its scope via
-- court_record/intimation until now; making it first-class lets the agenda
-- (/prazos) filter and RLS-isolate directly. kind/source/confirmed_*/doubled_reason
-- are the audit deltas (erd-prazos.md §4); rules_version pins which rule set derived
-- the days (§8), so "por que 15 dias?" is answerable from the row.
ALTER TABLE deadline
    ADD COLUMN tenant_id      uuid NOT NULL REFERENCES tenant(id),
    ADD COLUMN kind           text,                              -- CONTESTACAO|RECURSO|MANIFESTACAO|GENERICO|...
    ADD COLUMN source         text NOT NULL DEFAULT 'RULE',      -- RULE|AI|MANUAL (where the days came from)
    ADD COLUMN confirmed_by   uuid,                              -- who approved in F2 (nullable: rule-born, unconfirmed)
    ADD COLUMN confirmed_at   timestamptz,
    ADD COLUMN doubled_reason text,                              -- LITISCONSORCIO_229|FAZENDA_183|MP_180|DEFENSORIA_186
    ADD COLUMN rules_version  text NOT NULL DEFAULT 'v0';        -- which deadline_rule set derived this

-- status becomes a closed set. A rule-derived prazo is BORN 'PENDING' (a suggestion);
-- it only becomes 'OPEN' on the human F2 confirmation (2c, future). CANCELLED is the
-- revocation when the intimação is retificada. The CHECK is belt-and-suspenders on
-- top of the app validation for this safety-critical (deadline) data.
ALTER TABLE deadline
    ALTER COLUMN status SET DEFAULT 'PENDING',
    ADD CONSTRAINT deadline_status_check
        CHECK (status IN ('PENDING', 'OPEN', 'MET', 'MISSED', 'CANCELLED'));
-- The partial index `WHERE status = 'OPEN'` (0001) stays as-is: it powers the
-- expiry sweep over confirmed, live prazos — exactly the rows the sweep must scan.

-- deadline is now tenant-scoped → same tenant_isolation policy as every user table.
ALTER TABLE deadline ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON deadline
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── 2. deadline_rule — the versioned rules layer (§8) ───────────────────────
-- Global reference data (no tenant_id, like holiday) — a per-tenant override is a
-- later concern. Maps cheap signals (intimation.type + optional court_prefix) to a
-- safe {kind, days, counting}. The 2c resolver picks the most specific / highest
-- priority active match; this migration only seeds the safe v0 rules — NO resolution
-- logic here.
CREATE TABLE deadline_rule (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  rules_version   text NOT NULL,
  intimation_type text NOT NULL,                     -- CITACAO|INTIMACAO|COMUNICACAO|* ('*' = catch-all)
  court_prefix    text,                              -- e.g. 'TRT' for rito-specific; NULL = any court
  kind            text NOT NULL,                     -- the legible prazo kind the rule implies
  days            int  NOT NULL CHECK (days > 0),
  counting        text NOT NULL CHECK (counting IN ('BUSINESS', 'CALENDAR')),
  doubled         boolean NOT NULL DEFAULT false,
  priority        int  NOT NULL DEFAULT 0,           -- most specific / highest wins (2c resolves)
  active          boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now(),
  -- One rule per (version, type, court_prefix). NULLS NOT DISTINCT so the catch-all
  -- rows (court_prefix IS NULL) cannot silently duplicate — Postgres would otherwise
  -- treat every NULL as distinct and let two 'any-court' rules for the same type in.
  UNIQUE NULLS NOT DISTINCT (rules_version, intimation_type, court_prefix)
);

-- SEED — the safe, conservative v0 rules (erd-prazos.md §8). counting=BUSINESS is the
-- safe default (CPC art. 219, dias úteis); 2c may override to CALENDAR from the rito.
-- Bias: when no specific rule matches, the '*' catch-all gives a short GENERICO prazo
-- and the UI will flag "confirme" (a future slice) — never invent a precise date.
INSERT INTO deadline_rule (rules_version, intimation_type, court_prefix, kind, days, counting) VALUES
  ('v0', 'CITACAO',    NULL, 'CONTESTACAO',  15, 'BUSINESS'),
  ('v0', 'INTIMACAO',  NULL, 'MANIFESTACAO',  5, 'BUSINESS'),
  ('v0', 'COMUNICACAO', NULL, 'GENERICO',      5, 'BUSINESS'),
  ('v0', '*',          NULL, 'GENERICO',      5, 'BUSINESS');

-- ── 3. task — the actionable work item (§4) ─────────────────────────────────
-- 1 legal prazo (deadline) → N tasks (the F2 "criar tarefa"). The assignee lives on
-- the task, not the deadline: the prazo is the fact, the task is who does it. All FKs
-- except tenant_id are nullable (a task can be avulsa / manual). NO rows written here
-- (that is the F2 confirmation, a future slice) — schema only.
CREATE TABLE task (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenant(id),
  court_record_id  uuid REFERENCES court_record(id),   -- context (nullable: avulsa)
  deadline_id      uuid REFERENCES deadline(id),        -- the legal prazo it stems from (nullable)
  intimation_id    uuid REFERENCES intimation(id),      -- origin (nullable: manual task)
  title            text NOT NULL,
  description      text,
  kind             text,                                -- suggested action (peça, juntada, ciência…)
  due_date         date,                                -- own date (≤ deadline.end_date, or manual)
  status           text NOT NULL DEFAULT 'OPEN'
                     CHECK (status IN ('OPEN', 'DONE', 'DISMISSED')),
  source           text NOT NULL,                       -- AI|RULE|MANUAL
  assignee_user_id uuid,                                -- responsável ("meus prazos")
  created_by       uuid,
  created_at       timestamptz NOT NULL DEFAULT now(),
  completed_at     timestamptz
);
CREATE INDEX ON task (tenant_id, status);
CREATE INDEX ON task (due_date) WHERE status = 'OPEN';   -- varredura / agenda

-- task is tenant-scoped → same tenant_isolation policy as every user table.
ALTER TABLE task ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON task
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
