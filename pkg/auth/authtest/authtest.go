// Package authtest issues signed test tokens and serves their JWKS, so tests exercise the
// real verification (signature, issuer, audience, expiry) without an identity provider.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/elita001/order-microservices/pkg/auth"
)

const (
	Issuer   = "http://idp.test/realms/orderflow"
	Audience = "orderflow-api"
)

// IDP is a fake identity provider: a JWKS endpoint and a token signer.
type IDP struct {
	Config auth.Config
	signer jose.Signer
}

func NewIDP(t *testing.T) *IDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: key.Public(), KeyID: "test-key", Algorithm: string(jose.RS256), Use: "sig"}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
	}))
	t.Cleanup(jwks.Close)

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		t.Fatal(err)
	}
	return &IDP{Config: auth.Config{Issuer: Issuer, JWKSURL: jwks.URL, Audience: Audience}, signer: signer}
}

// Token options.
type Token struct {
	Subject   uuid.UUID
	Roles     []string
	Audience  string        // default Audience
	Issuer    string        // default Issuer
	ExpiresIn time.Duration // default 5 minutes; negative for an expired token
}

func (idp *IDP) Sign(t *testing.T, tok Token) string {
	t.Helper()
	if tok.Audience == "" {
		tok.Audience = Audience
	}
	if tok.Issuer == "" {
		tok.Issuer = Issuer
	}
	if tok.ExpiresIn == 0 {
		tok.ExpiresIn = 5 * time.Minute
	}
	now := time.Now()
	raw, err := jwt.Signed(idp.signer).Claims(jwt.Claims{
		Issuer:   tok.Issuer,
		Subject:  tok.Subject.String(),
		Audience: jwt.Audience{tok.Audience},
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		Expiry:   jwt.NewNumericDate(now.Add(tok.ExpiresIn)),
	}).Claims(map[string]any{
		"preferred_username": "test-user",
		"realm_access":       map[string]any{"roles": tok.Roles},
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Customer returns a valid token of a customer.
func (idp *IDP) Customer(t *testing.T, id uuid.UUID) string {
	return idp.Sign(t, Token{Subject: id, Roles: []string{auth.RoleCustomer}})
}

// Support returns a valid token of a support agent, who may see all orders.
func (idp *IDP) Support(t *testing.T, id uuid.UUID) string {
	return idp.Sign(t, Token{Subject: id, Roles: []string{auth.RoleSupport}})
}
