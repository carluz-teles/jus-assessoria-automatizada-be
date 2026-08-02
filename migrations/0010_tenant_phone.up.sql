-- 0010_tenant_phone.up.sql — adds the escritório's phone to the tenant profile.
-- Faithful to docs/erd-modelo-de-dados.md (source of truth for the schema).
-- NULLABLE, no default: the phone is optional (an org may have none), and the
-- tenant row is provisioned from the Clerk webhook before onboarding fills it in.

-- tenant.phone — the escritório's phone, captured during onboarding (the phone is
-- the company's, not the user's). Written by PUT /organization/profile.
ALTER TABLE tenant ADD COLUMN phone text;
