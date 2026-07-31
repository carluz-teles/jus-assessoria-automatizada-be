package middleware_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// fakeVerifier is a TokenVerifier double: it returns the canned identity (or
// error) regardless of the token, so Auth is testable without a real Clerk JWT.
type fakeVerifier struct {
	userID, orgID, role string
	err                 error
}

func (f fakeVerifier) Verify(context.Context, string) (string, string, string, error) {
	return f.userID, f.orgID, f.role, f.err
}

// fakeResolver is a PrincipalResolver double returning a canned principal/error.
type fakeResolver struct {
	principal httpx.Principal
	err       error
}

func (f fakeResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return f.principal, f.err
}

func TestAuth_InjectsPrincipalDownstream(t *testing.T) {
	t.Parallel()

	v := fakeVerifier{userID: "user_xyz", orgID: "org_abc", role: "org:admin"}
	r := fakeResolver{principal: httpx.Principal{UserID: "u-1", TenantID: "tenant-uuid", Role: "ADMIN"}}

	app := fiber.New()
	app.Get("/v1/me", middleware.Auth(v, r), func(c *fiber.Ctx) error {
		// The handler reads the tenant only from the resolved principal (§4d.3).
		return c.SendString(httpx.TenantFromCtx(c))
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer any.jwt.here")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tenant-uuid" {
		t.Fatalf("downstream tenant = %q, want tenant-uuid", body)
	}
}

func TestAuth_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		authHeader string
		verifier   fakeVerifier
		resolver   fakeResolver
		wantStatus int
		wantKind   string
	}{
		{
			name:       "missing bearer is 401",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
			wantKind:   string(apperr.KindUnauthorized),
		},
		{
			name:       "verifier error is 401",
			authHeader: "Bearer bad.jwt",
			verifier:   fakeVerifier{err: apperr.NewUnauthorized("boom")},
			wantStatus: http.StatusUnauthorized,
			wantKind:   string(apperr.KindUnauthorized),
		},
		{
			name:       "unprovisioned principal (not found) becomes 401",
			authHeader: "Bearer ok.jwt",
			resolver:   fakeResolver{err: apperr.NewNotFound("user not found")},
			wantStatus: http.StatusUnauthorized,
			wantKind:   string(apperr.KindUnauthorized),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Get("/v1/me", middleware.Auth(tt.verifier, tt.resolver), func(c *fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK) // must never be reached
			})

			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			if tt.authHeader != "" {
				req.Header.Set(fiber.HeaderAuthorization, tt.authHeader)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := decodeKind(t, resp.Body); got != tt.wantKind {
				t.Fatalf("error kind = %q, want %q", got, tt.wantKind)
			}
		})
	}
}

func TestAuthUser_ValidTokenInjectsClerkUserIDAndContinues(t *testing.T) {
	t.Parallel()

	// A verified token but NO resolver: AuthUser must not require a tenant, so a
	// signed-up user with no org still reaches the handler (the onboarding case).
	v := fakeVerifier{userID: "user_onboarding", orgID: "", role: ""}

	app := fiber.New()
	app.Get("/v1/lookup/cnpj/x", middleware.AuthUser(v), func(c *fiber.Ctx) error {
		id, ok := httpx.ClerkUserIDFromCtx(c)
		if !ok {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		// No Principal is set — AuthUser never resolves a tenant.
		if _, hasPrincipal := httpx.PrincipalFromCtx(c); hasPrincipal {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		return c.SendString(id)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/lookup/cnpj/x", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer any.jwt.here")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "user_onboarding" {
		t.Fatalf("clerk user id = %q, want user_onboarding", body)
	}
}

// AC5: AuthUser injects the verified org id + role (additively, alongside the clerk
// user id) for the JIT sync endpoint — without resolving a tenant.
func TestAuthUser_InjectsClerkOrgAndRole(t *testing.T) {
	t.Parallel()

	v := fakeVerifier{userID: "user_xyz", orgID: "org_abc", role: "org:admin"}

	app := fiber.New()
	app.Get("/v1/identity/sync", middleware.AuthUser(v), func(c *fiber.Ctx) error {
		orgID, role, ok := httpx.ClerkOrgFromCtx(c)
		if !ok {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		// The clerk user id marker is still set too (additive, not a replacement).
		if _, hasUser := httpx.ClerkUserIDFromCtx(c); !hasUser {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		// No Principal — AuthUser never resolves a tenant.
		if _, hasPrincipal := httpx.PrincipalFromCtx(c); hasPrincipal {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		return c.SendString(orgID + " " + role)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/identity/sync", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer any.jwt.here")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "org_abc org:admin" {
		t.Fatalf("injected org context = %q, want \"org_abc org:admin\"", body)
	}
}

// A verified token with NO org leaves ClerkOrgFromCtx reporting absent (the sync
// handler turns that into a 401), while the clerk user id is still injected — so the
// tenant-less /me read and lookups keep working unchanged.
func TestAuthUser_NoOrgInToken_ClerkOrgAbsent(t *testing.T) {
	t.Parallel()

	v := fakeVerifier{userID: "user_onboarding", orgID: "", role: ""}

	app := fiber.New()
	app.Get("/v1/identity/me", middleware.AuthUser(v), func(c *fiber.Ctx) error {
		if _, _, ok := httpx.ClerkOrgFromCtx(c); ok {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		id, ok := httpx.ClerkUserIDFromCtx(c)
		if !ok {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		return c.SendString(id)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/identity/me", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer any.jwt.here")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "user_onboarding" {
		t.Fatalf("clerk user id = %q, want user_onboarding", body)
	}
}

func TestAuthUser_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		authHeader string
		verifier   fakeVerifier
	}{
		{
			name:       "missing bearer is 401",
			authHeader: "",
		},
		{
			name:       "verifier error is 401",
			authHeader: "Bearer bad.jwt",
			verifier:   fakeVerifier{err: apperr.NewUnauthorized("boom")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Get("/v1/lookup/cep/x", middleware.AuthUser(tt.verifier), func(c *fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK) // must never be reached
			})

			req := httptest.NewRequest(http.MethodGet, "/v1/lookup/cep/x", nil)
			if tt.authHeader != "" {
				req.Header.Set(fiber.HeaderAuthorization, tt.authHeader)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := decodeKind(t, resp.Body); got != string(apperr.KindUnauthorized) {
				t.Fatalf("error kind = %q, want %q", got, apperr.KindUnauthorized)
			}
		})
	}
}

func TestRequireRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		principal  *httpx.Principal
		wantStatus int
		wantKind   string
	}{
		{
			name:       "matching role passes through",
			principal:  &httpx.Principal{Role: "ADMIN"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "mismatched role is 403",
			principal:  &httpx.Principal{Role: "LAWYER"},
			wantStatus: http.StatusForbidden,
			wantKind:   string(apperr.KindForbidden),
		},
		{
			name:       "no principal (auth skipped) is 401",
			principal:  nil,
			wantStatus: http.StatusUnauthorized,
			wantKind:   string(apperr.KindUnauthorized),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Get("/v1/admin",
				func(c *fiber.Ctx) error {
					if tt.principal != nil {
						httpx.SetPrincipal(c, *tt.principal)
					}
					return c.Next()
				},
				middleware.RequireRole("ADMIN"),
				func(c *fiber.Ctx) error { return c.SendString("ok") },
			)

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/admin", nil))
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantKind != "" {
				if got := decodeKind(t, resp.Body); got != tt.wantKind {
					t.Fatalf("error kind = %q, want %q", got, tt.wantKind)
				}
			}
		})
	}
}

// decodeKind reads the {kind,...} error envelope and returns its kind.
func decodeKind(t *testing.T, body io.Reader) string {
	t.Helper()
	var env struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Kind
}
