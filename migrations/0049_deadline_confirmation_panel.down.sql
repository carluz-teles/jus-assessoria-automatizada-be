-- Reverte 0049. Assume que nenhuma linha ficou em NO_DEADLINE (o CHECK de 5 estados
-- falharia com dados nesse estado — comportamento esperado de um down destrutivo).
ALTER TABLE deadline_rule DROP COLUMN legal_citation;

ALTER TABLE deadline DROP CONSTRAINT deadline_status_check;
ALTER TABLE deadline ADD CONSTRAINT deadline_status_check
  CHECK (status IN ('PENDING', 'OPEN', 'MET', 'MISSED', 'CANCELLED'));

ALTER TABLE deadline
  DROP COLUMN anchor_event,
  DROP COLUMN manual_extra_days,
  DROP COLUMN legal_citation;
