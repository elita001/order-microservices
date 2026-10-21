package rabbitmq

import (
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Headers added to messages that are retried or dead-lettered.
const (
	HeaderRetryCount         = "x-retry-count"          // failed attempts so far
	HeaderOriginalRoutingKey = "x-original-routing-key" // the event type
	HeaderLastError          = "x-last-error"
	HeaderError              = "x-error" // the final error, in the DLQ
	HeaderOriginalQueue      = "x-original-queue"
	HeaderFailedAt           = "x-failed-at"
)

const maxErrorLength = 1000

// DeadLetterQueue is where messages end up after a permanent error or the last retry.
func DeadLetterQueue(queue string) string {
	return queue + ".dlq"
}

// RetryQueue holds messages for delay before returning them to queue, e.g. "payment-service.retry.30s".
func RetryQueue(queue string, delay time.Duration) string {
	name := delay.String() // "2m0s" -> "2m", "1h0m0s" -> "1h"
	if strings.HasSuffix(name, "m0s") {
		name = strings.TrimSuffix(name, "0s")
	}
	if strings.HasSuffix(name, "h0m") {
		name = strings.TrimSuffix(name, "0m")
	}
	return queue + ".retry." + name
}

// RetryCount returns how many times processing of the message has already failed.
func RetryCount(headers amqp.Table) int {
	switch v := headers[HeaderRetryCount].(type) {
	case int:
		return v
	case int8:
		return int(v)
	case int16:
		return int(v)
	case int32:
		return int(v)
	case int64:
		return int(v)
	case uint8:
		return int(v)
	case uint16:
		return int(v)
	case uint32:
		return int(v)
	}
	return 0
}

// OriginalRoutingKey returns the event type of the message. After a retry the AMQP routing
// key is the queue name, so the original one travels in a header.
func OriginalRoutingKey(d amqp.Delivery) string {
	if key, ok := d.Headers[HeaderOriginalRoutingKey].(string); ok && key != "" {
		return key
	}
	return d.RoutingKey
}

func retryHeaders(d amqp.Delivery, routingKey string, retry int, err error) amqp.Table {
	headers := copyHeaders(d.Headers)
	headers[HeaderRetryCount] = int32(retry)
	headers[HeaderOriginalRoutingKey] = routingKey
	headers[HeaderLastError] = truncate(err.Error())
	return headers
}

func deadLetterHeaders(d amqp.Delivery, routingKey, queue string, retry int, err error) amqp.Table {
	headers := copyHeaders(d.Headers)
	delete(headers, HeaderLastError)
	headers[HeaderRetryCount] = int32(retry)
	headers[HeaderOriginalRoutingKey] = routingKey
	headers[HeaderOriginalQueue] = queue
	headers[HeaderError] = truncate(err.Error())
	headers[HeaderFailedAt] = time.Now().UTC().Format(time.RFC3339)
	return headers
}

// copyHeaders drops the headers RabbitMQ adds on dead-lettering (x-death and friends):
// they grow with every retry and describe the broker's routing, not the failure.
func copyHeaders(src amqp.Table) amqp.Table {
	dst := amqp.Table{}
	for key, value := range src {
		if key == "x-death" || strings.HasPrefix(key, "x-first-death-") || strings.HasPrefix(key, "x-last-death-") {
			continue
		}
		dst[key] = value
	}
	return dst
}

func truncate(s string) string {
	if len(s) <= maxErrorLength {
		return s
	}
	return s[:maxErrorLength] + "…"
}
