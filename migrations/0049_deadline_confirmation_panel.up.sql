-- Painel de confirmação de prazo (design-handoff-v2 §3 "Máquina de estados do prazo").
-- Aditivo, sem rewrite de tabela (defaults não-voláteis): o termo inicial escolhível
-- (anchor), os dias extras manuais (feriado local / suspensão forense), a citação legal
-- da regra (snapshot), e o estado terminal NO_DEADLINE ("mera ciência").

-- anchor_event: qual evento observado da intimação ancora o start_date. DEADLINE_START é
-- o padrão legado (o deadline_start_at derivado); MADE_AVAILABLE/PUBLISHED re-ancoram nas
-- datas reais da intimação. (JOINDER/juntada não tem data na intimation — adiado.)
-- manual_extra_days: dias extras que o advogado informa (feriado local / suspensão que o
-- motor de calendário não conhece); aplicados PELO motor, nunca por aritmética crua.
-- legal_citation: snapshot congelado da citação da regra na derivação/confirm (como
-- rules_version), pra o painel mostrar "art. 218, §1º, CPC" mesmo que a regra evolua.
ALTER TABLE deadline
  ADD COLUMN anchor_event      text NOT NULL DEFAULT 'DEADLINE_START'
    CHECK (anchor_event IN ('MADE_AVAILABLE', 'PUBLISHED', 'DEADLINE_START')),
  ADD COLUMN manual_extra_days int  NOT NULL DEFAULT 0 CHECK (manual_extra_days >= 0),
  ADD COLUMN legal_citation    text;

-- NO_DEADLINE = o advogado declarou "mera ciência" (sem prazo a cumprir). DISTINTO de
-- CANCELLED (que é a revogação por retificação da intimação, dirigida por evento). Um
-- prazo NO_DEADLINE fica FORA dos guards de MarkMissed (status='OPEN') e do reconcile
-- (status IN ('MISSED','OPEN')) → nunca vira atraso nem MISSED. Reversível por reopen.
ALTER TABLE deadline DROP CONSTRAINT deadline_status_check;
ALTER TABLE deadline ADD CONSTRAINT deadline_status_check
  CHECK (status IN ('PENDING', 'OPEN', 'MET', 'MISSED', 'CANCELLED', 'NO_DEADLINE'));

-- Citação legal é propriedade da REGRA (por isso vive na deadline_rule) mas é copiada
-- (snapshot) pro deadline na derivação. Catch-all ('*') e COMUNICACAO/GENERICO ficam NULL
-- → a UI mostra "confirme a regra" em vez de inventar fundamento.
ALTER TABLE deadline_rule ADD COLUMN legal_citation text;
UPDATE deadline_rule SET legal_citation = 'art. 335, CPC'      WHERE rules_version = 'v0' AND intimation_type = 'CITACAO';
UPDATE deadline_rule SET legal_citation = 'art. 218, §1º, CPC' WHERE rules_version = 'v0' AND intimation_type = 'INTIMACAO';
