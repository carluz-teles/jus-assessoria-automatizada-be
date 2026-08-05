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

	"github.com/jusassessoria/platform/internal/acquisition"
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
		notifications.NewRepository(pool),
		ch,
		notifications.NewDedup(),
		database.NewUnitOfWork(pool),
	)
}

// newWebhookUC wires the provider-webhook use case (bounce/complaint) against the
// real container — a pool-backed repo (the delivery lookup is tenant-less) plus the
// shared unit of work for the tenant-scoped status write.
func newWebhookUC(pool *pgxpool.Pool) *notifications.WebhookUseCase {
	return notifications.NewWebhookUseCase(
		notifications.NewRepository(pool),
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

// AC3/AC4: a Resend bounce webhook locates the delivery by its provider_message_id
// (a tenant-less lookup on the pool) and flips it to BOUNCED under that delivery's
// tenant scope — provider id preserved, reason recorded. A replay is idempotent.
func TestNotifications_Webhook_BounceUpdatesDeliveryByProviderID(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-notif-bounce", 0)
	recipientID, _ := seedRecipient(t, pool, tenantID, "org-notif-bounce")

	// Send a member_joined first so a SENT delivery exists carrying the provider id.
	const providerID = "resend-bounce-1"
	if err := newNotifyUC(pool, &spyChannel{id: providerID}).
		OnNotificationRequested(ctx, memberJoined("evt_bounce_1", tenantID, recipientID)); err != nil {
		t.Fatalf("seed send: %v", err)
	}
	if row, _ := readDelivery(t, pool, tenantID); row.status != string(notifications.DeliverySent) {
		t.Fatalf("precondition: delivery = %+v, want SENT", row)
	}

	// The webhook carries no tenant — it finds the delivery by provider id alone and
	// flips it to BOUNCED with a reason.
	uc := newWebhookUC(pool)
	if err := uc.MarkDeliveryOutcome(ctx, providerID, notifications.DeliveryBounced, "hard bounce"); err != nil {
		t.Fatalf("MarkDeliveryOutcome: %v", err)
	}

	row, ok := readDelivery(t, pool, tenantID)
	if !ok {
		t.Fatal("delivery row vanished")
	}
	if row.status != string(notifications.DeliveryBounced) {
		t.Fatalf("status = %q, want BOUNCED", row.status)
	}
	if row.providerID == nil || *row.providerID != providerID {
		t.Fatalf("provider id = %v, want %q preserved", row.providerID, providerID)
	}
	if row.errText == nil || *row.errText != "hard bounce" {
		t.Fatalf("error = %v, want the bounce reason", row.errText)
	}

	// Idempotent replay: the same webhook again leaves the state untouched.
	if err := uc.MarkDeliveryOutcome(ctx, providerID, notifications.DeliveryBounced, "hard bounce"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if row2, _ := readDelivery(t, pool, tenantID); row2.status != string(notifications.DeliveryBounced) {
		t.Fatalf("status after replay = %q, want BOUNCED", row2.status)
	}

	// An unknown provider id is a no-op ack, not an error.
	if err := uc.MarkDeliveryOutcome(ctx, "never-sent-id", notifications.DeliveryComplained, "x"); err != nil {
		t.Fatalf("unknown id should ack, got %v", err)
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

// newInAppUC wires the in-app use case (slice 1a) against the real container.
func newInAppUC(pool *pgxpool.Pool) *notifications.InAppUseCase {
	return notifications.NewInAppUseCase(
		notifications.NewRepository(pool),
		notifications.NewDedup(),
		database.NewUnitOfWork(pool),
	)
}

// notificationRow is an aviso read directly (RLS bypassed as owner), carrying the
// in-app materialized columns (nullable title/body). Read state moved off the row to
// the per-user notification_read table (migration 0018), so it is no longer read here.
type notificationRow struct {
	typ   string
	title *string
	body  *string
}

func readNotification(t *testing.T, pool *pgxpool.Pool, tenantID string) (notificationRow, bool) {
	t.Helper()
	var row notificationRow
	err := pool.QueryRow(context.Background(),
		`SELECT type, title, body FROM notification WHERE tenant_id = $1`, tenantID,
	).Scan(&row.typ, &row.title, &row.body)
	if errors.Is(err, pgx.ErrNoRows) {
		return notificationRow{}, false
	}
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	return row, true
}

// backfillFinished builds an acquisition.backfill_finished for a tenant with a terminal
// status and error tally.
func backfillFinished(eventID, tenantID, status string, slicesError int) notifications.BackfillFinished {
	return notifications.BackfillFinished{
		Base:          events.Base{EventID: eventID, Aggregate: tenantID},
		TenantID:      tenantID,
		BackfillJobID: uuid.NewString(),
		IntegrationID: uuid.NewString(),
		TotalSlices:   10,
		SlicesOK:      10 - slicesError,
		SlicesError:   slicesError,
		Status:        status,
	}
}

// docketEntryObserved builds an acquisition.docket_entry_observed for a tenant.
func docketEntryObserved(eventID, tenantID string) notifications.DocketEntryObserved {
	return notifications.DocketEntryObserved{
		Base:          events.Base{EventID: eventID, Aggregate: tenantID},
		TenantID:      tenantID,
		SyncRunID:     uuid.NewString(),
		CourtRecordID: uuid.NewString(),
		DocketEntryID: uuid.NewString(),
		Hash:          "hash-int-1",
	}
}

// CA8: a backfill_finished (COMPLETED) persists one import_finished aviso with its
// materialized title/body and a NULL read_at, plus one IN_APP/QUEUED delivery — read
// back through a real round-trip. A replay (same event id) creates nothing more.
func TestNotifications_InApp_BackfillFinished_PersistsImportFinished(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inapp-import", 0)

	uc := newInAppUC(pool)
	if err := uc.OnBackfillFinished(ctx, backfillFinished("evt_inapp_bf_1", tenantID, acquisition.BackfillStatusCompleted, 0)); err != nil {
		t.Fatalf("OnBackfillFinished: %v", err)
	}

	if n := countNotifications(t, pool, tenantID); n != 1 {
		t.Fatalf("notification rows = %d, want 1", n)
	}
	row, ok := readNotification(t, pool, tenantID)
	if !ok {
		t.Fatal("notification row was not created")
	}
	if row.typ != notifications.TypeImportFinished {
		t.Fatalf("type = %q, want import_finished", row.typ)
	}
	if row.title == nil || *row.title == "" || row.body == nil || *row.body == "" {
		t.Fatalf("title/body = %v / %v, want both materialized", row.title, row.body)
	}

	delivery, ok := readDelivery(t, pool, tenantID)
	if !ok {
		t.Fatal("delivery row was not created")
	}
	if delivery.channel != notifications.ChannelInApp || delivery.status != string(notifications.DeliveryQueued) {
		t.Fatalf("delivery = %+v, want IN_APP/QUEUED", delivery)
	}
	if got := countBillingProcessedEvent(t, pool, "notifications.backfill", "evt_inapp_bf_1"); got != 1 {
		t.Fatalf("processed_event rows = %d, want 1", got)
	}

	// Replay: the same event id creates nothing more.
	if err := uc.OnBackfillFinished(ctx, backfillFinished("evt_inapp_bf_1", tenantID, acquisition.BackfillStatusCompleted, 0)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := countNotifications(t, pool, tenantID); n != 1 {
		t.Fatalf("notification rows after replay = %d, want 1", n)
	}
}

// CA5: a docket_entry_observed is suppressed while a backfill is RUNNING — no aviso is
// persisted — but the event is dedup-marked, so a redelivery stays suppressed.
func TestNotifications_InApp_DocketEntry_SuppressedDuringBackfill(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inapp-suppress", 0)
	integrationID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)
	seedBackfillJob(t, pool, tenantID, integrationID, 10) // RUNNING

	uc := newInAppUC(pool)
	if err := uc.OnDocketEntryObserved(ctx, docketEntryObserved("evt_inapp_dk_suppressed", tenantID)); err != nil {
		t.Fatalf("OnDocketEntryObserved: %v", err)
	}

	if n := countNotifications(t, pool, tenantID); n != 0 {
		t.Fatalf("notification rows = %d, want 0 (suppressed during backfill)", n)
	}
	if got := countBillingProcessedEvent(t, pool, "notifications.docket", "evt_inapp_dk_suppressed"); got != 1 {
		t.Fatalf("processed_event rows = %d, want 1 (marked before suppressing)", got)
	}
}

// CA6: with no backfill running, a docket_entry_observed persists a new_andamento aviso
// plus one IN_APP/QUEUED delivery.
func TestNotifications_InApp_DocketEntry_CreatesNewAndamento(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inapp-andamento", 0)

	uc := newInAppUC(pool)
	if err := uc.OnDocketEntryObserved(ctx, docketEntryObserved("evt_inapp_dk_1", tenantID)); err != nil {
		t.Fatalf("OnDocketEntryObserved: %v", err)
	}

	row, ok := readNotification(t, pool, tenantID)
	if !ok {
		t.Fatal("notification row was not created")
	}
	if row.typ != notifications.TypeNewAndamento {
		t.Fatalf("type = %q, want new_andamento", row.typ)
	}
	delivery, ok := readDelivery(t, pool, tenantID)
	if !ok {
		t.Fatal("delivery row was not created")
	}
	if delivery.channel != notifications.ChannelInApp || delivery.status != string(notifications.DeliveryQueued) {
		t.Fatalf("delivery = %+v, want IN_APP/QUEUED", delivery)
	}
}

// HasRunningBackfillForTenant reflects the tenant's backfill state through a real tx
// (RLS in force): true while a job is RUNNING, false once COMPLETED, and false for a
// different tenant's RUNNING job (isolation).
func TestNotifications_InApp_HasRunningBackfillForTenant(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-hasrun-a", 0)
	seedTenant(t, pool, tenantB, "org-hasrun-b", 0)
	integA := seedIntegration(t, pool, tenantA, acquisition.SourceDJEN)
	jobA := seedBackfillJob(t, pool, tenantA, integA, 10) // RUNNING

	repo := notifications.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)
	hasRunning := func(tenantID string) bool {
		t.Helper()
		var running bool
		if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
			var e error
			running, e = repo.HasRunningBackfillForTenant(ctx, tx, tenantID)
			return e
		}); err != nil {
			t.Fatalf("HasRunningBackfillForTenant(%s): %v", tenantID, err)
		}
		return running
	}

	if !hasRunning(tenantA) {
		t.Fatal("tenant A with a RUNNING job: got false, want true")
	}
	// Tenant B has no job of its own — A's RUNNING job must not leak across the tenant.
	if hasRunning(tenantB) {
		t.Fatal("tenant B (no job): got true, want false — RUNNING job leaked across tenants")
	}
	// Close A's job → no longer running.
	mustExec(t, pool, `UPDATE backfill_job SET status='COMPLETED' WHERE id=$1`, jobA)
	if hasRunning(tenantA) {
		t.Fatal("tenant A after COMPLETED: got true, want false")
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
