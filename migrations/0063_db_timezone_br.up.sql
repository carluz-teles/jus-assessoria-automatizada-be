-- 0063_db_timezone_br — CURRENT_DATE em queries de prazo (deadline.end_date -
-- CURRENT_DATE) precisa ser a data do FORO no Brasil. Postgres em UTC retorna
-- a data um dia à frente entre 21:00 e 23:59 BRT (00:00-03:00 UTC), gerando
-- days_left errado, filtros de urgência (atraso/hoje/próximos) errados e
-- "vence hoje" virar "1 dia em atraso" na virada da tarde/noite.
--
-- Fix: setar timezone default do database pra America/Sao_Paulo. Aplica a
-- toda nova session (o pool do api reconecta e passa a usar). Timestamps
-- armazenados continuam em UTC (comportamento padrão do timestamptz — não
-- muda semântica); só a janela de "que dia é hoje" respeita o fuso do foro.
--
-- Escopo: 44 usos de CURRENT_DATE em queries dos slices acquisition, deadline
-- e draft. Todos passam a computar contra a data BR sem tocar em nenhuma
-- query — a mudança acontece na resolução do CURRENT_DATE em runtime.
--
-- Persistência: pg_db_role_setting sobrevive a restart do container. Em prod
-- (Railway), esta migration garante que o setting seja aplicado a novos
-- ambientes sem ter que rodar comando manual.
--
-- ALTER DATABASE exige um identificador literal (não aceita current_database()
-- direto) — current_database() via EXECUTE dinâmico faz a migration funcionar
-- em qualquer nome de banco (dev, testcontainers, Railway), não só "jusassessoria".

DO $$
BEGIN
  EXECUTE format('ALTER DATABASE %I SET timezone = %L', current_database(), 'America/Sao_Paulo');
END
$$;
