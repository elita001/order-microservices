# OrderFlow

A microservices system for placing and processing orders, written in Go.

## Services

| Service              | Status    | Description                    |
|----------------------|-----------|--------------------------------|
| Order Service        | ✅ Step 3 | create, retrieve, and list orders; updates order status from payment results |
| Payment Service      | ✅ Step 3 | charges orders through a payment provider stub, publishes the result |
| Notification Service | ⏳        | notifications                  |
| API Gateway          | ⏳        | entry point                    |

## How it works

```
client ──POST /orders──▶ Order Service ──order.created──▶ RabbitMQ ──▶ Payment Service ──▶ provider stub
                              ▲                                              │
                              └──── payment.succeeded / payment.failed ◀─────┘
```

1. Order Service saves the order (`pending`) and the `order.created` event in one transaction.
2. Payment Service consumes `order.created`, charges the customer through the provider
   stub, and saves the payment and `payment.succeeded` or `payment.failed` in one transaction.
3. Order Service consumes the result and moves the order to `paid` or `payment_failed`.

Services share one PostgreSQL database, but each owns its tables and migrations and never
reads another service's tables: they communicate only through APIs and events.

## Running

```bash
make up
```

On the first run `make up` copies `.env.example` to `.env`. Edit `.env` to change
passwords, host ports (e.g., if `5432` is already taken), or the payment stub behavior;
it is ignored by git. Running `docker compose` directly without `.env` fails with a hint
to create it.

Configuration follows the 12-factor approach: services read settings only from
environment variables and contain no secrets or credentials. `.env` is for local
development only; in production, values come from the platform (Kubernetes Secrets,
Vault, CI/CD variables).

Management UIs:

- RabbitMQ: http://localhost:15672 (credentials `RABBITMQ_USER` / `RABBITMQ_PASSWORD` from `.env`, `app` / `app` by default)

Useful commands:

| Command         | Description                                          |
|-----------------|------------------------------------------------------|
| `make logs`     | follow the logs of all application services          |
| `make payments` | latest orders with their payment status              |
| `make outbox`   | latest outbox messages of all services and their delivery state |
| `make test`     | unit tests with the race detector                    |

## Example requests

```bash
curl -s -X POST 'localhost:8081/api/v1/orders' \
  -H 'Content-Type: application/json' \
  -d '{
    "customer_id": "7b0c8a3e-2f1d-4e5a-9b6c-1d2e3f4a5b6c",
    "currency": "RUB",
    "items": [{"product_id": "book-42", "quantity": 2, "price": 59900}]
  }'
```

The order is created as `pending`. About a second later it becomes `paid` or
`payment_failed` (by default the stub declines ~20% of payments):

```bash
curl -s 'localhost:8081/api/v1/orders/<id>'
```

List a customer's orders:

```bash
curl -s 'localhost:8081/api/v1/orders?customer_id=<customer-id>&limit=20&offset=0'
```

Amounts are stored in the currency's smallest units (kopecks): `59900` = RUB 599.00.

## Events

| Event               | Producer | Consumers | Payload contract                                 |
|---------------------|----------|-----------|--------------------------------------------------|
| `order.created`     | order    | payment   | [`pkg/events/order.go`](pkg/events/order.go)     |
| `payment.succeeded` | payment  | order     | [`pkg/events/payment.go`](pkg/events/payment.go) |
| `payment.failed`    | payment  | order     | [`pkg/events/payment.go`](pkg/events/payment.go) |

All events go through the `orderflow.events` topic exchange; the event type is the
routing key. Each consuming service has its own durable quorum queue (`order-service`,
`payment-service`) and declares the exchange, queue, and bindings on connect, so the
services can start in any order and messages wait in the queue while a consumer is down.

### Transactional outbox

Services never publish to RabbitMQ from business code. Instead:

1. The business data and the event row in the service's outbox table (`order_outbox`,
   `payment_outbox`) are written in **one database transaction**, so the event exists
   if and only if the data change exists.
2. A background **outbox relay** (runs inside each service) locks a batch of unpublished
   rows with `FOR UPDATE SKIP LOCKED`, publishes them, waits for **publisher confirms**,
   and only then sets `published_at`.
3. If RabbitMQ is down, the services keep working. The relay records `attempts` and
   `last_error`, retries with exponential backoff, and reconnects automatically.
4. Published rows are deleted after `OUTBOX_RETENTION`.

### Idempotent consumers

Delivery is **at-least-once**: an event can be delivered twice (e.g., the process crashes
after the broker confirm but before `published_at` is saved), so every handler is idempotent:

- **Payment Service** keeps one payment per order (`UNIQUE (order_id)`). A pending payment
  is created *before* calling the provider, and its ID is the provider's idempotency key:
  if processing is interrupted and the message is redelivered, the provider returns the
  same result instead of charging twice. A completed payment is never charged again.
- **Order Service** changes the status only from `pending`
  (`UPDATE ... WHERE status = 'pending'`), so repeated or late events change nothing.

Consumer error handling: a malformed message is rejected (it cannot succeed on retry); any
other error (e.g., the database is down) requeues the message after `CONSUMER_REQUEUE_DELAY`.
Quorum queues drop a message after 20 redeliveries, so a poison message cannot loop forever.

### Payment provider stub

The stub responds after a random delay between `PAYMENT_STUB_MIN_DELAY` and
`PAYMENT_STUB_MAX_DELAY` and declines a `PAYMENT_STUB_FAILURE_RATE` share of payments
(`insufficient_funds`, `card_declined`, `card_expired`). Like a real provider, it is
idempotent: the outcome is derived from the idempotency key.

## Configuration

Common variables (both services):

| Variable                    | Default            | Description                              |
|-----------------------------|--------------------|------------------------------------------|
| `HTTP_ADDR`                 | `:8081` / `:8082`  | listen address (order / payment)         |
| `POSTGRES_DSN`              | — (required)       | PostgreSQL connection string             |
| `RABBITMQ_URL`              | — (required)       | AMQP connection string                   |
| `RABBITMQ_EXCHANGE`         | `orderflow.events` | topic exchange for integration events    |
| `RABBITMQ_QUEUE`            | `<service>-service`| queue the service consumes               |
| `LOG_LEVEL`                 | `info`             | `debug`, `info`, `warn`, `error`         |
| `SHUTDOWN_TIMEOUT`          | `10s`              | time for in-flight requests on shutdown  |
| `OUTBOX_BATCH_SIZE`         | `100`              | messages per relay transaction           |
| `OUTBOX_POLL_INTERVAL`      | `1s`               | pause when the outbox is empty           |
| `OUTBOX_MAX_BACKOFF`        | `30s`              | maximum pause after failures             |
| `OUTBOX_PUBLISH_TIMEOUT`    | `5s`               | per message, including broker confirm    |
| `OUTBOX_BATCH_TIMEOUT`      | `8s`               | a started batch is finished on shutdown  |
| `OUTBOX_RETENTION`          | `168h`             | how long published rows are kept         |
| `OUTBOX_CLEANUP_INTERVAL`   | `1h`               | how often old rows are deleted           |
| `CONSUMER_PREFETCH`         | `10`               | unacknowledged messages per connection   |
| `CONSUMER_CONCURRENCY`      | `4`                | messages handled in parallel             |
| `CONSUMER_HANDLER_TIMEOUT`  | `10s`              | a started handler is finished on shutdown |
| `CONSUMER_REQUEUE_DELAY`    | `1s`               | pause before requeueing a failed message |
| `CONSUMER_MAX_BACKOFF`      | `30s`              | maximum pause between reconnects         |

Payment Service only:

| Variable                    | Default | Description                         |
|-----------------------------|---------|-------------------------------------|
| `PAYMENT_STUB_FAILURE_RATE` | `0.2`   | share of declined payments, 0..1    |
| `PAYMENT_STUB_MIN_DELAY`    | `100ms` | minimum provider response time      |
| `PAYMENT_STUB_MAX_DELAY`    | `1s`    | maximum provider response time      |

## Project structure

```
cmd/<service>/                           entry points, configuration, dependency wiring
internal/<service>/domain/               business entities, domain events and errors
internal/<service>/application/          use cases and ports
internal/<service>/adapter/http/         HTTP delivery adapter
internal/<service>/adapter/messaging/    RabbitMQ delivery adapter (event handlers)
internal/<service>/adapter/postgres/     PostgreSQL persistence adapter
internal/payment/adapter/provider/       payment provider adapters (stub)
pkg/                                     shared infrastructure: logger, HTTP helpers, shutdown
pkg/outbox/                              transactional outbox: storing messages and the relay
pkg/rabbitmq/                            RabbitMQ connection, publisher with confirms, consumer
pkg/events/                              integration event contracts shared between services
migrations/<service>/                    SQL migrations per service (golang-migrate)
```

The application layer depends on its ports and the domain, not on the HTTP, RabbitMQ,
or PostgreSQL adapters. The executable wires concrete adapters into the application service.

Create a migration: `make migrate-new service=payment name=add_refunds`.
