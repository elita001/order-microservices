// Package auth verifies OAuth 2.0 access tokens (JWT) issued by the identity provider
// (Keycloak) and carries the authenticated principal through the request context.
//
// Every service that serves an API verifies the token itself (zero trust): the gateway
// rejects bad tokens early, but a service never trusts identity headers set by a caller,
// so reaching its port directly does not allow acting as another user.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

// Roles assigned in the identity provider.
const (
	RoleCustomer = "customer"
	RoleSupport  = "support" // may see every customer's orders
)

// Config is usually loaded from environment variables with the OIDC_ prefix.
type Config struct {
	// Issuer must equal the token's "iss" claim: the public URL clients get tokens from.
	Issuer string `env:"ISSUER,required"`
	// JWKSURL is where the signing keys are fetched; inside Docker it differs from the
	// public issuer URL (http://keycloak:8080/... instead of http://localhost:8180/...).
	JWKSURL string `env:"JWKS_URL,required"`
	// Audience must be in the token's "aud" claim, so a token issued for another API is rejected.
	Audience string `env:"AUDIENCE" envDefault:"orderflow-api"`
}

// Principal is the authenticated user.
type Principal struct {
	UserID   uuid.UUID // the token's "sub": also the customer ID of the user's orders
	Username string
	Roles    []string
}

func (p Principal) HasRole(role string) bool {
	return slices.Contains(p.Roles, role)
}

var (
	ErrMissingToken = errors.New("missing bearer token")
	ErrInvalidToken = errors.New("invalid token")
)

// Verifier checks the signature (keys from JWKS, cached and refreshed on rotation),
// issuer, audience, and expiry of access tokens.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

func NewVerifier(ctx context.Context, cfg Config) *Verifier {
	keys := oidc.NewRemoteKeySet(ctx, cfg.JWKSURL)
	return &Verifier{verifier: oidc.NewVerifier(cfg.Issuer, keys, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
	})}
}

// claims are the parts of a Keycloak access token we use.
type claims struct {
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (v *Verifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	userID, err := uuid.Parse(token.Subject)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: subject %q is not a UUID", ErrInvalidToken, token.Subject)
	}
	var c claims
	if err := token.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return Principal{UserID: userID, Username: c.PreferredUsername, Roles: c.RealmAccess.Roles}, nil
}

// BearerToken extracts the token from the Authorization header.
func BearerToken(r *http.Request) (string, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", ErrMissingToken
	}
	return token, nil
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
