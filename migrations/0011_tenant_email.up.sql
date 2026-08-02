-- 0011_tenant_email.up.sql — adds the escritório's e-mail to the tenant profile.
-- Faithful to docs/erd-modelo-de-dados.md (source of truth for the schema).
-- NULLABLE, no default: the e-mail is optional (an org may have none), and the
-- tenant row is provisioned from the Clerk webhook before onboarding fills it in.

-- tenant.email — the escritório's e-mail, captured during onboarding (the e-mail is
-- the company's, not the user's). Written by PUT /organization/profile.
ALTER TABLE tenant ADD COLUMN email text;
