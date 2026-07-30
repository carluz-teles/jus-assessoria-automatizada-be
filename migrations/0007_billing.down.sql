-- 0006_billing.down.sql — drops the subscription table (RLS policy and the
-- UNIQUE-backed indexes go with it).

DROP TABLE subscription;
