-- Linhagem de reconciliação. Liga cada processo/intimação à janela (sync_run) que
-- o descobriu, liga a janela ao seu backfill_job (o "guarda-chuva" de uma
-- importação), e passa a contar processos/intimações NOVOS por janela.
--
-- Por quê court_records_new/intimations_new: o items_new legado conta ANDAMENTOS
-- novos (docket_entry), não processos — então uma janela de descoberta DJEN, que
-- cria court_record + intimation mas nenhum andamento, aparecia com "0 novos"
-- enquanto o total de processos do tenant subia. As duas colunas dedicadas tornam
-- a contagem por janela consistente com o total.

ALTER TABLE court_record ADD COLUMN sync_run_id uuid REFERENCES sync_run(id);
ALTER TABLE intimation   ADD COLUMN sync_run_id uuid REFERENCES sync_run(id);

ALTER TABLE sync_run
  ADD COLUMN backfill_job_id   uuid REFERENCES backfill_job(id),
  ADD COLUMN court_records_new int NOT NULL DEFAULT 0,
  ADD COLUMN intimations_new   int NOT NULL DEFAULT 0;

-- O collapse lista itens por janela (sync_run_id); o guarda-chuva agrupa janelas
-- por job (backfill_job_id).
CREATE INDEX ON court_record (sync_run_id);
CREATE INDEX ON intimation (sync_run_id);
CREATE INDEX ON sync_run (backfill_job_id);
