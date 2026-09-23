# Arquitetura e decisões

Este documento responde, na ordem, cada tópico que o enunciado exige registrar:
dinheiro, transações, idempotência, locks, referências pendentes, reversões,
inbox/outbox, autenticação, autorização, uso do Fx e shutdown. A última seção
lista as limitações e o que ficou de fora.

---

## 1. Organização

```
cmd/wallet                  main: lê a configuração e entrega ao Fx
internal/domain/money       Money (value object, int64 privado)
internal/domain/wallet      Wallet (raiz do agregado) e LedgerEntry
internal/domain/wager       Transaction, máquina de estados, Decide, Fingerprint
internal/domain/event       eventos de integração tipados
internal/domain/guard       teste que varre a AST atrás de float
internal/app                casos de uso e portas
internal/contract           wire compartilhado por HTTP e SQS
internal/adapter/postgres   pgx + SQL explícito + migrations
internal/adapter/httpapi    chi, DTOs, middlewares
internal/adapter/auth       OIDC/JWKS
internal/adapter/sqs        consumidor, publicador, provisionamento
internal/worker             relay da outbox, resolvedor de pendências
internal/observability      slog JSON, métricas Prometheus
internal/bootstrap          ÚNICO pacote que conhece o Fx
internal/config             configuração validada na inicialização
test/{testenv,integration,e2e}
```

A dependência aponta sempre para dentro. `internal/domain` importa apenas
`github.com/google/uuid` e a biblioteca padrão — nada de Fx, HTTP, SQS ou pgx.
Concentrar o Fx em `internal/bootstrap` transforma essa independência em algo
**verificável pelos imports**, em vez de prometido em prosa.

As portas (`UnitOfWork`, repositórios, `Clock`, `IDGenerator`, `Metrics`,
`Publisher`) são declaradas em `internal/app`, do lado de quem consome, e
implementadas pelos adaptadores.

---

## 2. Dinheiro

`Money` é imutável: `int64` de unidades mínimas mais um código ISO 4217. As
moedas aceitas (BRL, USD, EUR, GBP, ARS, MXN, CAD) têm todas escala 2.

**Por que `int64` e não uma biblioteca decimal.** O enunciado permite os dois.
A coluna no banco é `BIGINT`, então com `int64` o mapeamento é 1:1 e sem
conversão. Com `NUMERIC` e um tipo decimal o valor também seria exato, mas
passaria por mais camadas de serialização — e cada camada é um lugar onde um
`float64` pode se esconder. Menos superfície, menos risco.

**Limites.** `[MinInt64+1, MaxInt64]` unidades mínimas, cerca de ±92
quatrilhões de reais. `MinInt64` é proibido na construção para que `Neg` nunca
estoure. Soma, subtração, negação e parsing devolvem `ErrOverflow` em vez de
dar a volta.

**Parsing.** Regex `^(0|[1-9][0-9]*)\.[0-9]{2}$`, aplicada antes de qualquer
conversão. Rejeita vazio, `NaN`, `Infinity`, notação científica, sinal, escala
diferente de dois e zeros à esquerda. Nada é arredondado.

**Normalização antes do hash.** Exatamente duas casas decimais é uma escolha
deliberada: aceitar só a forma canônica **elimina** a normalização. `"25.0"`
não é uma segunda grafia de `"25.00"` — é entrada inválida, e o cliente
corrige e reenvia com a mesma chave. A única normalização aplicada antes do
fingerprint é a caixa dos UUIDs, que vão para minúsculas.

**Nenhum float em lugar nenhum.** O campo é privado e não existe construtor
que aceite `float64`. No JSON, `amount` é sempre string; um número JSON no
lugar vira 400. E o teste em `internal/domain/guard` varre a AST de domínio,
casos de uso, contrato e adaptadores atrás de `float32`, `float64`, literal de
ponto flutuante e qualquer chamada com `Float` no nome — ele foi validado com
um canário, para não passar no vazio.

---

## 3. Transações SQL e concorrência

**Biblioteca: pgx v5 com SQL explícito.** O enunciado pede que transações,
locks e constraints permaneçam explícitos e verificáveis. `SELECT ... FOR
UPDATE`, `ON CONFLICT DO NOTHING` e `FOR UPDATE SKIP LOCKED` são o coração da
solução; num ORM eles virariam string mágica ou não existiriam.

**Delimitação da transação.** Os repositórios recebem um `Querier`, satisfeito
tanto por `*pgxpool.Pool` quanto por `pgx.Tx`. Quem abre e fecha a transação é
o **caso de uso**, via `UnitOfWork.Do`, nunca o repositório. É isso que permite
confirmar saldo, ledger, estado da transação, inbox e outbox no mesmo commit.

**Dois níveis de isolamento, por propósito diferente.**

*Escrita: READ COMMITTED.* Não `SERIALIZABLE`. Toda coordenação financeira já
passa pelo `FOR UPDATE` da carteira, então o nível serializável não
acrescentaria garantia — acrescentaria erros de serialização sob disputa,
exatamente no cenário das duas apostas concorrentes, e exigiria uma camada de
retry para resolver um problema criado por ele mesmo.

*Leitura de conferência: REPEATABLE READ, somente-leitura* (`UnitOfWork.
Snapshot`). READ COMMITTED tira um snapshot NOVO a cada statement, e a
reconciliação lê o saldo e depois soma o ledger: uma operação commitada entre
as duas leituras produz uma diferença sem que nada esteja errado. O enunciado
pede a comparação "em uma visão consistente dos dados", e é isso que
`REPEATABLE READ` entrega — sem bloquear escritores, que seguem normalmente
enquanto a conferência continua vendo o mundo como era quando começou.

Uma divergência falsa não é cosmética: ela dispara a métrica de alarme,
registra erro no log e responde ao provedor que a carteira está inconsistente.
Quem for investigar não acha nada — e na vez em que o alarme tocar de verdade,
ninguém olha.

**Lock pessimista por carteira.** `SELECT ... FROM wallets WHERE id = $1 FOR
UPDATE`.

Por que pessimista e não otimista: o teste obrigatório é o pior caso para
versão otimista — duas escritas simultâneas na *mesma* linha. Com retry
otimista, uma falha, relê e tenta de novo; sob 50 requisições paralelas isso
vira uma fila de retries desperdiçando trabalho. Com `FOR UPDATE`, a segunda
transação espera no banco, sem queimar ciclo.

Por que isso **não** é lock global: o lock é de linha, então carteiras
diferentes não se tocam. E como cada transação trava **exatamente uma**
carteira, não existe ordem de aquisição — logo, não existe deadlock possível.
Isso é propriedade do desenho, não sorte: nenhuma operação do desafio
movimenta duas carteiras.

**A ordem das operações no caso de uso importa:**

1. `SELECT ... FOR UPDATE` na carteira
2. busca de idempotência
3. resolução da referência
4. `Decide` (função pura)
5. `INSERT` da transação, `UPDATE` do saldo condicionado à versão, `INSERT` do
   lançamento, `INSERT` dos eventos na outbox
6. commit

Travar **antes** de consultar idempotência é deliberado: assim a busca enxerga
tudo que outra instância já commitou para aquela carteira. Consultar antes de
travar abriria uma janela entre "não achei" e "vou criar".

**Defesa em profundidade.** `UpdateBalance` condiciona a versão anterior na
cláusula `WHERE`. O lock já serializa os escritores, mas se algum caminho
futuro esquecer o `FOR UPDATE`, um escritor atrasado atualiza zero linhas e
recebe conflito em vez de sobrescrever trabalho confirmado.

**`lock_timeout` de 5s** por conexão evita que uma instância fique presa
indefinidamente atrás de uma carteira travada por outra que não responde.

---

## 4. Invariantes garantidas pelo banco

Com três instâncias, lock de aplicação não vale nada. O enunciado exige que as
invariantes valham "independentemente de locks locais e da deduplicação do SQS
FIFO", e é por isso que tudo abaixo está no schema:

| Invariante | Mecanismo |
|---|---|
| Saldo nunca negativo | `CHECK (balance_minor >= 0)` |
| Uma carteira por (jogador, moeda) | `UNIQUE (player_id, currency)` |
| Identidade da carteira imutável | trigger `wallets_guard` |
| Versão anda junto com o saldo | trigger `wallets_guard` |
| Operação externa única | índice parcial `(provider_id, external_transaction_id)` |
| Chave de idempotência única por provedor | índice parcial `(provider_id, idempotency_key)` |
| Uma abertura por carteira | índice parcial `(wallet_id) WHERE kind='OPENING'` |
| **Uma reversão bem-sucedida por transação** | índice parcial em `reference_transaction_id` |
| Transação terminal congelada | trigger `wager_transactions_guard` |
| Ledger append-only | triggers de `UPDATE`, `DELETE` e `TRUNCATE` |
| Aritmética do lançamento | `CHECK (balance_after = balance_before ± amount)` |
| Lançamento casa com sua transação | FK composta `(transaction_id, wallet_id, currency, amount_minor)` |
| Ledger encadeado | trigger `ledger_entries_chain` |
| **Saldo e ledger casam no commit** | constraint triggers `DEFERRABLE INITIALLY DEFERRED` |
| Inbox sem reprocesso | `PRIMARY KEY (consumer_name, message_id)` |
| Snapshot da outbox imutável | trigger `outbox_events_guard` |

Três merecem explicação.

**O append-only exige trigger, não constraint.** Nenhuma constraint impede
`UPDATE`. O enunciado pede imutabilidade "pelos mecanismos de proteção do
banco", e só trigger entrega isso.

**Os constraint triggers adiados são o par que fecha a regra.** Disparam dos
dois lados: alterar a carteira sem lançar no ledger falha no commit, e lançar
no ledger sem atualizar a carteira também. É o que transforma "cada mudança
financeira exige o lançamento correspondente, confirmado junto com o saldo" em
garantia do banco. Precisam ser `DEFERRED` porque, dentro da transação, os dois
passos acontecem em momentos distintos — só no commit é que o par tem de estar
coerente.

**A FK composta impede lançamento cruzado.** Ela aponta para
`(id, wallet_id, currency, amount_minor)` da transação, então um lançamento não
pode referenciar transação de outra carteira, de outra moeda ou de outro valor.
Junto com `UNIQUE (wallet_id, transaction_id)`, isso dá "no máximo um
lançamento por transação" sem índice redundante.

**Papel de menor privilégio.** A migration cria `wallet_app`, um papel
`NOLOGIN` com `SELECT, INSERT` no ledger e sem `UPDATE`, `DELETE` ou
`TRUNCATE`. A aplicação conecta como `wallet_service`, membro desse grupo; as
migrations rodam como dono do schema, num serviço separado do compose. Sem ser
dono nem superusuário, o runtime também não consegue desabilitar trigger nem
mudar `session_replication_role` — o append-only resiste até a um bug da
própria aplicação.

---

## 5. Idempotência

Persistente em PostgreSQL, sobrevive ao reinício de todos os processos.

**Três regras, dois índices:**

| Situação | Resposta |
|---|---|
| Mesma chave, mesmo hash | resultado gravado, `idempotentReplay: true` |
| Mesma chave, hash diferente | 409 `IDEMPOTENCY_CONFLICT` |
| Mesma `(providerId, externalTransactionId)`, outra chave | 409 `DUPLICATE_EXTERNAL_TRANSACTION` |

A terceira é a que se perde com facilidade: não basta indexar a chave, são dois
índices únicos independentes.

**Escopo por provedor.** A chave é única em `(provider_id, idempotency_key)`. A
mesma string vinda de outro provedor é outra operação — então não há vazamento
entre provedores em replay.

**A chave nunca é substituída.** Por HTTP ela vem do header `Idempotency-Key`;
por SQS, de `data.idempotencyKey`. O servidor não calcula uma no lugar, e o
corpo HTTP com `idempotencyKey` é recusado para que as duas não divirjam.

**O hash** é SHA-256 do JSON canônico (chaves em ordem alfabética, sem espaço,
sem escape HTML) destes campos: `providerId`, `externalTransactionId`,
`playerId`, `walletId`, `roundId`, `gameId`, `kind`, `amount`, `currency` e
`referenceExternalTransactionId` (omitido quando ausente).

Ficam **fora**: a chave de idempotência, os headers, e o envelope do SQS
(`messageId`, `type`, `occurredAt`). O motivo é operacional: a mesma operação
pode chegar pelos dois caminhos, e se metadados de transporte entrassem no
hash, a deduplicação cruzada que o enunciado exige nunca casaria.

**O replay devolve o saldo do processamento ORIGINAL**, não o atual. Por isso
`result_balance_minor` é gravado na própria transação e um `CHECK` exige que
`PROCESSED` sempre o tenha. Recalcular no replay daria o número errado assim
que a carteira se movesse de novo.

**Corrida entre duplicatas.** Duas requisições idênticas simultâneas: a
primeira insere, a segunda bloqueia no índice único até a primeira commitar, e
então recebe o conflito e lê o resultado gravado. O teste das 50 requisições
paralelas passa sem nenhum lock de aplicação.

**Entrada inválida não é persistida.** JSON quebrado, `Money` malformado, UUID
inválido ou tipo desconhecido são permanentes e corrigíveis: o cliente conserta
e reenvia com a mesma chave.

---

## 6. Máquina de estados

```
PENDING ──┬─► PROCESSED   (terminal)
          ├─► REJECTED    (terminal)
          ├─► FAILED      (terminal)
          └─► PENDING_REFERENCE ──┬─► PENDING_REFERENCE (nova tentativa)
                                  ├─► PROCESSED
                                  └─► REJECTED
```

**`PENDING` existe apenas em memória e nunca é commitado.** O enunciado permite
explicitamente concluir de forma síncrona operações sem dependência, "sem
commit intermediário de aceite" — e é o que fazemos: tudo acontece numa única
transação SQL e a linha nasce já terminal.

A consequência é boa: **não existe `PENDING` órfão para retomar** depois de uma
queda. O `CHECK` da coluna `status` nem aceita o valor, então isso é garantia
do schema e não promessa da aplicação. `Rehydrate` também recusa `PENDING`.

A única espera durável é `PENDING_REFERENCE`, e ela tem worker próprio.

**Transitório × permanente.** Essa classificação decide três coisas ao mesmo
tempo: se a transação é refeita, se a mensagem volta para a fila ou vai para a
DLQ, e qual HTTP o provedor recebe.

| Classe | Exemplos | Destino |
|---|---|---|
| Transitório | `23505`, `23P01`, `40001`, `40P01`, classes 08/53/57/58, `lock_timeout` | retry |
| Permanente de negócio | saldo insuficiente, moeda divergente, reversão duplicada | `REJECTED`, mensagem sai da fila |
| Permanente de infra | payload malformado, violação de `CHECK` | `FAILED`, DLQ |

Violação de unicidade é **transitória** porque refazer a transação enxerga o
vencedor da corrida e responde como replay. Violação de `CHECK` é permanente:
insistir dá o mesmo resultado.

**Rejeição de negócio é sucesso do ponto de vista da mensageria.** O enunciado
diz isso com todas as letras. Mandar uma aposta sem saldo para a DLQ seria erro
de desenho.

---

## 7. Operações e reversões

| Tipo | Movimento | Referência |
|---|---|---|
| `BET` | débito; sem saldo → `INSUFFICIENT_FUNDS` | proibida |
| `WIN` | crédito | opcional |
| `LOSS` | nenhum (`0.00`, sem lançamento, sem versionar) | proibida |
| `REFUND` | crédito do valor da `BET` | obrigatória, precisa ser `BET` |
| `ROLLBACK` | contrário ao original | obrigatória |

**Direção do `ROLLBACK`**, por tabela explícita:

| Tipo revertido | Direção |
|---|---|
| `BET` | crédito (desfaz um débito) |
| `WIN` | débito (desfaz um crédito) |
| `REFUND` | débito (desfaz um estorno) |

Tipo fora dessa tabela é rejeitado com `REFERENCE_KIND_INVALID`, nunca
"processado sem mover dinheiro" — o desfecho silencioso e errado.

**`LOSS`** processa, emite `WagerTransactionProcessed` e **não** emite
`WalletBalanceChanged`, porque nenhum saldo mudou. Não cria lançamento e não
incrementa a versão da carteira.

**`REFUND` × `ROLLBACK` sobre a mesma aposta.** Política adotada: **uma
transação admite no máximo UMA reversão bem-sucedida, de qualquer tipo.**

O motivo é financeiro, não de implementação: `REFUND` e `ROLLBACK` de uma mesma
`BET` devolvem o mesmo débito. Permitir os dois creditaria duas vezes. Um
segundo `REFUND`, ou um `ROLLBACK` de uma `BET` já estornada, recebe
`ALREADY_REVERSED`. A regra está no domínio **e** no índice parcial do banco.

Um `ROLLBACK` de um `REFUND` é permitido uma vez — ele desfaz o estorno,
debitando de novo — e deixa a `BET` com o estorno já consumido.

**Reversão parcial está fora do escopo**, como o enunciado define: valor
diferente do original é `REVERSAL_AMOUNT_MISMATCH`.

**Reversão que debitaria além do saldo** é `REVERSAL_INSUFFICIENT_FUNDS`,
código distinto de `INSUFFICIENT_FUNDS` conforme exigido. A razão de negócio é
real: aposta sem saldo é o jogador sem dinheiro, rotina; rollback sem saldo é
dinheiro que já saiu da carteira, e isso é incidente operacional.

---

## 8. Referências pendentes

Referência inexistente → `PENDING_REFERENCE`, evento
`WagerTransactionPendingReference`, HTTP 202.

O worker lista as pendências vencidas com `FOR UPDATE SKIP LOCKED`, e para cada
uma trava a carteira **antes** da linha — a mesma ordem do fluxo HTTP, porque
locks adquiridos em ordens diferentes por caminhos diferentes é exatamente como
nasce um deadlock.

A pendência é **relida depois** de a carteira ser travada. O `SKIP LOCKED` da
listagem solta o lock quando aquela transação commita, então duas instâncias
podem sair com a mesma pendência na mão; sem a releitura, a segunda trabalharia
sobre um estado já vencido e só descobriria no fim, quando a trigger do banco
recusasse a escrita. Funciona, mas é gastar uma transação inteira para
descobrir o que dá para saber antes de começar.

**A decisão mora no caso de uso, não no worker.** `ResolvePending` chama o
mesmo `Decide`: as regras não podem divergir entre quem chegou na hora certa e
quem chegou antes da referência. O worker empresta só o laço e o agendamento.

**Backoff** exponencial `base × 2^tentativas`, limitado ao máximo.

**Despertar antecipado:** quando uma operação externa chega a QUALQUER estado
terminal — processada, rejeitada ou falha —, as pendências que a referenciam
têm o `next_attempt_at` antecipado na mesma transação. Vale também para quem
não move dinheiro: uma reversão que aguarda uma aposta rejeitada não tem mais
nada a esperar, e cumprir o backoff até o TTL seria só tempo morto. Sem isso, uma reversão que chegou antes esperaria o backoff inteiro
depois que a referência já existe — tempo morto sem motivo.

**Esgotadas as tentativas:** `REJECTED` com `REFERENCE_NOT_FOUND` (nunca
chegou) ou `REFERENCE_NOT_PROCESSED` (chegou e terminou sem sucesso), mais o
evento de rejeição.

**Referência existente mas ainda pendente:** continua esperando. Rejeitar agora
descartaria uma operação que ainda pode concluir.

---

## 9. Inbox e outbox

### Inbox

**Por que ela existe se o SQS FIFO já deduplica:** a janela de deduplicação do
FIFO é de 5 minutos. Uma reentrega depois disso passa direto. O enunciado
antecipa isso ao exigir que as invariantes valham "independentemente da
deduplicação do SQS FIFO" — o FIFO é otimização, a inbox é a garantia.

O registro `(consumer_name, message_id)` acontece **dentro da mesma transação**
das mudanças de domínio, do ledger e dos eventos. `ON CONFLICT DO NOTHING` em
vez de consultar antes de inserir: consultar primeiro abriria uma janela em que
duas instâncias decidiriam processar a mesma mensagem.

`DeleteMessage` só acontece **depois do commit**. Morrer entre os dois é
seguro: a mensagem volta e a inbox reconhece a duplicata.

O mesmo `messageId` com conteúdo diferente é erro permanente — adulteração ou
bug do produtor — e vai para a DLQ.

### Outbox

O evento entra no mesmo `COMMIT` do saldo e do ledger. **Publicar antes de
confirmar é impossível por construção:** se a transação não commitar, o evento
não existe.

O publisher reivindica um lote com `FOR UPDATE SKIP LOCKED`, publica, marca e
só então commita. `SKIP LOCKED` é a resposta exata a "múltiplos publishers,
disputa por registros e recuperação de trabalho abandonado": cada publisher
leva um lote diferente sem coordenação externa, e um que morre com a transação
aberta devolve o lock ao banco — outro assume o mesmo evento.

**Morrer entre publicar e commitar republica o evento com o MESMO `eventId`.**
É entrega ao menos uma vez com identidade estável, que é o que o enunciado pede
ao exigir que republicações preservem o `eventId`; o consumidor deduplica por
ele.

A chamada ao destino tem timeout curto, porque o lock é mantido enquanto ela
acontece. Backoff exponencial em `attempts`/`next_attempt_at`; esgotadas as
tentativas, o evento é marcado como dead-lettered e a métrica sobe.

`payload` é `JSON` e não `JSONB` de propósito: `JSONB` normaliza chaves e
espaços, e o enunciado pede um snapshot imutável. `JSON` preserva o texto
exato.

### Eventos

| Evento | Gatilho |
|---|---|
| `WagerTransactionProcessed` | conclusão com sucesso, **incluindo `LOSS`** |
| `WagerTransactionRejected` | rejeição definitiva por regra de negócio |
| `WalletBalanceChanged` | alteração efetiva do saldo |
| `WagerTransactionPendingReference` | registro de espera |

Envelope com `eventId`, `eventType`, `aggregateId`, `correlationId`,
`causationId`, `occurredAt` (RFC 3339 UTC), `version` e `data` tipado. Tipo e
versão são definidos pelo **construtor** de cada evento, não pelo chamador —
assim um publicador não inventa um tipo.

---

## 10. Consumidor SQS

| Parâmetro | Valor | Por quê |
|---|---|---|
| `MessageGroupId` | `walletId` | ordem por carteira é a única que importa; carteiras distintas seguem em paralelo |
| `MessageDeduplicationId` (entrada) | `messageId` | identidade durável da mensagem |
| `MessageDeduplicationId` (saída) | `eventId` | estável entre republicações |
| `VisibilityTimeout` | 60s | folga sobre o p99 de processamento |
| `maxReceiveCount` | 5 | o que falhou cinco vezes não passa na sexta |

**`MessageGroupId = walletId` não é o que garante a correção.** Se fosse, o
requisito de valer "independentemente da deduplicação do FIFO" estaria
violado. A correção vem do `FOR UPDATE` e das constraints.

**Ordem dentro do lote.** Um `ReceiveMessage` pode trazer várias mensagens do
mesmo grupo, em ordem. Se a cabeça precisa voltar para a fila, as seguintes do
mesmo grupo são liberadas (visibilidade 0) em vez de processadas — senão a
ordem por carteira se perderia.

**Autorização na fila.** O consumidor lê o atributo de sistema `SenderId` e só
aceita o `providerId` declarado se `SQS_SENDER_PROVIDERS` permitir aquele
remetente. É o equivalente, na mensageria, ao `providerId` derivado do token no
HTTP — sem isso, qualquer produtor com acesso à fila operaria por qualquer
provedor. No LocalStack todo remetente é o account id `000000000000`, por isso
o curinga local; em produção cada provedor tem seu principal IAM.

**Mensagens inválidas** (JSON quebrado, tipo desconhecido, envelope incompleto)
são permanentes: reentregar dá o mesmo resultado.

---

## 11. Autenticação e autorização

**IdP: Keycloak**, recomendado pelo enunciado, roda em container sem depender
de nuvem e permite provisionamento do realm por arquivo — o que mantém o
`docker compose up` reprodutível.

**Fluxo: `client_credentials`.** Não há usuário final nesta API: quem chama é
um serviço. Cadastro de senha e emissão própria de token estão fora do escopo.

**Validação:** JWKS via `coreos/go-oidc`, RS256, com cache e renovação
automática de chave. Emissor, audiência e expiração são conferidos. Qualquer
falha vira o mesmo `401` — detalhar qual checagem falhou ajudaria quem tenta
forjar um token.

**Modelo de permissões:**

| Client | Claim | Pode |
|---|---|---|
| `provider-a`, `provider-b` | `provider_id` | operar e consultar **apenas as próprias** transações |
| `internal-wallet-service` | `scope: wallets:write` | abrir carteira; operar por qualquer provedor |

**A identidade autenticada determina o `providerId`.** O campo do corpo é
apenas conferido contra o claim; divergência é 403. Isso vale nas escritas, nas
consultas e **em replays** — um provedor que reenvia a chave de outro não
recebe o resultado alheio.

Na consulta por id, a resposta a um provedor não autorizado é **404**, não 403:
confirmar a existência do recurso alheio já seria vazamento.

O serviço interno passa por qualquer provedor porque é ele que opera o worker
de pendências, que reavalia operações de todos.

Sem credencial válida, a requisição é recusada no middleware: **nenhum efeito
financeiro e nenhuma exposição de dado** num acesso não autorizado.

---

## 12. Uso do Fx e shutdown

`internal/bootstrap` é o único pacote que importa o Fx, organizado em
`fx.Module`: plataforma, persistência, mensageria, autenticação, casos de uso,
HTTP e workers.

**Inicialização valida dependências.** O `OnStart` do pool faz `Ping`: subir
com banco inacessível só adiaria a descoberta para a primeira aposta. A
configuração é validada antes disso, no `main`, e reporta **todos** os
problemas de uma vez — reportar um por vez faria descobri-los em série, com um
deploy a cada descoberta.

**Workers rodam com contexto próprio**, não o do `OnStart`. O contexto do
`OnStart` é cancelado assim que a inicialização termina, e um worker atrelado a
ele morreria no instante em que a aplicação ficasse pronta. O `OnStop` cancela
o contexto do worker e **espera** o término, com prazo — sem essa espera, o
shutdown devolveria o controle antes de o trabalho em andamento acabar.

**Ordem do encerramento.** O Fx executa os `OnStop` na ordem inversa dos
`OnStart`, então o pool do Postgres fecha **depois** dos workers e do servidor.
É exatamente o requisito de "fechamento das dependências após a finalização dos
componentes que as utilizam".

**Em `SIGTERM`:**

- o servidor HTTP para de aceitar conexões novas e conclui as em andamento;
- o consumidor SQS para de chamar `ReceiveMessage` e libera a visibilidade do
  que não couber no prazo, para reentrega segura;
- os workers de outbox e pendências terminam o ciclo corrente;
- só então o pool fecha.

---

## 13. Observabilidade

Logs JSON com `slog`, carregando `correlationId`, `messageId`,
`transactionId`, `walletId` e `providerId`. Credencial e payload financeiro
completo **não** são registrados.

Métricas Prometheus num registro próprio — não o global, que vaza entre testes
e impediria duas instâncias no mesmo processo. A lista está no README.

`outbox_pending_age_seconds` é a métrica mais útil num incidente: ela cresce
quando a publicação para, mesmo com todo o resto parecendo saudável.

`/health/live` não toca em dependência: um liveness que depende do banco
derruba o processo toda vez que o banco pisca, que é o oposto do que se quer.
`/health/ready` confere Postgres e SQS.

---

## 14. Limitações e o que ficou de fora

Declarado abertamente, como o enunciado pede.

**Opcionais não implementados:**

- **Partidas dobradas.** O ledger é de partida simples, com saldo anterior e
  posterior em cada lançamento. O enunciado trata partidas dobradas como
  diferencial opcional.
- **Tracing OpenTelemetry e dashboards.** Opcionais. Há `correlationId`
  atravessando log, evento e resposta, o que cobre a rastreabilidade pedida.
- **Testes de carga.** Opcionais, e sem meta mínima de RPS no enunciado.

**Escolhas de escopo:**

- **Multi-moeda.** Os cenários rodam em BRL, como o enunciado permite. `Money`
  carrega a moeda, há sete moedas de escala 2 aceitas, e existem testes de
  incompatibilidade entre moedas — mas não há conversão cambial.
- **Moedas de escala diferente de 2** (JPY, KWD) ficam fora: `Scale` é uma
  constante, e suportá-las exigiria escala por moeda.
- **Migrations aplicadas por `psql`** no serviço `migrate` do compose, em vez
  de `golang-migrate` embarcado. Com uma única migration, o arquivo `.up.sql` /
  `.down.sql` versionado entrega o que se pede sem uma dependência a mais. A
  numeração já segue a convenção da ferramenta, então adotá-la depois é trocar
  o executor, não reescrever.

**Pontos que mereceriam atenção antes de produção:**

- **Segredos.** As senhas do compose são locais e estão em texto claro, como o
  enunciado permite para o ambiente de desenvolvimento. Em produção viriam de
  um cofre.
- **A dead-letter da outbox não tem reprocessamento automático.** Um evento
  esgotado fica marcado e visível pela métrica, mas reenviá-lo é operação
  manual hoje.
- **`SQS_SENDER_PROVIDERS` com curinga** é aceitável no LocalStack, onde todo
  remetente é o mesmo account id, mas em produção o mapa precisa ser explícito
  por principal IAM.
