package keycloak

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/customer/domain"
	"github.com/elita001/order-microservices/pkg/breaker"
)

// fakeKeycloak serves the token endpoint and the users of the Admin API.
func fakeKeycloak(t *testing.T, users map[string]string) (*httptest.Server, *int) {
	t.Helper()
	tokenRequests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/orderflow/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests++
		if id, secret, ok := r.BasicAuth(); !ok || id != "customer-service" || secret != "s3cret" {
			http.Error(w, `{"error":"unauthorized_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"admin-token","token_type":"Bearer","expires_in":300}`)
	})
	mux.HandleFunc("GET /admin/realms/orderflow/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := users[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &tokenRequests
}

func newDirectory(t *testing.T, url string) *Directory {
	t.Helper()
	return New(context.Background(), Config{
		URL: url, Realm: "orderflow", ClientID: "customer-service", ClientSecret: "s3cret", Timeout: time.Second,
		Breaker: breaker.Config{Failures: 2, OpenTimeout: time.Minute, HalfOpenRequests: 1},
	}, slog.New(slog.DiscardHandler))
}

func TestUser(t *testing.T) {
	id := uuid.New()
	srv, tokenRequests := fakeKeycloak(t, map[string]string{
		id.String(): fmt.Sprintf(`{"id":%q,"username":"alice","email":"alice@example.com","firstName":"Alice","lastName":"C","enabled":true}`, id),
	})
	dir := newDirectory(t, srv.URL)

	for range 2 {
		c, err := dir.User(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if c.ID != id || c.Username != "alice" || c.Email != "alice@example.com" || c.FirstName != "Alice" {
			t.Errorf("customer = %+v", c)
		}
	}
	if *tokenRequests != 1 {
		t.Errorf("token requested %d times, want 1: the token must be cached", *tokenRequests)
	}
}

func TestUser_NotFoundDoesNotOpenBreaker(t *testing.T) {
	srv, _ := fakeKeycloak(t, nil)
	dir := newDirectory(t, srv.URL)

	for range 3 { // more than Failures
		if _, err := dir.User(context.Background(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	}
}

func TestUser_KeycloakDownOpensBreaker(t *testing.T) {
	srv, _ := fakeKeycloak(t, nil)
	srv.Close()
	dir := newDirectory(t, srv.URL)

	for range 2 {
		if _, err := dir.User(context.Background(), uuid.New()); err == nil {
			t.Fatal("expected an error")
		}
	}
	if _, err := dir.User(context.Background(), uuid.New()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("got %v, want ErrOpen after 2 failures", err)
	}
}
