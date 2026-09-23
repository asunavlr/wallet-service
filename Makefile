GO ?= go

.PHONY: build test race vet fmt lint db-up db-down migrate-up migrate-down

build:
	$(GO) build ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

## banco local de desenvolvimento (os testes usam testcontainers e não dependem disto)
db-up:
	docker run -d --name wallet-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_USER=wallet \
		-e POSTGRES_DB=wallet -p 55432:5432 postgres:16-alpine

db-down:
	docker rm -f wallet-pg

migrate-up:
	docker exec -i wallet-pg psql -U wallet -d wallet -v ON_ERROR_STOP=1 \
		< internal/adapter/postgres/migrations/000001_init.up.sql

migrate-down:
	docker exec -i wallet-pg psql -U wallet -d wallet -v ON_ERROR_STOP=1 \
		< internal/adapter/postgres/migrations/000001_init.down.sql
