//go:build integration

// Billing integration tests — prove the Stripe webhook projection against a REAL
// Postgres: a customer.subscription.created upserts the subscription row AND
// commits billing.subscription_activated in the SAME transaction (atomic; a failed
// publish rolls the whole write back); the dedup guard (processed_event, consumer
// "billing") makes a replay a no-op that neither re-writes nor re-publishes; and
// the subscription table's RLS policy isolates one tenant from another.
//
// The Stripe gateway is stubbed (no network, no signing): VerifyWebhook returns a
// crafted event. The plan is resolved from the REAL local catalog (migration
// 0037's `plan` table, see seedBillingPlan) — plan resolution moved off the
// gateway into the repository, so exercising it for real is the point of these
// tests. Everything below the gateway — repo, outbox, dedup, unit of work — is
// the real thing against the container.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// stubGateway is a billing.StripeGateway that returns a crafted event, so the
// integration test drives the real projection without Stripe. Plan resolution no
// longer lives on the gateway (see seedBillingPlan) — the stub only ever needs to
// hand back the event.
type stubGateway struct {
	event billing.StripeEvent
}

func (g stubGateway) VerifyWebhook([]byte, string) (billing.StripeEvent, error) {
	return g.event, nil
}

// The 4-B endpoint methods are unused by the webhook/read-model projection tests
// below (they drive Stripe, which stays out of the container), so they are inert
// here — present only to satisfy billing.StripeGateway.
func (g stubGateway) EnsureCustomer(context.Context, string) (string, error) {
	return "", nil
}

func (g stubGateway) CreateCheckoutSession(context.Context, billing.CheckoutParams) (string, error) {
	return "", nil
}

func (g stubGateway) CreatePortalSession(context.Context, string, string) (string, error) {
	return "", nil
}

func (g stubGateway) ListPlans(context.Context) ([]billing.StripePlan, error) {
	return nil, nil
}

// seedBillingPlan upserts a plan row wired to priceID with the given process
// ceiling, so applySubscription's local FindPlanByStripePriceID resolves it —
// idempotent (ON CONFLICT on the UNIQUE stripe_price_id) so every test in this
// file can call it without colliding on a shared container.
func seedBillingPlan(t *testing.T, pool *pgxpool.Pool, priceID string, maxProcesses int) {
	t.Helper()

	mustExec(t, pool, `
		INSERT INTO plan (code, name, min_processes, max_processes, price_per_process_cents, stripe_price_id)
		VALUES ('test-pro', 'pro', 1, $2, 35, $1)
		ON CONFLICT (stripe_price_id) DO UPDATE
		   SET max_processes = EXCLUDED.max_processes`,
		priceID, maxProcesses,
	)
}

// newBillingUC wires the billing use case against the real container with the
// given (stubbed) gateway.
func newBillingUC(pool *pgxpool.Pool, gw billing.StripeGateway) *billing.UseCase {
	return billing.NewUseCase(
		billing.NewRepository(pool),
		gw,
		events.NewOutbox(),
		billing.NewDedup(),
		database.NewUnitOfWork(pool),
	)
}

// subscriptionRow is the raw subscription as read directly (RLS bypassed as owner).
type subscriptionRow struct {
	status     string
	plan       *string
	customerID *string
	subID      *string
	limit      *int32
	periodEnd  *time.Time
}

func readSubscription(t *testing.T, pool *pgxpool.Pool, tenantID string) (subscriptionRow, bool) {
	t.Helper()
	var row subscriptionRow
	err := pool.QueryRow(context.Background(),
		`SELECT status, plan, stripe_customer_id, stripe_subscription_id, active_process_limit, current_period_end
		   FROM subscription WHERE tenant_id = $1`, tenantID,
	).Scan(&row.status, &row.plan, &row.customerID, &row.subID, &row.limit, &row.periodEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return subscriptionRow{}, false
	}
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	return row, true
}

// countBillingOutbox counts unpublished outbox rows of a given type for a tenant.
func countBillingOutbox(t *testing.T, pool *pgxpool.Pool, eventType, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox
		  WHERE type = $1 AND published_at IS NULL AND aggregate_id::text = $2`,
		eventType, tenantID).Scan(&n); err != nil {
		t.Fatalf("count billing outbox: %v", err)
	}
	return n
}

// createdEvent builds a customer.subscription.created StripeEvent for a tenant.
// The Stripe ids are derived from the tenant so they stay globally unique across
// tests that share the container schema (stripe_customer_id is UNIQUE).
func createdEvent(eventID, tenantID string, periodEnd time.Time) billing.StripeEvent {
	return billing.StripeEvent{
		ID:       eventID,
		Type:     billing.EventSubscriptionCreated,
		TenantID: tenantID,
		Subscription: &billing.StripeSubscription{
			ID: "sub_" + tenantID, CustomerID: "cus_" + tenantID, Status: "active",
			PriceID: "price_1", CurrentPeriodEnd: periodEnd,
		},
	}
}

// AC3/AC6: a created event upserts the subscription AND commits
// subscription_activated in the same tx, with plan + active_process_limit
// resolved from the local plan catalog; a replay (same event id) neither
// re-writes nor re-publishes.
func TestBilling_SubscriptionCreated_UpsertAndOutboxSameTx(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-billing-created", 0)
	seedBillingPlan(t, pool, "price_1", 50)

	periodEnd := time.Unix(1893456000, 0).UTC()
	gw := stubGateway{event: createdEvent("evt_created_1", tenantID, periodEnd)}
	uc := newBillingUC(pool, gw)

	if err := uc.HandleWebhook(ctx, nil, ""); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}

	// The projection landed with the resolved plan/limit and the Stripe ids.
	row, ok := readSubscription(t, pool, tenantID)
	if !ok {
		t.Fatal("subscription row was not created")
	}
	if row.status != string(billing.StatusActive) {
		t.Fatalf("status = %q, want active", row.status)
	}
	if row.plan == nil || *row.plan != "pro" || row.limit == nil || *row.limit != 50 {
		t.Fatalf("plan/limit = %v/%v, want pro/50", row.plan, row.limit)
	}
	if row.customerID == nil || *row.customerID != "cus_"+tenantID || row.subID == nil || *row.subID != "sub_"+tenantID {
		t.Fatalf("stripe ids = %v/%v", row.customerID, row.subID)
	}
	if row.periodEnd == nil || !row.periodEnd.Equal(periodEnd) {
		t.Fatalf("period end = %v, want %v", row.periodEnd, periodEnd)
	}

	// Exactly one subscription_activated, tied to the tenant, unpublished (same tx).
	if n := countBillingOutbox(t, pool, billing.TypeSubscriptionActivated, tenantID); n != 1 {
		t.Fatalf("subscription_activated outbox rows = %d, want 1", n)
	}
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE type = $1 AND aggregate_id::text = $2`,
		billing.TypeSubscriptionActivated, tenantID).Scan(&payload); err != nil {
		t.Fatalf("read event payload: %v", err)
	}
	var ev billing.SubscriptionActivated
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("unmarshal subscription_activated: %v", err)
	}
	if ev.TenantID != tenantID || ev.Plan != "pro" || ev.EventID == "" || ev.AggregateID() != tenantID {
		t.Fatalf("event payload = %+v, unexpected", ev)
	}

	// The processed_event mark was written under the "billing" consumer.
	if got := countBillingProcessedEvent(t, pool, "billing", "evt_created_1"); got != 1 {
		t.Fatalf("processed_event rows = %d, want 1", got)
	}

	// AC2: a replay (identical event id) is a no-op — no second event, one row.
	if err := uc.HandleWebhook(ctx, nil, ""); err != nil {
		t.Fatalf("HandleWebhook replay: %v", err)
	}
	if n := countBillingOutbox(t, pool, billing.TypeSubscriptionActivated, tenantID); n != 1 {
		t.Fatalf("outbox rows after replay = %d, want 1 (no republish)", n)
	}
}

// AC6 (atomicity): if the publish fails, the whole unit of work rolls back — no
// subscription row is written, no event and no dedup mark survive.
func TestBilling_SubscriptionCreated_PublishFailRollsBackAll(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-billing-rollback", 0)
	seedBillingPlan(t, pool, "price_1", 50)

	// failingOutbox (defined in acquisition_test.go) aborts the publish inside the tx.
	uc := billing.NewUseCase(
		billing.NewRepository(pool),
		stubGateway{event: createdEvent("evt_rollback_1", tenantID, time.Unix(1893456000, 0).UTC())},
		failingOutbox{err: errors.New("outbox unreachable")},
		billing.NewDedup(),
		database.NewUnitOfWork(pool),
	)

	if err := uc.HandleWebhook(context.Background(), nil, ""); err == nil {
		t.Fatal("HandleWebhook() error = nil, want the publish failure")
	}

	if _, ok := readSubscription(t, pool, tenantID); ok {
		t.Fatal("subscription row survived a rolled-back tx")
	}
	if n := countBillingOutbox(t, pool, billing.TypeSubscriptionActivated, tenantID); n != 0 {
		t.Fatalf("outbox rows after rollback = %d, want 0", n)
	}
	if got := countBillingProcessedEvent(t, pool, "billing", "evt_rollback_1"); got != 0 {
		t.Fatalf("processed_event rows after rollback = %d, want 0", got)
	}
}

// AC3/AC6: GetSubscription reads the tenant's projection back from REAL Postgres
// (FindByTenant on the pool) — the read model the endpoint serves without any
// Stripe call. A tenant that never checked out gets ErrSubscriptionNotFound.
func TestBilling_GetSubscription_ReadsProjectionFromPostgres(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-billing-getsub", 0)
	seedBillingPlan(t, pool, "price_1", 50)

	periodEnd := time.Unix(1893456000, 0).UTC()
	gw := stubGateway{event: createdEvent("evt_getsub_1", tenantID, periodEnd)}
	uc := newBillingUC(pool, gw)

	// A missing tenant is the typed not-found, never (nil, nil).
	if _, err := uc.GetSubscription(ctx, tenantID); !errors.Is(err, billing.ErrSubscriptionNotFound) {
		t.Fatalf("GetSubscription before checkout = %v, want ErrSubscriptionNotFound", err)
	}

	// Project the subscription via the webhook, then read it back.
	if err := uc.HandleWebhook(ctx, nil, ""); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}

	sub, err := uc.GetSubscription(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.TenantID != tenantID || sub.Plan != "pro" || sub.Status != billing.StatusActive || sub.ActiveProcessLimit != 50 {
		t.Fatalf("read model = %+v", sub)
	}
	if sub.StripeCustomerID != "cus_"+tenantID {
		t.Fatalf("stripe customer id = %q, want cus_%s", sub.StripeCustomerID, tenantID)
	}
	if sub.CurrentPeriodEnd == nil || !sub.CurrentPeriodEnd.Equal(periodEnd) {
		t.Fatalf("period end = %v, want %v", sub.CurrentPeriodEnd, periodEnd)
	}
}

// AC1: the subscription table's RLS policy isolates tenants — with app.tenant_id
// set, the app_rls role sees only that tenant's row; unset, it sees none.
func TestBilling_RLS_TenantIsolation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// The non-owner role the policies bind to (idempotent — shared with the RLS
	// suite, created here too so this test stands alone regardless of order).
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
			CREATE ROLE app_rls;
		END IF;
	END $$`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA public TO app_rls`)
	mustExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls`)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-billing-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-billing-rls-b", 0)
	seedBillingPlan(t, pool, "price_1", 50)

	// One subscription for tenant A only.
	gw := stubGateway{event: createdEvent("evt_rls_a", tenantA, time.Unix(1893456000, 0).UTC())}
	if err := newBillingUC(pool, gw).HandleWebhook(ctx, nil, ""); err != nil {
		t.Fatalf("HandleWebhook(A): %v", err)
	}

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id
		want     int
	}{
		{name: "tenant A sees its own subscription", tenantID: tenantA, want: 1},
		{name: "tenant B sees none", tenantID: tenantB, want: 0},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countSubscriptionsAsRLSRole(t, tt.tenantID); got != tt.want {
				t.Errorf("subscription count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countBillingProcessedEvent counts dedup marks for a (consumer, event_id). The
// suite already has a single-arg countProcessedEvent for another consumer, so this
// consumer-aware variant is billing-specific.
func countBillingProcessedEvent(t *testing.T, pool *pgxpool.Pool, consumer, eventID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_event WHERE consumer = $1 AND event_id = $2`,
		consumer, eventID).Scan(&n); err != nil {
		t.Fatalf("count processed_event: %v", err)
	}
	return n
}

// countSubscriptionsAsRLSRole counts subscription rows visible to the non-owner
// app_rls role with app.tenant_id set (or unset when empty), on a DEDICATED
// connection — mirrors countAppUsersAsRLSRole (see rls_test.go for the pristine-
// connection rationale).
func countSubscriptionsAsRLSRole(t *testing.T, tenantID string) int {
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
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM subscription").Scan(&count); err != nil {
		t.Fatalf("count subscription: %v", err)
	}
	return count
}
