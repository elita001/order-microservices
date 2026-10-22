package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHTTP_UsesRoutePatternAndSkipsProbes(t *testing.T) {
	r := chi.NewRouter()
	r.Use(HTTP)
	r.Get("/orders/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	r.Get("/healthz", func(http.ResponseWriter, *http.Request) {})

	for _, path := range []string{"/orders/1", "/orders/2", "/healthz", "/nope"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	if got := testutil.ToFloat64(httpRequests.WithLabelValues("GET", "/orders/{id}", "404")); got != 2 {
		t.Errorf("requests for /orders/{id} = %v, want 2 (one series for all IDs)", got)
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues("GET", "unmatched", "404")); got != 1 {
		t.Errorf("unmatched requests = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(httpRequests); got != 2 {
		t.Errorf("series = %d, want 2: /healthz must not be recorded", got)
	}
}

func TestNormalizeMethod(t *testing.T) {
	if normalizeMethod("FOOBAR") != "OTHER" || normalizeMethod(http.MethodPost) != http.MethodPost {
		t.Error("unexpected method normalization")
	}
}
