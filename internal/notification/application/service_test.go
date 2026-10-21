package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/notification/domain"
)

type notificationKey struct {
	kind    domain.Kind
	orderID uuid.UUID
}

// fakeRepo emulates the unique (kind, order_id) constraint and the transaction rollback.
type fakeRepo struct {
	sent map[notificationKey]*domain.Notification
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{sent: map[notificationKey]*domain.Notification{}}
}

func (f *fakeRepo) SendOnce(ctx context.Context, n *domain.Notification, send func(ctx context.Context) error) (bool, error) {
	key := notificationKey{n.Kind, n.OrderID}
	if _, ok := f.sent[key]; ok {
		return false, nil
	}
	if err := send(ctx); err != nil {
		return false, err
	}
	f.sent[key] = n
	return true, nil
}

func (f *fakeRepo) Sent(_ context.Context, kind domain.Kind, orderID uuid.UUID) (bool, error) {
	_, ok := f.sent[notificationKey{kind, orderID}]
	return ok, nil
}

type fakeCustomers struct{}

func (fakeCustomers) Email(_ context.Context, id uuid.UUID) (string, error) {
	return id.String() + "@example.com", nil
}

type fakeMailer struct {
	sent []Email
	err  error
}

func (f *fakeMailer) Send(_ context.Context, email Email) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, email)
	return nil
}

type discardLogger struct{}

func (discardLogger) Info(string, ...any) {}

func orderCreatedInput() OrderCreatedInput {
	return OrderCreatedInput{
		OrderID:     uuid.MustParse("4de9d7f2-d34d-4c4c-84a6-a06fe033cdf4"),
		CustomerID:  uuid.New(),
		Items:       []Item{{ProductID: "book-42", Quantity: 2, Price: 59900}},
		TotalAmount: 119800,
		Currency:    "RUB",
	}
}

func TestService_NotifyOrderCreated(t *testing.T) {
	mailer := &fakeMailer{}
	in := orderCreatedInput()

	err := NewService(newFakeRepo(), fakeCustomers{}, mailer, discardLogger{}).NotifyOrderCreated(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("sent %d emails, want 1", len(mailer.sent))
	}
	email := mailer.sent[0]
	if email.To != in.CustomerID.String()+"@example.com" {
		t.Errorf("to = %q", email.To)
	}
	if email.Subject != "Order #4DE9D7F2 received" {
		t.Errorf("subject = %q", email.Subject)
	}
	for _, want := range []string{"2 × book-42 — 1198.00 RUB", "Total: 1198.00 RUB"} {
		if !strings.Contains(email.Text, want) || !strings.Contains(email.HTML, want) {
			t.Errorf("email does not contain %q:\n%s", want, email.Text)
		}
	}
}

func TestService_DuplicateEventSendsOneEmail(t *testing.T) {
	mailer := &fakeMailer{}
	svc := NewService(newFakeRepo(), fakeCustomers{}, mailer, discardLogger{})
	in := orderCreatedInput()

	for range 2 {
		if err := svc.NotifyOrderCreated(context.Background(), in); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if len(mailer.sent) != 1 {
		t.Errorf("sent %d emails, want 1", len(mailer.sent))
	}
}

func TestService_MailerErrorAllowsRetry(t *testing.T) {
	repo := newFakeRepo()
	mailer := &fakeMailer{err: errors.New("smtp is down")}
	svc := NewService(repo, fakeCustomers{}, mailer, discardLogger{})
	in := orderCreatedInput()

	if err := svc.NotifyOrderCreated(context.Background(), in); err == nil {
		t.Fatal("expected error")
	}
	mailer.err = nil
	if err := svc.NotifyOrderCreated(context.Background(), in); err != nil {
		t.Fatalf("unexpected error on retry: %v", err)
	}
	if len(mailer.sent) != 1 {
		t.Errorf("sent %d emails after retry, want 1", len(mailer.sent))
	}
}

func TestService_DifferentKindsAreSentSeparately(t *testing.T) {
	mailer := &fakeMailer{}
	svc := NewService(newFakeRepo(), fakeCustomers{}, mailer, discardLogger{})
	orderID, customerID := uuid.New(), uuid.New()

	steps := []error{
		svc.NotifyPaymentFailed(context.Background(), PaymentFailedInput{
			OrderID: orderID, CustomerID: customerID, Amount: 100, Currency: "USD", Reason: "insufficient_funds"}),
		svc.NotifyPaymentSucceeded(context.Background(), PaymentSucceededInput{
			OrderID: orderID, CustomerID: customerID, Amount: 100, Currency: "USD", TransactionID: "tx"}),
		svc.NotifyOrderShipped(context.Background(), OrderShippedInput{
			OrderID: orderID, CustomerID: customerID, Carrier: "CDEK", TrackingNumber: "OF1",
			EstimatedDeliveryAt: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}),
	}
	for _, err := range steps {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if len(mailer.sent) != 3 {
		t.Fatalf("sent %d emails, want 3", len(mailer.sent))
	}
	if !strings.Contains(mailer.sent[0].Text, "Reason: insufficient funds") {
		t.Errorf("payment failed email: %s", mailer.sent[0].Text)
	}
	if !strings.Contains(mailer.sent[2].Text, "Estimated delivery: October 25, 2026") {
		t.Errorf("shipped email: %s", mailer.sent[2].Text)
	}
}

func TestService_Validation(t *testing.T) {
	err := NewService(newFakeRepo(), fakeCustomers{}, &fakeMailer{}, discardLogger{}).
		NotifyOrderCreated(context.Background(), OrderCreatedInput{CustomerID: uuid.New()})
	if !IsPermanentError(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
}

func TestFormatMoney(t *testing.T) {
	tests := map[int64]string{0: "0.00 RUB", 5: "0.05 RUB", 59900: "599.00 RUB", 123456: "1234.56 RUB", -150: "-1.50 RUB"}
	for amount, want := range tests {
		if got := formatMoney(amount, "RUB"); got != want {
			t.Errorf("formatMoney(%d) = %q, want %q", amount, got, want)
		}
	}
}

func TestContent_HTMLEscapesEventData(t *testing.T) {
	in := orderCreatedInput()
	in.Items[0].ProductID = "<script>alert(1)</script>"
	html, err := orderCreatedContent(in).html()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(html, "<script>") {
		t.Error("product ID is not escaped in HTML")
	}
}

func TestService_ShippedEmailWaitsForPaymentEmail(t *testing.T) {
	mailer := &fakeMailer{}
	svc := NewService(newFakeRepo(), fakeCustomers{}, mailer, discardLogger{})
	orderID, customerID := uuid.New(), uuid.New()
	shipped := OrderShippedInput{OrderID: orderID, CustomerID: customerID, Carrier: "CDEK", TrackingNumber: "OF1"}

	err := svc.NotifyOrderShipped(context.Background(), shipped)
	if !errors.Is(err, ErrOutOfOrder) || IsPermanentError(err) {
		t.Fatalf("expected a retryable ErrOutOfOrder, got %v", err)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("sent %d emails before the payment email, want 0", len(mailer.sent))
	}

	if err := svc.NotifyPaymentSucceeded(context.Background(), PaymentSucceededInput{
		OrderID: orderID, CustomerID: customerID, Amount: 100, Currency: "RUB", TransactionID: "tx"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.NotifyOrderShipped(context.Background(), shipped); err != nil { // the retry
		t.Fatalf("unexpected error on retry: %v", err)
	}
	if len(mailer.sent) != 2 || !strings.Contains(mailer.sent[1].Subject, "shipped") {
		t.Errorf("emails = %+v, want payment then shipped", mailer.sent)
	}
}
