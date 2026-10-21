package breaker

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

var errDown = errors.New("dependency is down")

func TestBreaker(t *testing.T) {
	b := New[int]("test", Config{Failures: 3, OpenTimeout: 50 * time.Millisecond, HalfOpenRequests: 1}, nil, slog.New(slog.DiscardHandler))
	calls := 0
	fail := func() (int, error) { calls++; return 0, errDown }
	succeed := func() (int, error) { calls++; return 1, nil }

	for range 3 {
		if _, err := b.Execute(fail); !errors.Is(err, errDown) {
			t.Fatalf("expected the dependency error, got %v", err)
		}
	}
	if _, err := b.Execute(succeed); !errors.Is(err, ErrOpen) {
		t.Fatalf("expected ErrOpen after 3 failures, got %v", err)
	}
	if calls != 3 {
		t.Errorf("dependency called %d times, want 3: an open breaker must not call it", calls)
	}

	time.Sleep(60 * time.Millisecond) // half-open: one trial call
	if got, err := b.Execute(succeed); err != nil || got != 1 {
		t.Fatalf("trial call: got %d, %v", got, err)
	}
	if _, err := b.Execute(succeed); err != nil {
		t.Fatalf("breaker must be closed after a successful trial: %v", err)
	}
}

func TestBreaker_IgnoredErrors(t *testing.T) {
	errBusiness := errors.New("declined")
	b := New[int]("test", Config{Failures: 1, OpenTimeout: time.Minute}, func(err error) bool {
		return !errors.Is(err, errBusiness)
	}, slog.New(slog.DiscardHandler))

	for _, err := range []error{errBusiness, context.Canceled, errBusiness} {
		_, _ = b.Execute(func() (int, error) { return 0, err })
	}
	if _, err := b.Execute(func() (int, error) { return 1, nil }); err != nil {
		t.Fatalf("business errors and cancellations must not open the breaker: %v", err)
	}
}
