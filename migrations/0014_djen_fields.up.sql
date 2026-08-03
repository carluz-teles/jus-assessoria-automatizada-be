-- 0014_djen_fields — schema base do connector DJEN (sub-fatia A). O feed DJEN
-- carrega campos que a intimation (herdada da era `notification`) nunca modelou:
--   • type          — a comunicação é INTIMACAO, CITACAO ou COMUNICACAO
--                     (DJEN tipoComunicacao) e alimenta a regra de contagem de
--                     prazo (citação × intimação);
--   • status        — o DJEN pode RETRATAR uma publicação (data_cancelamento);
--                     a intimação vira CANCELLED e a slice de prazos revoga o
--                     deadline derivado — sem isso, prazo fantasma. Default ACTIVE;
--   • source_url    — link profundo no tribunal (DJEN 'link');
--   • cancelled_at  — DJEN data_cancelamento (quando foi retratada);
--   • cancel_reason — DJEN motivo_cancelamento.
-- court_record ganha judging_body (órgão julgador: DATAJUD orgaoJulgador /
-- DJEN nomeOrgao). Todos os campos já estão documentados em
-- docs/erd-modelo-de-dados.md. Enums são text validados na app (convenção
-- CHECK-on-app), nunca CHECK no banco. `recipients` já existe desde a 0001.
ALTER TABLE intimation
    ADD COLUMN type          text,
    ADD COLUMN status        text NOT NULL DEFAULT 'ACTIVE',
    ADD COLUMN source_url    text,
    ADD COLUMN cancelled_at  date,
    ADD COLUMN cancel_reason text;

ALTER TABLE court_record
    ADD COLUMN judging_body text;
