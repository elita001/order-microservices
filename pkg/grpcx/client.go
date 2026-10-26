package grpcx

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/requestid"
)

// ClientConfig is usually loaded from environment variables with a <SERVICE>_GRPC_ prefix.
type ClientConfig struct {
	// Addr is a gRPC target, e.g. "order:9081" or "dns:///order:9081". With several
	// addresses behind the name, calls are spread over all of them (round robin).
	Addr string `env:"ADDR,required"`
	// Timeout is the deadline of a call that has none (including its retries). It is longer
	// than the servers' REQUEST_TIMEOUT (3s), so a server stuck on its database answers
	// UNAVAILABLE itself (the gateway's 503) before the caller gives up (504).
	Timeout time.Duration `env:"TIMEOUT" envDefault:"5s"`
}

// ClientOptions configures NewClient.
type ClientOptions struct {
	// Service is the full proto service name, e.g. "orderflow.order.v1.OrderService".
	Service string
	// RetryMethods are retried on UNAVAILABLE produced by gRPC itself (unreachable server,
	// broken connection), up to 3 attempts within the deadline; servers built with NewServer
	// forbid retries of their own UNAVAILABLE answers. List only methods that are safe to
	// repeat: reads, or writes with an idempotency key.
	RetryMethods []string
	// Credentials attach an access token to every call: ForwardToken or ServiceToken.
	Credentials credentials.PerRPCCredentials
}

// NewClient creates a client connection with tracing, request ID propagation, metrics,
// default deadlines, and retries. The connection is established lazily on the first call
// and re-established automatically, so a service may start before its dependencies.
//
// Traffic is plaintext inside the Docker network. In production it is encrypted: TLS
// credentials here (and RequireTransportSecurity in the token credentials), or mTLS from
// a service mesh.
func NewClient(cfg ClientConfig, o ClientOptions) (*grpc.ClientConn, error) {
	serviceConfig, err := serviceConfig(o.Service, o.RetryMethods)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()), // sends traceparent
		grpc.WithChainUnaryInterceptor(
			defaultTimeoutInterceptor(cfg.Timeout),
			clientMetrics().UnaryClientInterceptor(),
			propagateRequestIDInterceptor,
		),
		grpc.WithDefaultServiceConfig(serviceConfig),
		// Detects a dead connection (e.g., the peer's host is gone) in ~40s instead of
		// waiting for TCP timeouts; pings only while calls are in flight.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	}
	if o.Credentials != nil {
		opts = append(opts, grpc.WithPerRPCCredentials(o.Credentials))
	}
	conn, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpc client for %s: %w", cfg.Addr, err)
	}
	return conn, nil
}

// serviceConfig enables round-robin load balancing and retries of the given methods.
func serviceConfig(service string, retryMethods []string) (string, error) {
	type name struct {
		Service string `json:"service"`
		Method  string `json:"method"`
	}
	config := map[string]any{"loadBalancingConfig": []any{map[string]any{"round_robin": map[string]any{}}}}
	if len(retryMethods) > 0 {
		names := make([]name, 0, len(retryMethods))
		for _, method := range retryMethods {
			names = append(names, name{Service: service, Method: method})
		}
		config["methodConfig"] = []any{map[string]any{
			"name": names,
			// Exponential backoff with jitter between attempts (gRPC A6 retry design).
			"retryPolicy": map[string]any{
				"maxAttempts":          3,
				"initialBackoff":       "0.1s",
				"maxBackoff":           "1s",
				"backoffMultiplier":    2,
				"retryableStatusCodes": []string{"UNAVAILABLE"},
			},
		}}
	}
	b, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("grpc service config: %w", err)
	}
	return string(b), nil
}

// defaultTimeoutInterceptor sets a deadline on calls without one, so a hung server never
// blocks the caller indefinitely. The deadline travels to the server (grpc-timeout).
func defaultTimeoutInterceptor(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok && d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func propagateRequestIDInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if id := requestid.FromContext(ctx); id != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, requestIDMetadata, id)
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// ForwardToken sends the access token of the request being handled (auth.TokenFromContext),
// so the called service authenticates and authorizes the same user (zero trust: it never
// relies on the caller having checked the token).
func ForwardToken() credentials.PerRPCCredentials {
	return forwardToken{}
}

type forwardToken struct{}

func (forwardToken) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, ok := auth.TokenFromContext(ctx)
	if !ok {
		return nil, nil // the server rejects the call with UNAUTHENTICATED
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (forwardToken) RequireTransportSecurity() bool { return false }

// ServiceToken sends the calling service's own token (OAuth 2.0 client credentials, see
// auth.ClientCredentials). ts must cache the token, as clientcredentials token sources do.
func ServiceToken(ts oauth2.TokenSource) credentials.PerRPCCredentials {
	return serviceToken{ts: ts}
}

type serviceToken struct {
	ts oauth2.TokenSource
}

func (s serviceToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := s.ts.Token()
	if err != nil {
		// The call fails with UNAVAILABLE: the identity provider may recover, so callers retry.
		return nil, fmt.Errorf("get service access token: %w", err)
	}
	return map[string]string{"authorization": "Bearer " + token.AccessToken}, nil
}

func (serviceToken) RequireTransportSecurity() bool { return false }
