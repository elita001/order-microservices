package application

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/order/domain"
)

type fakeRepo struct {
	created   *domain.Order
	events    []domain.Event
	err       error
	gotLimit  int
	gotOffset int

	statuses map[uuid.UUID]domain.Status
}

func (f *fakeRepo) Create(_ context.Context, order *domain.Order, events ...domain.Event) error {
	f.created = order
	f.events = events
	return f.err
}

func (f *fakeRepo) GetByID(_ context.Context, _ uuid.UUID) (*domain.Order, error) {
	return nil, domain.ErrNotFound
}

func (f *fakeRepo) ListByCustomer(_ context.Context, _ uuid.UUID, limit, offset int) ([]domain.Order, error) {
	f.gotLimit, f.gotOffset = limit, offset
	return []domain.Order{}, f.err
}

func (f *fakeRepo) UpdateStatus(_ context.Context, id uuid.UUID, from []domain.Status, to domain.Status, _ time.Time) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	current, ok := f.statuses[id]
	if !ok || !slices.Contains(from, current) {
		return false, nil
	}
	f.statuses[id] = to
	return true, nil
}

type discardLogger struct{}

func (discardLogger) Info(string, ...any) {}

func newTestService(repo Repository) *Service {
	return NewService(repo, discardLogger{})
}

func validInput() CreateOrderInput {
	return CreateOrderInput{
		CustomerID: uuid.New(),
		Currency:   "RUB",
		Items: []domain.Item{
			{ProductID: "book", Quantity: 2, Price: 50000},
			{ProductID: "pen", Quantity: 3, Price: 1500},
		},
	}
}

func TestService_Create_CalculatesTotal(t *testing.T) {
	repo := &fakeRepo{}
	order, err := newTestService(repo).Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := int64(2*50000 + 3*1500); order.TotalAmount != want {
		t.Errorf("total = %d, want %d", order.TotalAmount, want)
	}
	if order.Status != domain.StatusPending {
		t.Errorf("status = %q, want %q", order.Status, domain.StatusPending)
	}
	if repo.created == nil {
		t.Error("order was not saved")
	}
}

func TestService_Create_SavesOrderCreatedEvent(t *testing.T) {
	repo := &fakeRepo{}
	order, err := newTestService(repo).Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repo.events) != 1 {
		t.Fatalf("got %d events, want 1", len(repo.events))
	}
	event, ok := repo.events[0].(domain.OrderCreated)
	if !ok {
		t.Fatalf("event is %T, want domain.OrderCreated", repo.events[0])
	}
	if event.OrderID != order.ID || event.TotalAmount != order.TotalAmount {
		t.Errorf("event %+v does not match order %+v", event, order)
	}
}

func TestService_Create_Validation(t *testing.T) {
	tests := map[string]func(in *CreateOrderInput){
		"empty customer":   func(in *CreateOrderInput) { in.CustomerID = uuid.Nil },
		"bad currency":     func(in *CreateOrderInput) { in.Currency = "BTC" },
		"no items":         func(in *CreateOrderInput) { in.Items = nil },
		"zero quantity":    func(in *CreateOrderInput) { in.Items[0].Quantity = 0 },
		"negative price":   func(in *CreateOrderInput) { in.Items[0].Price = -1 },
		"empty product id": func(in *CreateOrderInput) { in.Items[0].ProductID = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := validInput()
			mutate(&input)
			_, err := newTestService(&fakeRepo{}).Create(context.Background(), input)
			var validationErr *domain.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
		})
	}
}

func TestService_Create_RepoErrorIsWrapped(t *testing.T) {
	dbErr := errors.New("db is down")
	_, err := newTestService(&fakeRepo{err: dbErr}).Create(context.Background(), validInput())
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected wrapped db error, got %v", err)
	}
}

func TestService_ListByCustomer_Limit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{"default when zero", 0, defaultListLimit},
		{"as is", 50, 50},
		{"clamped to max", 1000, maxListLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{}
			result, err := newTestService(repo).ListByCustomer(context.Background(),
				ListOrdersInput{CustomerID: uuid.New(), Limit: tt.limit})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo.gotLimit != tt.wantLimit || result.Limit != tt.wantLimit {
				t.Errorf("limit: repo=%d result=%d, want %d", repo.gotLimit, result.Limit, tt.wantLimit)
			}
		})
	}
}

func TestService_CompletePayment(t *testing.T) {
	tests := []struct {
		name       string
		current    domain.Status
		succeeded  bool
		wantStatus domain.Status
	}{
		{"succeeded", domain.StatusPending, true, domain.StatusPaid},
		{"failed", domain.StatusPending, false, domain.StatusPaymentFailed},
		{"duplicate event is ignored", domain.StatusPaid, false, domain.StatusPaid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := uuid.New()
			repo := &fakeRepo{statuses: map[uuid.UUID]domain.Status{id: tt.current}}
			err := newTestService(repo).CompletePayment(context.Background(),
				CompletePaymentInput{OrderID: id, Succeeded: tt.succeeded})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := repo.statuses[id]; got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestService_CompletePayment_UnknownOrderIsIgnored(t *testing.T) {
	repo := &fakeRepo{statuses: map[uuid.UUID]domain.Status{}}
	err := newTestService(repo).CompletePayment(context.Background(),
		CompletePaymentInput{OrderID: uuid.New(), Succeeded: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestService_CompletePayment_RepoErrorIsReturned(t *testing.T) {
	dbErr := errors.New("db is down")
	err := newTestService(&fakeRepo{err: dbErr}).CompletePayment(context.Background(),
		CompletePaymentInput{OrderID: uuid.New(), Succeeded: true})
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected wrapped db error, got %v", err)
	}
}

func TestService_MarkShipped(t *testing.T) {
	tests := []struct {
		name       string
		current    domain.Status
		wantStatus domain.Status
	}{
		{"paid order is shipped", domain.StatusPaid, domain.StatusShipped},
		{"delivery event before payment event", domain.StatusPending, domain.StatusShipped},
		{"duplicate event is ignored", domain.StatusShipped, domain.StatusShipped},
		{"failed payment is not shipped", domain.StatusPaymentFailed, domain.StatusPaymentFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := uuid.New()
			repo := &fakeRepo{statuses: map[uuid.UUID]domain.Status{id: tt.current}}
			if err := newTestService(repo).MarkShipped(context.Background(), id); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := repo.statuses[id]; got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestService_LatePaymentEventDoesNotOverrideShipped(t *testing.T) {
	id := uuid.New()
	repo := &fakeRepo{statuses: map[uuid.UUID]domain.Status{id: domain.StatusPending}}
	svc := newTestService(repo)

	if err := svc.MarkShipped(context.Background(), id); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := svc.CompletePayment(context.Background(), CompletePaymentInput{OrderID: id, Succeeded: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := repo.statuses[id]; got != domain.StatusShipped {
		t.Errorf("status = %q, want %q", got, domain.StatusShipped)
	}
}
