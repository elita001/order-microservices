package domain

// Event is a fact about a payment that other services may react to.
// Events are persisted atomically with the payment and published via the transactional outbox.
type Event interface {
	isEvent()
}

// PaymentSucceeded is raised when the provider has charged the customer.
type PaymentSucceeded struct {
	Payment Payment
}

// PaymentFailed is raised when the provider has declined the payment.
type PaymentFailed struct {
	Payment Payment
}

func (PaymentSucceeded) isEvent() {}
func (PaymentFailed) isEvent()    {}
