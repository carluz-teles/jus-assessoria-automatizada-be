-- Tela "Capturas" (acquisition slice). Trilha de auditoria unificada dos três
-- fluxos de captura, por-tenant:
--   • DAILY_CAPTURE — o fan-out diário do firehose nacional (capture_run, kind);
--   • ENRICHMENT    — o enriquecimento DATAJUD do dia (capture_run, kind);
--   • INITIAL_LOAD  — a carga inicial de onboarding (backfill_job agregando sync_run).
-- capture_run é escrito na MESMA tx dos writes que já existem (writeForTenant /
-- applyEnrichment); backfill_job/sync_run continuam do fluxo de reconciliação. A
-- lista faz UNION ALL das duas fontes; o read model deriva o rótulo/DisplayStatus.

-- name: ListCaptureRuns :many
-- As capturas do tenant, mais recentes primeiro. UNION ALL de capture_run (DAILY_
-- CAPTURE, 1 linha por dia; ENRICHMENT, 1 linha por importação) com backfill_job
-- (INITIAL_LOAD, uma linha por importação agregando as janelas sync_run). Os tallies do
-- INITIAL_LOAD somam as colunas dedicadas das janelas (court_records_new/intimations_new/
-- court_records_updated) e os errors vêm de slices_error. A linha ENRICHMENT não tem
-- janela própria (window_from/to NULL): ela DERIVA a janela da importação via LEFT JOIN
-- backfill_job por capture_run.backfill_job_id (COALESCE cai na coluna própria da linha
-- quando não há import — DAILY_CAPTURE). window/started/finished normalizados para o
-- mesmo shape das duas fontes. Bounded por $2 (sem paginação v0).
SELECT
    cr.id,
    cr.source,
    cr.kind,
    COALESCE(bj.window_from, cr.window_from)::date AS window_from,
    COALESCE(bj.window_to, cr.window_to)::date     AS window_to,
    cr.started_at,
    cr.finished_at,
    cr.status,
    cr.court_records_new,
    cr.intimations_new,
    cr.court_records_updated,
    cr.errors
FROM capture_run cr
LEFT JOIN backfill_job bj ON bj.id = cr.backfill_job_id
WHERE cr.tenant_id = $1
UNION ALL
SELECT
    b.id,
    i.source,
    'INITIAL_LOAD'::text                                        AS kind,
    b.window_from,
    b.window_to,
    b.created_at                                                AS started_at,
    (CASE WHEN b.status = 'RUNNING' THEN NULL ELSE MAX(s.finished_at) END)::timestamptz AS finished_at,
    b.status,
    COALESCE(SUM(s.court_records_new), 0)::int                  AS court_records_new,
    COALESCE(SUM(s.intimations_new), 0)::int                    AS intimations_new,
    COALESCE(SUM(s.court_records_updated), 0)::int              AS court_records_updated,
    b.slices_error                                              AS errors
FROM backfill_job b
JOIN integration i ON i.id = b.integration_id
LEFT JOIN sync_run s ON s.backfill_job_id = b.id
WHERE b.tenant_id = $1
GROUP BY b.id, i.source
ORDER BY started_at DESC
LIMIT $2;

-- name: GetCaptureRunByID :one
-- Uma linha capture_run do tenant (detalhe de DAILY_CAPTURE / ENRICHMENT). A carga
-- inicial (INITIAL_LOAD) continua no endpoint de reconciliation existente — este é
-- só para as duas capturas nativas. A linha ENRICHMENT deriva a janela da importação
-- (LEFT JOIN backfill_job), como no ListCaptureRuns. Um miss (pgx.ErrNoRows) vira o 404.
SELECT
    cr.id,
    cr.source,
    cr.kind,
    COALESCE(bj.window_from, cr.window_from)::date AS window_from,
    COALESCE(bj.window_to, cr.window_to)::date     AS window_to,
    cr.started_at,
    cr.finished_at,
    cr.status,
    cr.court_records_new,
    cr.intimations_new,
    cr.court_records_updated,
    cr.errors
FROM capture_run cr
LEFT JOIN backfill_job bj ON bj.id = cr.backfill_job_id
WHERE cr.tenant_id = $1 AND cr.id = $2;

-- name: GetCaptureSummary :one
-- Os KPIs do topo da tela, escopados ao tenant. last_capture_at é a captura
-- bem-sucedida mais recente (OK/PARTIAL, com finished_at); intimations_new_today
-- soma as intimações novas das capturas iniciadas hoje (started_at >= hoje).
SELECT
    (MAX(finished_at) FILTER (WHERE status IN ('OK', 'PARTIAL')))::timestamptz AS last_capture_at,
    COALESCE(SUM(intimations_new) FILTER (WHERE started_at >= now()::date), 0)::bigint AS intimations_new_today
FROM capture_run
WHERE tenant_id = $1;

-- name: CountDeadlinesCreatedBetween :one
-- Quantos prazos (deadline) o tenant derivou dentro da janela de uma captura
-- [started_at, finished_at] — a coluna "prazos" por linha da tabela. deadline.
-- created_at nasce em 0046. Escopado por tenant (barreira 1 + RLS).
SELECT count(*) FROM deadline
WHERE tenant_id = $1 AND created_at >= $2 AND created_at <= $3;

-- name: CountTasksCreatedBetween :one
-- Quantas tarefas (task) foram criadas dentro da janela de uma captura
-- [started_at, finished_at]. task.created_at já existe. Escopado por tenant.
SELECT count(*) FROM task
WHERE tenant_id = $1 AND created_at >= $2 AND created_at <= $3;

-- name: CountDeadlinesCreatedToday :one
-- O KPI "prazos derivados hoje": deadline criados hoje (created_at >= hoje),
-- por tenant.
SELECT count(*) FROM deadline
WHERE tenant_id = $1 AND created_at >= now()::date;

-- name: CountWatchedOABsForDJEN :one
-- O card "OABs" de uma captura: quantas OABs o tenant monitora pela integração
-- DJEN (a fonte que descobre). Junta watched_oab à integration da fonte DJEN.
SELECT count(*) FROM watched_oab w
JOIN integration i ON i.id = w.integration_id
WHERE w.tenant_id = $1 AND i.source = 'DJEN';

-- name: InsertDailyCaptureRun :exec
-- Grava a linha DAILY_CAPTURE de um dia, na MESMA tx do write per-tenant do fan-out
-- (writeForTenant, sob o advisory lock do tenant). Abre e fecha de uma vez: o
-- fan-out é síncrono (upsert dos records + intimações), então started_at/finished_at
-- e o status final (OK) já são conhecidos. ON CONFLICT (tenant, source, kind,
-- window_from) faz um re-run do MESMO dia SOMAR aos contadores e avançar finished_at
-- — idempotência-friendly sob at-least-once (uma re-entrega do dia não zera o dia).
INSERT INTO capture_run
    (tenant_id, source, kind, window_from, window_to, started_at, finished_at,
     status, court_records_new, intimations_new, court_records_updated)
VALUES
    (@tenant_id, 'DJEN', 'DAILY_CAPTURE', @window_from, @window_to, @started_at,
     @finished_at, 'OK', @court_records_new, @intimations_new, @court_records_updated)
ON CONFLICT (tenant_id, source, kind, window_from) DO UPDATE SET
    court_records_new     = capture_run.court_records_new     + EXCLUDED.court_records_new,
    intimations_new       = capture_run.intimations_new       + EXCLUDED.intimations_new,
    court_records_updated = capture_run.court_records_updated + EXCLUDED.court_records_updated,
    finished_at           = EXCLUDED.finished_at,
    status                = 'OK';

-- name: IncrementImportEnrichmentRun :exec
-- Incrementa a linha ENRICHMENT DA IMPORTAÇÃO por UM enriquecimento DATAJUD aplicado, na
-- MESMA tx do applyEnrichment (sob o advisory lock do tenant). A primeira aplicação da
-- importação cria a linha em status RUNNING ("Em andamento") com court_records_updated=1;
-- as seguintes somam +1 e avançam finished_at, MANTENDO RUNNING — o status terminal
-- (OK/PARTIAL) é carimbado só pelo fecho por ETA (CloseImportEnrichmentRun), quando a
-- fila de enriquecimento drenou e a cobertura é conhecida. window_from/to ficam NULL: a
-- linha deriva a janela do backfill_job (ver ListCaptureRuns). O índice parcial
-- capture_run_enrichment_import_uq (backfill_job_id WHERE kind='ENRICHMENT') é o alvo do
-- conflito — uma linha por importação. Só chega aqui passado o dedup do evento, então uma
-- re-entrega nunca conta em dobro.
INSERT INTO capture_run
    (tenant_id, source, kind, backfill_job_id, started_at, finished_at,
     status, court_records_updated)
VALUES
    (@tenant_id, 'DATAJUD', 'ENRICHMENT', @backfill_job_id, @at, @at, 'RUNNING', 1)
ON CONFLICT (backfill_job_id) WHERE kind = 'ENRICHMENT' DO UPDATE SET
    court_records_updated = capture_run.court_records_updated + 1,
    finished_at           = EXCLUDED.finished_at;

-- name: IncrementImportEnrichmentRunBy :exec
-- Batch counterpart of IncrementImportEnrichmentRun: bump an import's ENRICHMENT row by
-- @delta applied enrichments (records a step graded) AND @errs REAL parse errors (hits
-- DATAJUD returned but we could not parse — NOT the "not in the index" 0-hit case, which is
-- expected and never counted). The first step creates the row RUNNING; later steps sum both
-- counters and advance finished_at, staying RUNNING — the terminal status is stamped by the
-- fecho (CloseImportEnrichmentRun), which decides OK vs PARTIAL purely from the accumulated
-- `errors` (0 errors → OK even if some processes were not found in DATAJUD). Same partial-
-- unique conflict target as the +1 variant. The caller skips this when delta==0 AND errs==0
-- (a no-op upsert would mint a spurious RUNNING row).
INSERT INTO capture_run
    (tenant_id, source, kind, backfill_job_id, started_at, finished_at,
     status, court_records_updated, errors)
VALUES
    (@tenant_id, 'DATAJUD', 'ENRICHMENT', @backfill_job_id, @at, @at, 'RUNNING', @delta, @errs)
ON CONFLICT (backfill_job_id) WHERE kind = 'ENRICHMENT' DO UPDATE SET
    court_records_updated = capture_run.court_records_updated + EXCLUDED.court_records_updated,
    errors                = capture_run.errors                + EXCLUDED.errors,
    finished_at           = EXCLUDED.finished_at;

-- name: CountImportDiscoveredRecords :one
-- O total de processos que a importação DESCOBRIU: SUM(sync_run.court_records_new) das
-- janelas do backfill_job. É a métrica de cobertura contra a qual o fecho por ETA compara
-- o contador de enriquecimentos aplicados (updated >= total → OK, senão PARTIAL).
SELECT COALESCE(SUM(s.court_records_new), 0)::bigint AS total
FROM sync_run s
WHERE s.tenant_id = $1 AND s.backfill_job_id = $2;

-- name: GetImportEnrichmentCounter :one
-- Lê os contadores da linha ENRICHMENT de uma importação: court_records_updated (quantos
-- processos foram enriquecidos) E errors (quantos hits o DATAJUD retornou mas não conseguimos
-- parsear — erro REAL). O fecho decide o status terminal SÓ por errors: 0 → OK ("Concluída"),
-- >0 → PARTIAL ("Falha parcial"); os não-encontrados no índice (0 hits) NÃO entram em errors,
-- então são normais e não rebaixam o status. Um miss (pgx.ErrNoRows) significa que a importação
-- não aplicou nenhum enriquecimento — o listener trata criando a linha honesta com updated=0.
SELECT court_records_updated, errors
FROM capture_run
WHERE tenant_id = $1 AND backfill_job_id = $2 AND kind = 'ENRICHMENT';

-- name: CloseImportEnrichmentRun :execrows
-- Fecho por ETA da linha ENRICHMENT de uma importação: carimba o status terminal
-- (@status = OK quando a cobertura foi atingida, PARTIAL senão) e finished_at = @at. Roda
-- no listener do EnrichmentRunCloseScheduled, agendado no fim do backfill + carência. Só
-- afeta a linha ENRICHMENT daquela importação. Retorna o número de linhas afetadas: 0
-- significa que a importação não aplicou nenhum enriquecimento (nenhuma linha foi criada)
-- — o listener trata esse caso criando uma linha honesta com updated=0.
UPDATE capture_run
SET status = @status, finished_at = @at
WHERE tenant_id = @tenant_id AND backfill_job_id = @backfill_job_id AND kind = 'ENRICHMENT';

-- name: InsertEmptyImportEnrichmentRun :exec
-- Fecho por ETA quando a importação não aplicou NENHUM enriquecimento (CloseImportEnrichmentRun
-- afetou 0 linhas): cria a linha ENRICHMENT terminal de forma honesta (court_records_updated=0,
-- status carimbado pelo fecho). ON CONFLICT DO NOTHING protege contra a corrida de uma
-- aplicação que chegou entre o UPDATE e este INSERT (o índice parcial é o alvo).
INSERT INTO capture_run
    (tenant_id, source, kind, backfill_job_id, started_at, finished_at,
     status, court_records_updated)
VALUES
    (@tenant_id, 'DATAJUD', 'ENRICHMENT', @backfill_job_id, @at, @at, @status, 0)
ON CONFLICT (backfill_job_id) WHERE kind = 'ENRICHMENT' DO NOTHING;
