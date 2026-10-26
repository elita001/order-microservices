package grpcx

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/errtrack"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/requestid"
)

// requestIDMetadata carries the request ID, like the X-Request-Id header and the
// x-request-id message header (gRPC metadata keys are lowercase).
const requestIDMetadata = requestid.MessageHeader

const maxRequestIDLength = 64

// Health checks and reflection are called by probes and tools without a token, and
// health checks are too frequent to log.
func isInfrastructureMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

// requestIDInterceptor continues the caller's request ID (the gateway generates one per
// external request). A call without one (e.g., from grpcurl) gets a new ID.
func requestIDInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	id := firstMetadata(ctx, requestIDMetadata)
	if id == "" || len(id) > maxRequestIDLength {
		id = uuid.NewString()
	}
	return handler(requestid.WithContext(ctx, id), req)
}

// retryPushbackMetadata is the gRPC retry pushback trailer (gRPC A6): a negative value tells
// the client not to retry the call.
const retryPushbackMetadata = "grpc-retry-pushback-ms"

// noRetryInterceptor stops client retries of an UNAVAILABLE the service returned on purpose
// (with an ErrorInfo reason, e.g. DATABASE_UNAVAILABLE): the service has already waited for
// its dependency, and repeating the call would only multiply the load on it and use up the
// caller's deadline. An UNAVAILABLE produced by gRPC itself (the server could not be reached,
// the connection broke) has no trailer and is retried, possibly on another replica.
func noRetryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if status.Code(err) == codes.Unavailable && Reason(err) != "" {
		_ = grpc.SetTrailer(ctx, metadata.Pairs(retryPushbackMetadata, "-1"))
	}
	return resp, err
}

// logInterceptor logs each call and stores a logger with request_id and trace_id in the context.
func logInterceptor(base *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		l := logger.WithCorrelation(ctx, base)
		resp, err := handler(logger.WithContext(ctx, l), req)
		if isInfrastructureMethod(info.FullMethod) {
			return resp, err
		}
		attrs := []any{
			"method", info.FullMethod,
			"code", status.Code(err).String(),
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if err != nil {
			attrs = append(attrs, "error", status.Convert(err).Message())
		}
		l.Info("grpc request", attrs...)
		return resp, err
	}
}

// recoveryInterceptor turns a panic in a handler into INTERNAL instead of crashing the process.
func recoveryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = recovered(ctx, rec, info.FullMethod)
		}
	}()
	return handler(ctx, req)
}

func recoveryStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = recovered(ss.Context(), rec, info.FullMethod)
		}
	}()
	return handler(srv, ss)
}

func recovered(ctx context.Context, rec any, method string) error {
	logger.FromContext(ctx).Error("panic recovered", "panic", rec, "method", method, "stack", string(debug.Stack()))
	errtrack.CapturePanic(ctx, rec, map[string]string{"grpc_method": method})
	return status.Error(codes.Internal, "internal error")
}

// timeoutInterceptor bounds the call by d. A caller's shorter deadline is kept: gRPC sends
// it in the grpc-timeout header, so the server stops working when the caller stops waiting.
func timeoutInterceptor(d time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > d {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return handler(ctx, req)
	}
}

// Errors of the auth interceptor, common to all services.
const (
	authErrorDomain = "auth.orderflow"
	// ReasonIdentityProviderUnavailable: the token could not be checked (the signing keys
	// could not be fetched), which is not the caller's fault.
	ReasonIdentityProviderUnavailable = "IDENTITY_PROVIDER_UNAVAILABLE"
)

// authInterceptor rejects calls without a valid access token in the "authorization"
// metadata with UNAUTHENTICATED and stores the principal in the context.
func authInterceptor(v *auth.Verifier, onFailure func(reason string)) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if isInfrastructureMethod(info.FullMethod) {
			return handler(ctx, req)
		}
		raw, err := auth.ParseBearer(firstMetadata(ctx, "authorization"))
		if err == nil {
			var authCtx context.Context
			if authCtx, err = v.Authenticate(ctx, raw); err == nil {
				return handler(authCtx, req)
			}
		}
		reason := auth.FailureReason(err)
		if onFailure != nil {
			onFailure(reason)
		}
		if errors.Is(err, auth.ErrKeysUnavailable) {
			logger.FromContext(ctx).Error("cannot verify tokens: signing keys are unavailable", "error", err)
			return nil, Error(codes.Unavailable, authErrorDomain, ReasonIdentityProviderUnavailable, "authentication is temporarily unavailable, retry later")
		}
		logger.FromContext(ctx).Info("call rejected: unauthenticated", "reason", reason, "error", err)
		return nil, status.Error(codes.Unauthenticated, "a valid access token is required")
	}
}

func firstMetadata(ctx context.Context, key string) string {
	if values := metadata.ValueFromIncomingContext(ctx, key); len(values) > 0 {
		return values[0]
	}
	return ""
}
