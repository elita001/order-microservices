package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/elita001/order-microservices/pkg/requestid"
)

func TestRequestID(t *testing.T) {
	tests := []struct {
		name         string
		incoming     string
		wantClientID string
	}{
		{"client ID is kept separately", "abc-123_x.y:z", "abc-123_x.y:z"},
		{"no client ID", "", ""},
		{"too long client ID is dropped", strings.Repeat("a", 65), ""},
		{"unsafe client ID is dropped", "id\nfake log line", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen, forwarded, clientID string
			handler := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = middleware.GetReqID(r.Context())
				forwarded = r.Header.Get(RequestIDHeader)
				clientID = requestid.ClientIDFromContext(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if _, err := uuid.Parse(seen); err != nil {
				t.Fatalf("request ID %q is not a generated UUID", seen)
			}
			if seen != forwarded || seen != rec.Header().Get(RequestIDHeader) {
				t.Errorf("context %q, forwarded %q, response %q must be equal",
					seen, forwarded, rec.Header().Get(RequestIDHeader))
			}
			if clientID != tt.wantClientID {
				t.Errorf("client ID = %q, want %q", clientID, tt.wantClientID)
			}
		})
	}
}
