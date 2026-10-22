package rabbitmq

import (
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/elita001/order-microservices/pkg/telemetry"
)

var tracer = telemetry.Tracer("github.com/elita001/order-microservices/pkg/rabbitmq")

// headerCarrier lets the OpenTelemetry propagator read and write AMQP headers.
type headerCarrier amqp.Table

func (c headerCarrier) Get(key string) string {
	if value, ok := c[key].(string); ok {
		return value
	}
	return ""
}

func (c headerCarrier) Set(key, value string) {
	c[key] = value
}

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}
