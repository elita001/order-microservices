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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/elita001/order-microservices/pkg/logger"
)

// Roles assigned in the identity provider.
const (
	RoleCustomer = "customer"
	RoleSupport  = "support" // may see every customer's orders and profiles
	// RoleCustomerReader is given to service accounts (e.g., the notification service)
	// that read customer profiles; it grants nothing else.
	RoleCustomerReader = "customer-reader"
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
	// ErrKeysUnavailable means the token could not be checked: the signing keys could not be
	// fetched (the identity provider is down and the keys are not cached yet). It is not the
	// client's fault, so it is answered with 503, not 401, which would make clients log in again.
	ErrKeysUnavailable = errors.New("token signing keys are unavailable")
)

// Verifier checks the signature (keys from JWKS, cached and refreshed on rotation),
// issuer, audience, and expiry of access tokens.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

func NewVerifier(ctx context.Context, cfg Config) *Verifier {
	keys := keySet{remote: oidc.NewRemoteKeySet(ctx, cfg.JWKSURL)}
	return &Verifier{verifier: oidc.NewVerifier(cfg.Issuer, keys, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
	})}
}

// keySet records a failure to fetch the signing keys in the context of the verification.
// go-oidc formats the key set's error into its own with %v, so the caller could not tell
// "the keys could not be fetched" from "the signature is wrong" otherwise.
type keySet struct {
	remote *oidc.RemoteKeySet
}

type (
	fetchFailure    struct{ err error }
	fetchFailureKey struct{}
)

func (k keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	payload, err := k.remote.VerifySignature(ctx, jwt)
	// RemoteKeySet reports fetch errors (network, non-2xx, bad JSON) as "fetching keys ...";
	// TestVerifier_KeysUnavailable catches a change of that in a new go-oidc version.
	if err != nil && strings.HasPrefix(err.Error(), "fetching keys") {
		if f, ok := ctx.Value(fetchFailureKey{}).(*fetchFailure); ok {
			f.err = err
		}
	}
	return payload, err
}

// claims are the parts of a Keycloak access token we use.
type claims struct {
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (v *Verifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	failure := &fetchFailure{}
	token, err := v.verifier.Verify(context.WithValue(ctx, fetchFailureKey{}, failure), rawToken)
	if err != nil {
		if failure.err != nil {
			return Principal{}, fmt.Errorf("%w: %w", ErrKeysUnavailable, failure.err)
		}
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

// Authenticate verifies the token and returns a context with the principal, the raw token
// (for calls to other services on the user's behalf), and the user ID for logs and traces.
func (v *Verifier) Authenticate(ctx context.Context, rawToken string) (context.Context, error) {
	p, err := v.Verify(ctx, rawToken)
	if err != nil {
		return ctx, err
	}
	// The user ID is pseudonymous (a UUID), fine for logs and traces.
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("enduser.id", p.UserID.String()))
	ctx = WithPrincipal(ctx, p)
	ctx = context.WithValue(ctx, tokenKey{}, rawToken)
	// Records logged with ctx (the use cases) and with the request logger (the adapters)
	// both get the user.
	ctx = logger.WithAttrs(ctx, "user_id", p.UserID.String())
	return logger.WithContext(ctx, logger.FromContext(ctx).With("user_id", p.UserID.String())), nil
}

// BearerToken extracts the token from the Authorization header.
func BearerToken(r *http.Request) (string, error) {
	return ParseBearer(r.Header.Get("Authorization"))
}

// ParseBearer extracts the token from an Authorization header (or gRPC metadata) value.
func ParseBearer(value string) (string, error) {
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", ErrMissingToken
	}
	return token, nil
}

// FailureReason is a bounded label for metrics: missing_token, invalid_token, or keys_unavailable.
func FailureReason(err error) string {
	switch {
	case errors.Is(err, ErrMissingToken):
		return "missing_token"
	case errors.Is(err, ErrKeysUnavailable):
		return "keys_unavailable"
	}
	return "invalid_token"
}

type (
	ctxKey   struct{}
	tokenKey struct{}
)

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// TokenFromContext returns the access token the request was authenticated with.
func TokenFromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(tokenKey{}).(string)
	return token, ok
}
