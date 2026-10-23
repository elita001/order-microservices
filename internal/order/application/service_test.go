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
	byKey    map[string]*domain.Order
}

func (f *fakeRepo) GetByIdempotencyKey(_ context.Context, customerID uuid.UUID, key string) (*domain.Order, error) {
	if order, ok := f.byKey[idemKey(customerID, key)]; ok {
		return order, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeRepo) Create(_ context.Context, order *domain.Order, events ...domain.Event) error {
	if f.err != nil {
		return f.err
	}
	if order.IdempotencyKey != "" {
		if f.byKey == nil {
			f.byKey = map[string]*domain.Order{}
		}
		// Like the unique (customer_id, idempotency_key) constraint.
		if _, ok := f.byKey[idemKey(order.CustomerID, order.IdempotencyKey)]; ok {
			return domain.ErrDuplicateIdempotencyKey
		}
		f.byKey[idemKey(order.CustomerID, order.IdempotencyKey)] = order
	}
	f.created = order
	f.events = events
	return f.err
}

func (f *fakeRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Order, error) {
	if f.created != nil && f.created.ID == id {
		return f.created, nil
	}
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

func idemKey(customerID uuid.UUID, key string) string { return customerID.String() + "|" + key }

type discardLogger struct{}

func (discardLogger) InfoContext(context.Context, string, ...any) {}

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
	result, err := newTestService(repo).Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	order := result.Order
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
	result, err := newTestService(repo).Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	order := result.Order
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
			customer := uuid.New()
			result, err := newTestService(repo).ListByCustomer(context.Background(),
				ListOrdersInput{Viewer: Viewer{CustomerID: customer}, CustomerID: customer, Limit: tt.limit})
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

func TestService_Create_IdempotencyKeyReplaysOrder(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo)
	in := validInput()
	in.IdempotencyKey = "key-1"

	first, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.Replayed || !second.Replayed {
		t.Errorf("replayed = %v, %v, want false, true", first.Replayed, second.Replayed)
	}
	if first.Order.ID != second.Order.ID {
		t.Errorf("replay returned order %s, want %s", second.Order.ID, first.Order.ID)
	}
}

func TestService_Create_IdempotencyKeyWithDifferentBody(t *testing.T) {
	svc := newTestService(&fakeRepo{})
	in := validInput()
	in.IdempotencyKey = "key-1"
	if _, err := svc.Create(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	in.Items[0].Quantity++
	if _, err := svc.Create(context.Background(), in); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("expected ErrIdempotencyKeyReused, got %v", err)
	}
}

// racingRepo emulates a concurrent request that created the order between the
// replay check and the insert.
type racingRepo struct {
	fakeRepo
	winner *domain.Order
}

func (r *racingRepo) Create(_ context.Context, order *domain.Order, _ ...domain.Event) error {
	winner := *order
	winner.ID = uuid.New()
	r.winner = &winner
	r.byKey = map[string]*domain.Order{idemKey(order.CustomerID, order.IdempotencyKey): &winner}
	return domain.ErrDuplicateIdempotencyKey
}

func TestService_Create_ConcurrentDuplicateReturnsWinner(t *testing.T) {
	repo := &racingRepo{}
	in := validInput()
	in.IdempotencyKey = "key-1"

	result, err := newTestService(repo).Create(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Replayed || result.Order.ID != repo.winner.ID {
		t.Errorf("result = %+v, want the concurrently created order", result)
	}
}

func TestService_Get_OnlyOwnOrdersUnlessSupport(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo)
	in := validInput()
	created, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	id := created.Order.ID

	if _, err := svc.Get(context.Background(), Viewer{CustomerID: in.CustomerID}, id); err != nil {
		t.Errorf("owner: %v", err)
	}
	// Another customer's order looks like a missing one: its existence is not revealed.
	if _, err := svc.Get(context.Background(), Viewer{CustomerID: uuid.New()}, id); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another customer: got %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(context.Background(), Viewer{CustomerID: uuid.New(), SeeAll: true}, id); err != nil {
		t.Errorf("support: %v", err)
	}
}

func TestService_ListByCustomer_Authorization(t *testing.T) {
	me, someone := uuid.New(), uuid.New()
	tests := []struct {
		name    string
		in      ListOrdersInput
		wantErr error
		wantFor uuid.UUID
	}{
		{"own orders by default", ListOrdersInput{Viewer: Viewer{CustomerID: me}}, nil, me},
		{"another customer's orders", ListOrdersInput{Viewer: Viewer{CustomerID: me}, CustomerID: someone}, domain.ErrForbidden, uuid.Nil},
		{"support lists any customer", ListOrdersInput{Viewer: Viewer{CustomerID: me, SeeAll: true}, CustomerID: someone}, nil, someone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &listRecorder{}
			_, err := newTestService(repo).ListByCustomer(context.Background(), tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
			if repo.customer != tt.wantFor {
				t.Errorf("listed orders of %s, want %s", repo.customer, tt.wantFor)
			}
		})
	}
}

type listRecorder struct {
	fakeRepo
	customer uuid.UUID
}

func (r *listRecorder) ListByCustomer(_ context.Context, customerID uuid.UUID, _, _ int) ([]domain.Order, error) {
	r.customer = customerID
	return nil, nil
}

// Idempotency keys belong to a customer: another customer with the same key and body
// creates their own order instead of getting someone else's.
func TestService_Create_IdempotencyKeyIsPerCustomer(t *testing.T) {
	svc := newTestService(&fakeRepo{})
	first, second := validInput(), validInput() // different customers
	first.IdempotencyKey, second.IdempotencyKey = "same-key", "same-key"

	a, err := svc.Create(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Create(context.Background(), second)
	if err != nil {
		t.Fatalf("second customer: %v", err)
	}
	if b.Replayed || a.Order.ID == b.Order.ID {
		t.Error("the second customer must get a new order, not a replay of the first one's")
	}
}
