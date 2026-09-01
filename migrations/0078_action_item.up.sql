-- 0078: action_item — the Providência (docs/erd-costura-providencia-tarefa-peca.md §2).
-- Bridges notification (intimação, 1:N) → action_item → task (1:1, created by a later
-- fatia's deadline listener) → draft (herda o piece_profile da providência). Tenant-scoped
-- (RLS, CLAUDE.md inegociável) via tenant_id.
--
-- Column choices, matched to the repo's existing conventions:
--   * intimation_id references intimation(id) — the table the app calls "intimação"
--     (renamed from notification in migration 0006); NEVER notification_id (there is a
--     real, distinct notification table for the avisos domain, internal/notifications).
--   * court_record_id mirrors task.court_record_id (0024): kept as its own nullable FK
--     rather than derived-only-via-join, so a read never has to hop through intimation to
--     scope by processo — same tradeoff task already made for the same reason.
--   * piece_profile_key/deadline_id/task_id are nullable: gera_peca=false carries no
--     piece_profile_key (ciência de despacho never has a peça); deadline_id is set once the
--     providência's prazo is known; task_id is filled by a FUTURE fatia's deadline listener
--     (schema-ready, not populated here).
CREATE TABLE action_item (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES tenant(id),
  intimation_id      uuid NOT NULL REFERENCES intimation(id),
  court_record_id    uuid REFERENCES court_record(id),
  tipo               text NOT NULL,   -- contestar | recorrer | manifestar | cumprir | ciencia ...
  gera_peca          boolean NOT NULL DEFAULT false,
  piece_profile_key  text REFERENCES piece_profile(key),
  tipo_origem        text NOT NULL CHECK (tipo_origem IN ('declarado', 'ia', 'manual')),
  tipo_status        text NOT NULL CHECK (tipo_status IN ('confiavel', 'a_confirmar')),
  deadline_id        uuid REFERENCES deadline(id),
  confianca          double precision,
  status             text NOT NULL DEFAULT 'SUGGESTED' CHECK (status IN ('SUGGESTED', 'CONFIRMED', 'DISCARDED')),
  task_id            uuid UNIQUE REFERENCES task(id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  -- gera_peca and piece_profile_key travel together: a peça-generating providência always
  -- names its tipo de peça, a ciência-only one never does.
  CHECK ( (gera_peca AND piece_profile_key IS NOT NULL) OR (NOT gera_peca AND piece_profile_key IS NULL) ),
  -- confianca is only meaningful for an IA-derived classification (docs §3): a
  -- declarado/manual item carries no confidence score.
  CHECK ( tipo_origem = 'ia' OR confianca IS NULL )
);

-- FK columns need their own index (Postgres does not auto-index them, unlike the PK).
CREATE INDEX ON action_item (tenant_id, intimation_id);
CREATE INDEX ON action_item (court_record_id);
CREATE INDEX ON action_item (deadline_id);
CREATE INDEX ON action_item (piece_profile_key);
-- task_id already gets a unique index from the UNIQUE constraint above.

ALTER TABLE action_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON action_item
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
