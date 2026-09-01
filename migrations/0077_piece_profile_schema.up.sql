-- 0077: Tipos de Peça — catálogo de perfis de geração + regras de conformidade + modelo de teses.
-- Reference tables (global, no tenant_id): base_skeleton, matter, format_profile, compliance_rule.
-- Catalog tables (also global in v1 — docs/erd-tipos-de-peca.md §7.1, "cadastro de dados", no
-- tenant_id): piece_profile, profile_section, profile_requirement, profile_rule, section_rule,
-- piece_profile_version.
-- Thesis model (tenant-scoped via draft): thesis, thesis_anchor, draft_segment, segment_anchor,
-- thesis_coverage.

-- ── Reference: base_skeleton ────────────────────────────────────────────────
CREATE TABLE base_skeleton (
  key  text PRIMARY KEY,        -- "default"
  slots jsonb NOT NULL           -- endereçamento, preâmbulo, ⟦miolo⟧, pedidos, fecho
);

-- ── Reference: matter ───────────────────────────────────────────────────────
CREATE TABLE matter (
  key  text PRIMARY KEY,        -- "civel" | "trabalhista" | "penal"
  nome text NOT NULL
);

-- ── Reference: format_profile ───────────────────────────────────────────────
CREATE TABLE format_profile (
  key                     text PRIMARY KEY,   -- "default" | "tribunal_x"
  fonte                   text NOT NULL,       -- "Times New Roman" | "Arial"
  tamanho_corpo           int NOT NULL,
  tamanho_citacao_longa   int NOT NULL,
  espacamento             text NOT NULL DEFAULT '1.5',
  alinhamento             text NOT NULL DEFAULT 'justificado',
  margens                 jsonb NOT NULL DEFAULT '{}',
  citacao_longa           jsonb NOT NULL DEFAULT '{}',
  export                  text NOT NULL DEFAULT 'PDF/A, pesquisável'
);

-- ── Reference: compliance_rule ──────────────────────────────────────────────
CREATE TABLE compliance_rule (
  key          text PRIMARY KEY,   -- "impugnacao_especifica" | "vedacao_inovacao" | ...
  descricao    text NOT NULL,
  severidade   text NOT NULL CHECK (severidade IN ('bloqueante', 'aviso', 'feedback')),
  fonte_legal  text,
  verificacao  text NOT NULL CHECK (verificacao IN ('por_ia_ancorada', 'deterministica', 'feedback_usuario'))
);

-- ── Catalog: piece_profile ───────────────────────────────────────────────────
-- version_atual is a free-form label (v1, v1.1, 2025-09-01, ...), not a counter — a new
-- piece_profile_version always carries an EXPLICIT version supplied by the caller (never
-- derived by incrementing this column). No tenant_id: v1 catalog is global (ERD §7.1).
CREATE TABLE piece_profile (
  key                  text PRIMARY KEY,   -- "contestacao" | "peticao_inicial" | "apelacao" ...
  nome                 text NOT NULL,
  polo                 text NOT NULL CHECK (polo IN ('ativo', 'passivo', 'ambos')),
  matter_key           text NOT NULL REFERENCES matter(key),
  base_skeleton_key    text NOT NULL REFERENCES base_skeleton(key),
  format_profile_key   text REFERENCES format_profile(key),
  version_atual        text NOT NULL DEFAULT 'v1',
  fonte_legal          jsonb NOT NULL DEFAULT '[]',
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now()
);

-- ── Catalog (via FK): profile_section ────────────────────────────────────────
CREATE TABLE profile_section (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  piece_profile_key  text NOT NULL REFERENCES piece_profile(key) ON DELETE CASCADE,
  key                text NOT NULL,   -- "preliminares" | "merito" | ...
  titulo             text NOT NULL,
  ordem              int NOT NULL,
  obrigatoria        text NOT NULL CHECK (obrigatoria IN ('sim', 'nao', 'condicional')),
  origem             text NOT NULL CHECK (origem IN ('moldura', 'argumentativa')),
  aceita_teses       boolean NOT NULL DEFAULT false,
  fonte_legal        text,
  UNIQUE (piece_profile_key, key)
);

-- ── Catalog (via FK): profile_requirement ────────────────────────────────────
CREATE TABLE profile_requirement (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  piece_profile_key  text NOT NULL REFERENCES piece_profile(key) ON DELETE CASCADE,
  campo              text NOT NULL,
  obrigatorio        boolean NOT NULL DEFAULT true,
  fonte_legal        text
);

-- ── Catalog (via FK): profile_rule ────────────────────────────────────────────
CREATE TABLE profile_rule (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  piece_profile_key     text NOT NULL REFERENCES piece_profile(key) ON DELETE CASCADE,
  compliance_rule_key   text NOT NULL REFERENCES compliance_rule(key),
  override_severidade   text CHECK (override_severidade IN ('bloqueante', 'aviso', 'feedback')),
  UNIQUE (piece_profile_key, compliance_rule_key)
);

-- ── Catalog (via FK): section_rule ────────────────────────────────────────────
CREATE TABLE section_rule (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  profile_section_id    uuid NOT NULL REFERENCES profile_section(id) ON DELETE CASCADE,
  compliance_rule_key   text NOT NULL REFERENCES compliance_rule(key),
  UNIQUE (profile_section_id, compliance_rule_key)
);

-- ── Catalog (via FK): piece_profile_version ──────────────────────────────────
CREATE TABLE piece_profile_version (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  piece_profile_key  text NOT NULL REFERENCES piece_profile(key) ON DELETE CASCADE,
  version            text NOT NULL,
  vigente_desde      timestamptz NOT NULL DEFAULT now(),
  snapshot           jsonb NOT NULL,   -- seções + regras na época
  UNIQUE (piece_profile_key, version)
);

-- ── Thesis model ────────────────────────────────────────────────────────────

CREATE TABLE thesis (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES tenant(id),
  draft_id           uuid NOT NULL REFERENCES draft(id),
  piece_profile_key  text REFERENCES piece_profile(key),
  notification_id    uuid REFERENCES intimation(id),
  enunciado          text NOT NULL,
  forca              text NOT NULL CHECK (forca IN ('favoravel', 'contraria_relevante')),
  estado             text NOT NULL DEFAULT 'proposta' CHECK (estado IN ('proposta', 'aprovada', 'descartada')),
  created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON thesis (tenant_id, draft_id);

-- thesis is tenant-scoped → same tenant_isolation policy as every user table (CLAUDE.md
-- inegociável: app filter + RLS, barrier 2 on top of the app's explicit tenant_id filter).
ALTER TABLE thesis ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON thesis
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE thesis_anchor (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  thesis_id        uuid NOT NULL REFERENCES thesis(id) ON DELETE CASCADE,
  tipo             text NOT NULL CHECK (tipo IN ('fato', 'direito')),
  alvo_documento   uuid REFERENCES document(id),
  alvo_fonte       text,
  motivo           text NOT NULL,
  status           text NOT NULL DEFAULT 'a_confirmar' CHECK (status IN ('a_confirmar', 'validada'))
);

CREATE TABLE draft_segment (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenant(id),
  draft_id            uuid NOT NULL REFERENCES draft(id),
  thesis_id           uuid NOT NULL REFERENCES thesis(id),
  profile_section_id  uuid REFERENCES profile_section(id),
  conteudo            text NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON draft_segment (tenant_id, draft_id);
CREATE INDEX ON draft_segment (thesis_id);

-- draft_segment is tenant-scoped → same tenant_isolation policy (CLAUDE.md inegociável).
ALTER TABLE draft_segment ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON draft_segment
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE segment_anchor (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  draft_segment_id    uuid NOT NULL REFERENCES draft_segment(id) ON DELETE CASCADE,
  thesis_anchor_id    uuid NOT NULL REFERENCES thesis_anchor(id),
  status              text NOT NULL DEFAULT 'a_confirmar' CHECK (status IN ('a_confirmar', 'validada'))
);

-- thesis_coverage is 1:0..1 with thesis (docs/erd-tipos-de-peca.md §1 erDiagram: "thesis ||--o|
-- thesis_coverage") — one coverage verdict per tese, recomputed (not accumulated) on re-check.
CREATE TABLE thesis_coverage (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  thesis_id   uuid NOT NULL UNIQUE REFERENCES thesis(id) ON DELETE CASCADE,
  resultado   text NOT NULL CHECK (resultado IN ('coberta', 'divergente', 'ausente')),
  detalhe     text,
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- ── Seed: perfis-semente (v1 do catálogo) — docs/erd-tipos-de-peca.md §6 ────────────────────

INSERT INTO matter (key, nome) VALUES
  ('civel', 'Cível');

INSERT INTO base_skeleton (key, slots) VALUES
  ('default', '["enderecamento", "preambulo", "miolo", "pedidos", "fecho"]');

INSERT INTO format_profile (key, fonte, tamanho_corpo, tamanho_citacao_longa, espacamento, alinhamento, margens, citacao_longa, export) VALUES
  ('default', 'Times New Roman', 12, 10, '1.5', 'justificado',
   '{"superior": 3, "inferior": 2, "esquerda": 3, "direita": 2}',
   '{"recuo": 4, "aspas": false}',
   'PDF/A, pesquisável');

-- compliance_rule catalog. fidelidade_sem_vies is the ERD §2 example of a `feedback`-severity
-- rule (checked by human review, not by the pipeline) — cataloged here per the seed instructions
-- but not wired to any profile_rule below: it is a general "sem viés" check, not specific to
-- peticao_inicial/contestacao/apelacao.
INSERT INTO compliance_rule (key, descricao, severidade, fonte_legal, verificacao) VALUES
  ('pedido_certo', 'O pedido deve ser certo e determinado', 'bloqueante', 'CPC art. 322', 'deterministica'),
  ('valor_causa', 'A petição deve indicar o valor da causa', 'bloqueante', 'CPC art. 292', 'deterministica'),
  ('impugnacao_especifica', 'Cada fato alegado pelo autor deve ser especificamente impugnado', 'bloqueante', 'CPC art. 341', 'por_ia_ancorada'),
  ('preliminares_antes_merito', 'As preliminares devem ser arguidas antes do mérito', 'bloqueante', 'CPC art. 337', 'deterministica'),
  ('eventualidade', 'As teses de mérito devem ser deduzidas em ordem eventual, sem se prejudicarem', 'aviso', 'CPC art. 336', 'por_ia_ancorada'),
  ('vedacao_inovacao', 'O recurso não pode inovar teses não suscitadas na instância anterior', 'bloqueante', 'CPC art. 1013, §1º', 'por_ia_ancorada'),
  ('dialeticidade', 'As razões devem impugnar especificamente os fundamentos da decisão recorrida', 'bloqueante', 'CPC art. 1010, II e III', 'por_ia_ancorada'),
  ('fidelidade_sem_vies', 'A peça deve refletir fielmente a posição da parte, sem viés indevido', 'feedback', NULL, 'feedback_usuario');

-- piece_profile: peticao_inicial (polo ativo) — fatos → direito → pedidos.
INSERT INTO piece_profile (key, nome, polo, matter_key, base_skeleton_key, format_profile_key, version_atual, fonte_legal) VALUES
  ('peticao_inicial', 'Petição Inicial', 'ativo', 'civel', 'default', 'default', 'v1', '["CPC art. 319", "CPC art. 322"]');

INSERT INTO profile_section (piece_profile_key, key, titulo, ordem, obrigatoria, origem, aceita_teses, fonte_legal) VALUES
  ('peticao_inicial', 'fatos', 'Dos Fatos', 1, 'sim', 'argumentativa', true, 'CPC art. 319, III'),
  ('peticao_inicial', 'direito', 'Do Direito', 2, 'sim', 'argumentativa', true, 'CPC art. 319, III'),
  ('peticao_inicial', 'pedidos', 'Dos Pedidos', 3, 'sim', 'argumentativa', false, 'CPC art. 322');

INSERT INTO profile_rule (piece_profile_key, compliance_rule_key) VALUES
  ('peticao_inicial', 'pedido_certo'),
  ('peticao_inicial', 'valor_causa');

-- piece_profile: contestacao (polo passivo) — preliminares → prejudiciais → impugnação
-- específica → mérito → pedidos → provas.
INSERT INTO piece_profile (key, nome, polo, matter_key, base_skeleton_key, format_profile_key, version_atual, fonte_legal) VALUES
  ('contestacao', 'Contestação', 'passivo', 'civel', 'default', 'default', 'v1', '["CPC art. 335", "CPC art. 336", "CPC art. 337", "CPC art. 341"]');

INSERT INTO profile_section (piece_profile_key, key, titulo, ordem, obrigatoria, origem, aceita_teses, fonte_legal) VALUES
  ('contestacao', 'preliminares', 'Das Preliminares', 1, 'condicional', 'argumentativa', true, 'CPC art. 337'),
  ('contestacao', 'prejudiciais', 'Das Prejudiciais de Mérito', 2, 'condicional', 'argumentativa', true, 'CPC art. 337, XVI'),
  ('contestacao', 'impugnacao_especifica', 'Da Impugnação Específica dos Fatos', 3, 'sim', 'argumentativa', true, 'CPC art. 341'),
  ('contestacao', 'merito', 'Do Mérito', 4, 'sim', 'argumentativa', true, 'CPC art. 336'),
  ('contestacao', 'pedidos', 'Dos Pedidos', 5, 'sim', 'argumentativa', false, 'CPC art. 322'),
  ('contestacao', 'provas', 'Das Provas', 6, 'nao', 'argumentativa', false, 'CPC art. 434');

INSERT INTO profile_rule (piece_profile_key, compliance_rule_key) VALUES
  ('contestacao', 'impugnacao_especifica'),
  ('contestacao', 'preliminares_antes_merito'),
  ('contestacao', 'eventualidade');

-- piece_profile: apelacao (ambos os polos — quem recorre depende de quem sucumbiu) —
-- cabimento/tempestividade → síntese → razões de reforma → prequestionamento → pedido.
INSERT INTO piece_profile (key, nome, polo, matter_key, base_skeleton_key, format_profile_key, version_atual, fonte_legal) VALUES
  ('apelacao', 'Apelação', 'ambos', 'civel', 'default', 'default', 'v1', '["CPC art. 1009", "CPC art. 1010", "CPC art. 1013"]');

INSERT INTO profile_section (piece_profile_key, key, titulo, ordem, obrigatoria, origem, aceita_teses, fonte_legal) VALUES
  ('apelacao', 'cabimento_tempestividade', 'Do Cabimento e da Tempestividade', 1, 'sim', 'argumentativa', false, 'CPC art. 1010, I'),
  ('apelacao', 'sintese', 'Da Síntese da Demanda', 2, 'sim', 'argumentativa', false, 'CPC art. 1010, I'),
  ('apelacao', 'razoes_reforma', 'Das Razões para a Reforma', 3, 'sim', 'argumentativa', true, 'CPC art. 1010, II e III'),
  ('apelacao', 'prequestionamento', 'Do Prequestionamento', 4, 'condicional', 'argumentativa', true, 'Súmula 282/356 STF'),
  ('apelacao', 'pedido', 'Do Pedido', 5, 'sim', 'argumentativa', false, 'CPC art. 1010, IV');

INSERT INTO profile_rule (piece_profile_key, compliance_rule_key) VALUES
  ('apelacao', 'vedacao_inovacao'),
  ('apelacao', 'dialeticidade');
