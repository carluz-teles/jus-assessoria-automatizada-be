-- Achado 2 (reconciliação de ciclo de vida): quando um court_record vira ARCHIVED
-- (processo concluído), os deadlines PENDING/OPEN/MISSED que ainda pendem nele
-- precisam de um estado terminal PRÓPRIO — distinto de CANCELLED (revogação por
-- retificação de intimação) — que fique auditável no histórico como "resolvido pela
-- conclusão do processo", não um cancelamento genérico. Aditiva: só amplia o CHECK
-- existente (0049), sem tocar linhas.
ALTER TABLE deadline DROP CONSTRAINT deadline_status_check;
ALTER TABLE deadline ADD CONSTRAINT deadline_status_check
  CHECK (status IN ('PENDING', 'OPEN', 'MET', 'MISSED', 'CANCELLED', 'NO_DEADLINE', 'RESOLVED_ON_CONCLUSION'));
