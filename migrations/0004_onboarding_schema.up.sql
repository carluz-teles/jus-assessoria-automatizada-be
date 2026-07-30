-- 0004_onboarding_schema.up.sql — prepares the data terrain for onboarding.
-- Faithful to docs/erd-modelo-de-dados.md (source of truth for the schema).
-- All columns are NULLABLE: the tenant/app_user rows are provisioned from Clerk
-- webhooks BEFORE the onboarding profile is filled in, so every field added here
-- has to tolerate an unset value. No UNIQUE on cnpj in v0 (docs).

-- app_user.phone — the user's phone, synced from the Clerk User (user.updated).
ALTER TABLE app_user ADD COLUMN phone text;

-- tenant company profile — the escritório's legal identity, captured during
-- onboarding (the columns are written by a later slice; here they are pure
-- schema). onboarding_completed_at gates the "is this tenant onboarded?" check.
ALTER TABLE tenant
  ADD COLUMN cnpj                    text,
  ADD COLUMN legal_name             text,
  ADD COLUMN trade_name             text,
  ADD COLUMN address                jsonb,
  ADD COLUMN onboarding_completed_at timestamptz;
