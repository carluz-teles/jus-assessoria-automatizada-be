package middleware

import (
	"context"
	"fmt"
	"sync"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/jwks"
	"github.com/clerk/clerk-sdk-go/v2/jwt"
)

// ClerkVerifier verifies Clerk session JWTs against the instance JWKS
// (docs §4d.3, via 1). It is the production TokenVerifier; unit tests use a fake.
type ClerkVerifier struct {
	issuer     string
	jwksClient *jwks.Client

	mu       sync.Mutex
	keyCache map[string]*clerk.JSONWebKey
}

var _ TokenVerifier = (*ClerkVerifier)(nil)

// NewClerkVerifier configures the SDK with the instance secret and returns a
// verifier that accepts tokens minted by issuer. clerk.SetKey installs the key
// process-wide (the SDK holds it globally), so call this once at boot. The secret
// and issuer come from config (CLERK_SECRET_KEY / the instance issuer URL); wiring
// them from env is the api boot slice.
func NewClerkVerifier(secret, issuer string) *ClerkVerifier {
	clerk.SetKey(secret)
	return &ClerkVerifier{
		issuer:     issuer,
		jwksClient: jwks.NewClient(&clerk.ClientConfig{}),
		keyCache:   map[string]*clerk.JSONWebKey{},
	}
}

// Verify decodes the token to read its key id, fetches (and caches) the matching
// JWK, then verifies the signature, expiry and issuer. It returns the Clerk user
// id (subject), the active org id and the active org role from the verified
// claims. Any failure along the way is an untrusted token — the middleware turns
// it into a 401.
func (v *ClerkVerifier) Verify(ctx context.Context, bearer string) (userID, orgID, role string, err error) {
	decoded, err := jwt.Decode(ctx, &jwt.DecodeParams{Token: bearer})
	if err != nil {
		return "", "", "", fmt.Errorf("decode token: %w", err)
	}

	jwk, err := v.jwk(ctx, decoded.KeyID)
	if err != nil {
		return "", "", "", fmt.Errorf("fetch jwk %q: %w", decoded.KeyID, err)
	}

	claims, err := jwt.Verify(ctx, &jwt.VerifyParams{
		Token: bearer,
		JWK:   jwk,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("verify token: %w", err)
	}

	// jwt.Verify already rejects issuers outside Clerk's known patterns; when an
	// explicit instance issuer is configured we pin to it exactly, so a token
	// minted by a different Clerk instance is rejected too.
	if v.issuer != "" && claims.Issuer != v.issuer {
		return "", "", "", fmt.Errorf("unexpected issuer %q", claims.Issuer)
	}

	return claims.Subject, claims.ActiveOrganizationID, claims.ActiveOrganizationRole, nil
}

// jwk returns the JSON web key for keyID, fetched once from the JWKS endpoint and
// cached thereafter — Clerk advises caching the key until the instance secret
// rotates (a process restart re-runs NewClerkVerifier). The mutex is held across
// the fetch so a burst of first requests triggers a single upstream call.
func (v *ClerkVerifier) jwk(ctx context.Context, keyID string) (*clerk.JSONWebKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if jwk, ok := v.keyCache[keyID]; ok {
		return jwk, nil
	}

	jwk, err := jwt.GetJSONWebKey(ctx, &jwt.GetJSONWebKeyParams{
		KeyID:      keyID,
		JWKSClient: v.jwksClient,
	})
	if err != nil {
		return nil, err
	}

	v.keyCache[keyID] = jwk
	return jwk, nil
}
