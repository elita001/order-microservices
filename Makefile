# Local settings (ports, credentials) for the targets that talk to the services from the host.
-include .env
RABBITMQ_URL ?= amqp://$(RABBITMQ_USER):$(RABBITMQ_PASSWORD)@localhost:$(or $(RABBITMQ_PORT),5672)/
SERVICE_QUEUES = order-service payment-service delivery-service notification-service

.PHONY: up down logs ps test tidy migrate-new outbox orders dlq-list dlq-replay rabbitmq-reset-queues dashboards lint test-integration test-alerts check token

up: .env      ## build and start everything
	docker compose up -d --build --remove-orphans

# Creates local settings on first run; an existing .env is never overwritten.
.env:
	cp .env.example .env

down:          ## stop everything (add -v to remove database data)
	docker compose down

logs:          ## follow application logs
	docker compose logs -f gateway order payment delivery notification

ps:
	docker compose ps

outbox:        ## show the latest outbox messages of all services
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -c \
		"SELECT event_type, aggregate_id, attempts, published_at, last_error FROM ( \
			SELECT * FROM order_outbox UNION ALL SELECT * FROM payment_outbox UNION ALL SELECT * FROM delivery_outbox \
		) o ORDER BY created_at DESC LIMIT 20"'

orders:        ## show the latest orders with their payment, shipment, and notifications
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -c \
		"SELECT o.id AS order_id, o.status, o.total_amount AS amount, p.status AS payment, p.failure_reason, \
			s.carrier, s.tracking_number, \
			(SELECT array_agg(n.kind ORDER BY n.sent_at) FROM notifications n WHERE n.order_id = o.id) AS emails \
		FROM orders o \
		LEFT JOIN payments p ON p.order_id = o.id \
		LEFT JOIN shipments s ON s.order_id = o.id \
		ORDER BY o.created_at DESC LIMIT 20"'

GOLANGCI_LINT_VERSION = v2.14.0

test:          ## unit tests (fast, no Docker)
	go test -race -count=1 ./...

test-integration: ## integration tests with real PostgreSQL and RabbitMQ (testcontainers, needs Docker)
	go test -tags integration -race -count=1 ./...

lint:          ## golangci-lint in Docker, the same version as in CI
	docker run --rm -v $(CURDIR):/app -v $(HOME)/go/pkg/mod:/go/pkg/mod -w /app \
		golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run

test-alerts:   ## unit tests of the Prometheus alert rules (deploy/prometheus/alerts_test.yml)
	docker run --rm --entrypoint promtool -v $(CURDIR)/deploy/prometheus:/etc/prometheus:ro -w /etc/prometheus \
		prom/prometheus:v3.5.0 test rules alerts_test.yml

check: lint test test-alerts test-integration ## everything CI checks, locally

tidy:
	go mod tidy

migrate-new:   ## make migrate-new service=payment name=add_refunds
	docker run --rm -v $(PWD)/migrations/$(service):/migrations migrate/migrate:v4.17.1 \
		create -ext sql -dir /migrations -seq $(name)

dlq-list:      ## show dead-lettered messages: make dlq-list queue=payment-service
	RABBITMQ_URL='$(RABBITMQ_URL)' go run ./cmd/dlq list -queue $(queue)

dlq-replay:    ## move dead-lettered messages back for processing: make dlq-replay queue=payment-service
	RABBITMQ_URL='$(RABBITMQ_URL)' go run ./cmd/dlq replay -queue $(queue) -limit 1000

rabbitmq-reset-queues: ## DEVELOPMENT ONLY: delete the service queues (and their messages), e.g. after queue arguments changed
	for q in $(SERVICE_QUEUES); do docker compose exec rabbitmq rabbitmqctl delete_queue $$q; done
	docker compose restart order payment delivery notification

dashboards:    ## regenerate Grafana dashboards from cmd/dashboards (Grafana Foundation SDK)
	go run ./cmd/dashboards
	docker compose restart grafana

token:         ## print an access token for API calls: make token user=alice (alice, bob: customers; support)
	@curl -s -X POST http://localhost:$(or $(KEYCLOAK_PORT),8180)/realms/orderflow/protocol/openid-connect/token \
		-d grant_type=password -d client_id=orderflow-cli -d username=$(or $(user),alice) -d password=$(or $(user),alice) \
		| python3 -c 'import json, sys; d = json.load(sys.stdin); print(d.get("access_token") or d)'
