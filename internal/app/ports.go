package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

// As portas são declaradas aqui, no lado de quem consome, e implementadas
// pelos adaptadores. O domínio não conhece nenhuma delas.

// UnitOfWork delimita a transação SQL.
//
// Quem abre e fecha a transação é o CASO DE USO, nunca o repositório: é isso
// que permite que saldo, ledger, transação, inbox e outbox sejam confirmados
// no mesmo commit. Os repositórios recebem o executor já dentro da transação
// e não têm opinião sobre o limite dela.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(context.Context, *Repos) error) error
}

// Repos reúne os repositórios ligados a uma mesma transação.
type Repos struct {
	Wallets      Wallets
	Transactions Transactions
	Ledger       Ledger
	Inbox        Inbox
	Outbox       Outbox
}

// Wallets acessa carteiras.
type Wallets interface {
	// Lock trava a linha da carteira com SELECT ... FOR UPDATE.
	//
	// É o ponto de serialização de toda operação financeira. O lock é de
	// linha: carteiras diferentes não disputam nada, e como cada transação
	// trava exatamente uma carteira, não existe ordem de aquisição nem
	// possibilidade de deadlock.
	Lock(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	ByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	ByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, c money.Currency) (*wallet.Wallet, error)
	Insert(ctx context.Context, w *wallet.Wallet) error
	// UpdateBalance grava o saldo condicionado à versão anterior. Um escritor
	// atrasado não sobrescreve: recebe ErrConflict.
	UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error
}

// Transactions acessa operações.
type Transactions interface {
	Insert(ctx context.Context, t *wager.Transaction) error
	Save(ctx context.Context, t *wager.Transaction) error
	ByID(ctx context.Context, id uuid.UUID) (*wager.Transaction, error)
	ByIdempotencyKey(ctx context.Context, providerID, key string) (*wager.Transaction, error)
	ByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error)
	// HasSuccessfulReversal informa se a transação já foi revertida.
	// O índice parcial no banco impõe o mesmo; esta consulta permite
	// rejeitar com código claro em vez de esperar a violação.
	HasSuccessfulReversal(ctx context.Context, referenceID uuid.UUID) (bool, error)
	// DuePending lista as pendências vencidas, travando-as com SKIP LOCKED
	// para que várias instâncias não trabalhem na mesma.
	DuePending(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
	// WakeWaitingFor antecipa as pendências que esperam esta referência,
	// para que não precisem aguardar o backoff.
	WakeWaitingFor(ctx context.Context, providerID, externalID string, now time.Time) error
}

// Ledger acessa lançamentos.
type Ledger interface {
	Insert(ctx context.Context, e *wallet.LedgerEntry) error
	// Page devolve uma página com cursor opaco e ordenação estável.
	Page(ctx context.Context, walletID uuid.UUID, cursor string, limit int) ([]LedgerRow, string, error)
	// Balance reconstrói o saldo somando créditos e subtraindo débitos.
	Balance(ctx context.Context, walletID uuid.UUID) (money.Money, int, error)
}

// LedgerRow é um lançamento lido, com o cursor da própria linha.
type LedgerRow struct {
	Entry  *wallet.LedgerEntry
	Cursor string
}

// Inbox deduplica mensagens consumidas de forma durável.
type Inbox interface {
	// Register grava a chegada. Devolve false quando a mensagem já havia sido
	// registrada — é o que absorve a reentrega depois de uma queda entre o
	// commit e o DeleteMessage.
	Register(ctx context.Context, consumer, messageID, payloadHash string, now time.Time) (bool, error)
	Complete(ctx context.Context, consumer, messageID string, txID *uuid.UUID, now time.Time) error
	// Hash devolve o hash registrado, para detectar o mesmo messageId com
	// conteúdo diferente.
	Hash(ctx context.Context, consumer, messageID string) (string, error)
}

// Outbox guarda eventos para publicação posterior ao commit.
type Outbox interface {
	Append(ctx context.Context, events ...OutboxEvent) error
	// Claim trava um lote de eventos pendentes com FOR UPDATE SKIP LOCKED.
	// Vários publishers competem sem coordenação externa, e um publisher que
	// morre libera o lock junto com a transação.
	Claim(ctx context.Context, now time.Time, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID, now time.Time) error
	Reschedule(ctx context.Context, eventID uuid.UUID, next time.Time, lastErr string) error
	DeadLetter(ctx context.Context, eventID uuid.UUID, now time.Time, lastErr string) error
	// OldestPendingAge alimenta a métrica de atraso da outbox.
	OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, error)
}

// OutboxEvent é o snapshot imutável de um evento de integração.
type OutboxEvent struct {
	EventID       uuid.UUID
	EventType     string
	EventVersion  int
	AggregateType string
	AggregateID   uuid.UUID
	PartitionKey  string
	Payload       []byte
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
	Attempts      int
}

// Publisher entrega um evento ao destino externo.
type Publisher interface {
	Publish(ctx context.Context, e OutboxEvent) error
}

// Clock isola o tempo, para que os testes controlem prazos e backoff.
type Clock interface{ Now() time.Time }

// IDGenerator isola a geração de identificadores.
type IDGenerator interface{ New() uuid.UUID }

// Metrics registra o que o enunciado pede observar.
type Metrics interface {
	TransactionResult(kind, status, source string)
	Duplicate(source string)
	Retry(component string)
	DeadLetter(component string)
	ConcurrencyConflict()
	ProcessingTime(source string, d time.Duration)
	OutboxLag(d time.Duration)
	ReconciliationDivergence()
}
