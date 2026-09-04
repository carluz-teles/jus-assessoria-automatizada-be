-- Reverte 0098. Assume que nenhuma linha ficou em RESOLVED_ON_CONCLUSION (o CHECK de 6
-- estados falharia com dados nesse estado — comportamento esperado de um down destrutivo,
-- mesma convenção do down de 0049).
ALTER TABLE deadline DROP CONSTRAINT deadline_status_check;
ALTER TABLE deadline ADD CONSTRAINT deadline_status_check
  CHECK (status IN ('PENDING', 'OPEN', 'MET', 'MISSED', 'CANCELLED', 'NO_DEADLINE'));
