-- 0047_capture_run_enrichment_by_import — re-modela a linha ENRICHMENT do capture_run
-- de BALDE-POR-DIA para FILHO-DA-IMPORTAÇÃO. O enriquecimento DATAJUD não é um evento
-- recorrente diário: é a 2ª fase de UMA importação (o backfill que roda quando uma OAB
-- é adicionada) — um conjunto FINITO de processos com começo e fim atrelados à
-- importação. A linha ENRICHMENT passa a representar esse filho: "Em andamento"
-- (RUNNING) enquanto enriquece os processos que a importação descobriu, "Concluída"
-- (OK) / "Falha parcial" (PARTIAL) quando o fecho por ETA reconcilia a cobertura.
--
-- backfill_job_id ATRELA a linha ENRICHMENT à importação (nullable: DAILY_CAPTURE não
-- usa). O contador court_records_updated conta aplicações REAIS de enriquecimento
-- (protegido pelo dedup de evento), não pode ser derivado por completeness+linhagem
-- porque o enriquecimento faz supersede+regrade e o registro perde a linhagem
-- sync_run→backfill.
ALTER TABLE capture_run ADD COLUMN backfill_job_id uuid REFERENCES backfill_job(id);

-- FK sem índice trava DELETE do pai e é lenta em join (o read junta a linha ENRICHMENT
-- ao backfill_job para derivar a janela). Índice explícito na coluna referenciadora.
CREATE INDEX capture_run_backfill_job_idx ON capture_run (backfill_job_id);

-- Re-chaveia ENRICHMENT por importação: UMA linha por backfill_job. O UNIQUE de 0046
-- (tenant, source, kind, window_from) continua servindo DAILY_CAPTURE (o balde do dia);
-- as linhas ENRICHMENT agora têm window_from NULL, então NÃO colidem nesse UNIQUE
-- (múltiplos NULLs são distintos). Este índice parcial é o alvo do ON CONFLICT do
-- upsert do enriquecimento (uma linha por importação). DAILY_CAPTURE tem
-- backfill_job_id NULL → fora do índice parcial, sem colisão.
CREATE UNIQUE INDEX capture_run_enrichment_import_uq
  ON capture_run (backfill_job_id) WHERE kind = 'ENRICHMENT';
