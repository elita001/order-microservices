.PHONY: up down logs ps test tidy migrate-new outbox payments

up: .env      ## build and start everything
	docker compose up -d --build --remove-orphans

# Creates local settings on first run; an existing .env is never overwritten.
.env:
	cp .env.example .env

down:          ## stop everything (add -v to remove database data)
	docker compose down

logs:          ## follow application logs
	docker compose logs -f order payment

ps:
	docker compose ps

outbox:        ## show the latest outbox messages of all services
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -c \
		"SELECT event_type, aggregate_id, attempts, published_at, last_error FROM ( \
			SELECT * FROM order_outbox UNION ALL SELECT * FROM payment_outbox \
		) o ORDER BY created_at DESC LIMIT 20"'

payments:      ## show orders and their payments
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -c \
		"SELECT o.id AS order_id, o.status AS order_status, o.total_amount, p.status AS payment_status, p.failure_reason \
		FROM orders o LEFT JOIN payments p ON p.order_id = o.id ORDER BY o.created_at DESC LIMIT 20"'

test:
	go test -race -count=1 ./...

tidy:
	go mod tidy

migrate-new:   ## make migrate-new service=payment name=add_refunds
	docker run --rm -v $(PWD)/migrations/$(service):/migrations migrate/migrate:v4.17.1 \
		create -ext sql -dir /migrations -seq $(name)
