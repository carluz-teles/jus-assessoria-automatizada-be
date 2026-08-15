package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/database"
)

// doWebhook runs req through a fiber app mounting the handler via Register (not a
// hand-listed route) so the test exercises the same entry point the api composes.
func doWebhook(t *testing.T, h *WebhookHandler, req *http.Request) *http.Response {
	t.Helper()
	app := fiber.New()
	h.Register(app)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// postStripe builds a POST to the webhook route carrying body (the raw bytes the
// signature is computed over in production; here the gateway is mocked).
func postStripe(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	return req
}

// okRepo satisfies every write with a benign row, so a well-formed event always
// reaches 200 — the HTTP tests care about status, not the projected fields.
func okRepo() *mockRepo {
	sub := &Subscription{TenantID: "tenant-uuid", Status: StatusActive}
	plan := &Plan{ID: "plan-pro", Name: "pro", MaxProcesses: intPtr(50), PricePerProcessCents: 35}
	return &mockRepo{
		upsert:          func(context.Context, database.Tx, UpsertParams) (*Subscription, error) { return sub, nil },
		updateStatus:    func(context.Context, database.Tx, string, Status) (*Subscription, error) { return sub, nil },
		findByCustomer:  func(context.Context, string) (*Subscription, error) { return sub, nil },
		findPlanByPrice: findPlanByPriceTo(plan),
	}
}

// TestWebhookHandler_Handle drives the whole public path (Register → Handle →
// use case) with the Stripe gateway mocked, so each event type is exercised
// without real signing. It is table-driven per AC6: every event, an invalid
// signature, an unknown type and a replay.
func TestWebhookHandler_Handle(t *testing.T) {
	activeSub := &StripeSubscription{ID: "sub_1", CustomerID: "cus_1", Status: "active", PriceID: "price_1"}

	tests := []struct {
		name       string
		verify     func([]byte, string) (StripeEvent, error)
		seen       bool // dedup: model a replay
		wantStatus int
	}{
		{
			name:       "customer.subscription.created acks 200",
			verify:     verifyTo(StripeEvent{ID: "evt_1", Type: EventSubscriptionCreated, TenantID: "tenant-uuid", Subscription: activeSub}),
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "customer.subscription.updated acks 200",
			verify:     verifyTo(StripeEvent{ID: "evt_2", Type: EventSubscriptionUpdated, TenantID: "tenant-uuid", Subscription: activeSub}),
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "customer.subscription.deleted acks 200",
			verify:     verifyTo(StripeEvent{ID: "evt_3", Type: EventSubscriptionDeleted, TenantID: "tenant-uuid", Subscription: activeSub}),
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "invoice.payment_failed acks 200",
			verify:     verifyTo(StripeEvent{ID: "evt_4", Type: EventPaymentFailed, TenantID: "tenant-uuid", Invoice: &StripeInvoice{ID: "in_1", CustomerID: "cus_1", AmountDue: 100}}),
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "checkout.session.completed no-op acks 200",
			verify:     verifyTo(StripeEvent{ID: "evt_5", Type: EventCheckoutCompleted}),
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "unknown type acks 200",
			verify:     verifyTo(StripeEvent{ID: "evt_6", Type: "customer.created"}),
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "replay acks 200 without writing",
			verify:     verifyTo(StripeEvent{ID: "evt_1", Type: EventSubscriptionCreated, TenantID: "tenant-uuid", Subscription: activeSub}),
			seen:       true,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "invalid signature rejects 400",
			verify:     func([]byte, string) (StripeEvent, error) { return StripeEvent{}, ErrInvalidSignature },
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing tenant retries as 400",
			verify:     verifyTo(StripeEvent{ID: "evt_7", Type: EventSubscriptionCreated, TenantID: "", Subscription: activeSub}),
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &mockGateway{verify: tt.verify}
			uc := NewUseCase(okRepo(), gw, &recordingOutbox{}, &fakeDedup{seen: tt.seen}, &fakeUOW{})
			h := NewWebhookHandler(uc)

			resp := doWebhook(t, h, postStripe(`{"id":"evt","type":"x"}`))
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}
