-- 0083_v1_deadline_motor — Motor de Prazos V1: audit trail, cross-validation,
-- deadline events, per-tenant policy, and V1 columns on deadline.
-- Reference: docs/erd-motor-de-prazos-v1.md
--
-- Moves:
--   1. calc_memory — deterministic calculation audit trail
--   2. applied_holiday — snapshot of holidays applied to a calculation
--   3. cross_validation — declared vs calculated validation
--   4. deadline_event — append-only audit trail
--   5. deadline_policy — per-tenant confirmation policy
--   6. deadline V1 columns (origem, selo, confirmacao_exigida, providencia, etc.)

-- ── 1. calc_memory ───────────────────────────────────────────────────────────
CREATE TABLE calc_memory (
  id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id                uuid NOT NULL REFERENCES tenant(id),
  deadline_id              uuid NOT NULL UNIQUE REFERENCES deadline(id),
  prazo_base               text,
  prazo_base_fonte         text,
  termo_inicial_regra      text,
  dias_uteis               boolean DEFAULT true,
  dobra_motivo             text,
  tabela_legal_ref         text,
  ia_tipo_inferido         text,
  ia_confianca             double precision,
  calendar_provider_version text,
  created_at               timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON calc_memory (tenant_id, deadline_id);

ALTER TABLE calc_memory ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON calc_memory
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── 2. applied_holiday ───────────────────────────────────────────────────────
CREATE TABLE applied_holiday (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenant(id),
  calc_memory_id  uuid NOT NULL REFERENCES calc_memory(id) ON DELETE CASCADE,
  data            date NOT NULL,
  nome            text,
  ambito          text,
  comarca         text,
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON applied_holiday (tenant_id, calc_memory_id);

ALTER TABLE applied_holiday ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON applied_holiday
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── 3. cross_validation ──────────────────────────────────────────────────────
CREATE TABLE cross_validation (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenant(id),
  deadline_id      uuid NOT NULL UNIQUE REFERENCES deadline(id),
  data_declarada   date,
  data_calculada   date,
  dif_dias         integer,
  resultado        text,
  causa_provavel   text,
  decisao          text,
  decidido_por     uuid REFERENCES app_user(id),
  created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON cross_validation (tenant_id, deadline_id);

ALTER TABLE cross_validation ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cross_validation
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── 4. deadline_event ────────────────────────────────────────────────────────
CREATE TABLE deadline_event (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenant(id),
  deadline_id uuid NOT NULL REFERENCES deadline(id),
  tipo        text NOT NULL,
  detalhe     text,
  ator_id     uuid REFERENCES app_user(id),
  em          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON deadline_event (tenant_id, deadline_id, em DESC);

ALTER TABLE deadline_event ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON deadline_event
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── 5. deadline_policy ───────────────────────────────────────────────────────
CREATE TABLE deadline_policy (
  tenant_id               uuid PRIMARY KEY REFERENCES tenant(id),
  confirmacao_obrigatoria boolean NOT NULL DEFAULT false,
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE deadline_policy ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON deadline_policy
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── 6. deadline V1 columns ──────────────────────────────────────────────────
ALTER TABLE deadline
  ADD COLUMN origem               text DEFAULT 'calculado',
  ADD COLUMN selo                 text DEFAULT 'a_apurar',
  ADD COLUMN confirmacao_exigida  boolean DEFAULT false,
  ADD COLUMN providencia          text,
  ADD COLUMN confirmado_por       uuid REFERENCES app_user(id),
  ADD COLUMN confirmado_em        timestamptz;
