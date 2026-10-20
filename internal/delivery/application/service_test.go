package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/delivery/domain"
)

// fakeRepo keeps shipments by order ID, like the unique constraint in the database.
type fakeRepo struct {
	byOrder map[uuid.UUID]*domain.Shipment
	events  []domain.Event
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byOrder: map[uuid.UUID]*domain.Shipment{}}
}

func (f *fakeRepo) GetOrCreate(_ context.Context, s *domain.Shipment) (*domain.Shipment, error) {
	if existing, ok := f.byOrder[s.OrderID]; ok {
		copied := *existing
		return &copied, nil
	}
	copied := *s
	f.byOrder[s.OrderID] = &copied
	return s, nil
}

func (f *fakeRepo) Complete(_ context.Context, s *domain.Shipment, events ...domain.Event) (bool, error) {
	stored := f.byOrder[s.OrderID]
	if stored.Status != domain.StatusPending {
		return false, nil
	}
	*stored = *s
	f.events = append(f.events, events...)
	return true, nil
}

type fakeCarrier struct {
	err   error
	calls int
}

func (f *fakeCarrier) RegisterShipment(context.Context, RegisterShipmentRequest) (RegisterShipmentResult, error) {
	f.calls++
	return RegisterShipmentResult{Carrier: "CDEK", TrackingNumber: "OF123", EstimatedDeliveryAt: time.Now()}, f.err
}

type discardLogger struct{}

func (discardLogger) Info(string, ...any) {}

func validInput() ShipOrderInput {
	return ShipOrderInput{OrderID: uuid.New(), CustomerID: uuid.New()}
}

func TestService_ShipOrder(t *testing.T) {
	repo := newFakeRepo()
	in := validInput()

	if err := NewService(repo, &fakeCarrier{}, discardLogger{}).ShipOrder(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	shipment := repo.byOrder[in.OrderID]
	if shipment.Status != domain.StatusRegistered || shipment.TrackingNumber != "OF123" {
		t.Errorf("shipment = %+v, want registered with OF123", shipment)
	}
	if len(repo.events) != 1 {
		t.Fatalf("got %d events, want 1", len(repo.events))
	}
	if _, ok := repo.events[0].(domain.ShipmentRegistered); !ok {
		t.Errorf("event is %T, want domain.ShipmentRegistered", repo.events[0])
	}
}

func TestService_ShipOrder_DuplicateDoesNotRegisterTwice(t *testing.T) {
	repo := newFakeRepo()
	carrier := &fakeCarrier{}
	svc := NewService(repo, carrier, discardLogger{})
	in := validInput()

	for range 2 {
		if err := svc.ShipOrder(context.Background(), in); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if carrier.calls != 1 || len(repo.events) != 1 {
		t.Errorf("carrier calls = %d, events = %d, want 1 and 1", carrier.calls, len(repo.events))
	}
}

func TestService_ShipOrder_CarrierErrorLeavesShipmentPending(t *testing.T) {
	repo := newFakeRepo()
	carrierErr := errors.New("carrier unavailable")
	in := validInput()

	err := NewService(repo, &fakeCarrier{err: carrierErr}, discardLogger{}).ShipOrder(context.Background(), in)
	if !errors.Is(err, carrierErr) {
		t.Fatalf("expected carrier error, got %v", err)
	}
	if status := repo.byOrder[in.OrderID].Status; status != domain.StatusPending {
		t.Errorf("status = %q, want pending so that a retry can complete it", status)
	}
}

func TestService_ShipOrder_Validation(t *testing.T) {
	for name, in := range map[string]ShipOrderInput{
		"empty order":    {CustomerID: uuid.New()},
		"empty customer": {OrderID: uuid.New()},
	} {
		t.Run(name, func(t *testing.T) {
			err := NewService(newFakeRepo(), &fakeCarrier{}, discardLogger{}).ShipOrder(context.Background(), in)
			if !IsValidationError(err) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}
}
