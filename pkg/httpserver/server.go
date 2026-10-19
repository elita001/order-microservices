// Package httpserver runs an HTTP server with graceful shutdown.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Run blocks until ctx is canceled (SIGINT/SIGTERM) or the server fails.
// On cancellation, it gives active requests shutdownTimeout to complete.
func Run(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration, log *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server started", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down http server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("http server stopped")
	return nil
}
