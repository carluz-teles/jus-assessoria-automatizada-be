package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- test doubles -----------------------------------------------------------

// mockRepo is a hand-written Repository double: each method delegates to a func
// field, so every test injects exactly the behavior it needs. Unset fields fail
// loudly (nil call) if a test reaches a path it did not expect.
type mockRepo struct {
	upsert         func(ctx context.Context, tx database.Tx, params UpsertParams) (*Subscription, error)
	updateStatus   func(ctx context.Context, tx database.Tx, tenantID string, status Status) (*Subscription, error)
	findByCustomer func(ctx context.Context, stripeCustomerID string) (*Subscription, error)
}

func (m *mockRepo) UpsertSubscription(ctx context.Context, tx database.Tx, params UpsertParams) (*Subscription, error) {
	return m.upsert(ctx, tx, params)
}

func (m *mockRepo) UpdateSubscriptionStatus(ctx context.Context, tx database.Tx, tenantID string, status Status) (*Subscription, error) {
	return m.updateStatus(ctx, tx, tenantID, status)
}

func (m *mockRepo) FindByStripeCustomer(ctx context.Context, stripeCustomerID string) (*Subscription, error) {
	return m.findByCustomer(ctx, stripeCustomerID)
}

// mockGateway is a StripeGateway double: verify returns the event a test crafts
// (so no real signing is needed), resolvePlan the plan a test wants.
type mockGateway struct {
	verify      func(payload []byte, sigHeader string) (StripeEvent, error)
	resolvePlan func(ctx context.Context, priceID string) (string, int, error)
}

func (m *mockGateway) VerifyWebhook(payload []byte, sigHeader string) (StripeEvent, error) {
	return m.verify(payload, sigHeader)
}

func (m *mockGateway) ResolvePlan(ctx context.Context, priceID string) (string, int, error) {
	return m.resolvePlan(ctx, priceID)
}

// fakeUOW is a no-op unit of work: it records the RLS scope the use case asked
// for and runs fn with a nil tx (the mocked repo/dedup never touch it). err
// injects a boundary failure to prove it propagates unwrapped.
type fakeUOW struct {
	scope  string
	called bool
	err    error
}

func (u *fakeUOW) Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.called = true
	u.scope = tenantID
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

// recordingOutbox captures what a use case publishes (and can inject a publish
// failure) so tests assert the right event is emitted in the same unit of work.
type recordingOutbox struct {
	published []events.Event
	err       error
}

func (r *recordingOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	if r.err != nil {
		return r.err
	}
	r.published = append(r.published, ev)
	return nil
}

// fakeDedup reports every event as first-seen by default; set seen=true to model
// an at-least-once replay. It records the ids it was asked to mark.
type fakeDedup struct {
	seen   bool
	err    error
	marked []string
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, _ /*consumer*/, eventID string) (bool, error) {
	d.marked = append(d.marked, eventID)
	return d.seen, d.err
}

// resolvePlanTo returns a ResolvePlan stub yielding a fixed plan/limit.
func resolvePlanTo(plan string, limit int) func(context.Context, string) (string, int, error) {
	return func(context.Context, string) (string, int, error) { return plan, limit, nil }
}

// verifyTo returns a VerifyWebhook stub yielding a fixed event.
func verifyTo(ev StripeEvent) func([]byte, string) (StripeEvent, error) {
	return func([]byte, string) (StripeEvent, error) { return ev, nil }
}

const periodEndUnix = 1893456000 // 2030-01-01T00:00:00Z, a stable non-zero window

// --- tests ------------------------------------------------------------------

func TestUseCase_HandleWebhook_SubscriptionCreated(t *testing.T) {
	periodEnd := time.Unix(periodEndUnix, 0).UTC()
	var gotParams UpsertParams
	repo := &mockRepo{
		upsert: func(_ context.Context, _ database.Tx, p UpsertParams) (*Subscription, error) {
			gotParams = p
			return &Subscription{
				TenantID:           p.TenantID,
				Plan:               p.Plan,
				Status:             p.Status,
				CurrentPeriodEnd:   &periodEnd,
				ActiveProcessLimit: p.ActiveProcessLimit,
			}, nil
		},
	}
	gw := &mockGateway{
		verify: verifyTo(StripeEvent{
			ID:       "evt_1",
			Type:     EventSubscriptionCreated,
			TenantID: "tenant-uuid",
			Subscription: &StripeSubscription{
				ID: "sub_1", CustomerID: "cus_1", Status: "active",
				PriceID: "price_1", CurrentPeriodEnd: periodEnd,
			},
		}),
		resolvePlan: resolvePlanTo("pro", 50),
	}
	outbox := &recordingOutbox{}
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, gw, outbox, dedup, uow)

	if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}

	// AC3: upsert projects active/trialing + plan + active_process_limit via ResolvePlan.
	if gotParams.TenantID != "tenant-uuid" || gotParams.StripeCustomerID != "cus_1" || gotParams.StripeSubscriptionID != "sub_1" {
		t.Fatalf("upsert ids = %+v", gotParams)
	}
	if gotParams.Status != StatusActive || gotParams.Plan != "pro" || gotParams.ActiveProcessLimit != 50 {
		t.Fatalf("upsert plan/status = %+v", gotParams)
	}
	if !gotParams.CurrentPeriodEnd.Equal(periodEnd) {
		t.Fatalf("period end = %v, want %v", gotParams.CurrentPeriodEnd, periodEnd)
	}
	// RLS scope is the tenant.
	if uow.scope != "tenant-uuid" {
		t.Fatalf("uow scope = %q, want tenant-uuid", uow.scope)
	}
	// AC3: subscription_activated published in the same unit of work.
	if len(outbox.published) != 1 || outbox.published[0].Type() != TypeSubscriptionActivated {
		t.Fatalf("published = %+v, want one subscription_activated", outbox.published)
	}
	act, ok := outbox.published[0].(SubscriptionActivated)
	if !ok || act.TenantID != "tenant-uuid" || act.Plan != "pro" || !act.CurrentPeriodEnd.Equal(periodEnd) {
		t.Fatalf("activated payload = %+v", outbox.published[0])
	}
	// Dedup was consulted with the Stripe event id.
	if len(dedup.marked) != 1 || dedup.marked[0] != "evt_1" {
		t.Fatalf("dedup marked = %v, want [evt_1]", dedup.marked)
	}
}

func TestUseCase_HandleWebhook_SubscriptionUpdated(t *testing.T) {
	repo := &mockRepo{
		upsert: func(_ context.Context, _ database.Tx, p UpsertParams) (*Subscription, error) {
			return &Subscription{TenantID: p.TenantID, Plan: p.Plan, Status: p.Status}, nil
		},
	}
	gw := &mockGateway{
		verify: verifyTo(StripeEvent{
			ID: "evt_2", Type: EventSubscriptionUpdated, TenantID: "tenant-uuid",
			Subscription: &StripeSubscription{ID: "sub_1", CustomerID: "cus_1", Status: "past_due", PriceID: "price_2"},
		}),
		resolvePlan: resolvePlanTo("enterprise", 200),
	}
	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, gw, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}

	// AC4: .updated → subscription_updated carrying the new plan + status.
	if len(outbox.published) != 1 || outbox.published[0].Type() != TypeSubscriptionUpdated {
		t.Fatalf("published = %+v, want one subscription_updated", outbox.published)
	}
	upd, ok := outbox.published[0].(SubscriptionUpdated)
	if !ok || upd.Plan != "enterprise" || upd.Status != StatusPastDue {
		t.Fatalf("updated payload = %+v", outbox.published[0])
	}
}

func TestUseCase_HandleWebhook_SubscriptionDeleted(t *testing.T) {
	var gotStatus Status
	var gotTenant string
	repo := &mockRepo{
		updateStatus: func(_ context.Context, _ database.Tx, tenantID string, status Status) (*Subscription, error) {
			gotTenant, gotStatus = tenantID, status
			return &Subscription{TenantID: tenantID, Status: status}, nil
		},
	}
	gw := &mockGateway{verify: verifyTo(StripeEvent{
		ID: "evt_3", Type: EventSubscriptionDeleted, TenantID: "tenant-uuid",
		Subscription: &StripeSubscription{ID: "sub_1", CustomerID: "cus_1", Status: "canceled"},
	})}
	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, gw, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}

	// AC4: .deleted → status canceled + subscription_canceled.
	if gotTenant != "tenant-uuid" || gotStatus != StatusCanceled {
		t.Fatalf("updateStatus(%q, %q), want (tenant-uuid, canceled)", gotTenant, gotStatus)
	}
	if len(outbox.published) != 1 || outbox.published[0].Type() != TypeSubscriptionCanceled {
		t.Fatalf("published = %+v, want one subscription_canceled", outbox.published)
	}
}

func TestUseCase_HandleWebhook_PaymentFailed(t *testing.T) {
	t.Run("tenant from metadata, status past_due, payment_failed emitted", func(t *testing.T) {
		var gotStatus Status
		repo := &mockRepo{
			updateStatus: func(_ context.Context, _ database.Tx, _ string, status Status) (*Subscription, error) {
				gotStatus = status
				return &Subscription{Status: status}, nil
			},
		}
		gw := &mockGateway{verify: verifyTo(StripeEvent{
			ID: "evt_4", Type: EventPaymentFailed, TenantID: "tenant-uuid",
			Invoice: &StripeInvoice{ID: "in_1", CustomerID: "cus_1", AmountDue: 4990},
		})}
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, gw, outbox, &fakeDedup{}, &fakeUOW{})

		if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}

		// AC4: invoice.payment_failed → past_due + payment_failed.
		if gotStatus != StatusPastDue {
			t.Fatalf("status = %q, want past_due", gotStatus)
		}
		if len(outbox.published) != 1 || outbox.published[0].Type() != TypePaymentFailed {
			t.Fatalf("published = %+v, want one payment_failed", outbox.published)
		}
		pf, ok := outbox.published[0].(PaymentFailed)
		if !ok || pf.TenantID != "tenant-uuid" || pf.InvoiceID != "in_1" || pf.AmountDue != 4990 {
			t.Fatalf("payment_failed payload = %+v", outbox.published[0])
		}
	})

	t.Run("no metadata falls back to the stored customer→tenant mapping", func(t *testing.T) {
		var findArg, updateTenant string
		repo := &mockRepo{
			findByCustomer: func(_ context.Context, customerID string) (*Subscription, error) {
				findArg = customerID
				return &Subscription{TenantID: "tenant-from-db"}, nil
			},
			updateStatus: func(_ context.Context, _ database.Tx, tenantID string, status Status) (*Subscription, error) {
				updateTenant = tenantID
				return &Subscription{TenantID: tenantID, Status: status}, nil
			},
		}
		gw := &mockGateway{verify: verifyTo(StripeEvent{
			ID: "evt_5", Type: EventPaymentFailed, TenantID: "", // metadata absent
			Invoice: &StripeInvoice{ID: "in_2", CustomerID: "cus_9", AmountDue: 100},
		})}
		outbox := &recordingOutbox{}
		uow := &fakeUOW{}
		uc := NewUseCase(repo, gw, outbox, &fakeDedup{}, uow)

		if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}

		// AC5: tenant recovered by Stripe customer, then scoped/updated to it.
		if findArg != "cus_9" || updateTenant != "tenant-from-db" || uow.scope != "tenant-from-db" {
			t.Fatalf("fallback: find=%q update=%q scope=%q", findArg, updateTenant, uow.scope)
		}
	})

	t.Run("no metadata and unknown customer retries as not-found", func(t *testing.T) {
		repo := &mockRepo{
			findByCustomer: func(context.Context, string) (*Subscription, error) {
				return nil, ErrSubscriptionNotFound
			},
		}
		gw := &mockGateway{verify: verifyTo(StripeEvent{
			ID: "evt_6", Type: EventPaymentFailed,
			Invoice: &StripeInvoice{ID: "in_3", CustomerID: "cus_x", AmountDue: 100},
		})}
		uow := &fakeUOW{}
		uc := NewUseCase(repo, gw, &recordingOutbox{}, &fakeDedup{}, uow)

		err := uc.HandleWebhook(context.Background(), nil, "")
		if !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("err = %v, want ErrSubscriptionNotFound", err)
		}
		// No write tx was opened — the tenant could not be resolved.
		if uow.called {
			t.Fatal("uow ran despite an unresolved tenant")
		}
	})
}

func TestUseCase_HandleWebhook_DedupReplayIsNoOp(t *testing.T) {
	repo := &mockRepo{
		upsert: func(context.Context, database.Tx, UpsertParams) (*Subscription, error) {
			t.Fatal("upsert ran on a replay")
			return nil, nil
		},
	}
	gw := &mockGateway{
		verify: verifyTo(StripeEvent{
			ID: "evt_dup", Type: EventSubscriptionCreated, TenantID: "tenant-uuid",
			Subscription: &StripeSubscription{ID: "sub_1", CustomerID: "cus_1", Status: "active", PriceID: "price_1"},
		}),
		resolvePlan: resolvePlanTo("pro", 50),
	}
	outbox := &recordingOutbox{}
	dedup := &fakeDedup{seen: true} // already processed
	uc := NewUseCase(repo, gw, outbox, dedup, &fakeUOW{})

	if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}

	// AC2: a replay neither upserts nor republishes.
	if len(outbox.published) != 0 {
		t.Fatalf("published = %+v, want none on replay", outbox.published)
	}
	if len(dedup.marked) != 1 {
		t.Fatalf("dedup consulted %d times, want 1", len(dedup.marked))
	}
}

func TestUseCase_HandleWebhook_MissingTenant(t *testing.T) {
	repo := &mockRepo{} // any call nil-panics — proving none happens
	gw := &mockGateway{
		verify: verifyTo(StripeEvent{
			ID: "evt_nt", Type: EventSubscriptionCreated, TenantID: "", // metadata absent
			Subscription: &StripeSubscription{ID: "sub_1", CustomerID: "cus_1", Status: "active", PriceID: "price_1"},
		}),
		resolvePlan: resolvePlanTo("pro", 50),
	}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, gw, &recordingOutbox{}, &fakeDedup{}, uow)

	// AC5: metadata absent → typed error (retry), no panic, no write.
	err := uc.HandleWebhook(context.Background(), nil, "")
	if !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
	if uow.called {
		t.Fatal("uow ran despite a missing tenant")
	}
}

func TestUseCase_HandleWebhook_UnknownAndNoOpAreAcked(t *testing.T) {
	tests := []struct {
		name  string
		event StripeEvent
	}{
		{name: "unknown type", event: StripeEvent{ID: "evt_u", Type: "customer.created"}},
		{name: "checkout.session.completed no-op", event: StripeEvent{ID: "evt_c", Type: EventCheckoutCompleted}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Empty repo: any repo/uow call would nil-panic, proving none happens.
			gw := &mockGateway{verify: verifyTo(tt.event)}
			uc := NewUseCase(&mockRepo{}, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

			// AC4: unknown → ack (nil error → 200). No effect.
			if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
				t.Fatalf("HandleWebhook(%s) = %v, want nil", tt.name, err)
			}
		})
	}
}

func TestUseCase_HandleWebhook_InvalidSignaturePropagates(t *testing.T) {
	gw := &mockGateway{verify: func([]byte, string) (StripeEvent, error) {
		return StripeEvent{}, ErrInvalidSignature
	}}
	uc := NewUseCase(&mockRepo{}, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

	// AC2: a verification failure surfaces the typed error (the edge maps it to 400).
	if err := uc.HandleWebhook(context.Background(), []byte(`{}`), "bad"); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
}

// normalizeStatus keeps only the product's v0 enum; everything else is active.
func TestNormalizeStatus(t *testing.T) {
	tests := []struct {
		raw  string
		want Status
	}{
		{"trialing", StatusTrialing},
		{"active", StatusActive},
		{"past_due", StatusPastDue},
		{"canceled", StatusCanceled},
		{"incomplete", StatusActive},
		{"unpaid", StatusActive},
		{"paused", StatusActive},
		{"", StatusActive},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := normalizeStatus(tt.raw); got != tt.want {
				t.Errorf("normalizeStatus(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
