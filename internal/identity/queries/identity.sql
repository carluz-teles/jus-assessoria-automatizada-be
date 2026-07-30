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
