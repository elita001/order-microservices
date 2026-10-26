// Package keycloak reads users from the Keycloak Admin REST API.
package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/elita001/order-microservices/internal/customer/application"
	"github.com/elita001/order-microservices/internal/customer/domain"
	"github.com/elita001/order-microservices/pkg/breaker"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

// Config is usually loaded from environment variables with the KEYCLOAK_ prefix.
type Config struct {
	// URL is Keycloak's internal address, e.g. http://keycloak:8080.
	URL   string `env:"URL,required"`
	Realm string `env:"REALM" envDefault:"orderflow"`
	// The service's own client in the realm. Its service account has only the view-users
	// role of realm-management: it can read users, not change them.
	ClientID     string         `env:"CLIENT_ID,required"`
	ClientSecret string         `env:"CLIENT_SECRET,required"`
	Timeout      time.Duration  `env:"TIMEOUT" envDefault:"2s"` // per HTTP call
	Breaker      breaker.Config `envPrefix:"BREAKER_"`
}

// Directory implements application.Directory. It authenticates with the client credentials
// grant: the token is cached and renewed shortly before it expires.
type Directory struct {
	client   *http.Client
	usersURL string
	breaker  *breaker.Breaker[*domain.Customer]
}

var _ application.Directory = (*Directory)(nil)

func New(ctx context.Context, cfg Config, log *slog.Logger) *Directory {
	base := &http.Client{Timeout: cfg.Timeout, Transport: telemetry.HTTPTransport(http.DefaultTransport)}
	credentials := clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		TokenURL:     fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", cfg.URL, url.PathEscape(cfg.Realm)),
	}
	// The token requests use the base client too (traced, with the timeout).
	client := credentials.Client(context.WithValue(ctx, oauth2.HTTPClient, base))
	client.Timeout = cfg.Timeout

	return &Directory{
		client:   client,
		usersURL: fmt.Sprintf("%s/admin/realms/%s/users/", cfg.URL, url.PathEscape(cfg.Realm)),
		// "No such user" is an answer, not a failure of Keycloak.
		breaker: breaker.New[*domain.Customer]("keycloak", cfg.Breaker, func(err error) bool {
			return !errors.Is(err, domain.ErrNotFound)
		}, log),
	}
}

// user is the part of Keycloak's UserRepresentation we use.
type user struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

func (d *Directory) User(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	return d.breaker.Execute(func() (*domain.Customer, error) { return d.fetch(ctx, id) })
}

func (d *Directory) fetch(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.usersURL+id.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("keycloak: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("keycloak: get user: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, domain.ErrNotFound
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("keycloak: get user: status %d: %s", resp.StatusCode, body)
	}

	var u user
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return nil, fmt.Errorf("keycloak: decode user: %w", err)
	}
	userID, err := uuid.Parse(u.ID)
	if err != nil || userID != id {
		return nil, fmt.Errorf("keycloak: unexpected user ID %q", u.ID)
	}
	return &domain.Customer{ID: userID, Username: u.Username, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName}, nil
}
