# OrderFlow

A microservices system for placing and processing orders, written in Go.

## Services

| Service              | Status    | Description                    |
|----------------------|-----------|--------------------------------|
| API Gateway          | ✅ Step 5 | the single entry point: request IDs, rate limiting, idempotency, timeouts |
| Order Service        | ✅ Step 5 | create, retrieve, and list orders; tracks order status from payment and delivery events |
| Payment Service      | ✅ Step 3 | charges orders through a payment provider stub, publishes the result |
| Delivery Service     | ✅ Step 4 | hands paid orders over to a carrier stub, publishes the tracking number |
| Notification Service | ✅ Step 4 | emails customers about every order step (Mailpit in development) |

## How it works

```
client ──▶ API Gateway ──▶ Order Service ─── order.created ───▶ Payment Service ──▶ payment provider stub
             │ Redis
                              ▲   ▲                                │
                              │   └──── payment.succeeded/failed ◀─┤
                              │                                    ▼
                              └──────── delivery.created ◀──── Delivery Service ──▶ carrier stub

             all events ──▶ Notification Service ──SMTP──▶ Mailpit (customer emails)
```

1. The API gateway checks the rate limit and the `Idempotency-Key` in Redis and forwards
   the request; Order Service saves the order (`pending`) and the `order.created` event
   in one transaction.
2. Payment Service consumes `order.created`, charges the customer through the provider
   stub, and saves the payment and `payment.succeeded` or `payment.failed` in one transaction.
3. Order Service moves the order to `paid` or `payment_failed`.
4. Delivery Service consumes `payment.succeeded`, registers a parcel with the carrier stub,
   and publishes `delivery.created` with the tracking number; the order becomes `shipped`.
5. Notification Service emails the customer on each step: order received, payment received
   or failed, order shipped.

Order statuses: `pending` → `paid` → `shipped`, or `pending` → `payment_failed`.

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

Clients use the API gateway at http://localhost:8080. The services' own ports
(8081–8084) are published for debugging only.

Management UIs:

- RabbitMQ: http://localhost:15672 (credentials `RABBITMQ_USER` / `RABBITMQ_PASSWORD` from `.env`, `app` / `app` by default)
- Mailpit (all emails sent by the system): http://localhost:8025

Useful commands:

| Command         | Description                                          |
|-----------------|------------------------------------------------------|
| `make logs`     | follow the logs of all application services          |
| `make orders`   | latest orders with their payment, shipment, and sent emails |
| `make outbox`   | latest outbox messages of all services and their delivery state |
| `make test`     | unit tests with the race detector                    |

## Example requests

```bash
curl -s -X POST 'localhost:8080/api/v1/orders' \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{
    "customer_id": "7b0c8a3e-2f1d-4e5a-9b6c-1d2e3f4a5b6c",
    "currency": "RUB",
    "items": [{"product_id": "book-42", "quantity": 2, "price": 59900}]
  }'
```

The order is created as `pending`. A second or two later it becomes `shipped` or
`payment_failed` (by default the payment stub declines ~20% of payments), and the
emails appear in Mailpit:

```bash
curl -s 'localhost:8080/api/v1/orders/<id>'
```

List a customer's orders:

```bash
curl -s 'localhost:8080/api/v1/orders?customer_id=<customer-id>&limit=20&offset=0'
```

Amounts are stored in the currency's smallest units (kopecks): `59900` = RUB 599.00.

## API Gateway

The gateway is the single entry point for clients. It contains no business logic and
applies the edge concerns to every request:

- **Request ID.** The client's `X-Request-Id` is kept if it is short and safe to log,
  otherwise a new one is generated. It is returned in the response and forwarded to the
  services, so the logs of the gateway and of Order Service share one `request_id`.
- **Rate limiting** per client IP in Redis (GCRA, exact across gateway replicas):
  `RATE_LIMIT_WRITE_*` for `POST`, `RATE_LIMIT_READ_*` for `GET`. Over the limit the
  client gets `429` with `Retry-After`.
- **Idempotency.** A client can safely retry `POST /orders` (e.g., after a timeout) with
  the same `Idempotency-Key` header, and only one order is created:

  | Situation | Response |
  |---|---|
  | First request | goes to Order Service; the response is stored in Redis for `IDEMPOTENCY_TTL` |
  | Repeat after it | the stored response with `Idempotent-Replayed: true` |
  | Repeat while the first is in progress | `409` with `Retry-After: 1` |
  | Same key, different body | `422 idempotency_key_reused` |
  | First request failed with 5xx | not stored, the client can retry |

- **Timeouts.** An upstream that cannot be reached gets `502`, one that does not respond
  within `UPSTREAM_TIMEOUT` gets `504`; the client never waits indefinitely.

**Redis is an accelerator, not a source of truth.** Every Redis call is bounded by
`REDIS_OPERATION_TIMEOUT` (a hanging Redis is worse than a failed one), and on errors the
gateway fails open: requests pass without rate limiting, and idempotency is enforced by
Order Service itself, which stores the key in `orders.idempotency_key` with a unique
constraint and replays the existing order. The two layers complement each other: Redis
answers repeats fast and keeps them away from the services, and the database guarantees
correctness when Redis is down or its entry has expired.

## Events

| Event               | Producer | Consumers | Payload contract                                 |
|---------------------|----------|-----------|--------------------------------------------------|
| `order.created`     | order    | payment, notification | [`pkg/events/order.go`](pkg/events/order.go) |
| `payment.succeeded` | payment  | order, delivery, notification | [`pkg/events/payment.go`](pkg/events/payment.go) |
| `payment.failed`    | payment  | order, notification | [`pkg/events/payment.go`](pkg/events/payment.go) |
| `delivery.created`  | delivery | order, notification | [`pkg/events/delivery.go`](pkg/events/delivery.go) |

All events go through the `orderflow.events` topic exchange; the event type is the
routing key. Each consuming service has its own durable quorum queue (`order-service`,
`payment-service`, `delivery-service`, `notification-service`) and declares the exchange, queue, and bindings on connect, so the
services can start in any order and messages wait in the queue while a consumer is down.

### Transactional outbox

Services never publish to RabbitMQ from business code. Instead:

1. The business data and the event row in the service's outbox table (`order_outbox`,
   `payment_outbox`, `delivery_outbox`) are written in **one database transaction**, so the event exists
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
- **Delivery Service** works the same way: one shipment per order, and the shipment ID is
  the carrier's idempotency key.
- **Order Service** changes the status only from the expected statuses
  (`UPDATE ... WHERE status = ANY(...)`), so repeated or late events change nothing.
  Events from different services can arrive out of order (e.g., `delivery.created` before
  `payment.succeeded`); a shipment implies payment, so `pending` → `shipped` is allowed
  and the late payment event is ignored.
- **Notification Service** sends at most one email of each kind per order
  (`UNIQUE (kind, order_id)`). The row is inserted and the email is sent in one
  transaction: a concurrent duplicate waits on the unique index, and a failed send rolls
  the row back so that a retry sends again. A crash right after sending can still cause
  a rare duplicate email: SMTP offers no stronger guarantee.

Consumer error handling: a malformed message is rejected (it cannot succeed on retry); any
other error (e.g., the database is down) requeues the message after `CONSUMER_REQUEUE_DELAY`.
Quorum queues drop a message after 20 redeliveries, so a poison message cannot loop forever.

### External service stubs

| Stub | Behavior |
|------|----------|
| Payment provider | random delay; declines a `PAYMENT_STUB_FAILURE_RATE` share of payments (`insufficient_funds`, `card_declined`, `card_expired`) — a business outcome, published as `payment.failed` |
| Carrier | random delay; fails a `DELIVERY_STUB_ERROR_RATE` share of calls with a technical error — the consumer retries until it succeeds |
| Customer directory | returns `customer-<first 8 chars of customer ID>@example.com` |
| Email | real SMTP to [Mailpit](https://mailpit.axllent.org), which catches every email |

Like real providers, the payment and carrier stubs are idempotent: the outcome (decline,
tracking number) is derived from the idempotency key, so a retry gets the same result.

## Configuration

API gateway:

| Variable                    | Default  | Description                                    |
|-----------------------------|----------|------------------------------------------------|
| `HTTP_ADDR`                 | `:8080`  | listen address                                 |
| `ORDER_SERVICE_URL`         | — (required) | Order Service base URL                     |
| `REDIS_URL`                 | — (required) | `redis://:password@host:6379/0`            |
| `REDIS_OPERATION_TIMEOUT`   | `300ms`  | one Redis call including dialing and retries   |
| `REDIS_DIAL_TIMEOUT`, `REDIS_READ_TIMEOUT`, `REDIS_WRITE_TIMEOUT` | `500ms`, `200ms`, `200ms` | Redis client timeouts |
| `UPSTREAM_TIMEOUT`          | `5s`     | whole upstream request                         |
| `UPSTREAM_DIAL_TIMEOUT`     | `2s`     | connecting to the upstream                     |
| `RATE_LIMIT_READ_RPS`, `RATE_LIMIT_READ_BURST`   | `50`, `100` | GET requests per second per client IP and burst |
| `RATE_LIMIT_WRITE_RPS`, `RATE_LIMIT_WRITE_BURST` | `5`, `10`   | POST requests per second per client IP and burst |
| `IDEMPOTENCY_TTL`           | `24h`    | how long a response is replayed                |
| `IDEMPOTENCY_LOCK_TTL`      | `30s`    | lock of a request in progress (crash safety)   |
| `LOG_LEVEL`, `SHUTDOWN_TIMEOUT` | `info`, `10s` | as in the services                     |

Common variables of the services:

| Variable                    | Default            | Description                              |
|-----------------------------|--------------------|------------------------------------------|
| `HTTP_ADDR`                 | `:8081`–`:8084`    | listen address (order, payment, delivery, notification) |
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

The notification service has no outbox, so the `OUTBOX_*` variables do not apply to it.

Service-specific variables:

| Service      | Variable                    | Default | Description                              |
|--------------|-----------------------------|---------|------------------------------------------|
| payment      | `PAYMENT_STUB_FAILURE_RATE` | `0.2`   | share of declined payments, 0..1         |
| payment      | `PAYMENT_STUB_MIN_DELAY`    | `100ms` | minimum provider response time           |
| payment      | `PAYMENT_STUB_MAX_DELAY`    | `1s`    | maximum provider response time           |
| delivery     | `DELIVERY_STUB_ERROR_RATE`  | `0.1`   | share of carrier calls failing with a technical error |
| delivery     | `DELIVERY_STUB_MIN_DELAY`   | `100ms` | minimum carrier response time            |
| delivery     | `DELIVERY_STUB_MAX_DELAY`   | `500ms` | maximum carrier response time            |
| notification | `SMTP_HOST`, `SMTP_PORT`    | `localhost`, `1025` | SMTP server                  |
| notification | `SMTP_USERNAME`, `SMTP_PASSWORD` | — | credentials; empty disables authentication |
| notification | `SMTP_TLS`                  | `false` | require STARTTLS                         |
| notification | `SMTP_FROM`                 | `OrderFlow <noreply@orderflow.local>` | sender |
| notification | `SMTP_TIMEOUT`              | `10s`   | timeout of one SMTP session              |

## Project structure

```
cmd/<service>/                           entry points, configuration, dependency wiring
internal/gateway/                        API gateway: routing, proxy, rate limiting, idempotency
internal/<service>/domain/               business entities, domain events and errors
internal/<service>/application/          use cases and ports
internal/<service>/adapter/http/         HTTP delivery adapter
internal/<service>/adapter/messaging/    RabbitMQ delivery adapter (event handlers)
internal/<service>/adapter/postgres/     PostgreSQL persistence adapter
internal/<service>/adapter/provider/     external provider adapters: payment provider, carrier (stubs)
internal/notification/adapter/smtp/      email delivery over SMTP
internal/notification/adapter/customer/  customer directory (stub)
pkg/                                     shared infrastructure: logger, HTTP helpers, shutdown
pkg/outbox/                              transactional outbox: storing messages and the relay
pkg/rabbitmq/                            RabbitMQ connection, publisher with confirms, consumer
pkg/events/                              integration event contracts shared between services
pkg/redisclient/                         Redis client with fail-fast timeouts
migrations/<service>/                    SQL migrations per service (golang-migrate)
```

The application layer depends on its ports and the domain, not on the HTTP, RabbitMQ,
or PostgreSQL adapters. The executable wires concrete adapters into the application service.

Create a migration: `make migrate-new service=payment name=add_refunds`.
