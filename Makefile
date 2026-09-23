GO ?= go

.PHONY: build test race vet fmt up down test-integration test-e2e test-all verificar migrate-up migrate-down

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

## Confere a solução contra o enunciado, item a item, com evidência para cada
## exigência. Exige o compose no ar.
verificar:
	./scripts/verificar.sh

## Migrations. DB=wallet por padrão; DB=wallet_test para o banco dos testes.
## O serviço `migrate` do compose já aplica nos dois ao subir; estes alvos
## servem para exercitar aplicação e reversão à mão.
DB ?= wallet

migrate-up:
	docker compose exec -T postgres psql -U wallet -d $(DB) -v ON_ERROR_STOP=1 \
		-f /dev/stdin < internal/adapter/postgres/migrations/000001_init.up.sql

## ATENÇÃO: derruba as tabelas e APAGA os dados do banco escolhido. Com as
## instâncias no ar, elas passam a errar até um migrate-up. É o comportamento
## esperado de uma reversão — mas é bom saber antes de rodar.
migrate-down:
	@printf 'Reverter a migration em "$(DB)" apaga todos os dados. Enter para seguir, Ctrl-C para abortar: '; read _
	docker compose exec -T postgres psql -U wallet -d $(DB) -v ON_ERROR_STOP=1 \
		-f /dev/stdin < internal/adapter/postgres/migrations/000001_init.down.sql
