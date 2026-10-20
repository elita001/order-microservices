package domain

// Event is a fact about a shipment that other services may react to.
// Events are persisted atomically with the shipment and published via the transactional outbox.
type Event interface {
	isEvent()
}

// ShipmentRegistered is raised when the carrier has accepted the parcel.
type ShipmentRegistered struct {
	Shipment Shipment
}

func (ShipmentRegistered) isEvent() {}
