GO ?= go

.PHONY: build test race vet fmt up down test-integration test-e2e test-all migrate-up migrate-down

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

## Ambiente completo (Postgres, LocalStack, Keycloak e três instâncias).
up:
	docker compose up -d --build

down:
	docker compose down -v

## Os testes de integração usam o Postgres do compose; os e2e, o ambiente todo.
test-integration:
	$(GO) test -race -tags integration ./test/integration/

test-e2e:
	$(GO) test -tags e2e ./test/e2e/ -timeout 20m

## Tudo: unitários, integração e ponta a ponta. Exige o compose no ar.
test-all: vet test race test-integration test-e2e

migrate-up:
	docker compose exec -T postgres psql -U wallet -d wallet -v ON_ERROR_STOP=1 \
		-f /dev/stdin < internal/adapter/postgres/migrations/000001_init.up.sql

migrate-down:
	docker compose exec -T postgres psql -U wallet -d wallet -v ON_ERROR_STOP=1 \
		-f /dev/stdin < internal/adapter/postgres/migrations/000001_init.down.sql
