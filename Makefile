.PHONY: up down logs ps test tidy migrate-new outbox

up:            ## build and start everything
	docker compose up -d --build

down:          ## stop everything (add -v to remove database data)
	docker compose down

logs:
	docker compose logs -f order

ps:
	docker compose ps

outbox:        ## show the latest outbox messages
	docker compose exec postgres psql -U app -d orderflow -c \
		"SELECT id, event_type, aggregate_id, attempts, published_at, last_error FROM outbox ORDER BY created_at DESC LIMIT 20"

test:
	go test -race -count=1 ./...

tidy:
	go mod tidy

migrate-new:   ## make migrate-new name=add_outbox
	docker run --rm -v $(PWD)/migrations:/migrations migrate/migrate:v4.17.1 \
		create -ext sql -dir /migrations -seq $(name)
