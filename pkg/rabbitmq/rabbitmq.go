// Package rabbitmq contains shared RabbitMQ helpers: connection setup, topology
// declaration, and a publisher with publisher confirms and automatic reconnection.
package rabbitmq

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	dialTimeout = 5 * time.Second
	heartbeat   = 10 * time.Second
)

// Dial opens a connection named after the service, so it is identifiable in the management UI.
func Dial(url, name string) (*amqp.Connection, error) {
	conn, err := amqp.DialConfig(url, amqp.Config{
		Heartbeat:  heartbeat,
		Locale:     "en_US",
		Dial:       amqp.DefaultDial(dialTimeout),
		Properties: amqp.Table{"connection_name": name},
	})
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}
	return conn, nil
}

// DeclareExchange declares a durable topic exchange. Declaration is idempotent,
// so every service declares the exchanges it uses on (re)connect.
func DeclareExchange(ch *amqp.Channel, name string) error {
	if err := ch.ExchangeDeclare(name, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %q: %w", name, err)
	}
	return nil
}
