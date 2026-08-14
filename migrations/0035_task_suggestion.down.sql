-- Reverte 0035_task_suggestion. A policy cai junto com a tabela (DROP TABLE remove suas
-- policies e índices), mas dropamos a policy explicitamente antes por simetria com o up.
DROP POLICY IF EXISTS tenant_isolation ON task_suggestion;
DROP TABLE IF EXISTS task_suggestion;
