package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/delivery/application"
)

func TestStub_SameKeySameParcel(t *testing.T) {
	stub := NewStub(StubConfig{})
	stub.now = func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }
	for range 50 {
		req := application.RegisterShipmentRequest{IdempotencyKey: uuid.NewString()}
		first, err1 := stub.RegisterShipment(context.Background(), req)
		second, err2 := stub.RegisterShipment(context.Background(), req)
		if err1 != nil || err2 != nil {
			t.Fatalf("unexpected errors: %v, %v", err1, err2)
		}
		if first != second {
			t.Fatalf("results differ: %+v and %+v", first, second)
		}
		if first.TrackingNumber == "" || first.Carrier == "" {
			t.Fatalf("incomplete result: %+v", first)
		}
	}
}

func TestStub_ErrorRateOne(t *testing.T) {
	_, err := NewStub(StubConfig{ErrorRate: 1}).RegisterShipment(context.Background(),
		application.RegisterShipmentRequest{IdempotencyKey: "k"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}
