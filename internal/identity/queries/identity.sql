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

-- name: GetActiveUserByClerkUser :one
-- Resolve the app_user behind a Clerk user ONLY while it still has an ACTIVE
-- membership — the authorization gate ResolvePrincipal reads. A soft-removed member
-- (status REMOVED) yields no row, so the caller maps it to ErrUserNotFound (→ 401),
-- exactly like an unknown user. The role returned is app_user.role (kept in sync by
-- an organizationMembership.updated), never the membership row's own copy.
SELECT u.* FROM app_user u
JOIN membership m
  ON m.app_user_id = u.id AND m.tenant_id = u.tenant_id
WHERE u.clerk_user_id = $1 AND m.status = 'ACTIVE';

-- name: GetUserByID :one
SELECT * FROM app_user
WHERE id = $1;

-- name: UpsertMembership :one
-- Provision or reactivate a membership from an organizationMembership.created
-- webhook. Idempotent for at-least-once delivery: ON CONFLICT re-asserts the
-- ACTIVE link (role/clerk id refreshed, removed_at cleared). The `joined` flag
-- tells the use case whether this is a genuine join (a brand-new row, or a REMOVED
-- one reactivated) versus a replay of an already-ACTIVE membership — so the
-- member_joined event fires once per real join, not on every retry. prev captures
-- the pre-upsert status: NULL (no row) or 'REMOVED' both differ from 'ACTIVE'.
WITH prev AS (
    SELECT status FROM membership WHERE tenant_id = $1 AND app_user_id = $2
)
INSERT INTO membership (tenant_id, app_user_id, clerk_membership_id, role, status)
VALUES ($1, $2, $3, $4, 'ACTIVE')
ON CONFLICT (tenant_id, app_user_id) DO UPDATE
   SET role                = EXCLUDED.role,
       clerk_membership_id = EXCLUDED.clerk_membership_id,
       status              = 'ACTIVE',
       removed_at          = NULL
RETURNING *, coalesce((SELECT status FROM prev), '') <> 'ACTIVE' AS joined;

-- name: SoftRemoveMembership :one
-- Soft-delete a membership from an organizationMembership.deleted webhook: flip
-- ACTIVE→REMOVED and stamp removed_at. WHERE status='ACTIVE' makes it fire at most
-- once — a replay (already REMOVED) or an unknown clerk id matches no row, so
-- RETURNING is empty (pgx.ErrNoRows) and the caller treats it as an idempotent
-- no-op that republishes nothing. Role and app_user are untouched: a soft-delete
-- preserves the projection, it does not rewrite it.
UPDATE membership
   SET status = 'REMOVED',
       removed_at = now()
 WHERE clerk_membership_id = $1 AND status = 'ACTIVE'
RETURNING *;

-- name: UpdateMembershipRole :exec
-- Sync a role change from an organizationMembership.updated webhook. The role lives
-- in two places: membership.role (the link) and app_user.role (what ResolvePrincipal
-- authorizes on) — this updates BOTH in one statement so they never drift. The
-- data-modifying CTE always runs to completion (Postgres guarantees it), so the
-- membership write happens even though its output only feeds the app_user update.
-- Idempotent: re-applying the same role is a harmless no-op; an unknown clerk id
-- matches no membership, leaves the CTE empty, and updates nothing.
WITH synced AS (
    UPDATE membership
       SET role = $2
     WHERE clerk_membership_id = $1
    RETURNING app_user_id
)
UPDATE app_user
   SET role = $2
 WHERE id = (SELECT app_user_id FROM synced);

-- name: GetActiveMemberClerkUser :one
-- Resolve the Clerk user id behind an ACTIVE membership, scoped to the tenant —
-- the lookup DELETE /v1/organization/members/:id needs before it can ask Clerk to
-- revoke access. Tenant-scoped by m.tenant_id (barrier 1; RLS is barrier 2 on both
-- tables). A REMOVED or foreign membership matches no row, so the caller gets the
-- same not-found as an unknown id — never leaking whether the member exists under
-- another tenant.
SELECT u.clerk_user_id
FROM membership m
JOIN app_user u ON u.id = m.app_user_id AND u.tenant_id = m.tenant_id
WHERE m.tenant_id = $1 AND m.app_user_id = $2 AND m.status = 'ACTIVE';

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

-- name: ListOrgMembers :many
-- The escritório's team for the responsável selector (and the /organization members
-- list): every ACTIVE membership of the tenant, joined to its app_user for name/email.
-- Tenant-scoped by m.tenant_id (barrier 1; RLS is barrier 2 on both tables). Only ACTIVE
-- rows — a soft-removed member drops out. The role is the membership's own copy (kept in
-- lockstep with app_user.role by organizationMembership.updated). Ordered by name so the
-- selector is stable; the team is small, so no cursor.
SELECT u.id, u.name, u.email, m.role
FROM membership m
JOIN app_user u ON u.id = m.app_user_id AND u.tenant_id = m.tenant_id
WHERE m.tenant_id = $1 AND m.status = 'ACTIVE'
ORDER BY u.name, u.id;

-- name: UpdateOrgProfile :one
-- Persist the escritório's company profile during onboarding and stamp the
-- onboarding gate exactly once: COALESCE keeps the first completion time across
-- replays (idempotent — a second PUT does not move onboarding_completed_at).
-- phone and email are optional (the company's, not the user's) and written
-- straight — an absent value arrives as SQL NULL and simply clears the column.
-- WHERE id scopes the write to the caller's own tenant (app-level barrier; the
-- tenant table has no tenant_id of its own and therefore no RLS policy).
UPDATE tenant
   SET cnpj = $2,
       legal_name = $3,
       trade_name = $4,
       address = $5,
       phone = $6,
       email = $7,
       onboarding_completed_at = COALESCE(onboarding_completed_at, now())
WHERE id = $1
RETURNING *;
