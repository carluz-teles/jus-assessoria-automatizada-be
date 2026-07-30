package identity

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	svix "github.com/svix/svix-webhooks/go"

	"github.com/jusassessoria/platform/lib/database"
)

// testWebhookSecret is svix's documented example signing secret (whsec_ + base64).
// The test both signs and verifies with it, so no real Clerk instance is needed.
const testWebhookSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

// signedRequest builds a POST carrying body plus valid svix-id/-timestamp/
// -signature headers computed over body with secret — exactly what Clerk sends.
func signedRequest(t *testing.T, secret string, body []byte) *http.Request {
	t.Helper()

	wh, err := svix.NewWebhook(secret)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}

	now := time.Now()
	sig, err := wh.Sign("msg_test", now, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/clerk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("svix-id", "msg_test")
	req.Header.Set("svix-timestamp", strconv.FormatInt(now.Unix(), 10))
	req.Header.Set("svix-signature", sig)
	return req
}

func TestWebhookHandler_Handle(t *testing.T) {
	t.Run("organization.created provisions the tenant", func(t *testing.T) {
		var got struct{ clerkOrgID, name string }
		repo := &mockRepo{
			upsertTenant: func(_ context.Context, _ database.Tx, clerkOrgID, name string) (*Tenant, error) {
				got.clerkOrgID, got.name = clerkOrgID, name
				return &Tenant{ID: "t-1", ClerkOrgID: clerkOrgID, Name: name}, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organization.created","data":{"id":"org_abc","name":"Escritório"}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got.clerkOrgID != "org_abc" || got.name != "Escritório" {
			t.Fatalf("UpsertTenant args = %+v", got)
		}
	})

	t.Run("organizationMembership.created provisions the user with the mapped role", func(t *testing.T) {
		var got struct {
			clerkUserID, tenantID, email, name string
			role                               Role
		}
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				return &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}, nil
			},
			upsertUser: func(_ context.Context, _ database.Tx, clerkUserID, tenantID, email, name, _ string, role Role) (*AppUser, error) {
				got.clerkUserID, got.tenantID, got.email, got.name, got.role = clerkUserID, tenantID, email, name, role
				return &AppUser{ID: "u-1", ClerkUserID: clerkUserID}, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organizationMembership.created","data":{` +
			`"role":"org:admin",` +
			`"organization":{"id":"org_abc","name":"Escritório"},` +
			`"public_user_data":{"user_id":"user_xyz","identifier":"ana@b.com","first_name":"Ana","last_name":"Lima"}}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got.clerkUserID != "user_xyz" || got.tenantID != "tenant-uuid" || got.email != "ana@b.com" || got.name != "Ana Lima" {
			t.Fatalf("UpsertUser args = %+v", got)
		}
		if got.role != RoleAdmin {
			t.Fatalf("role = %q, want ADMIN (org:admin maps to admin)", got.role)
		}
	})

	t.Run("user.updated resyncs email, name and the primary phone but preserves the role", func(t *testing.T) {
		existing := &AppUser{ID: "u-1", ClerkUserID: "user_xyz", TenantID: "tenant-uuid", Role: RoleLawyer}
		var got struct {
			email, name, phone string
			role               Role
		}
		repo := &mockRepo{
			findUser: func(context.Context, string) (*AppUser, error) { return existing, nil },
			upsertUser: func(_ context.Context, _ database.Tx, _, _, email, name, phone string, role Role) (*AppUser, error) {
				got.email, got.name, got.phone, got.role = email, name, phone, role
				return existing, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		// Two phone numbers; primary_phone_number_id selects the second, proving
		// the primary (not the first listed) is the one persisted.
		body := []byte(`{"type":"user.updated","data":{"id":"user_xyz","first_name":"Ana","last_name":"Souza",` +
			`"primary_email_address_id":"idn_1","email_addresses":[{"id":"idn_1","email_address":"ana@new.com"}],` +
			`"primary_phone_number_id":"phn_2","phone_numbers":[` +
			`{"id":"phn_1","phone_number":"+5511111111111"},{"id":"phn_2","phone_number":"+5511987654321"}]}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got.email != "ana@new.com" || got.name != "Ana Souza" {
			t.Fatalf("resynced fields = %+v", got)
		}
		// AC2: the primary phone number reaches the upsert.
		if got.phone != "+5511987654321" {
			t.Fatalf("phone = %q, want the primary number", got.phone)
		}
		if got.role != RoleLawyer {
			t.Fatalf("role = %q, want LAWYER preserved", got.role)
		}
	})

	t.Run("user.updated without any phone forwards empty without panicking", func(t *testing.T) {
		existing := &AppUser{ID: "u-1", ClerkUserID: "user_xyz", TenantID: "tenant-uuid", Role: RoleLawyer}
		var gotPhone string
		repo := &mockRepo{
			findUser: func(context.Context, string) (*AppUser, error) { return existing, nil },
			upsertUser: func(_ context.Context, _ database.Tx, _, _, _, _, phone string, _ Role) (*AppUser, error) {
				gotPhone = phone
				return existing, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		// AC3: no phone_numbers array at all — the handler must not panic and must
		// forward empty so the upsert's COALESCE leaves the stored phone as-is.
		body := []byte(`{"type":"user.updated","data":{"id":"user_xyz","first_name":"Ana","last_name":"Souza",` +
			`"primary_email_address_id":"idn_1","email_addresses":[{"id":"idn_1","email_address":"ana@new.com"}]}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if gotPhone != "" {
			t.Fatalf("phone = %q, want empty when the payload carries none", gotPhone)
		}
	})

	t.Run("unknown event type is acknowledged and ignored", func(t *testing.T) {
		repo := &mockRepo{} // any repo call would nil-panic — proving none happens
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"session.created","data":{"id":"sess_1"}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("tampered signature is rejected before provisioning", func(t *testing.T) {
		repo := &mockRepo{
			upsertTenant: func(context.Context, database.Tx, string, string) (*Tenant, error) {
				t.Fatal("provisioned despite an invalid signature")
				return nil, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organization.created","data":{"id":"org_abc","name":"Escritório"}}`)
		req := signedRequest(t, testWebhookSecret, body)
		req.Header.Set("svix-signature", "v1,deadbeefdeadbeefdeadbeefdeadbeef") // corrupt it
		resp := doWebhook(t, h, req)

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// doWebhook runs req through a fiber app mounting the handler and returns the
// response. It mounts via Register (not a hand-listed route) so this exercises
// the same entry point the api composes with.
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
