package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/payment/application"
)

func charge(t *testing.T, s *Stub, key string) application.ChargeResult {
	t.Helper()
	result, err := s.Charge(context.Background(), application.ChargeRequest{IdempotencyKey: key, Amount: 100, Currency: "RUB"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return result
}

func TestStub_FailureRateBounds(t *testing.T) {
	approveAll, declineAll := NewStub(StubConfig{FailureRate: 0}), NewStub(StubConfig{FailureRate: 1})
	for range 100 {
		key := uuid.NewString()
		if !charge(t, approveAll, key).Approved {
			t.Fatal("payment declined with failure rate 0")
		}
		if result := charge(t, declineAll, key); result.Approved || result.DeclineReason == "" {
			t.Fatalf("result = %+v, want declined with a reason for failure rate 1", result)
		}
	}
}

func TestStub_SameKeySameResult(t *testing.T) {
	stub := NewStub(StubConfig{FailureRate: 0.5})
	for range 100 {
		key := uuid.NewString()
		if first, second := charge(t, stub, key), charge(t, stub, key); first != second {
			t.Fatalf("results differ for key %s: %+v and %+v", key, first, second)
		}
	}
}

func TestStub_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewStub(StubConfig{MinDelay: 1e9, MaxDelay: 1e9}).Charge(ctx, application.ChargeRequest{IdempotencyKey: "k"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
