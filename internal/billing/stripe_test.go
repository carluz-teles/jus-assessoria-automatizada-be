package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	stripe "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/jusassessoria/platform/lib/apperr"
)

// testWebhookSecret is a Stripe-format signing secret; the test both signs and
// verifies with it, so no real Stripe instance is needed.
const testWebhookSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

// sign returns the Stripe-Signature header for body under secret, using the SDK's
// own test signer so the format matches ConstructEvent exactly.
func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret})
	return sp.Header
}

// newGateway builds a gateway with the test signing secret; the API client is
// never called by VerifyWebhook (only the checkout/portal/plans methods hit the
// network), so an empty key is fine here.
func newGateway() *stripeGateway {
	return &stripeGateway{webhookSecret: testWebhookSecret}
}

func TestStripeGateway_VerifyWebhook_Subscription(t *testing.T) {
	body := []byte(`{"id":"evt_1","object":"event","type":"customer.subscription.created","data":{"object":{` +
		`"id":"sub_1","customer":"cus_1","status":"active",` +
		`"metadata":{"tenant_id":"tenant-uuid"},` +
		`"items":{"data":[{"current_period_end":1893456000,"price":{"id":"price_1"}}]}}}}`)

	ev, err := newGateway().VerifyWebhook(body, sign(t, testWebhookSecret, body))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}

	if ev.ID != "evt_1" || ev.Type != EventSubscriptionCreated || ev.TenantID != "tenant-uuid" {
		t.Fatalf("event envelope = %+v", ev)
	}
	if ev.Subscription == nil {
		t.Fatal("Subscription is nil")
	}
	// The customer id, price id (from items[0]) and the paid-through window are all
	// lifted off the SDK shape into the neutral type.
	if ev.Subscription.ID != "sub_1" || ev.Subscription.CustomerID != "cus_1" {
		t.Fatalf("subscription ids = %+v", ev.Subscription)
	}
	if ev.Subscription.PriceID != "price_1" {
		t.Fatalf("price id = %q, want price_1", ev.Subscription.PriceID)
	}
	if ev.Subscription.Status != "active" {
		t.Fatalf("status = %q, want active", ev.Subscription.Status)
	}
	if got := ev.Subscription.CurrentPeriodEnd.Unix(); got != periodEndUnix {
		t.Fatalf("current period end unix = %d, want %d", got, periodEndUnix)
	}
}

func TestStripeGateway_VerifyWebhook_Invoice(t *testing.T) {
	body := []byte(`{"id":"evt_4","object":"event","type":"invoice.payment_failed","data":{"object":{` +
		`"id":"in_1","customer":"cus_1","amount_due":4990}}}`)

	ev, err := newGateway().VerifyWebhook(body, sign(t, testWebhookSecret, body))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}

	if ev.Type != EventPaymentFailed || ev.Invoice == nil {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Invoice.ID != "in_1" || ev.Invoice.CustomerID != "cus_1" || ev.Invoice.AmountDue != 4990 {
		t.Fatalf("invoice = %+v", ev.Invoice)
	}
	// No metadata on this invoice → TenantID empty (the use case recovers it via
	// the stored customer→tenant mapping).
	if ev.TenantID != "" {
		t.Fatalf("tenant id = %q, want empty", ev.TenantID)
	}
}

func TestStripeGateway_VerifyWebhook_UnknownTypeVerifiesButCarriesNoObject(t *testing.T) {
	body := []byte(`{"id":"evt_x","object":"event","type":"customer.created","data":{"object":{"id":"cus_1"}}}`)

	ev, err := newGateway().VerifyWebhook(body, sign(t, testWebhookSecret, body))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Type != "customer.created" || ev.Subscription != nil || ev.Invoice != nil {
		t.Fatalf("unknown event carried an object: %+v", ev)
	}
}

func TestStripeGateway_VerifyWebhook_BadSignature(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"customer.subscription.created","data":{"object":{}}}`)

	// Sign with a different secret → verification must fail with the typed error.
	_, err := newGateway().VerifyWebhook(body, sign(t, "whsec_wrongwrongwrongwrongwrongwrong0", body))
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
}

// --- write path: idempotency keys + SDK error mapping (fatia 4-B hardening) -----

// recordedCall captures the path and Idempotency-Key of one Stripe SDK call so a
// test can assert the gateway sent a deterministic key on its writes.
type recordedCall struct {
	path           string
	idempotencyKey string
}

// stubBackend is a fake stripe.Backend standing in for the network. It records the
// idempotency key of every call and, unless err is set, simulates Stripe's
// idempotent create: the SAME key returns the SAME customer id instead of a new
// one. err (optionally scoped to failPath) drives the error-mapping tests.
type stubBackend struct {
	calls    []recordedCall
	err      error  // returned by a matching call, simulating an SDK failure
	failPath string // when non-empty, err fires only for calls whose path contains it

	customersByKey map[string]string // idempotency key → customer id (idempotent store)
	nextCustomer   int
}

func newStubBackend() *stubBackend {
	return &stubBackend{customersByKey: map[string]string{}}
}

func (b *stubBackend) Call(_, path, _ string, params stripe.ParamsContainer, v stripe.LastResponseSetter) error {
	idem := ""
	if p := params.GetParams(); p != nil && p.IdempotencyKey != nil {
		idem = *p.IdempotencyKey
	}
	b.calls = append(b.calls, recordedCall{path: path, idempotencyKey: idem})

	if b.err != nil && (b.failPath == "" || strings.Contains(path, b.failPath)) {
		return b.err
	}

	switch out := v.(type) {
	case *stripe.Customer:
		id, ok := b.customersByKey[idem]
		if !ok {
			b.nextCustomer++
			id = fmt.Sprintf("cus_%d", b.nextCustomer)
			b.customersByKey[idem] = id
		}
		out.ID = id
	case *stripe.CheckoutSession:
		out.URL = "https://checkout.stripe/session"
	}
	return nil
}

func (b *stubBackend) CallStreaming(_, _, _ string, _ stripe.ParamsContainer, _ stripe.StreamingLastResponseSetter) error {
	return nil
}

func (b *stubBackend) CallRaw(_, _, _ string, _ []byte, _ *stripe.Params, _ stripe.LastResponseSetter) error {
	return nil
}

func (b *stubBackend) CallMultipart(_, _, _, _ string, _ *bytes.Buffer, _ *stripe.Params, _ stripe.LastResponseSetter) error {
	return nil
}

func (b *stubBackend) SetMaxNetworkRetries(int64) {}

// newBackedGateway builds a gateway whose SDK client routes every call through b
// (no network). White-box: the test constructs the concrete gateway directly, the
// same way newGateway() does for the webhook path.
func newBackedGateway(b stripe.Backend) *stripeGateway {
	backends := &stripe.Backends{API: b, Connect: b, Uploads: b, MeterEvents: b}
	return &stripeGateway{client: stripe.NewClient("sk_test_x", stripe.WithBackends(backends))}
}

// AC1: two EnsureCustomer calls for the SAME tenant send the same deterministic
// idempotency key, so the idempotent Stripe (the stub) returns one customer — not
// two — for the tenant; a different tenant derives a different key and customer.
func TestStripeGateway_EnsureCustomer_IdempotentKeyPerTenant(t *testing.T) {
	t.Parallel()

	b := newStubBackend()
	g := newBackedGateway(b)
	ctx := context.Background()

	id1, err := g.EnsureCustomer(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("EnsureCustomer #1: %v", err)
	}
	id2, err := g.EnsureCustomer(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("EnsureCustomer #2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("same tenant provisioned two customers: %q vs %q", id1, id2)
	}

	wantKey := customerIdempotencyKey("tenant-1")
	if len(b.calls) != 2 || b.calls[0].idempotencyKey != wantKey || b.calls[1].idempotencyKey != wantKey {
		t.Fatalf("idempotency keys = %+v, want both %q", b.calls, wantKey)
	}

	id3, err := g.EnsureCustomer(ctx, "tenant-2")
	if err != nil {
		t.Fatalf("EnsureCustomer tenant-2: %v", err)
	}
	if id3 == id1 {
		t.Fatalf("different tenants shared customer id %q", id3)
	}
	if b.calls[2].idempotencyKey == wantKey {
		t.Fatalf("tenant-2 reused tenant-1's idempotency key %q", wantKey)
	}
}

// AC2: CreateCheckoutSession sends a deterministic idempotency key per tenant+price
// — a double-click/immediate retry reuses it, while a different price is a distinct
// operation with a distinct key.
func TestStripeGateway_CreateCheckoutSession_DeterministicIdempotencyKey(t *testing.T) {
	t.Parallel()

	b := newStubBackend()
	g := newBackedGateway(b)
	ctx := context.Background()

	p := CheckoutParams{
		CustomerID: "cus_1", PriceID: "price_1", TenantID: "tenant-1",
		SuccessURL: "https://app/ok", CancelURL: "https://app/no",
	}
	if _, err := g.CreateCheckoutSession(ctx, p); err != nil {
		t.Fatalf("CreateCheckoutSession #1: %v", err)
	}
	if _, err := g.CreateCheckoutSession(ctx, p); err != nil {
		t.Fatalf("CreateCheckoutSession #2: %v", err)
	}

	wantKey := checkoutIdempotencyKey("tenant-1", "price_1")
	if len(b.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(b.calls))
	}
	for i, c := range b.calls {
		if !strings.Contains(c.path, "/checkout/sessions") {
			t.Fatalf("call %d path = %q, want the checkout sessions endpoint", i, c.path)
		}
		if c.idempotencyKey != wantKey {
			t.Fatalf("call %d key = %q, want deterministic %q", i, c.idempotencyKey, wantKey)
		}
	}

	p2 := p
	p2.PriceID = "price_2"
	if _, err := g.CreateCheckoutSession(ctx, p2); err != nil {
		t.Fatalf("CreateCheckoutSession price_2: %v", err)
	}
	if b.calls[2].idempotencyKey == wantKey {
		t.Fatalf("a different price reused key %q", wantKey)
	}
}

// AC3/AC4: every gateway call funnels its SDK error through mapStripeError — an
// invalid_request_error (bad/inactive price_id) becomes a 400 carrying Stripe's
// message, while a Stripe 5xx or a transport error stays a 503.
func TestStripeGateway_MapsSDKErrors(t *testing.T) {
	t.Parallel()

	invalid := &stripe.Error{Type: stripe.ErrorTypeInvalidRequest, Msg: "No such price: 'price_bad'"}
	apiErr := &stripe.Error{Type: stripe.ErrorTypeAPI, Msg: "server error"} // Stripe 5xx
	network := errors.New("dial tcp: connection refused")                   // transport failure

	errCases := []struct {
		name     string
		sdkErr   error
		wantKind apperr.Kind
	}{
		{"invalid_request maps to 400", invalid, apperr.KindInvalid},
		{"stripe 5xx stays 503", apiErr, apperr.KindUnavailable},
		{"network error stays 503", network, apperr.KindUnavailable},
	}

	methods := []struct {
		name string
		run  func(ctx context.Context, g *stripeGateway) error
	}{
		{"EnsureCustomer", func(ctx context.Context, g *stripeGateway) error {
			_, err := g.EnsureCustomer(ctx, "tenant-1")
			return err
		}},
		{"CreateCheckoutSession", func(ctx context.Context, g *stripeGateway) error {
			_, err := g.CreateCheckoutSession(ctx, CheckoutParams{TenantID: "tenant-1", PriceID: "price_bad"})
			return err
		}},
		{"CreatePortalSession", func(ctx context.Context, g *stripeGateway) error {
			_, err := g.CreatePortalSession(ctx, "cus_1", "https://app/back")
			return err
		}},
	}

	for _, m := range methods {
		for _, ec := range errCases {
			t.Run(m.name+"/"+ec.name, func(t *testing.T) {
				t.Parallel()

				b := newStubBackend()
				b.err = ec.sdkErr
				g := newBackedGateway(b)

				err := m.run(context.Background(), g)
				var ae *apperr.AppError
				if !errors.As(err, &ae) {
					t.Fatalf("err = %v, want *apperr.AppError", err)
				}
				if ae.Kind != ec.wantKind {
					t.Fatalf("kind = %q, want %q (err=%v)", ae.Kind, ec.wantKind, err)
				}
				// The 400 surfaces Stripe's own explanation so the caller can fix it.
				if ec.wantKind == apperr.KindInvalid && !strings.Contains(ae.Message, "No such price") {
					t.Fatalf("invalid message = %q, want it to carry Stripe's detail", ae.Message)
				}
			})
		}
	}
}

// AC5 (handler, full stack): a bad price_id fails at CreateCheckoutSession with a
// Stripe invalid_request → the endpoint answers 400, NOT the old 503. Uses the real
// gateway (stub backend) behind the real use case and HTTP handler; the customer
// create succeeds first, only the checkout call fails.
func TestHandler_Checkout_InvalidPrice_MapsTo400(t *testing.T) {
	t.Parallel()

	b := newStubBackend()
	b.err = &stripe.Error{Type: stripe.ErrorTypeInvalidRequest, Msg: "No such price: 'price_bad'"}
	b.failPath = "/checkout/sessions"
	gw := newBackedGateway(b)

	repo := &mockRepo{
		findByTenant: func(context.Context, string) (*Subscription, error) {
			return nil, ErrSubscriptionNotFound
		},
	}
	uc := NewUseCase(repo, gw, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{}, WithCheckoutConfig(checkoutCfg))
	app := newApp(uc, roleAdmin, "tenant-1")

	status, body := do(t, app, http.MethodPost, "/v1/billing/checkout", `{"price_id":"price_bad"}`, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a bad price is the client's fault, not a 503); body=%s", status, body)
	}
}
