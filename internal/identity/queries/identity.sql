-- identity slice queries (tenant + app_user).
-- Upserts are idempotent so the Clerk webhook (at-least-once) can replay safely.

-- name: UpsertTenant :one
-- Provision or refresh a tenant from its Clerk Organization.
INSERT INTO tenant (clerk_org_id, name)
VALUES ($1, $2)
ON CONFLICT (clerk_org_id) DO UPDATE
   SET name = EXCLUDED.name
RETURNING *;

-- name: GetTenantByClerkOrg :one
SELECT * FROM tenant
WHERE clerk_org_id = $1;

-- name: GetTenantByID :one
SELECT * FROM tenant
WHERE id = $1;

-- name: UpsertUser :one
-- Provision or refresh an app_user from its Clerk User. Role is set on first
-- insert only; membership webhooks resync email/name, not the product role.
-- phone is COALESCEd, not overwritten: only user.updated carries a phone, while
-- membership.created (which also flows through here) carries none. The COALESCE
-- keeps an at-least-once membership replay from clearing a phone already synced
-- from user.updated — the phone-less path passes NULL and leaves it untouched.
INSERT INTO app_user (clerk_user_id, tenant_id, email, name, role, phone)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (clerk_user_id) DO UPDATE
   SET email = EXCLUDED.email,
       name  = EXCLUDED.name,
       phone = COALESCE(EXCLUDED.phone, app_user.phone)
RETURNING *;

-- name: GetUserByClerkUser :one
SELECT * FROM app_user
WHERE clerk_user_id = $1;

-- name: GetUserByID :one
SELECT * FROM app_user
WHERE id = $1;

-- name: GetMeByClerkUser :one
-- Onboarding read model for GET /identity/me: the caller's internal tenant and
-- its onboarding gate, joined from app_user by Clerk user id. No row → the
-- authenticated user has no tenant yet (the domain treats that as "not
-- onboarded", a 200 with nulls, not an error).
SELECT u.tenant_id,
       t.onboarding_completed_at
FROM app_user u
JOIN tenant t ON t.id = u.tenant_id
WHERE u.clerk_user_id = $1;

-- name: UpdateOrgProfile :one
-- Persist the escritório's company profile during onboarding and stamp the
-- onboarding gate exactly once: COALESCE keeps the first completion time across
-- replays (idempotent — a second PUT does not move onboarding_completed_at).
-- WHERE id scopes the write to the caller's own tenant (app-level barrier; the
-- tenant table has no tenant_id of its own and therefore no RLS policy).
UPDATE tenant
   SET cnpj = $2,
       legal_name = $3,
       trade_name = $4,
       address = $5,
       onboarding_completed_at = COALESCE(onboarding_completed_at, now())
WHERE id = $1
RETURNING *;
