package auth

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/elita001/order-microservices/pkg/telemetry"
)

// ClientCredentials is a service's own OAuth 2.0 client, for calls made on the service's
// behalf rather than a user's (e.g., the notification service reading customer profiles
// while handling an event). Usually loaded from environment variables with the OIDC_ prefix.
type ClientCredentials struct {
	// TokenURL is the identity provider's token endpoint on the internal network.
	TokenURL     string `env:"TOKEN_URL,required"`
	ClientID     string `env:"CLIENT_ID,required"`
	ClientSecret string `env:"CLIENT_SECRET,required"`
}

// TokenSource gets tokens with the client credentials grant. It caches a token and gets a
// new one shortly before it expires, so the identity provider is called once per token
// lifetime, not once per request.
func (c ClientCredentials) TokenSource(ctx context.Context) oauth2.TokenSource {
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: telemetry.HTTPTransport(http.DefaultTransport)}
	cfg := clientcredentials.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, TokenURL: c.TokenURL}
	return cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, httpClient))
}
