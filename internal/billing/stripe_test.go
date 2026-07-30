package billing

import (
	"errors"
	"testing"

	"github.com/stripe/stripe-go/v86/webhook"
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
// never called by VerifyWebhook (only ResolvePlan hits the network), so an empty
// key is fine here.
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
