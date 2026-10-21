package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"valid id is kept", "abc-123_x.y:z", true},
		{"missing id is generated", "", false},
		{"too long id is replaced", strings.Repeat("a", 65), false},
		{"unsafe characters are replaced", "id\nfake log line", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen, forwarded string
			handler := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = middleware.GetReqID(r.Context())
				forwarded = r.Header.Get(RequestIDHeader)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if seen == "" || seen != forwarded || seen != rec.Header().Get(RequestIDHeader) {
				t.Fatalf("context %q, forwarded %q, response %q must be equal and not empty",
					seen, forwarded, rec.Header().Get(RequestIDHeader))
			}
			if (seen == tt.incoming) != tt.keep {
				t.Errorf("id = %q, incoming %q, keep = %v", seen, tt.incoming, tt.keep)
			}
		})
	}
}
