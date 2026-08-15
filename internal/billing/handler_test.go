package billing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// --- HTTP test doubles -------------------------------------------------------

// stubVerifier accepts any bearer token — Auth's job here is only to gate on the
// token's presence, not to test Clerk.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

// stubResolver returns a principal with the configured role and tenant, standing
// in for the identity slice's resolver.
type stubResolver struct {
	role   string
	tenant string
}

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: "u-1", TenantID: r.tenant, Role: r.role}, nil
}

// fakeHandlerUC records what the handler passed and returns canned results (or an
// injected error, to drive the 409/404 branches). It satisfies handlerUC — the
// narrow port the Handler depends on — so the HTTP tests never touch Stripe.
type fakeHandlerUC struct {
	checkoutURL string
	portalURL   string
	sub         *Subscription
	plans       []StripePlan
	err         error
	// now backs Now() — the trial_status/days_until_trial_end tests pin it;
	// every other test leaves it zero, which falls back to time.Now() (the exact
	// value never matters when sub.Status is not trialing).
	now time.Time

	gotTenantID string
	gotPriceID  string
}

func (f *fakeHandlerUC) Now() time.Time {
	if f.now.IsZero() {
		return time.Now()
	}
	return f.now
}

func (f *fakeHandlerUC) StartCheckout(_ context.Context, tenantID, priceID string) (string, error) {
	f.gotTenantID, f.gotPriceID = tenantID, priceID
	return f.checkoutURL, f.err
}

func (f *fakeHandlerUC) OpenPortal(_ context.Context, tenantID string) (string, error) {
	f.gotTenantID = tenantID
	return f.portalURL, f.err
}

func (f *fakeHandlerUC) GetSubscription(_ context.Context, tenantID string) (*Subscription, error) {
	f.gotTenantID = tenantID
	return f.sub, f.err
}

func (f *fakeHandlerUC) ListPlans(context.Context) ([]StripePlan, error) {
	return f.plans, f.err
}

// newApp builds an app whose /v1 group mirrors production: Auth resolves a
// principal with the given role/tenant, then the billing routes mount under it.
func newApp(uc handlerUC, role, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: role, tenant: tenant}))
	NewHandler(uc).RegisterV1(v1)
	return app
}

// do drives one request through app, returning status and raw body.
func do(t *testing.T, app *fiber.App, method, path, body, bearer string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set(fiber.HeaderAuthorization, "Bearer "+bearer)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// --- tests -------------------------------------------------------------------

// AC5: no bearer token → 401 at the auth boundary, the handler never runs.
func TestHandler_Checkout_NoToken_401(t *testing.T) {
	t.Parallel()

	app := newApp(&fakeHandlerUC{}, roleAdmin, "tenant-1")
	status, _ := do(t, app, http.MethodPost, "/v1/billing/checkout", `{"price_id":"price_1"}`, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// AC5: every billing route is ADMIN-only — a LAWYER gets 403, use case untouched.
func TestHandler_Routes_Lawyer_403(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, method, path, body string
	}{
		{"checkout", http.MethodPost, "/v1/billing/checkout", `{"price_id":"price_1"}`},
		{"portal", http.MethodPost, "/v1/billing/portal", ""},
		{"subscription", http.MethodGet, "/v1/billing/subscription", ""},
		{"plans", http.MethodGet, "/v1/billing/plans", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			uc := &fakeHandlerUC{}
			app := newApp(uc, "LAWYER", "tenant-1")
			status, _ := do(t, app, tt.method, tt.path, tt.body, "jwt")
			if status != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403", tt.name, status)
			}
			if uc.gotTenantID != "" {
				t.Fatalf("%s: use case ran despite a 403 (tenant=%q)", tt.name, uc.gotTenantID)
			}
		})
	}
}

// AC1 (handler): an ADMIN with a valid body → 200 + checkout_url, tenant taken
// from the principal (not the body), price_id forwarded from the body.
func TestHandler_Checkout_Admin_200(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{checkoutURL: "https://checkout.stripe/x"}
	app := newApp(uc, roleAdmin, "tenant-42")

	status, body := do(t, app, http.MethodPost, "/v1/billing/checkout", `{"price_id":"price_9"}`, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotTenantID != "tenant-42" {
		t.Fatalf("tenant passed to uc = %q, want tenant-42 (from principal)", uc.gotTenantID)
	}
	if uc.gotPriceID != "price_9" {
		t.Fatalf("price_id passed to uc = %q, want price_9", uc.gotPriceID)
	}
	if !strings.Contains(body, `"checkout_url":"https://checkout.stripe/x"`) {
		t.Fatalf("unexpected checkout body: %s", body)
	}
}

// AC1: an empty price_id is a 400 at the edge; the use case is never called.
func TestHandler_Checkout_EmptyPriceID_400(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newApp(uc, roleAdmin, "tenant-1")

	status, body := do(t, app, http.MethodPost, "/v1/billing/checkout", `{"price_id":""}`, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
	if uc.gotPriceID != "" || uc.gotTenantID != "" {
		t.Fatalf("use case ran on an invalid body (tenant=%q price=%q)", uc.gotTenantID, uc.gotPriceID)
	}
}

// AC1: a tenant that already subscribes surfaces ErrAlreadySubscribed as a 409.
func TestHandler_Checkout_AlreadySubscribed_409(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{err: ErrAlreadySubscribed}
	app := newApp(uc, roleAdmin, "tenant-1")

	status, _ := do(t, app, http.MethodPost, "/v1/billing/checkout", `{"price_id":"price_1"}`, "jwt")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
}

// AC2 (handler): an ADMIN → 200 + portal_url, tenant from the principal.
func TestHandler_Portal_Admin_200(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{portalURL: "https://portal.stripe/y"}
	app := newApp(uc, roleAdmin, "tenant-7")

	status, body := do(t, app, http.MethodPost, "/v1/billing/portal", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotTenantID != "tenant-7" {
		t.Fatalf("tenant passed to uc = %q, want tenant-7", uc.gotTenantID)
	}
	if !strings.Contains(body, `"portal_url":"https://portal.stripe/y"`) {
		t.Fatalf("unexpected portal body: %s", body)
	}
}

// AC2: a tenant with no Stripe customer surfaces ErrNoStripeCustomer as a 404.
func TestHandler_Portal_NoCustomer_404(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{err: ErrNoStripeCustomer}
	app := newApp(uc, roleAdmin, "tenant-1")

	status, _ := do(t, app, http.MethodPost, "/v1/billing/portal", "", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// AC3 (handler): GET subscription → 200 with the read model (plan/status/
// current_period_end/active_process_limit), tenant from the principal.
func TestHandler_Subscription_Admin_200(t *testing.T) {
	t.Parallel()

	periodEnd := time.Unix(periodEndUnix, 0).UTC()
	uc := &fakeHandlerUC{sub: &Subscription{
		Plan: "pro", Status: StatusActive, CurrentPeriodEnd: &periodEnd, ActiveProcessLimit: 50,
	}}
	app := newApp(uc, roleAdmin, "tenant-3")

	status, body := do(t, app, http.MethodGet, "/v1/billing/subscription", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotTenantID != "tenant-3" {
		t.Fatalf("tenant passed to uc = %q, want tenant-3", uc.gotTenantID)
	}
	if !strings.Contains(body, `"plan":"pro"`) || !strings.Contains(body, `"active_process_limit":50`) {
		t.Fatalf("unexpected subscription body: %s", body)
	}
}

// AC (fatia 2): the subscription view's trial_status/days_until_trial_end cover
// all four states — computed against the use case's Now(), never wall-clock time.
func TestHandler_Subscription_TrialStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		sub            *Subscription
		wantTrialField string
		wantDaysField  string // "" when days_until_trial_end must be absent/null
	}{
		{
			name:           "active subscription (never trialed) → not_in_trial, no days field",
			sub:            &Subscription{Plan: "pro", Status: StatusActive, ActiveProcessLimit: 50},
			wantTrialField: `"trial_status":"not_in_trial"`,
			wantDaysField:  `"days_until_trial_end":null`,
		},
		{
			name: "trialing, 5 days left → trial_active",
			sub: &Subscription{
				Plan: "starter", Status: StatusTrialing, ActiveProcessLimit: 20,
				TrialEndsAt: timePtr(now.AddDate(0, 0, 5)),
			},
			wantTrialField: `"trial_status":"trial_active"`,
			wantDaysField:  `"days_until_trial_end":5`,
		},
		{
			name: "trialing, 2 days left (the lead window) → trial_ending_soon",
			sub: &Subscription{
				Plan: "starter", Status: StatusTrialing, ActiveProcessLimit: 20,
				TrialEndsAt: timePtr(now.AddDate(0, 0, 2)),
			},
			wantTrialField: `"trial_status":"trial_ending_soon"`,
			wantDaysField:  `"days_until_trial_end":2`,
		},
		{
			name: "trialing, trial_ends_at already past → trial_expired",
			sub: &Subscription{
				Plan: "starter", Status: StatusTrialing, ActiveProcessLimit: 20,
				TrialEndsAt: timePtr(now.AddDate(0, 0, -1)),
			},
			wantTrialField: `"trial_status":"trial_expired"`,
			wantDaysField:  `"days_until_trial_end":-1`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			uc := &fakeHandlerUC{sub: tt.sub, now: now}
			app := newApp(uc, roleAdmin, "tenant-1")

			status, body := do(t, app, http.MethodGet, "/v1/billing/subscription", "", "jwt")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, body)
			}
			if !strings.Contains(body, tt.wantTrialField) {
				t.Fatalf("body = %s, want to contain %s", body, tt.wantTrialField)
			}
			if tt.wantDaysField != "" && !strings.Contains(body, tt.wantDaysField) {
				t.Fatalf("body = %s, want to contain %s", body, tt.wantDaysField)
			}
		})
	}
}

// timePtr is a small test helper for the *time.Time fields Subscription carries.
func timePtr(t time.Time) *time.Time { return &t }

// AC3: a tenant that never checked out surfaces ErrSubscriptionNotFound as a 404.
func TestHandler_Subscription_NotFound_404(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{err: ErrSubscriptionNotFound}
	app := newApp(uc, roleAdmin, "tenant-1")

	status, _ := do(t, app, http.MethodGet, "/v1/billing/subscription", "", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// AC4 (handler): GET plans → 200 with the catalog wrapped in {data:[...]}.
func TestHandler_Plans_Admin_200(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{plans: []StripePlan{
		{PriceID: "price_1", Name: "Pro", Amount: 4990, Interval: "month", ActiveProcessLimit: 50},
	}}
	app := newApp(uc, roleAdmin, "tenant-1")

	status, body := do(t, app, http.MethodGet, "/v1/billing/plans", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, `"data"`) || !strings.Contains(body, `"price_id":"price_1"`) {
		t.Fatalf("unexpected plans body: %s", body)
	}
}
