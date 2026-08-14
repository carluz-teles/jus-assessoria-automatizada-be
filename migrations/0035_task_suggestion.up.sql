-- 0035_task_suggestion — proveniência das tarefas sugeridas pela IA (feedback loop,
-- camada 1 do erd-ai-advisory §"feedback loop"). Quando o advogado abre o F2 de um prazo,
-- o endpoint GET /v1/prazos/:id/suggested-tasks compõe o meta-prompt (internal/advisory) e
-- pede as tarefas ao LLM (lib/llm). Esta tabela GRAVA o que a IA sugeriu — com QUAL versão
-- de prompt e QUAL modelo — para que, no confirm (F2 "Aprovar tudo"), possamos medir o
-- DELTA entre a sugestão da IA e o que o humano confirmou (manteve/removeu/acrescentou).
-- Esse delta, emitido como evento, é a matéria-prima do "quão confiável foi a IA" e do
-- "no que ela se baseou" que alimentam a melhoria do prompt versionado.
--
-- SCHEMA ONLY — zero lógica de slice (essa vive em internal/deadline). A escrita é
-- best-effort no caminho de leitura (uma falha aqui NUNCA quebra o F2): o registro é
-- proveniência, não estado do produto.

-- 1 prazo → N registros de sugestão (um por vez que o F2 é aberto; guardamos o histórico,
-- lendo o mais recente no confirm). tenant_id é o inegociável do CLAUDE.md (toda tabela de
-- usuário carrega e é isolada por 2 barreiras: o filtro da app + RLS). deadline_id CASCADEs:
-- a sugestão não tem vida própria — sumir o prazo sumir sua proveniência. intimation_id é a
-- chave 1:1 pela qual o confirm correlaciona (o confirm é keyed por intimação); fica SEM FK
-- (a intimação vive no slice acquisition — slices conversam por evento, decisão P1).
-- prompt_version + model são a proveniência (qual instrução, qual LLM); suggested é o payload
-- exato que a IA devolveu (jsonb: [{title, kind}, …]) para o diff no confirm.
CREATE TABLE task_suggestion (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  deadline_id    uuid NOT NULL REFERENCES deadline(id) ON DELETE CASCADE,
  intimation_id  uuid NOT NULL,
  prompt_version text  NOT NULL,
  model          text  NOT NULL,
  suggested      jsonb NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- O confirm lê a sugestão MAIS RECENTE por intimação (ORDER BY created_at DESC LIMIT 1);
-- este índice serve exatamente essa leitura (a chave de correlação + o desempate temporal).
CREATE INDEX task_suggestion_intimation_idx ON task_suggestion (intimation_id, created_at DESC);

-- task_suggestion é tenant-scoped → mesma policy tenant_isolation de toda tabela de usuário
-- (barrier 2 sobre o filtro explícito por tenant_id, barrier 1). Shape idêntico a task/deadline.
ALTER TABLE task_suggestion ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON task_suggestion
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
