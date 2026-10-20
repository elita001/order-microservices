# OrderFlow

A microservices system for placing and processing orders, written in Go.

## Services

| Service              | Status    | Description                    |
|----------------------|-----------|--------------------------------|
| Order Service        | ✅ Step 2 | create, retrieve, and list orders; publishes `order.created` via transactional outbox |
| Payment Service      | ⏳        | payment simulation             |
| Notification Service | ⏳        | notifications                  |
| API Gateway          | ⏳        | entry point                    |

## Running

```bash
make up
```

On the first run `make up` copies `.env.example` to `.env`. Edit `.env` to change
passwords or host ports (e.g., if `5432` is already taken); it is ignored by git.
Running `docker compose` directly without `.env` fails with a hint to create it.

Configuration follows the 12-factor approach: services read settings only from
environment variables and contain no secrets or credentials. `.env` is for local
development only; in production, values come from the platform (Kubernetes Secrets,
Vault, CI/CD variables).

Management UIs:

- RabbitMQ: http://localhost:15672 (credentials `RABBITMQ_USER` / `RABBITMQ_PASSWORD` from `.env`, `app` / `app` by default)

## Example requests

```bash
curl -s -X POST 'localhost:8081/api/v1/orders' \
  -H 'Content-Type: application/json' \
  -d '{
    "customer_id": "7b0c8a3e-2f1d-4e5a-9b6c-1d2e3f4a5b6c",
    "currency": "RUB",
    "items": [{"product_id": "book-42", "quantity": 2, "price": 59900}]
  }'

curl -s 'localhost:8081/api/v1/orders/<id>'
```

List a customer's orders:

```bash
curl -s 'localhost:8081/api/v1/orders?customer_id=<customer-id>&limit=20&offset=0'
```

Amounts are stored in the currency's smallest units (kopecks): `59900` = RUB 599.00.

## Events and the transactional outbox

Order Service never publishes to RabbitMQ from the request handler. Instead:

1. The order, its items, and an `order.created` event row in the `outbox` table are
   written in **one database transaction**, so the event exists if and only if the order exists.
2. A background **outbox relay** (runs inside the service) locks a batch of unpublished
   rows with `FOR UPDATE SKIP LOCKED`, publishes them to the `orderflow.events` topic
   exchange, waits for **publisher confirms**, and only then sets `published_at`.
3. If RabbitMQ is down, orders are still accepted. The relay records `attempts` and
   `last_error`, retries with exponential backoff, and reconnects automatically.
4. Published rows are deleted after `OUTBOX_RETENTION`.

Delivery is **at-least-once**: an event can be delivered twice (e.g., the process crashes
after the broker confirm but before `published_at` is saved). Every message carries a
unique `message_id` (UUIDv7) that consumers use for deduplication.

| Event           | Routing key     | Payload contract                     |
|-----------------|-----------------|--------------------------------------|
| `order.created` | `order.created` | [`pkg/events/order.go`](pkg/events/order.go) |

### Watching events locally

No service consumes events yet, so RabbitMQ drops them. To see them, bind a debug queue
(the exchange is declared on the first publish, so create one order first; the
commands use the default credentials from `.env.example`):

```bash
curl -u app:app -X PUT localhost:15672/api/queues/%2F/debug.events \
  -H 'content-type: application/json' -d '{"durable":false}'
curl -u app:app -X POST localhost:15672/api/bindings/%2F/e/orderflow.events/q/debug.events \
  -H 'content-type: application/json' -d '{"routing_key":"#"}'
```

Then create an order and open the queue in the management UI ("Get messages").
`make outbox` shows the latest outbox rows and their delivery state.

## Configuration

| Variable                  | Default            | Description                              |
|---------------------------|--------------------|------------------------------------------|
| `HTTP_ADDR`               | `:8081`            | listen address                           |
| `POSTGRES_DSN`            | — (required)       | PostgreSQL connection string             |
| `RABBITMQ_URL`            | — (required)       | AMQP connection string                   |
| `RABBITMQ_EXCHANGE`       | `orderflow.events` | topic exchange for integration events    |
| `LOG_LEVEL`               | `info`             | `debug`, `info`, `warn`, `error`         |
| `SHUTDOWN_TIMEOUT`        | `10s`              | time for in-flight requests on shutdown  |
| `OUTBOX_BATCH_SIZE`       | `100`              | messages per relay transaction           |
| `OUTBOX_POLL_INTERVAL`    | `1s`               | pause when the outbox is empty           |
| `OUTBOX_MAX_BACKOFF`      | `30s`              | maximum pause after failures             |
| `OUTBOX_PUBLISH_TIMEOUT`  | `5s`               | per message, including broker confirm    |
| `OUTBOX_BATCH_TIMEOUT`    | `8s`               | a started batch is finished on shutdown  |
| `OUTBOX_RETENTION`        | `168h`             | how long published rows are kept        |
| `OUTBOX_CLEANUP_INTERVAL` | `1h`               | how often old rows are deleted           |

## Project structure

```
cmd/<service>/                          entry points, configuration, dependency wiring
internal/<service>/domain/              business entities and domain errors
internal/<service>/application/         use cases and ports
internal/<service>/adapter/http/        HTTP delivery adapter
internal/<service>/adapter/postgres/    PostgreSQL persistence adapter
pkg/                                    shared infrastructure: logger, HTTP helpers, shutdown
pkg/outbox/                             transactional outbox: storing messages and the relay
pkg/rabbitmq/                           RabbitMQ connection, exchange, publisher with confirms
pkg/events/                             integration event contracts shared between services
migrations/                             SQL migrations (golang-migrate)
```

The application layer depends on its repository port and the domain, not on
the HTTP or PostgreSQL adapters. The executable wires concrete adapters into
the application service.
