package postgres

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsUnavailable reports whether err means the database could not be reached or is
// shutting down, as opposed to an error in the query or the data. HTTP handlers answer
// such errors with 503, so clients and load balancers know to retry later.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &connectErr), errors.As(err, &netErr), pgconn.Timeout(err),
		errors.Is(err, context.DeadlineExceeded):
		return true
	case errors.As(err, &pgErr):
		// Class 08: connection exception; 57P01-57P03: admin/crash shutdown, cannot connect now.
		return pgErr.Code[:2] == "08" || pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
	}
	return false
}
