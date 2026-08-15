-- notifications slice queries (the avisos domain: notification + delivery).
-- Writes participate in the use case's transaction (transactional outbox / dedup
-- commit together); the recipient read runs inside that same tx so RLS scopes it
-- to the event's tenant. Absence is a typed error at the mapper, never (nil, nil).

-- name: InsertNotification :one
-- Record the aviso itself (the fact that a user should be told something), inside
-- the caller's tx. recipient_user_id is nullable (some avisos are tenant-level);
-- title/body are the materialized in-app text (NULL for EMAIL avisos, which render
-- at send); payload is the template data. status starts CREATED — the delivery rows
-- carry the per-channel lifecycle.
INSERT INTO notification (
    tenant_id, recipient_user_id, type, title, body, payload, status
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: InsertDelivery :one
-- Open a per-channel delivery attempt for a notification, inside the caller's tx.
-- UNIQUE(notification_id, channel) is the idempotency floor: at most one delivery
-- per channel per notification. The consumer-side event dedup (processed_event)
-- already prevents re-processing, so a fresh notification never conflicts here.
INSERT INTO notification_delivery (
    notification_id, tenant_id, channel, status, provider_message_id, error
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateDeliveryStatus :one
-- Record the outcome of a send: SENT with the provider's message id, or FAILED
-- with the reason. Scoped by id AND tenant_id (app-layer barrier) on top of RLS.
-- No row (a delivery from another tenant) → the mapper maps pgx.ErrNoRows to a
-- typed not-found.
UPDATE notification_delivery
   SET status = $3,
       provider_message_id = $4,
       error = $5,
       updated_at = now()
 WHERE id = $1
   AND tenant_id = $2
RETURNING *;

-- name: FindRecipientEmail :one
-- Resolve the recipient's e-mail by internal app_user id, scoped to the tenant
-- (WHERE + RLS). No row → the mapper maps pgx.ErrNoRows to a typed not-found, and
-- the use case records a FAILED delivery rather than dropping the aviso.
SELECT email FROM app_user
WHERE id = $1
  AND tenant_id = $2;

-- name: HasRunningBackfillForTenant :one
-- Report whether the tenant has a backfill_job still RUNNING. The in-app use case
-- suppresses a per-andamento aviso while the onboarding import is in flight (the
-- import_finished aviso covers that window). Runs inside the caller's tx, so RLS and
-- the explicit tenant_id are the two isolation barriers.
SELECT EXISTS (
    SELECT 1 FROM backfill_job
    WHERE tenant_id = $1 AND status = 'RUNNING'
) AS running;

-- name: ListNotifications :many
-- The in-app inbox for ONE user: the avisos visible to them — tenant-level
-- (recipient_user_id IS NULL) OR addressed to them — newest first, keyset-paginated
-- on (created_at, id) descending. `read` is per-user: EXISTS a receipt for THIS user
-- in notification_read. @unread_only filters to the ones this user has not read;
-- @type ('' = all) filters to one closed-set type. The caller passes the max sentinel
-- cursor ('9999-…', max-uuid) for the first page, so there is no conditional WHERE.
-- tenant_id ($1) is barrier 1; RLS is barrier 2.
SELECT n.id, n.type, n.title, n.body, n.payload, n.created_at,
       EXISTS (
           SELECT 1 FROM notification_read r
           WHERE r.notification_id = n.id AND r.user_id = @user_id::uuid
       ) AS read
FROM notification n
WHERE n.tenant_id = $1
  -- In-app inbox: only avisos with materialized in-app text. EMAIL avisos leave
  -- title NULL (they render at send), so excluding them keeps an empty-text row
  -- out of the bell and off the unread badge.
  AND n.title IS NOT NULL
  AND (n.recipient_user_id IS NULL OR n.recipient_user_id = @user_id::uuid)
  AND (@type::text = '' OR n.type = @type::text)
  AND (
      NOT @unread_only::boolean
      OR NOT EXISTS (
          SELECT 1 FROM notification_read r
          WHERE r.notification_id = n.id AND r.user_id = @user_id::uuid
      )
  )
  AND (n.created_at, n.id) < (@last_created::timestamptz, @last_id::uuid)
ORDER BY n.created_at DESC, n.id DESC
LIMIT $2;

-- name: CountUnread :one
-- The unread-badge count: how many avisos visible to the user carry no read receipt
-- from them. tenant_id ($1) is barrier 1; RLS is barrier 2.
SELECT count(*) FROM notification n
WHERE n.tenant_id = $1
  -- Same in-app filter as ListNotifications: EMAIL avisos (title NULL) never
  -- count toward the in-app unread badge.
  AND n.title IS NOT NULL
  AND (n.recipient_user_id IS NULL OR n.recipient_user_id = @user_id::uuid)
  AND NOT EXISTS (
      SELECT 1 FROM notification_read r
      WHERE r.notification_id = n.id AND r.user_id = @user_id::uuid
  );

-- name: NotificationVisibleTo :one
-- Assert an aviso exists AND is visible to the user in the tenant (tenant-level or
-- addressed to them). mark-read uses it to 404 an id from another tenant / addressed
-- to someone else, distinct from the idempotent receipt insert below (which cannot
-- tell "unknown aviso" from "already read" on its own). Runs in the caller's tx.
SELECT EXISTS (
    SELECT 1 FROM notification n
    WHERE n.id = @notification_id::uuid
      AND n.tenant_id = @tenant_id::uuid
      AND (n.recipient_user_id IS NULL OR n.recipient_user_id = @user_id::uuid)
) AS visible;

-- name: MarkNotificationRead :exec
-- Record the user's read receipt for one aviso, in the caller's tx. Idempotent: a
-- second call is a no-op via the (notification_id, user_id) PK. Visibility is
-- asserted separately (NotificationVisibleTo), so an unknown/cross-tenant id 404s.
INSERT INTO notification_read (notification_id, user_id, tenant_id)
VALUES (@notification_id::uuid, @user_id::uuid, @tenant_id::uuid)
ON CONFLICT (notification_id, user_id) DO NOTHING;

-- name: MarkAllNotificationsRead :exec
-- Record read receipts for every aviso visible to the user that they have not read
-- yet, in the caller's tx. Idempotent: a re-run inserts nothing (the NOT EXISTS
-- filter plus the ON CONFLICT floor).
INSERT INTO notification_read (notification_id, user_id, tenant_id)
SELECT n.id, @user_id::uuid, @tenant_id::uuid
FROM notification n
WHERE n.tenant_id = @tenant_id::uuid
  AND (n.recipient_user_id IS NULL OR n.recipient_user_id = @user_id::uuid)
  AND NOT EXISTS (
      SELECT 1 FROM notification_read r
      WHERE r.notification_id = n.id AND r.user_id = @user_id::uuid
  )
ON CONFLICT (notification_id, user_id) DO NOTHING;

-- name: FindPreferenceChannels :one
-- Resolve the recipient's saved channel override for (tenant, user, type), inside
-- the caller's tx — the routing check OnNotificationRequested makes before sending.
-- No row means "no override": the mapper turns pgx.ErrNoRows into a typed
-- not-found the use case reads as the default (every channel enabled), never a
-- silently-empty slice that would suppress every channel by accident.
SELECT channels FROM notification_preference
WHERE tenant_id = $1 AND app_user_id = $2 AND type = $3;

-- name: ListPreferences :many
-- The caller's own saved overrides (GET /v1/notifications/preferences), on the
-- pool — a screen read, no tx. Types the user never touched are absent (the
-- default applies implicitly); the FE reconciles against its own static list of
-- notification types. tenant_id ($1) is barrier 1; RLS is barrier 2.
SELECT * FROM notification_preference
WHERE tenant_id = $1 AND app_user_id = $2
ORDER BY type;

-- name: UpsertPreference :one
-- Save the caller's full enabled-channel set for one type (PUT
-- /v1/notifications/preferences), inside the caller's tx. Idempotent: a repeat PUT
-- with the same channels is a harmless no-op write; ON CONFLICT replaces the set
-- whole (channels is not a delta).
INSERT INTO notification_preference (tenant_id, app_user_id, type, channels)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, app_user_id, type) DO UPDATE
   SET channels = EXCLUDED.channels,
       updated_at = now()
RETURNING *;

-- name: FindDeliveryByProviderMessageID :one
-- Locate a delivery by the provider's message id (the Resend email id), on the
-- pool. A provider bounce/complaint webhook carries no tenant, so this read crosses
-- tenants (the owner bypasses RLS) to recover the delivery AND its tenant; the
-- caller then scopes the status update to that tenant (barrier 2). No row → the
-- mapper maps pgx.ErrNoRows to a typed not-found (an unknown id the webhook acks).
SELECT * FROM notification_delivery
WHERE provider_message_id = $1
LIMIT 1;
