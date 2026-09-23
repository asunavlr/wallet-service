# Wallet Service

Serviço de carteira para provedores de jogos: processa `BET`, `WIN`, `LOSS`,
`REFUND` e `ROLLBACK` por HTTP e por SQS, com as mesmas garantias financeiras
nas duas entradas.

Go 1.27 · Uber Fx · PostgreSQL · AWS SQS (LocalStack) · Keycloak

---

## Como rodar

### Pré-requisitos

- Docker e Docker Compose
- Go 1.27 (só para rodar os testes fora do container)

### Subir tudo

```sh
cp .env.example .env      # opcional: o compose já traz os valores locais
docker compose up --build
```

Isso sobe PostgreSQL, LocalStack, Keycloak e **três instâncias** do serviço
atrás do mesmo banco. As migrations rodam num serviço à parte, como dono do
schema; as filas e o realm do Keycloak são provisionados automaticamente.

| Serviço | Endereço |
|---|---|
| app-1 | http://localhost:8080 |
| app-2 | http://localhost:8082 |
| app-3 | http://localhost:8083 |
| Keycloak | http://localhost:8081 (admin/admin) |
| PostgreSQL | localhost:5432 (wallet/dev) |
| LocalStack | http://localhost:4566 |

Três instâncias não é enfeite: o enunciado pede que as garantias sejam
demonstradas com pelo menos três processos independentes, e é contra elas que
os testes e2e rodam.

### Derrubar

```sh
docker compose down -v    # -v também apaga o volume do Postgres
```

---

## Migrations

Versionadas em `internal/adapter/postgres/migrations/`, com aplicação e
reversão separadas.

```sh
# aplicar (é o que o serviço `migrate` do compose faz)
docker compose run --rm migrate

# ou manualmente, contra um Postgres já de pé
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f internal/adapter/postgres/migrations/000001_init.up.sql

# reverter
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f internal/adapter/postgres/migrations/000001_init.down.sql
```

A reversão derruba as tabelas e funções, mas **não** remove o papel
`wallet_app`: papéis são do cluster inteiro e podem ser compartilhados. Para
removê-lo: `DROP ROLE wallet_app;`.

---

## Autenticação

Keycloak, realm `wallet`, fluxo `client_credentials`. Provisionado por
`deploy/keycloak/realm-wallet.json` — não há passo manual.

| Client | Secret | Papel |
|---|---|---|
| `provider-a` | `provider-a-secret` | provedor de jogos |
| `provider-b` | `provider-b-secret` | provedor de jogos |
| `internal-wallet-service` | `internal-secret` | serviço interno |

Obter um token:

```sh
TOKEN=$(curl -s -X POST \
  http://localhost:8081/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=provider-a \
  -d client_secret=provider-a-secret | jq -r .access_token)
```

**A identidade manda sobre o corpo.** O `providerId` da requisição é conferido
contra o claim `provider_id` do token; divergência é 403. Um provedor não lê
nem escreve nada de outro, inclusive em replay.

Abrir carteira exige o escopo `wallets:write`, que só o cliente interno tem.

---

## Exemplos de chamada

### Abrir carteira (serviço interno)

```sh
INTERNO=$(curl -s -X POST \
  http://localhost:8081/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=internal-wallet-service \
  -d client_secret=internal-secret | jq -r .access_token)

curl -X POST http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNO" \
  -H 'Content-Type: application/json' \
  -d '{
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "initialBalance": { "amount": "1000.00", "currency": "BRL" }
  }'
```

### Enviar uma aposta

```sh
curl -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }'
```

### Enviar pela fila

```sh
aws --endpoint-url http://localhost:4566 sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id 0192f291-27dd-7d3f-8071-5f8685deef37 \
  --message-deduplication-id msg-123 \
  --message-body '{
    "messageId": "msg-123",
    "type": "WagerTransactionRequested",
    "occurredAt": "2026-09-23T12:00:00Z",
    "data": {
      "providerId": "provider-a",
      "externalTransactionId": "transaction-124",
      "idempotencyKey": "provider-a:transaction-124",
      "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
      "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
      "roundId": "round-987",
      "gameId": "fortune-chimp",
      "kind": "BET",
      "money": { "amount": "25.00", "currency": "BRL" }
    }
  }'
```

### Reconciliação

```sh
curl -X POST http://localhost:8080/wallets/$WALLET/reconciliation \
  -H "Authorization: Bearer $TOKEN"
```

---

## Endpoints

| Método | Rota | Acesso |
|---|---|---|
| `POST` | `/wallets` | interno |
| `GET` | `/wallets/{walletId}` | autenticado |
| `GET` | `/wallets/{walletId}/ledger?cursor=&limit=` | autenticado |
| `POST` | `/wallets/{walletId}/reconciliation` | autenticado |
| `POST` | `/wagering/transactions` | provedor |
| `GET` | `/wagering/transactions/{transactionId}` | dono da transação |
| `GET` | `/providers/{providerId}/wagering/transactions/{externalTransactionId}` | o próprio provedor |
| `GET` | `/health/live` · `/health/ready` | público |
| `GET` | `/metrics` | público |

### Códigos de resposta

As quatro situações que precisam ser distinguíveis têm código próprio:

| Situação | HTTP | `code` |
|---|---|---|
| Sucesso | 200 | — |
| Carteira criada | 201 | — |
| Aceito, esperando referência | 202 | — |
| Entrada inválida | 400 | `INVALID_REQUEST` |
| Sem credencial ou credencial inválida | 401 | `UNAUTHENTICATED` |
| Provedor sem permissão | 403 | `FORBIDDEN` |
| Não encontrado | 404 | `NOT_FOUND` |
| Chave reutilizada com outro conteúdo | 409 | `IDEMPOTENCY_CONFLICT` |
| Operação já registrada com outra chave | 409 | `DUPLICATE_EXTERNAL_TRANSACTION` |
| **Rejeição de negócio** | **422** | ver `failureCode` |
| Conflito de concorrência persistente | 503 | `CONCURRENCY_CONFLICT` |
| Dependência indisponível | 503 | `DEPENDENCY_UNAVAILABLE` |

Rejeição de negócio é **422**, não 400 nem 500: o pedido foi entendido e
processado, e a recusa ficou registrada e auditável. Respostas 503 trazem
`Retry-After`.

### `failureCode`

| Código | Significado | Corrigível? |
|---|---|---|
| `INSUFFICIENT_FUNDS` | aposta não cabe no saldo | sim, com mais saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | reversão debitaria além do saldo | incidente operacional |
| `REFERENCE_NOT_FOUND` | referência não chegou no prazo | não |
| `REFERENCE_NOT_PROCESSED` | referência terminou sem sucesso | não |
| `REFERENCE_MISMATCH` | referência de outro contexto | não |
| `REFERENCE_KIND_INVALID` | tipo referenciado não admite a reversão | não |
| `ALREADY_REVERSED` | referência já revertida uma vez | não |
| `REVERSAL_AMOUNT_MISMATCH` | reversão parcial, fora do escopo | sim |
| `CURRENCY_MISMATCH` | moeda diferente da carteira | sim |
| `INVALID_AMOUNT` | valor fora da política do tipo | sim |
| `INTERNAL_FAILURE` | falha permanente de infraestrutura | não |

`INSUFFICIENT_FUNDS` e `REVERSAL_INSUFFICIENT_FUNDS` são separados
deliberadamente: o primeiro é o jogador sem dinheiro, rotina; o segundo é
dinheiro que já saiu da carteira, que é incidente.

---

## Testes

```sh
go vet ./...
go test ./...                              # unitários, sem dependência externa
go test -race ./...
```

### Integração (PostgreSQL real)

```sh
make db-up && make migrate-up              # Postgres em :55432
go test -race -tags integration ./test/integration/
```

Ou aponte para outro banco com `TEST_DATABASE_URL`.

### Ponta a ponta (ambiente completo)

```sh
docker compose up -d --build
go test -tags e2e ./test/e2e/ -v
```

Os testes e2e usam as três instâncias e tokens reais do Keycloak.

Alguns deles derrubam e reiniciam containers de propósito (os cenários de
recuperação), e um espera até 90 s — mais que o `VisibilityTimeout` de 60 s da
fila, porque depois de matar uma instância as mensagens que ela tinha em mãos
só voltam a ficar visíveis quando esse prazo expira. Use `-short` para pular os
que mexem nos containers.

**Total: 103 testes unitários, 17 de integração e 20 ponta a ponta.**

### O que é verificado

| Cenário | Onde |
|---|---|
| `Money`: escala, overflow, `NaN`, exponente, moedas incompatíveis | `internal/domain/money` |
| **Nenhum float no caminho do dinheiro** (varredura da AST) | `internal/domain/guard` |
| Invariantes do schema: 24 recusas esperadas | `test/integration` |
| 100.00 com duas apostas simultâneas de 80.00 | `test/integration`, `test/e2e` |
| Mesma aposta 50× em paralelo → um débito | `test/integration` |
| Carteiras distintas em paralelo | `test/integration` |
| Três instâncias independentes | `test/e2e` |
| Reversão antes da referência | `test/integration` |
| Expiração da pendência | `test/integration` |
| Dois publishers disputando a outbox | `test/integration` |
| Republicação preservando o `eventId` | `test/integration` |
| Idempotência sobrevivendo a reinício | `test/integration` |
| Isolamento entre provedores, inclusive em replay | `test/e2e` |
| Mesma operação por HTTP e por fila, deduplicada | `test/e2e` |
| Mensagem repetida absorvida pela inbox | `test/e2e` |
| `OPENING` pela fila indo direto à DLQ | `test/e2e` |
| Reinício completo preservando idempotência e pendências | `test/e2e` |
| Queda de uma instância sem perder operação | `test/e2e` |

---

## Observabilidade

Logs JSON com `correlationId`, `messageId`, `transactionId`, `walletId` e
`providerId`. Credencial e payload financeiro completo **não** são
registrados.

Métricas em `/metrics`:

| Métrica | O que mostra |
|---|---|
| `wagering_transactions_total{kind,status,source}` | resultados por tipo e origem |
| `wagering_duplicates_total{source}` | repetições reconhecidas |
| `wagering_retries_total{component}` | novas tentativas |
| `wagering_dead_letter_total{component}` | o que foi para a DLQ |
| `wallet_lock_conflicts_total` | conflitos de concorrência |
| `wagering_processing_seconds{source}` | latência |
| `outbox_pending_age_seconds` | atraso da outbox |
| `reconciliation_divergences_total` | divergências de reconciliação |

`outbox_pending_age_seconds` é o alarme mais útil: ele cresce quando a
publicação para, mesmo com todo o resto parecendo saudável.

---

## Variáveis de ambiente

Todas em `.env.example`. As obrigatórias:

| Variável | Para quê |
|---|---|
| `DATABASE_URL` | conexão com o PostgreSQL |
| `OIDC_ISSUER_URL` | realm do Keycloak; a autenticação não é opcional |

O serviço **recusa subir** com configuração inválida, e reporta todos os
problemas de uma vez em vez de um por deploy.

---

## Decisões técnicas

Em [`ARCHITECTURE.md`](ARCHITECTURE.md): dinheiro, transações, idempotência,
locks, referências pendentes, reversões, inbox/outbox, autenticação,
autorização, uso do Fx, shutdown, e as limitações assumidas.
