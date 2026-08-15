package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// intPtr is a small test helper for the *int fields Plan/UpsertParams carry
// (MaxProcesses, CustomPricePerProcessCents).
func intPtr(n int) *int { return &n }

// --- test doubles -----------------------------------------------------------

// mockRepo is a hand-written Repository double: each method delegates to a func
// field, so every test injects exactly the behavior it needs. Unset fields fail
// loudly (nil call) if a test reaches a path it did not expect.
type mockRepo struct {
	upsert          func(ctx context.Context, tx database.Tx, params UpsertParams) (*Subscription, error)
	updateStatus    func(ctx context.Context, tx database.Tx, tenantID string, status Status) (*Subscription, error)
	findByCustomer  func(ctx context.Context, stripeCustomerID string) (*Subscription, error)
	findByTenant    func(ctx context.Context, tenantID string) (*Subscription, error)
	findPlanByID    func(ctx context.Context, tx database.Tx, planID string) (*Plan, error)
	findPlanByPrice func(ctx context.Context, tx database.Tx, priceID string) (*Plan, error)
	findPlanByCode  func(ctx context.Context, tx database.Tx, code string) (*Plan, error)
	listPlans       func(ctx context.Context, tx database.Tx) ([]*Plan, error)
	defaultTrial    func(ctx context.Context, tx database.Tx) (*TrialPolicy, error)
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

func (m *mockRepo) FindByTenant(ctx context.Context, tenantID string) (*Subscription, error) {
	return m.findByTenant(ctx, tenantID)
}

func (m *mockRepo) FindPlanByID(ctx context.Context, tx database.Tx, planID string) (*Plan, error) {
	return m.findPlanByID(ctx, tx, planID)
}

func (m *mockRepo) FindPlanByStripePriceID(ctx context.Context, tx database.Tx, priceID string) (*Plan, error) {
	return m.findPlanByPrice(ctx, tx, priceID)
}

func (m *mockRepo) FindPlanByCode(ctx context.Context, tx database.Tx, code string) (*Plan, error) {
	return m.findPlanByCode(ctx, tx, code)
}

func (m *mockRepo) ListActivePlans(ctx context.Context, tx database.Tx) ([]*Plan, error) {
	return m.listPlans(ctx, tx)
}

func (m *mockRepo) FindDefaultTrialPolicy(ctx context.Context, tx database.Tx) (*TrialPolicy, error) {
	return m.defaultTrial(ctx, tx)
}

// findPlanByPriceTo returns a FindPlanByStripePriceID stub yielding a fixed plan.
func findPlanByPriceTo(plan *Plan) func(context.Context, database.Tx, string) (*Plan, error) {
	return func(context.Context, database.Tx, string) (*Plan, error) { return plan, nil }
}

// mockGateway is a StripeGateway double: each method delegates to a func field so
// a test injects only the behavior its path needs (an unset field nil-panics,
// proving that path was not expected). verify returns the event a webhook test
// crafts (no real signing); the 4-B fields drive the checkout/portal/plans use
// cases. Plan resolution moved to the repo (FindPlanByStripePriceID, see
// findPlanByPriceTo) — the gateway no longer resolves plans.
type mockGateway struct {
	verify         func(payload []byte, sigHeader string) (StripeEvent, error)
	ensureCustomer func(ctx context.Context, tenantID string) (string, error)
	createCheckout func(ctx context.Context, params CheckoutParams) (string, error)
	createPortal   func(ctx context.Context, customerID, returnURL string) (string, error)
	listPlans      func(ctx context.Context) ([]StripePlan, error)
}

func (m *mockGateway) VerifyWebhook(payload []byte, sigHeader string) (StripeEvent, error) {
	return m.verify(payload, sigHeader)
}

func (m *mockGateway) EnsureCustomer(ctx context.Context, tenantID string) (string, error) {
	return m.ensureCustomer(ctx, tenantID)
}

func (m *mockGateway) CreateCheckoutSession(ctx context.Context, params CheckoutParams) (string, error) {
	return m.createCheckout(ctx, params)
}

func (m *mockGateway) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	return m.createPortal(ctx, customerID, returnURL)
}

func (m *mockGateway) ListPlans(ctx context.Context) ([]StripePlan, error) {
	return m.listPlans(ctx)
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

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	u.called = true
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

// verifyTo returns a VerifyWebhook stub yielding a fixed event.
func verifyTo(ev StripeEvent) func([]byte, string) (StripeEvent, error) {
	return func([]byte, string) (StripeEvent, error) { return ev, nil }
}

const periodEndUnix = 1893456000 // 2030-01-01T00:00:00Z, a stable non-zero window

// --- tests ------------------------------------------------------------------

func TestUseCase_HandleWebhook_SubscriptionCreated(t *testing.T) {
	periodEnd := time.Unix(periodEndUnix, 0).UTC()
	proPlan := &Plan{ID: "plan-pro", Name: "pro", MaxProcesses: intPtr(50), PricePerProcessCents: 35}
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
		findPlanByPrice: findPlanByPriceTo(proPlan),
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
	}
	outbox := &recordingOutbox{}
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, gw, outbox, dedup, uow)

	if err := uc.HandleWebhook(context.Background(), nil, ""); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}

	// AC3: upsert projects active/trialing + plan + active_process_limit via the
	// local plan (FindPlanByStripePriceID + effectiveEntitlement).
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
	enterprisePlan := &Plan{ID: "plan-enterprise", Name: "enterprise", MaxProcesses: nil, PricePerProcessCents: 15}
	repo := &mockRepo{
		upsert: func(_ context.Context, _ database.Tx, p UpsertParams) (*Subscription, error) {
			return &Subscription{TenantID: p.TenantID, Plan: p.Plan, Status: p.Status}, nil
		},
		findPlanByPrice: findPlanByPriceTo(enterprisePlan),
	}
	gw := &mockGateway{
		verify: verifyTo(StripeEvent{
			ID: "evt_2", Type: EventSubscriptionUpdated, TenantID: "tenant-uuid",
			Subscription: &StripeSubscription{ID: "sub_1", CustomerID: "cus_1", Status: "past_due", PriceID: "price_2"},
		}),
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

// --- checkout / portal / read-model use cases (fatia 4-B) -------------------

// checkoutCfg is a fixed CheckoutConfig the endpoint use cases redirect through.
var checkoutCfg = CheckoutConfig{
	SuccessURL: "https://app/success",
	CancelURL:  "https://app/cancel",
	ReturnURL:  "https://app/billing",
	TrialDays:  14,
}

// AC1: no prior subscription → a fresh Customer is provisioned, then Checkout is
// created with the config URLs/trial and the tenant stamped on the params.
func TestUseCase_StartCheckout_NoSubscription_EnsuresCustomer(t *testing.T) {
	var gotParams CheckoutParams
	var ensuredTenant string
	repo := &mockRepo{
		findByTenant: func(context.Context, string) (*Subscription, error) {
			return nil, ErrSubscriptionNotFound
		},
	}
	gw := &mockGateway{
		ensureCustomer: func(_ context.Context, tenantID string) (string, error) {
			ensuredTenant = tenantID
			return "cus_new", nil
		},
		createCheckout: func(_ context.Context, p CheckoutParams) (string, error) {
			gotParams = p
			return "https://checkout/session", nil
		},
	}
	uc := NewUseCase(repo, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{}, WithCheckoutConfig(checkoutCfg))

	url, err := uc.StartCheckout(context.Background(), "tenant-1", "price_1")
	if err != nil {
		t.Fatalf("StartCheckout: %v", err)
	}
	if url != "https://checkout/session" {
		t.Fatalf("url = %q", url)
	}
	if ensuredTenant != "tenant-1" {
		t.Fatalf("EnsureCustomer tenant = %q, want tenant-1", ensuredTenant)
	}
	if gotParams.CustomerID != "cus_new" || gotParams.PriceID != "price_1" || gotParams.TenantID != "tenant-1" {
		t.Fatalf("checkout params = %+v", gotParams)
	}
	if gotParams.SuccessURL != checkoutCfg.SuccessURL || gotParams.CancelURL != checkoutCfg.CancelURL || gotParams.TrialDays != 14 {
		t.Fatalf("checkout config not forwarded: %+v", gotParams)
	}
}

// AC1: a canceled subscription still holds its Stripe customer id — re-subscribing
// reuses it instead of provisioning a second Customer.
func TestUseCase_StartCheckout_ReusesStoredCustomer(t *testing.T) {
	var gotParams CheckoutParams
	repo := &mockRepo{
		findByTenant: func(context.Context, string) (*Subscription, error) {
			return &Subscription{TenantID: "tenant-1", StripeCustomerID: "cus_old", Status: StatusCanceled}, nil
		},
	}
	gw := &mockGateway{
		ensureCustomer: func(context.Context, string) (string, error) {
			t.Fatal("EnsureCustomer ran despite a stored customer id")
			return "", nil
		},
		createCheckout: func(_ context.Context, p CheckoutParams) (string, error) {
			gotParams = p
			return "https://checkout/session", nil
		},
	}
	uc := NewUseCase(repo, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{}, WithCheckoutConfig(checkoutCfg))

	if _, err := uc.StartCheckout(context.Background(), "tenant-1", "price_1"); err != nil {
		t.Fatalf("StartCheckout: %v", err)
	}
	if gotParams.CustomerID != "cus_old" {
		t.Fatalf("customer id = %q, want the stored cus_old", gotParams.CustomerID)
	}
}

// AC1: a live subscription (active/trialing) refuses a second checkout with 409.
func TestUseCase_StartCheckout_AlreadySubscribed_Conflicts(t *testing.T) {
	for _, status := range []Status{StatusActive, StatusTrialing} {
		t.Run(string(status), func(t *testing.T) {
			repo := &mockRepo{
				findByTenant: func(context.Context, string) (*Subscription, error) {
					return &Subscription{TenantID: "tenant-1", StripeCustomerID: "cus_1", Status: status}, nil
				},
			}
			gw := &mockGateway{
				createCheckout: func(context.Context, CheckoutParams) (string, error) {
					t.Fatal("checkout created despite a live subscription")
					return "", nil
				},
			}
			uc := NewUseCase(repo, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{}, WithCheckoutConfig(checkoutCfg))

			if _, err := uc.StartCheckout(context.Background(), "tenant-1", "price_1"); !errors.Is(err, ErrAlreadySubscribed) {
				t.Fatalf("err = %v, want ErrAlreadySubscribed", err)
			}
		})
	}
}

// AC2: a stored customer opens the portal with the config return URL.
func TestUseCase_OpenPortal_Opens(t *testing.T) {
	var gotCustomer, gotReturn string
	repo := &mockRepo{
		findByTenant: func(context.Context, string) (*Subscription, error) {
			return &Subscription{TenantID: "tenant-1", StripeCustomerID: "cus_1", Status: StatusActive}, nil
		},
	}
	gw := &mockGateway{
		createPortal: func(_ context.Context, customerID, returnURL string) (string, error) {
			gotCustomer, gotReturn = customerID, returnURL
			return "https://portal/session", nil
		},
	}
	uc := NewUseCase(repo, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{}, WithCheckoutConfig(checkoutCfg))

	url, err := uc.OpenPortal(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("OpenPortal: %v", err)
	}
	if url != "https://portal/session" || gotCustomer != "cus_1" || gotReturn != checkoutCfg.ReturnURL {
		t.Fatalf("portal url=%q customer=%q return=%q", url, gotCustomer, gotReturn)
	}
}

// AC2: a tenant that never checked out (no subscription, or a row without a
// customer id) has nothing to manage → ErrNoStripeCustomer (404), no Stripe call.
func TestUseCase_OpenPortal_NoCustomer(t *testing.T) {
	tests := []struct {
		name string
		find func(context.Context, string) (*Subscription, error)
	}{
		{
			name: "no subscription row",
			find: func(context.Context, string) (*Subscription, error) { return nil, ErrSubscriptionNotFound },
		},
		{
			name: "row without a stored customer id",
			find: func(context.Context, string) (*Subscription, error) {
				return &Subscription{TenantID: "tenant-1", Status: StatusCanceled}, nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &mockGateway{
				createPortal: func(context.Context, string, string) (string, error) {
					t.Fatal("portal opened without a customer")
					return "", nil
				},
			}
			uc := NewUseCase(&mockRepo{findByTenant: tt.find}, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{}, WithCheckoutConfig(checkoutCfg))

			if _, err := uc.OpenPortal(context.Background(), "tenant-1"); !errors.Is(err, ErrNoStripeCustomer) {
				t.Fatalf("err = %v, want ErrNoStripeCustomer", err)
			}
		})
	}
}

// AC3: GetSubscription reads the local projection — no Stripe call at request time.
func TestUseCase_GetSubscription_ReadsLocalProjection(t *testing.T) {
	want := &Subscription{TenantID: "tenant-1", Plan: "pro", Status: StatusActive, ActiveProcessLimit: 50}
	repo := &mockRepo{
		findByTenant: func(_ context.Context, tenantID string) (*Subscription, error) {
			if tenantID != "tenant-1" {
				t.Fatalf("tenant = %q, want tenant-1", tenantID)
			}
			return want, nil
		},
	}
	// An empty gateway proves no Stripe call happens on the read path.
	uc := NewUseCase(repo, &mockGateway{}, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

	got, err := uc.GetSubscription(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got != want {
		t.Fatalf("got = %+v, want %+v", got, want)
	}
}

// AC4: ListPlans passes the Stripe catalog through unchanged.
func TestUseCase_ListPlans_ReturnsCatalog(t *testing.T) {
	want := []StripePlan{{PriceID: "price_1", Name: "Pro", Amount: 4990, Interval: "month", ActiveProcessLimit: 50}}
	gw := &mockGateway{listPlans: func(context.Context) ([]StripePlan, error) { return want, nil }}
	uc := NewUseCase(&mockRepo{}, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

	got, err := uc.ListPlans(context.Background())
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(got) != 1 || got[0].PriceID != "price_1" || got[0].ActiveProcessLimit != 50 {
		t.Fatalf("plans = %+v", got)
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

// TestEffectiveEntitlement covers the sole plan+subscription+override combinator:
// no override, an override, and the no-ceiling (Enterprise) plan.
func TestEffectiveEntitlement(t *testing.T) {
	t.Parallel()

	growthPlan := &Plan{MaxProcesses: intPtr(500), PricePerProcessCents: 35}
	enterprisePlan := &Plan{MaxProcesses: nil, PricePerProcessCents: 15}

	tests := []struct {
		name          string
		sub           *Subscription
		plan          *Plan
		wantLimit     int
		wantPriceCent int
	}{
		{
			name:          "no override uses the plan's table rate and band ceiling",
			sub:           &Subscription{},
			plan:          growthPlan,
			wantLimit:     500,
			wantPriceCent: 35,
		},
		{
			name:          "a negotiated override wins over the plan's table rate",
			sub:           &Subscription{CustomPricePerProcessCents: intPtr(22)},
			plan:          growthPlan,
			wantLimit:     500,
			wantPriceCent: 22,
		},
		{
			name:          "no ceiling (Enterprise) resolves to the sentinel limit",
			sub:           &Subscription{},
			plan:          enterprisePlan,
			wantLimit:     unlimitedActiveProcesses,
			wantPriceCent: 15,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			limit, price := effectiveEntitlement(tt.sub, tt.plan)
			if limit != tt.wantLimit {
				t.Errorf("activeProcessLimit = %d, want %d", limit, tt.wantLimit)
			}
			if price != tt.wantPriceCent {
				t.Errorf("pricePerProcessCents = %d, want %d", price, tt.wantPriceCent)
			}
		})
	}
}

// --- OnTenantProvisioned (fatia 2: trial provisioning) -----------------------

var defaultTrialPolicy = &TrialPolicy{
	Name: "default", IsDefault: true, TrialDays: 14, ActiveProcessLimit: 20, Active: true,
}

var starterPlan = &Plan{ID: "plan-starter", Code: starterPlanCode, Name: "starter", MaxProcesses: intPtr(10), PricePerProcessCents: 50}

// AC: a fresh identity.tenant_provisioned starts a trialing subscription on the
// Starter plan, with the TRIAL policy's own ActiveProcessLimit (not the plan's),
// trial_ends_at = now + TrialDays, and publishes subscription_activated +
// trial_ending_soon_check in the same unit of work.
func TestUseCase_OnTenantProvisioned_ProvisionsTrial(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var gotParams UpsertParams
	repo := &mockRepo{
		defaultTrial: func(context.Context, database.Tx) (*TrialPolicy, error) { return defaultTrialPolicy, nil },
		findPlanByCode: func(_ context.Context, _ database.Tx, code string) (*Plan, error) {
			if code != starterPlanCode {
				t.Fatalf("plan code = %q, want %q", code, starterPlanCode)
			}
			return starterPlan, nil
		},
		upsert: func(_ context.Context, _ database.Tx, p UpsertParams) (*Subscription, error) {
			gotParams = p
			return &Subscription{
				ID: "sub-1", TenantID: p.TenantID, Plan: p.Plan, Status: p.Status,
				ActiveProcessLimit: p.ActiveProcessLimit, PlanID: p.PlanID, TrialEndsAt: p.TrialEndsAt,
			}, nil
		},
	}
	outbox := &recordingOutbox{}
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &mockGateway{}, outbox, dedup, uow, WithClock(func() time.Time { return now }))

	ev := TenantProvisioned{Base: events.Base{EventID: "tenant-provisioned:tenant-1"}, TenantID: "tenant-1"}
	if err := uc.OnTenantProvisioned(context.Background(), ev); err != nil {
		t.Fatalf("OnTenantProvisioned: %v", err)
	}

	if uow.scope != "tenant-1" {
		t.Fatalf("uow scope = %q, want tenant-1", uow.scope)
	}
	if gotParams.Status != StatusTrialing || gotParams.ActiveProcessLimit != defaultTrialPolicy.ActiveProcessLimit {
		t.Fatalf("upsert = %+v, want trialing with the TRIAL policy's limit (%d)", gotParams, defaultTrialPolicy.ActiveProcessLimit)
	}
	if gotParams.PlanID == nil || *gotParams.PlanID != starterPlan.ID {
		t.Fatalf("upsert plan_id = %v, want the resolved Starter plan %q", gotParams.PlanID, starterPlan.ID)
	}
	wantTrialEndsAt := now.AddDate(0, 0, defaultTrialPolicy.TrialDays)
	if gotParams.TrialEndsAt == nil || !gotParams.TrialEndsAt.Equal(wantTrialEndsAt) {
		t.Fatalf("upsert trial_ends_at = %v, want %v", gotParams.TrialEndsAt, wantTrialEndsAt)
	}

	if len(outbox.published) != 2 {
		t.Fatalf("published %d events, want 2 (subscription_activated + trial_ending_soon_check)", len(outbox.published))
	}
	if outbox.published[0].Type() != TypeSubscriptionActivated {
		t.Fatalf("event[0] type = %q, want %q", outbox.published[0].Type(), TypeSubscriptionActivated)
	}
	check, ok := outbox.published[1].(TrialEndingSoonCheck)
	if !ok || check.Type() != TypeTrialEndingSoonCheck {
		t.Fatalf("event[1] = %+v, want TrialEndingSoonCheck", outbox.published[1])
	}
	if check.SubscriptionID != "sub-1" || !check.TrialEndsAt.Equal(wantTrialEndsAt) {
		t.Fatalf("trial_ending_soon_check = %+v, want subscription sub-1 / trial_ends_at %v", check, wantTrialEndsAt)
	}

	if len(dedup.marked) != 1 || dedup.marked[0] != ev.EventID {
		t.Fatalf("dedup marked = %v, want [%s]", dedup.marked, ev.EventID)
	}
}

// AC: a replay (dedup reports seen) neither queries the catalog nor upserts —
// the trial is provisioned exactly once, no matter how many times identity
// redelivers tenant_provisioned for the same tenant.
func TestUseCase_OnTenantProvisioned_DedupReplayIsNoOp(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{
		defaultTrial: func(context.Context, database.Tx) (*TrialPolicy, error) {
			t.Fatal("trial policy looked up on a replay")
			return nil, nil
		},
		upsert: func(context.Context, database.Tx, UpsertParams) (*Subscription, error) {
			t.Fatal("upsert ran on a replay")
			return nil, nil
		},
	}
	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, &mockGateway{}, outbox, &fakeDedup{seen: true}, &fakeUOW{})

	ev := TenantProvisioned{Base: events.Base{EventID: "tenant-provisioned:tenant-1"}, TenantID: "tenant-1"}
	if err := uc.OnTenantProvisioned(context.Background(), ev); err != nil {
		t.Fatalf("OnTenantProvisioned: %v", err)
	}
	if len(outbox.published) != 0 {
		t.Fatalf("published = %+v, want none on replay", outbox.published)
	}
}

// AC: no default trial policy configured fails LOUD (ErrNoDefaultTrialPolicy) —
// an operational misconfiguration, never a silent "no trial" fallback — and
// writes nothing (no plan lookup, no upsert, no publish).
func TestUseCase_OnTenantProvisioned_NoDefaultTrialPolicy_FailsLoud(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{
		defaultTrial: func(context.Context, database.Tx) (*TrialPolicy, error) { return nil, ErrNoDefaultTrialPolicy },
		findPlanByCode: func(context.Context, database.Tx, string) (*Plan, error) {
			t.Fatal("plan looked up despite no default trial policy")
			return nil, nil
		},
		upsert: func(context.Context, database.Tx, UpsertParams) (*Subscription, error) {
			t.Fatal("upsert ran despite no default trial policy")
			return nil, nil
		},
	}
	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, &mockGateway{}, outbox, &fakeDedup{}, &fakeUOW{})

	ev := TenantProvisioned{Base: events.Base{EventID: "tenant-provisioned:tenant-1"}, TenantID: "tenant-1"}
	err := uc.OnTenantProvisioned(context.Background(), ev)
	if !errors.Is(err, ErrNoDefaultTrialPolicy) {
		t.Fatalf("err = %v, want ErrNoDefaultTrialPolicy", err)
	}
	if len(outbox.published) != 0 {
		t.Fatalf("published = %+v, want none", outbox.published)
	}
}

// AC: the Starter plan referenced by starterPlanCode missing from the catalog
// (ErrPlanNotFound) fails LOUD too — same "misconfiguration, not a silent
// fallback" contract as the missing default trial policy above — and writes
// nothing (no upsert, no publish) despite the trial policy having resolved
// fine.
func TestUseCase_OnTenantProvisioned_StarterPlanNotFound_FailsLoud(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{
		defaultTrial: func(context.Context, database.Tx) (*TrialPolicy, error) { return defaultTrialPolicy, nil },
		findPlanByCode: func(_ context.Context, _ database.Tx, code string) (*Plan, error) {
			if code != starterPlanCode {
				t.Fatalf("plan code = %q, want %q", code, starterPlanCode)
			}
			return nil, ErrPlanNotFound
		},
		upsert: func(context.Context, database.Tx, UpsertParams) (*Subscription, error) {
			t.Fatal("upsert ran despite the Starter plan being unresolved")
			return nil, nil
		},
	}
	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, &mockGateway{}, outbox, &fakeDedup{}, &fakeUOW{})

	ev := TenantProvisioned{Base: events.Base{EventID: "tenant-provisioned:tenant-1"}, TenantID: "tenant-1"}
	err := uc.OnTenantProvisioned(context.Background(), ev)
	if !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("err = %v, want ErrPlanNotFound", err)
	}
	if len(outbox.published) != 0 {
		t.Fatalf("published = %+v, want none", outbox.published)
	}
}

// --- OnTrialEndingSoonCheck (fatia 2: the trial's scheduled re-check) --------

// AC: a subscription still trialing, inside the lead window, re-checked at fire
// time publishes the REAL trial_ending_soon with the CURRENT trial_ends_at/days
// left — never the stale values the check's own payload carried.
func TestUseCase_OnTrialEndingSoonCheck_StillTrialingPublishes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	currentTrialEndsAt := now.AddDate(0, 0, 2) // exactly at the lead window
	repo := &mockRepo{
		findByTenant: func(context.Context, string) (*Subscription, error) {
			return &Subscription{TenantID: "tenant-1", Status: StatusTrialing, TrialEndsAt: &currentTrialEndsAt}, nil
		},
	}
	outbox := &recordingOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &mockGateway{}, outbox, &fakeDedup{}, uow, WithClock(func() time.Time { return now }))

	ev := TrialEndingSoonCheck{
		Base: events.Base{EventID: "trial-ending-soon:sub-1"}, TenantID: "tenant-1", SubscriptionID: "sub-1",
		TrialEndsAt: now.AddDate(0, 0, 1), // stale — the fire handler must re-read, not trust this
	}
	if err := uc.OnTrialEndingSoonCheck(context.Background(), ev); err != nil {
		t.Fatalf("OnTrialEndingSoonCheck: %v", err)
	}

	if len(outbox.published) != 1 {
		t.Fatalf("published %d events, want 1", len(outbox.published))
	}
	got, ok := outbox.published[0].(TrialEndingSoon)
	if !ok {
		t.Fatalf("published[0] = %+v, want TrialEndingSoon", outbox.published[0])
	}
	if got.TenantID != "tenant-1" || got.DaysLeft != 2 || !got.TrialEndsAt.Equal(currentTrialEndsAt) {
		t.Fatalf("trial_ending_soon = %+v, want tenant-1/2 days/%v (the RE-READ state)", got, currentTrialEndsAt)
	}
}

// AC: a subscription no longer trialing (converted or canceled between
// provisioning and the mark's ETA) is a silent no-op — never a stale aviso.
func TestUseCase_OnTrialEndingSoonCheck_NoLongerTrialing_NoOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sub  *Subscription
	}{
		{name: "converted to active", sub: &Subscription{TenantID: "tenant-1", Status: StatusActive}},
		{name: "canceled", sub: &Subscription{TenantID: "tenant-1", Status: StatusCanceled}},
		{name: "trialing with no trial_ends_at (defensive)", sub: &Subscription{TenantID: "tenant-1", Status: StatusTrialing}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := &mockRepo{findByTenant: func(context.Context, string) (*Subscription, error) { return tt.sub, nil }}
			outbox := &recordingOutbox{}
			uc := NewUseCase(repo, &mockGateway{}, outbox, &fakeDedup{}, &fakeUOW{})

			ev := TrialEndingSoonCheck{Base: events.Base{EventID: "trial-ending-soon:sub-1"}, TenantID: "tenant-1"}
			if err := uc.OnTrialEndingSoonCheck(context.Background(), ev); err != nil {
				t.Fatalf("OnTrialEndingSoonCheck: %v", err)
			}
			if len(outbox.published) != 0 {
				t.Fatalf("published = %+v, want none", outbox.published)
			}
		})
	}
}

// AC: a replay (dedup reports seen) never re-reads the subscription or republishes.
func TestUseCase_OnTrialEndingSoonCheck_DedupReplayIsNoOp(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{
		findByTenant: func(context.Context, string) (*Subscription, error) {
			t.Fatal("subscription read on a replay")
			return nil, nil
		},
	}
	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, &mockGateway{}, outbox, &fakeDedup{seen: true}, &fakeUOW{})

	ev := TrialEndingSoonCheck{Base: events.Base{EventID: "trial-ending-soon:sub-1"}, TenantID: "tenant-1"}
	if err := uc.OnTrialEndingSoonCheck(context.Background(), ev); err != nil {
		t.Fatalf("OnTrialEndingSoonCheck: %v", err)
	}
	if len(outbox.published) != 0 {
		t.Fatalf("published = %+v, want none on replay", outbox.published)
	}
}

// AC: applySubscription resolves the plan LOCALLY (repo, inside the tx), never
// via the gateway; a price id with no matching local plan fails loud with
// ErrPlanUnresolved rather than projecting a zeroed entitlement.
func TestUseCase_HandleWebhook_SubscriptionCreated_PlanUnresolved(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{
		upsert: func(context.Context, database.Tx, UpsertParams) (*Subscription, error) {
			t.Fatal("upsert ran despite an unresolved plan")
			return nil, nil
		},
		findPlanByPrice: func(context.Context, database.Tx, string) (*Plan, error) {
			return nil, ErrPlanNotFound
		},
	}
	gw := &mockGateway{verify: verifyTo(StripeEvent{
		ID: "evt_unresolved", Type: EventSubscriptionCreated, TenantID: "tenant-uuid",
		Subscription: &StripeSubscription{ID: "sub_1", CustomerID: "cus_1", Status: "active", PriceID: "price_unknown"},
	})}
	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, gw, outbox, &fakeDedup{}, &fakeUOW{})

	err := uc.HandleWebhook(context.Background(), nil, "")
	if !errors.Is(err, ErrPlanUnresolved) {
		t.Fatalf("err = %v, want ErrPlanUnresolved", err)
	}
	if len(outbox.published) != 0 {
		t.Fatalf("published = %+v, want none", outbox.published)
	}
}
