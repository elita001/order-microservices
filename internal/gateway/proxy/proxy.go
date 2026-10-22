// Package proxy forwards requests to upstream services with timeouts.
package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

// Config is usually loaded from environment variables with the UPSTREAM_ prefix.
type Config struct {
	DialTimeout time.Duration `env:"DIAL_TIMEOUT" envDefault:"2s"`
	Timeout     time.Duration `env:"TIMEOUT" envDefault:"5s"` // the whole upstream request
}

// New returns a reverse proxy to target. An upstream that does not respond within the
// timeout gets 504, an unreachable one gets 502; the client never waits indefinitely.
func New(target *url.URL, cfg Config) http.Handler {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
	}
	rp := &httputil.ReverseProxy{
		// Rewrite (unlike Director) drops client-supplied X-Forwarded-* headers,
		// so clients cannot spoof their address.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		// Traces the upstream call and forwards the trace context in the traceparent header.
		Transport:    telemetry.HTTPTransport(transport),
		ErrorHandler: handleError,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
		defer cancel()
		rp.ServeHTTP(w, r.WithContext(ctx))
	})
}

func handleError(w http.ResponseWriter, r *http.Request, err error) {
	log := logger.FromContext(r.Context())
	switch {
	case errors.Is(r.Context().Err(), context.Canceled) && !errors.Is(err, context.DeadlineExceeded):
		// The client has gone away; nobody will read the response.
		log.Info("client closed request", "error", err)
	case isDialError(err):
		// Could not connect at all (DNS, refused, dial timeout): the service is down.
		log.Error("upstream unavailable", "error", err)
		httpx.WriteError(w, http.StatusBadGateway, "upstream_unavailable", "upstream service is unavailable")
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		// Connected, but the service did not respond in time.
		log.Error("upstream timeout", "error", err)
		httpx.WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", "upstream service did not respond in time")
	default:
		log.Error("upstream unavailable", "error", err)
		httpx.WriteError(w, http.StatusBadGateway, "upstream_unavailable", "upstream service is unavailable")
	}
}

func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
