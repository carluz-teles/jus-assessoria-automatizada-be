-- 0001_init.down.sql — reverses 0001_init.up.sql.
-- DROP TABLE ... CASCADE drops each table's RLS policies and FK dependencies with
-- it, so tables are dropped in reverse dependency order and policies need no
-- explicit DROP POLICY. Rollback in production is rare (the real safety net is the
-- daily backup); down exists for dev and for reverting a bad deploy before new
-- data lands (docs/erd-backend.md 5b.3).

DROP TABLE IF EXISTS backfill_job CASCADE;
DROP TABLE IF EXISTS processed_event CASCADE;
DROP TABLE IF EXISTS outbox CASCADE;
DROP TABLE IF EXISTS deadline CASCADE;
DROP TABLE IF EXISTS petition CASCADE;
DROP TABLE IF EXISTS review CASCADE;
DROP TABLE IF EXISTS draft CASCADE;
DROP TABLE IF EXISTS chunk CASCADE;
DROP TABLE IF EXISTS document CASCADE;
DROP TABLE IF EXISTS case_link CASCADE;
DROP TABLE IF EXISTS notification CASCADE;
DROP TABLE IF EXISTS docket_entry CASCADE;
DROP TABLE IF EXISTS sync_run CASCADE;
DROP TABLE IF EXISTS court_record CASCADE;
DROP TABLE IF EXISTS court_case CASCADE;
DROP TABLE IF EXISTS integration CASCADE;
DROP TABLE IF EXISTS app_user CASCADE;
DROP TABLE IF EXISTS tenant CASCADE;

-- Extensions (vector, pgcrypto) are intentionally NOT dropped: they are
-- database-global infrastructure created with IF NOT EXISTS and may be shared by
-- other schemas, so tearing them down here would be too broad a blast radius.
