//go:build integration

// Notifications integration tests — prove the avisos domain against a REAL Postgres:
// a notification.requested creates the notification + a QUEUED delivery in one
// tenant-scoped tx, the recipient's e-mail is resolved through that tx (so RLS is in
// force), and the send phase flips the delivery to SENT with the provider id. A
// replay (same event id) is a no-op via the processed_event dedup. An unresolvable
// recipient is recorded FAILED, not dropped. And the two tables' RLS policies isolate
// one tenant from another.
//
// The Channel is stubbed (no network, no Resend): it captures the message and returns
// a fixed id. Everything below it — repo, dedup, unit of work, the real schema — runs
// against the container.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// spyChannel is a notifications.Channel that records what it was asked to send and
// returns a preset id/error, so the integration test drives the real persistence
// without an e-mail provider.
type spyChannel struct {
	sent []notifications.EmailMessage
	id   string
	err  error
}

func (c *spyChannel) Kind() string { return notifications.ChannelEmail }

func (c *spyChannel) Send(_ context.Context, msg notifications.EmailMessage) (string, error) {
	c.sent = append(c.sent, msg)
	return c.id, c.err
}

// newNotifyUC wires the notify use case against the real container with the given
// (stubbed) channel.
func newNotifyUC(pool *pgxpool.Pool, ch notifications.Channel) *notifications.NotifyUseCase {
	return notifications.NewNotifyUseCase(
		notifications.NewRepository(),
		ch,
		notifications.NewDedup(),
		database.NewUnitOfWork(pool),
	)
}

// seedRecipient inserts one app_user for the tenant and returns its id and e-mail.
func seedRecipient(t *testing.T, pool *pgxpool.Pool, tenantID, org string) (appUserID, email string) {
	t.Helper()
	email = "recipient@" + org + ".test"
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO app_user (clerk_user_id, tenant_id, email) VALUES ($1, $2, $3) RETURNING id`,
		org+"-recipient", tenantID, email,
	).Scan(&appUserID); err != nil {
		t.Fatalf("seed recipient: %v", err)
	}
	return appUserID, email
}

// memberJoined builds a member_joined notification.requested for a tenant/recipient.
func memberJoined(eventID, tenantID, recipientID string) notifications.NotificationRequested {
	return notifications.NotificationRequested{
		Base:            events.Base{EventID: eventID, Aggregate: tenantID},
		TenantID:        tenantID,
		RecipientUserID: recipientID,
		Type:            "member_joined",
		Payload:         map[string]any{"member_name": "Ana", "org_name": org(tenantID)},
	}
}

// org is a stable per-tenant label for the payload (the tenant uuid is unique).
func org(tenantID string) string { return "org-" + tenantID[:8] }

// deliveryRow is a delivery read directly (RLS bypassed as owner).
type deliveryRow struct {
	channel    string
	status     string
	providerID *string
	errText    *string
}

func readDelivery(t *testing.T, pool *pgxpool.Pool, tenantID string) (deliveryRow, bool) {
	t.Helper()
	var row deliveryRow
	err := pool.QueryRow(context.Background(),
		`SELECT d.channel, d.status, d.provider_message_id, d.error
		   FROM notification_delivery d
		   JOIN notification n ON n.id = d.notification_id
		  WHERE n.tenant_id = $1`, tenantID,
	).Scan(&row.channel, &row.status, &row.providerID, &row.errText)
	if errors.Is(err, pgx.ErrNoRows) {
		return deliveryRow{}, false
	}
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	return row, true
}

// countNotifications counts notification rows for a tenant (owner read).
func countNotifications(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// AC2/AC6: a member_joined event creates the notification + delivery(QUEUED) in the
// tenant-scoped tx, resolves the recipient's e-mail through it, then sends → the
// delivery is SENT with the provider id; a replay neither re-creates nor re-sends.
func TestNotifications_MemberJoined_PersistsResolvesAndSends(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-notif-joined", 0)
	recipientID, email := seedRecipient(t, pool, tenantID, "org-notif-joined")

	ch := &spyChannel{id: "resend-int-1"}
	uc := newNotifyUC(pool, ch)

	if err := uc.OnNotificationRequested(ctx, memberJoined("evt_notif_1", tenantID, recipientID)); err != nil {
		t.Fatalf("OnNotificationRequested: %v", err)
	}

	// Exactly one notification and one delivery, the delivery SENT with the provider id.
	if n := countNotifications(t, pool, tenantID); n != 1 {
		t.Fatalf("notification rows = %d, want 1", n)
	}
	row, ok := readDelivery(t, pool, tenantID)
	if !ok {
		t.Fatal("delivery row was not created")
	}
	if row.channel != notifications.ChannelEmail || row.status != string(notifications.DeliverySent) {
		t.Fatalf("delivery = %+v, want EMAIL/SENT", row)
	}
	if row.providerID == nil || *row.providerID != "resend-int-1" {
		t.Fatalf("provider id = %v, want resend-int-1", row.providerID)
	}
	// The e-mail was resolved through the tenant-scoped tx and sent to that address.
	if len(ch.sent) != 1 || ch.sent[0].To != email {
		t.Fatalf("sent = %+v, want one to %q", ch.sent, email)
	}

	// The dedup mark landed under the "notifications" consumer (reuses the billing helper).
	if got := countBillingProcessedEvent(t, pool, "notifications", "evt_notif_1"); got != 1 {
		t.Fatalf("processed_event rows = %d, want 1", got)
	}

	// AC3: a replay (identical event id) is a no-op — no second notification/delivery/send.
	if err := uc.OnNotificationRequested(ctx, memberJoined("evt_notif_1", tenantID, recipientID)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := countNotifications(t, pool, tenantID); n != 1 {
		t.Fatalf("notification rows after replay = %d, want 1", n)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("sends after replay = %d, want 1", len(ch.sent))
	}
}

// AC4/AC6: a recipient not in the tenant cannot be resolved → the delivery is
// recorded FAILED with a reason, no e-mail is sent, and the listener does not fail.
func TestNotifications_UnresolvableRecipient_RecordsFailed(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-notif-failed", 0)

	// A recipient id that belongs to no app_user in this tenant.
	ch := &spyChannel{id: "unused"}
	uc := newNotifyUC(pool, ch)

	if err := uc.OnNotificationRequested(ctx, memberJoined("evt_notif_nf", tenantID, uuid.NewString())); err != nil {
		t.Fatalf("OnNotificationRequested: %v", err)
	}

	row, ok := readDelivery(t, pool, tenantID)
	if !ok {
		t.Fatal("delivery row was not created")
	}
	if row.status != string(notifications.DeliveryFailed) || row.errText == nil || *row.errText == "" {
		t.Fatalf("delivery = %+v, want FAILED with a reason", row)
	}
	if len(ch.sent) != 0 {
		t.Fatalf("sent %d e-mails for an unresolvable recipient, want 0", len(ch.sent))
	}
}

// AC1/AC6: the notification tables' RLS policies isolate tenants — with app.tenant_id
// set, the app_rls role sees only that tenant's notification; unset, it sees none.
func TestNotifications_RLS_TenantIsolation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// The non-owner role the policies bind to (idempotent — shared with the other
	// RLS suites; created here too so this test stands alone regardless of order).
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
			CREATE ROLE app_rls;
		END IF;
	END $$`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA public TO app_rls`)
	mustExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls`)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-notif-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-notif-rls-b", 0)
	recipientA, _ := seedRecipient(t, pool, tenantA, "org-notif-rls-a")

	if err := newNotifyUC(pool, &spyChannel{id: "x"}).OnNotificationRequested(ctx, memberJoined("evt_rls_a", tenantA, recipientA)); err != nil {
		t.Fatalf("OnNotificationRequested(A): %v", err)
	}

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id
		want     int
	}{
		{name: "tenant A sees its own notification", tenantID: tenantA, want: 1},
		{name: "tenant B sees none", tenantID: tenantB, want: 0},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countNotificationsAsRLSRole(t, tt.tenantID); got != tt.want {
				t.Errorf("notification count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countNotificationsAsRLSRole counts notification rows visible to the non-owner
// app_rls role with app.tenant_id set (or unset when empty), on a DEDICATED
// connection — mirrors countSubscriptionsAsRLSRole (see rls_test.go for the
// pristine-connection rationale).
func countNotificationsAsRLSRole(t *testing.T, tenantID string) int {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) // read-only probe; never commit

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_rls"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if tenantID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			t.Fatalf("set_config: %v", err)
		}
	}

	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM notification").Scan(&count); err != nil {
		t.Fatalf("count notification: %v", err)
	}
	return count
}
