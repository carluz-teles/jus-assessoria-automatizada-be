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

	t.Run("organizationMembership.created provisions the user + membership with the mapped role", func(t *testing.T) {
		var got struct {
			clerkUserID, tenantID, email, name string
			role                               Role
		}
		var gotMembership struct {
			clerkMembershipID string
			role              Role
		}
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				return &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}, nil
			},
			findUser: func(context.Context, string) (*AppUser, error) { return nil, ErrUserNotFound },
			upsertUser: func(_ context.Context, _ database.Tx, clerkUserID, tenantID, email, name, _ string, role Role) (*AppUser, error) {
				got.clerkUserID, got.tenantID, got.email, got.name, got.role = clerkUserID, tenantID, email, name, role
				return &AppUser{ID: "u-1", ClerkUserID: clerkUserID, TenantID: tenantID, Role: role}, nil
			},
			upsertMembership: func(_ context.Context, _ database.Tx, _, _, clerkMembershipID string, role Role) (*Membership, bool, error) {
				gotMembership.clerkMembershipID, gotMembership.role = clerkMembershipID, role
				return &Membership{ID: "m-1", AppUserID: "u-1", Role: role, Status: MembershipActive}, true, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organizationMembership.created","data":{` +
			`"id":"orgmem_1",` +
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
		// The membership carries the clerk membership id (data.id) and the mapped role.
		if gotMembership.clerkMembershipID != "orgmem_1" || gotMembership.role != RoleAdmin {
			t.Fatalf("UpsertMembership args = %+v", gotMembership)
		}
	})

	t.Run("organizationMembership.deleted soft-removes the membership by its clerk id", func(t *testing.T) {
		var got struct{ clerkMembershipID string }
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				return &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}, nil
			},
			softRemoveMember: func(_ context.Context, _ database.Tx, clerkMembershipID string) (*Membership, bool, error) {
				got.clerkMembershipID = clerkMembershipID
				return &Membership{ID: "m-1", TenantID: "tenant-uuid", AppUserID: "u-1", Status: MembershipRemoved}, true, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organizationMembership.deleted","data":{` +
			`"id":"orgmem_1",` +
			`"organization":{"id":"org_abc","name":"Escritório"},` +
			`"public_user_data":{"user_id":"user_xyz"}}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got.clerkMembershipID != "orgmem_1" {
			t.Fatalf("SoftRemoveMembership clerk id = %q, want orgmem_1", got.clerkMembershipID)
		}
	})

	t.Run("organizationMembership.updated syncs the role by clerk id with the mapped role", func(t *testing.T) {
		var got struct {
			clerkMembershipID string
			role              Role
		}
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				return &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}, nil
			},
			updateMemberRole: func(_ context.Context, _ database.Tx, clerkMembershipID string, role Role) error {
				got.clerkMembershipID, got.role = clerkMembershipID, role
				return nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organizationMembership.updated","data":{` +
			`"id":"orgmem_1",` +
			`"role":"org:admin",` +
			`"organization":{"id":"org_abc","name":"Escritório"},` +
			`"public_user_data":{"user_id":"user_xyz"}}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got.clerkMembershipID != "orgmem_1" || got.role != RoleAdmin {
			t.Fatalf("UpdateMembershipRole args = %+v (org:admin maps to ADMIN)", got)
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

	t.Run("user.updated falls back to unsafe_metadata.phone when no phone_number is set", func(t *testing.T) {
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

		// The onboarding wizard stores the phone in unsafe_metadata (a verified
		// phone_number needs an SMS round-trip). With no phone_numbers, primaryPhone
		// must fall back to it so the phone still reaches the upsert.
		body := []byte(`{"type":"user.updated","data":{"id":"user_xyz","first_name":"Ana","last_name":"Souza",` +
			`"primary_email_address_id":"idn_1","email_addresses":[{"id":"idn_1","email_address":"ana@new.com"}],` +
			`"unsafe_metadata":{"phone":"+5511987654321"}}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if gotPhone != "+5511987654321" {
			t.Fatalf("phone = %q, want the unsafe_metadata fallback", gotPhone)
		}
	})

	t.Run("organization.updated reprojects the tenant name", func(t *testing.T) {
		var got struct{ clerkOrgID, name string }
		repo := &mockRepo{
			upsertTenant: func(_ context.Context, _ database.Tx, clerkOrgID, name string) (*Tenant, error) {
				got.clerkOrgID, got.name = clerkOrgID, name
				return &Tenant{ID: "t-1", ClerkOrgID: clerkOrgID, Name: name}, nil
			},
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organization.updated","data":{"id":"org_abc","name":"Escritório Renomeado"}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got.clerkOrgID != "org_abc" || got.name != "Escritório Renomeado" {
			t.Fatalf("UpsertTenant args = %+v", got)
		}
	})

	t.Run("organization.deleted acks without deleting local data", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				return &Tenant{ID: "t-1", ClerkOrgID: "org_abc"}, nil
			},
			// Any write call would nil-panic — proving OnOrganizationDeleted never
			// touches the tenant row (it only logs).
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organization.deleted","data":{"id":"org_abc"}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("organization.deleted for an unknown clerk org is a silent no-op", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return nil, ErrTenantNotFound },
		}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organization.deleted","data":{"id":"org_never_seen"}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	// FLO-74: organizationInvitation.accepted is a deliberate ack — Clerk fires
	// organizationMembership.created for the same acceptance, which already does
	// the real work. Any repo call here would nil-panic, proving this case never
	// reaches the use case.
	t.Run("organizationInvitation.accepted is acknowledged without touching the repo", func(t *testing.T) {
		repo := &mockRepo{}
		h := NewWebhookHandler(testWebhookSecret, NewUseCase(repo, noopOutbox{}, &fakeUOW{}))

		body := []byte(`{"type":"organizationInvitation.accepted","data":{"id":"orginv_1"}}`)
		resp := doWebhook(t, h, signedRequest(t, testWebhookSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
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
