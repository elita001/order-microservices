package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/order/domain"
)

type fakeRepo struct {
	created   *domain.Order
	err       error
	gotLimit  int
	gotOffset int
}

func (f *fakeRepo) Create(_ context.Context, order *domain.Order) error {
	f.created = order
	return f.err
}

func (f *fakeRepo) GetByID(_ context.Context, _ uuid.UUID) (*domain.Order, error) {
	return nil, domain.ErrNotFound
}

func (f *fakeRepo) ListByCustomer(_ context.Context, _ uuid.UUID, limit, offset int) ([]domain.Order, error) {
	f.gotLimit, f.gotOffset = limit, offset
	return []domain.Order{}, f.err
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
