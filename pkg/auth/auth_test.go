package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/auth/authtest"
)

func TestVerifier(t *testing.T) {
	idp := authtest.NewIDP(t)
	other := authtest.NewIDP(t) // a different signing key
	verifier := auth.NewVerifier(context.Background(), idp.Config)
	user := uuid.New()

	valid, err := verifier.Verify(context.Background(), idp.Sign(t, authtest.Token{Subject: user, Roles: []string{"customer"}}))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if valid.UserID != user || !valid.HasRole(auth.RoleCustomer) || valid.HasRole(auth.RoleSupport) {
		t.Errorf("principal = %+v", valid)
	}

	for name, token := range map[string]string{
		"expired":        idp.Sign(t, authtest.Token{Subject: user, ExpiresIn: -time.Minute}),
		"wrong audience": idp.Sign(t, authtest.Token{Subject: user, Audience: "another-api"}),
		"wrong issuer":   idp.Sign(t, authtest.Token{Subject: user, Issuer: "http://evil.test/realms/orderflow"}),
		"wrong key":      other.Sign(t, authtest.Token{Subject: user}),
		"garbage":        "not-a-jwt",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("got %v, want ErrInvalidToken", err)
			}
		})
	}
}

// Keycloak is down and the keys are not cached: the token cannot be checked, which is
// not the client's fault (503, not 401).
func TestVerifier_KeysUnavailable(t *testing.T) {
	idp := authtest.NewIDP(t)
	cfg := idp.Config
	cfg.JWKSURL = "http://127.0.0.1:1/certs" // nothing listens on port 1
	verifier := auth.NewVerifier(context.Background(), cfg)

	_, err := verifier.Verify(context.Background(), idp.Customer(t, uuid.New()))
	if !errors.Is(err, auth.ErrKeysUnavailable) || errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("got %v, want ErrKeysUnavailable", err)
	}
	// A token that is invalid by itself stays invalid, whatever the keys.
	if _, err := verifier.Verify(context.Background(), "garbage"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("garbage: got %v, want ErrInvalidToken", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+idp.Customer(t, uuid.New()))
	var reasons []string
	auth.Middleware(verifier, func(reason string) { reasons = append(reasons, reason) })(http.NotFoundHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("middleware: %d, want 503 with Retry-After", rec.Code)
	}
	if len(reasons) != 1 || reasons[0] != "keys_unavailable" {
		t.Errorf("failure reasons = %v", reasons)
	}
}

func TestMiddleware(t *testing.T) {
	idp := authtest.NewIDP(t)
	verifier := auth.NewVerifier(context.Background(), idp.Config)
	user := uuid.New()
	var failures []string
	handler := auth.Middleware(verifier, func(reason string) { failures = append(failures, reason) })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.FromContext(r.Context())
			if !ok || p.UserID != user {
				t.Errorf("principal in context = %+v, %v", p, ok)
			}
			w.WriteHeader(http.StatusNoContent)
		}))

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"valid token", "Bearer " + idp.Customer(t, user), http.StatusNoContent},
		{"lowercase scheme", "bearer " + idp.Customer(t, user), http.StatusNoContent},
		{"no header", "", http.StatusUnauthorized},
		{"basic auth", "Basic YWxpY2U6c2VjcmV0", http.StatusUnauthorized},
		{"invalid token", "Bearer nope", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
		})
	}
	if len(failures) != 3 || failures[0] != "missing_token" || failures[2] != "invalid_token" {
		t.Errorf("failure reasons = %v", failures)
	}
}
