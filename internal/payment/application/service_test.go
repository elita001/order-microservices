package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/payment/domain"
)

// fakeRepo keeps payments by order ID, like the unique constraint in the database.
type fakeRepo struct {
	byOrder map[uuid.UUID]*domain.Payment
	events  []domain.Event
	err     error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byOrder: map[uuid.UUID]*domain.Payment{}}
}

func (f *fakeRepo) GetOrCreate(_ context.Context, p *domain.Payment) (*domain.Payment, error) {
	if f.err != nil {
		return nil, f.err
	}
	if existing, ok := f.byOrder[p.OrderID]; ok {
		copied := *existing
		return &copied, nil
	}
	copied := *p
	f.byOrder[p.OrderID] = &copied
	return p, nil
}

func (f *fakeRepo) Complete(_ context.Context, p *domain.Payment, events ...domain.Event) (bool, error) {
	stored := f.byOrder[p.OrderID]
	if stored.Status != domain.StatusPending {
		return false, nil
	}
	*stored = *p
	f.events = append(f.events, events...)
	return true, nil
}

type fakeProvider struct {
	result ChargeResult
	err    error
	calls  int
}

func (f *fakeProvider) Charge(context.Context, ChargeRequest) (ChargeResult, error) {
	f.calls++
	return f.result, f.err
}

type discardLogger struct{}

func (discardLogger) Info(string, ...any) {}

func validInput() ProcessPaymentInput {
	return ProcessPaymentInput{OrderID: uuid.New(), CustomerID: uuid.New(), Amount: 119800, Currency: "RUB"}
}

func TestService_ProcessPayment_Approved(t *testing.T) {
	repo := newFakeRepo()
	provider := &fakeProvider{result: ChargeResult{Approved: true, TransactionID: "tx-1"}}
	in := validInput()

	if err := NewService(repo, provider, discardLogger{}).ProcessPayment(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	payment := repo.byOrder[in.OrderID]
	if payment.Status != domain.StatusSucceeded || payment.TransactionID != "tx-1" {
		t.Errorf("payment = %+v, want succeeded with tx-1", payment)
	}
	if len(repo.events) != 1 {
		t.Fatalf("got %d events, want 1", len(repo.events))
	}
	if _, ok := repo.events[0].(domain.PaymentSucceeded); !ok {
		t.Errorf("event is %T, want domain.PaymentSucceeded", repo.events[0])
	}
}

func TestService_ProcessPayment_Declined(t *testing.T) {
	repo := newFakeRepo()
	provider := &fakeProvider{result: ChargeResult{Approved: false, DeclineReason: "insufficient_funds"}}
	in := validInput()

	if err := NewService(repo, provider, discardLogger{}).ProcessPayment(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	payment := repo.byOrder[in.OrderID]
	if payment.Status != domain.StatusFailed || payment.FailureReason != "insufficient_funds" {
		t.Errorf("payment = %+v, want failed with insufficient_funds", payment)
	}
	if _, ok := repo.events[0].(domain.PaymentFailed); !ok {
		t.Errorf("event is %T, want domain.PaymentFailed", repo.events[0])
	}
}

func TestService_ProcessPayment_DuplicateDoesNotChargeTwice(t *testing.T) {
	repo := newFakeRepo()
	provider := &fakeProvider{result: ChargeResult{Approved: true}}
	svc := NewService(repo, provider, discardLogger{})
	in := validInput()

	for range 2 {
		if err := svc.ProcessPayment(context.Background(), in); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if provider.calls != 1 {
		t.Errorf("provider called %d times, want 1", provider.calls)
	}
	if len(repo.events) != 1 {
		t.Errorf("got %d events, want 1", len(repo.events))
	}
}

func TestService_ProcessPayment_ProviderErrorLeavesPaymentPending(t *testing.T) {
	repo := newFakeRepo()
	providerErr := errors.New("provider timeout")
	provider := &fakeProvider{err: providerErr}
	in := validInput()

	err := NewService(repo, provider, discardLogger{}).ProcessPayment(context.Background(), in)
	if !errors.Is(err, providerErr) {
		t.Fatalf("expected provider error, got %v", err)
	}
	if status := repo.byOrder[in.OrderID].Status; status != domain.StatusPending {
		t.Errorf("status = %q, want pending so that a retry can complete it", status)
	}
	if len(repo.events) != 0 {
		t.Errorf("got %d events, want none", len(repo.events))
	}
}

func TestService_ProcessPayment_Validation(t *testing.T) {
	tests := map[string]func(in *ProcessPaymentInput){
		"empty order":    func(in *ProcessPaymentInput) { in.OrderID = uuid.Nil },
		"empty customer": func(in *ProcessPaymentInput) { in.CustomerID = uuid.Nil },
		"zero amount":    func(in *ProcessPaymentInput) { in.Amount = 0 },
		"bad currency":   func(in *ProcessPaymentInput) { in.Currency = "RUBLE" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			mutate(&in)
			err := NewService(newFakeRepo(), &fakeProvider{}, discardLogger{}).ProcessPayment(context.Background(), in)
			if !IsValidationError(err) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}
}
