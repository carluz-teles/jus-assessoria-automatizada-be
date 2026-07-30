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
	findByTenant   func(ctx context.Context, tenantID string) (*Subscription, error)
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

// mockGateway is a StripeGateway double: each method delegates to a func field so
// a test injects only the behavior its path needs (an unset field nil-panics,
// proving that path was not expected). verify returns the event a webhook test
// crafts (no real signing), resolvePlan the plan; the 4-B fields drive the
// checkout/portal/plans use cases.
type mockGateway struct {
	verify         func(payload []byte, sigHeader string) (StripeEvent, error)
	resolvePlan    func(ctx context.Context, priceID string) (string, int, error)
	ensureCustomer func(ctx context.Context, tenantID string) (string, error)
	createCheckout func(ctx context.Context, params CheckoutParams) (string, error)
	createPortal   func(ctx context.Context, customerID, returnURL string) (string, error)
	listPlans      func(ctx context.Context) ([]Plan, error)
}

func (m *mockGateway) VerifyWebhook(payload []byte, sigHeader string) (StripeEvent, error) {
	return m.verify(payload, sigHeader)
}

func (m *mockGateway) ResolvePlan(ctx context.Context, priceID string) (string, int, error) {
	return m.resolvePlan(ctx, priceID)
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

func (m *mockGateway) ListPlans(ctx context.Context) ([]Plan, error) {
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
	want := []Plan{{PriceID: "price_1", Name: "Pro", Amount: 4990, Interval: "month", ActiveProcessLimit: 50}}
	gw := &mockGateway{listPlans: func(context.Context) ([]Plan, error) { return want, nil }}
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
