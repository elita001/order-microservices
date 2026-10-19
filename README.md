# OrderFlow

A microservices system for placing and processing orders, written in Go.

## Services

| Service              | Status    | Description                    |
|----------------------|-----------|--------------------------------|
| Order Service        | ✅ Step 1 | create, retrieve, and list orders     |
| Payment Service      | ⏳        | payment simulation             |
| Notification Service | ⏳        | notifications                  |
| API Gateway          | ⏳        | entry point                    |

## Running

```bash
go mod tidy   # run once to download dependencies and create go.sum
make up
```

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

## Project structure

```
cmd/<service>/                          entry points, configuration, dependency wiring
internal/<service>/domain/              business entities and domain errors
internal/<service>/application/         use cases and ports
internal/<service>/adapter/http/        HTTP delivery adapter
internal/<service>/adapter/postgres/    PostgreSQL persistence adapter
pkg/                                    shared infrastructure: logger, HTTP helpers, shutdown
migrations/                             SQL migrations (golang-migrate)
```

The application layer depends on its repository port and the domain, not on
the HTTP or PostgreSQL adapters. The executable wires concrete adapters into
the application service.
