-- 0004_onboarding_schema.down.sql — drops the onboarding columns in reverse.

ALTER TABLE tenant
  DROP COLUMN onboarding_completed_at,
  DROP COLUMN address,
  DROP COLUMN trade_name,
  DROP COLUMN legal_name,
  DROP COLUMN cnpj;

ALTER TABLE app_user DROP COLUMN phone;
