// Package domain contains the customer service's business model and errors.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Customer is a user of the identity provider who places orders. The identity fields
// (username, email, names) are owned by the identity provider; the service keeps a copy.
type Customer struct {
	ID        uuid.UUID // the identity provider's user ID (the "sub" claim)
	Username  string
	Email     string // personal data: never logged
	FirstName string
	LastName  string
	SyncedAt  time.Time // when the copy was last refreshed from the identity provider
}

// ErrNotFound means there is no such user, or the viewer may not see them.
var ErrNotFound = errors.New("customer not found")

// ErrDirectoryUnavailable means the identity provider could not be asked, and there is
// no local copy to fall back to.
var ErrDirectoryUnavailable = errors.New("user directory is unavailable")
