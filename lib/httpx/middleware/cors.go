package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

// CORS returns the browser CORS middleware for the api's cross-origin frontend.
// The FE (Next.js on its own origin) calls the api with an "Authorization: Bearer"
// header, which makes every request — even a GET — a non-simple request: the
// browser first issues a preflight OPTIONS and then blocks any response that lacks
// Access-Control-Allow-Origin. Without this, the FE's fetch rejects and the caller
// never sees the (successful) 200 the api actually returned — exactly the failure
// that masqueraded as an onboarding timeout.
//
// It answers the preflight (204, headers stamped) and adds the CORS headers to the
// real response. It must be mounted globally BEFORE the /v1 auth dispatch so the
// unauthenticated OPTIONS preflight is not rejected with a 401. allowedOrigins is
// the comma-separated allowlist from config (CORS_ALLOWED_ORIGINS).
//
// AllowCredentials stays false: authentication rides on the bearer token, never a
// cookie, so credentialed CORS (which forbids a wildcard and demands an exact
// origin echo) is unnecessary. AllowHeaders must list Authorization and
// Content-Type — the two the FE sends — because a preflight only permits the
// headers it enumerates.
func CORS(allowedOrigins string) fiber.Handler {
	return cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowHeaders:     "Authorization,Content-Type,Accept",
		AllowCredentials: false,
		MaxAge:           3600,
	})
}
