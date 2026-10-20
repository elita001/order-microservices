package events

import (
	"time"

	"github.com/google/uuid"
)

// AggregateShipment is the aggregate type of delivery events.
const AggregateShipment = "shipment"

// DeliveryCreatedType is both the event type and the RabbitMQ routing key.
const DeliveryCreatedType = "delivery.created"

// DeliveryCreated is published when a paid order has been handed over to a carrier.
type DeliveryCreated struct {
	ShipmentID          uuid.UUID `json:"shipment_id"`
	OrderID             uuid.UUID `json:"order_id"`
	CustomerID          uuid.UUID `json:"customer_id"`
	Carrier             string    `json:"carrier"`
	TrackingNumber      string    `json:"tracking_number"`
	EstimatedDeliveryAt time.Time `json:"estimated_delivery_at"`
	CreatedAt           time.Time `json:"created_at"`
}
