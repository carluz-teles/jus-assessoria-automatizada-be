-- 0046_capture_run — trilha de auditoria da captura DIÁRIA e do ENRIQUECIMENTO,
-- por-tenant. Hoje só a "carga inicial" (backfill_job + sync_run) deixa registro
-- auditável; a captura diária (MatchUseCase, fan-out do firehose nacional) e o
-- enriquecimento DATAJUD (EnrichmentUseCase) escrevem sem deixar rastro. A tela
-- "Capturas" lê capture_run UNION backfill_job (INITIAL_LOAD).
--
-- Por que PER-TENANT (tenant_id NOT NULL, não nacional/NULL): a linha DAILY_CAPTURE
-- representa o efeito REAL naquele tenant ("+3 proc · +12 intim · 3 OABs · 9 prazos"),
-- escrito na MESMA tx do write per-tenant que já existe (writeForTenant), sob o
-- advisory lock do tenant. Isso mantém a RLS padrão do projeto (tenant_isolation
-- simples, sem escape hatch de sistema).
--
-- kind: DAILY_CAPTURE (uma linha por dia de fan-out do firehose) | ENRICHMENT (uma
-- linha por dia agregando os N enriquecimentos DATAJUD daquele tenant). window_from/
-- window_to marcam o dia coberto; o UNIQUE (tenant, source, kind, window_from) é o
-- alvo do ON CONFLICT que o enriquecimento usa para incrementar court_records_updated
-- ao longo do dia.
CREATE TABLE capture_run (
    id                    uuid        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    tenant_id             uuid        NOT NULL REFERENCES tenant(id),
    source                text        NOT NULL,                          -- DJEN | DATAJUD
    kind                  text        NOT NULL CHECK (kind IN ('DAILY_CAPTURE', 'ENRICHMENT')),
    window_from           date,
    window_to             date,
    started_at            timestamptz NOT NULL DEFAULT now(),
    finished_at           timestamptz,
    status                text        NOT NULL DEFAULT 'RUNNING'
                            CHECK (status IN ('RUNNING', 'OK', 'PARTIAL', 'FAILED')),
    court_records_new     int         NOT NULL DEFAULT 0,
    intimations_new       int         NOT NULL DEFAULT 0,
    court_records_updated int         NOT NULL DEFAULT 0,
    errors                int         NOT NULL DEFAULT 0,
    error                 jsonb,
    created_at            timestamptz NOT NULL DEFAULT now(),
    -- O upsert diário do enriquecimento (ON CONFLICT) e a idempotência do dia
    -- dependem deste UNIQUE — uma linha por (tenant, fonte, tipo, dia).
    UNIQUE (tenant_id, source, kind, window_from)
);

-- A tela lista as capturas do tenant, mais recentes primeiro (started_at DESC).
CREATE INDEX capture_run_tenant_started_idx ON capture_run (tenant_id, started_at DESC);
-- Varredura por fonte/tipo (KPIs, diagnóstico operacional cross-tenant via sistema).
CREATE INDEX capture_run_source_kind_started_idx ON capture_run (source, kind, started_at DESC);

-- capture_run é tenant-scoped → mesma tenant_isolation de toda tabela de usuário.
-- Sem escape hatch de sistema: as duas escritas (DAILY_CAPTURE, ENRICHMENT) rodam
-- na tx per-tenant (app.tenant_id setado); a tela lê per-tenant. Fail-closed.
ALTER TABLE capture_run ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON capture_run
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- sync_run ganha court_records_updated (aditivo, DEFAULT 0): o detail drawer da
-- carga inicial passa a mostrar quantos processos a janela ATUALIZOU (reobservou),
-- não só quantos descobriu (court_records_new). Simétrico às colunas de 0020.
ALTER TABLE sync_run ADD COLUMN court_records_updated int NOT NULL DEFAULT 0;

-- deadline ganha created_at: a tela precisa contar prazos DERIVADOS por captura
-- (deadline criados na janela [started_at, finished_at]) e o KPI "prazos derivados
-- hoje". A tabela deadline nasceu (0001) sem created_at; task.created_at já existe
-- (0031/0024). IF NOT EXISTS por segurança. DEFAULT now() é volátil, mas deadline
-- tem 0 linhas relevantes de auditoria retroativa aqui — o custo de rewrite é
-- desprezível no v0 (a slice de prazos está começando).
ALTER TABLE deadline ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS deadline_tenant_created_idx ON deadline (tenant_id, created_at);
