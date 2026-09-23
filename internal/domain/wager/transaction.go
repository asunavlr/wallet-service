package wager

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

// Transaction é a operação registrada, com sua máquina de estados.
//
// As transições são métodos, não atribuição de campo: é isso que garante que
// um estado terminal nunca volta atrás. O banco impõe a mesma regra por
// trigger, então a garantia vale mesmo entre processos.
type Transaction struct {
	id       uuid.UUID
	origin   Origin
	kind     Kind
	status   Status
	walletID uuid.UUID
	playerID uuid.UUID
	amount   money.Money

	// metadados da origem externa
	providerID     string
	externalID     string
	idempotencyKey string
	payloadHash    string
	roundID        string
	gameID         string
	referenceExtID string
	referenceID    *uuid.UUID

	// resultado
	failureCode   FailureCode
	resultBalance *money.Money

	attempts      int
	nextAttemptAt *time.Time
	correlationID string
	createdAt     time.Time
	updatedAt     time.Time
}

// ExternalInput são os campos de uma operação vinda de provedor.
type ExternalInput struct {
	ID             uuid.UUID
	ProviderID     string
	ExternalID     string
	IdempotencyKey string
	PayloadHash    string
	PlayerID       uuid.UUID
	WalletID       uuid.UUID
	RoundID        string
	GameID         string
	Kind           Kind
	Amount         money.Money
	ReferenceExtID string
	CorrelationID  string
}

// NewExternal cria a transação de uma operação de provedor, em PENDING.
//
// PENDING só existe aqui, em memória: operações sem dependência são
// concluídas na mesma transação SQL e gravadas já terminais.
func NewExternal(in ExternalInput, now time.Time) (*Transaction, error) {
	switch {
	case in.ID == uuid.Nil, in.PlayerID == uuid.Nil, in.WalletID == uuid.Nil:
		return nil, fmt.Errorf("%w: identificadores obrigatórios", ErrInvalidTransaction)
	case in.ProviderID == "", in.ExternalID == "", in.IdempotencyKey == "",
		in.PayloadHash == "", in.RoundID == "", in.GameID == "":
		return nil, fmt.Errorf("%w: metadados externos obrigatórios", ErrInvalidTransaction)
	case now.IsZero():
		return nil, fmt.Errorf("%w: instante obrigatório", ErrInvalidTransaction)
	}
	if !in.Kind.External() {
		return nil, fmt.Errorf("%w: %q não pode vir de um provedor", ErrInvalidTransaction, in.Kind)
	}
	if !in.Amount.Valid() {
		return nil, fmt.Errorf("%w: valor inválido", ErrInvalidTransaction)
	}
	// Política de valor zero, verificada na construção para que uma transação
	// malformada nunca chegue a existir.
	if in.Kind == Loss && !in.Amount.IsZero() {
		return nil, fmt.Errorf("%w: LOSS exige valor 0.00", ErrInvalidTransaction)
	}
	if in.Kind != Loss && !in.Amount.IsPositive() {
		return nil, fmt.Errorf("%w: %s exige valor positivo", ErrInvalidTransaction, in.Kind)
	}
	// Referência: obrigatória em reversão, opcional em WIN, proibida no resto.
	if in.Kind.RequiresReference() && in.ReferenceExtID == "" {
		return nil, fmt.Errorf("%w: %s exige referência", ErrInvalidTransaction, in.Kind)
	}
	if !in.Kind.AllowsReference() && in.ReferenceExtID != "" {
		return nil, fmt.Errorf("%w: %s não admite referência", ErrInvalidTransaction, in.Kind)
	}

	return &Transaction{
		id: in.ID, origin: External, kind: in.Kind, status: Pending,
		walletID: in.WalletID, playerID: in.PlayerID, amount: in.Amount,
		providerID: in.ProviderID, externalID: in.ExternalID,
		idempotencyKey: in.IdempotencyKey, payloadHash: in.PayloadHash,
		roundID: in.RoundID, gameID: in.GameID, referenceExtID: in.ReferenceExtID,
		correlationID: in.CorrelationID, createdAt: now, updatedAt: now,
	}, nil
}

// NewOpening cria o crédito interno de abertura de carteira, já processado.
//
// Não tem provedor, chave, rodada, jogo nem referência — o schema recusa
// qualquer um desses numa operação interna.
func NewOpening(id, walletID, playerID uuid.UUID, amount money.Money, correlationID string, now time.Time) (*Transaction, error) {
	switch {
	case id == uuid.Nil, walletID == uuid.Nil, playerID == uuid.Nil:
		return nil, fmt.Errorf("%w: identificadores obrigatórios", ErrInvalidTransaction)
	case !amount.Valid() || !amount.IsPositive():
		return nil, fmt.Errorf("%w: abertura exige valor positivo", ErrInvalidTransaction)
	case now.IsZero():
		return nil, fmt.Errorf("%w: instante obrigatório", ErrInvalidTransaction)
	}
	saldo := amount
	return &Transaction{
		id: id, origin: Internal, kind: Opening, status: Processed,
		walletID: walletID, playerID: playerID, amount: amount,
		resultBalance: &saldo, correlationID: correlationID,
		createdAt: now, updatedAt: now,
	}, nil
}

// RehydrateInput são os campos lidos do banco.
type RehydrateInput struct {
	ID             uuid.UUID
	Origin         Origin
	Kind           Kind
	Status         Status
	WalletID       uuid.UUID
	PlayerID       uuid.UUID
	Amount         money.Money
	ProviderID     string
	ExternalID     string
	IdempotencyKey string
	PayloadHash    string
	RoundID        string
	GameID         string
	ReferenceExtID string
	ReferenceID    *uuid.UUID
	FailureCode    FailureCode
	ResultBalance  *money.Money
	Attempts       int
	NextAttemptAt  *time.Time
	CorrelationID  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Rehydrate reconstrói uma transação persistida.
//
// Não reaplica transição nem emite evento. As validações transformam uma
// linha inconsistente em erro aqui, em vez de deixá-la virar dinheiro errado
// mais adiante.
func Rehydrate(in RehydrateInput) (*Transaction, error) {
	if in.ID == uuid.Nil || in.WalletID == uuid.Nil || in.PlayerID == uuid.Nil {
		return nil, fmt.Errorf("%w: identificadores obrigatórios", ErrInvalidTransaction)
	}
	if _, err := ValidKind(string(in.Kind)); err != nil {
		return nil, err
	}
	if !in.Amount.Valid() {
		return nil, fmt.Errorf("%w: valor inválido", ErrInvalidTransaction)
	}
	switch in.Status {
	case PendingReference, Processed, Rejected, Failed:
	default:
		return nil, fmt.Errorf("%w: estado persistido %q", ErrInvalidTransaction, in.Status)
	}
	if in.Status == Processed && in.ResultBalance == nil {
		return nil, fmt.Errorf("%w: PROCESSED sem saldo resultante", ErrInvalidTransaction)
	}
	if (in.Status == Rejected || in.Status == Failed) && in.FailureCode == "" {
		return nil, fmt.Errorf("%w: %s sem código de falha", ErrInvalidTransaction, in.Status)
	}
	if in.Status == PendingReference && in.NextAttemptAt == nil {
		return nil, fmt.Errorf("%w: pendência sem próxima tentativa", ErrInvalidTransaction)
	}
	if in.ReferenceID != nil && *in.ReferenceID == in.ID {
		return nil, fmt.Errorf("%w: transação referencia a si mesma", ErrInvalidTransaction)
	}

	return &Transaction{
		id: in.ID, origin: in.Origin, kind: in.Kind, status: in.Status,
		walletID: in.WalletID, playerID: in.PlayerID, amount: in.Amount,
		providerID: in.ProviderID, externalID: in.ExternalID,
		idempotencyKey: in.IdempotencyKey, payloadHash: in.PayloadHash,
		roundID: in.RoundID, gameID: in.GameID,
		referenceExtID: in.ReferenceExtID, referenceID: in.ReferenceID,
		failureCode: in.FailureCode, resultBalance: in.ResultBalance,
		attempts: in.Attempts, nextAttemptAt: in.NextAttemptAt,
		correlationID: in.CorrelationID,
		createdAt:     in.CreatedAt, updatedAt: in.UpdatedAt,
	}, nil
}

// ─── transições ─────────────────────────────────────────────────────────────

// guard recusa qualquer transição a partir de estado terminal.
func (t *Transaction) guard() error {
	if t.status.Terminal() {
		return fmt.Errorf("%w: %s está em %s", ErrTerminal, t.id, t.status)
	}
	return nil
}

// Process conclui a operação com sucesso, guardando o saldo observado.
//
// O saldo é gravado aqui porque é ele que o replay devolve — o enunciado
// exige o saldo do processamento original, não o saldo atual da carteira.
func (t *Transaction) Process(balance money.Money, referenceID *uuid.UUID, now time.Time) error {
	if err := t.guard(); err != nil {
		return err
	}
	if !balance.Valid() {
		return fmt.Errorf("%w: saldo observado inválido", ErrInvalidTransition)
	}
	if t.kind.RequiresReference() && referenceID == nil {
		return fmt.Errorf("%w: %s exige referência resolvida", ErrInvalidTransition, t.kind)
	}
	if !t.kind.AllowsReference() && referenceID != nil {
		return fmt.Errorf("%w: %s não admite referência", ErrInvalidTransition, t.kind)
	}
	t.status = Processed
	t.resultBalance = &balance
	t.referenceID = referenceID
	t.failureCode = ""
	t.nextAttemptAt = nil
	t.updatedAt = now
	return nil
}

// Reject recusa por regra de negócio. É estado terminal e auditável.
func (t *Transaction) Reject(code FailureCode, balance *money.Money, now time.Time) error {
	if err := t.guard(); err != nil {
		return err
	}
	if code == "" {
		return fmt.Errorf("%w: rejeição exige código", ErrInvalidTransition)
	}
	t.status = Rejected
	t.failureCode = code
	t.resultBalance = balance
	t.nextAttemptAt = nil
	t.updatedAt = now
	return nil
}

// Fail registra falha permanente de infraestrutura, para auditoria.
func (t *Transaction) Fail(code FailureCode, now time.Time) error {
	if err := t.guard(); err != nil {
		return err
	}
	if code == "" {
		return fmt.Errorf("%w: falha exige código", ErrInvalidTransition)
	}
	t.status = Failed
	t.failureCode = code
	t.nextAttemptAt = nil
	t.updatedAt = now
	return nil
}

// AwaitReference registra a espera por uma referência ainda indisponível e
// agenda a próxima tentativa.
func (t *Transaction) AwaitReference(next time.Time, now time.Time) error {
	if err := t.guard(); err != nil {
		return err
	}
	if !t.kind.AllowsReference() {
		return fmt.Errorf("%w: %s não espera referência", ErrInvalidTransition, t.kind)
	}
	if next.IsZero() {
		return fmt.Errorf("%w: espera exige próxima tentativa", ErrInvalidTransition)
	}
	t.status = PendingReference
	t.attempts++
	t.nextAttemptAt = &next
	t.updatedAt = now
	return nil
}

// ─── leitura ────────────────────────────────────────────────────────────────

func (t *Transaction) ID() uuid.UUID               { return t.id }
func (t *Transaction) Origin() Origin              { return t.origin }
func (t *Transaction) Kind() Kind                  { return t.kind }
func (t *Transaction) Status() Status              { return t.status }
func (t *Transaction) WalletID() uuid.UUID         { return t.walletID }
func (t *Transaction) PlayerID() uuid.UUID         { return t.playerID }
func (t *Transaction) Amount() money.Money         { return t.amount }
func (t *Transaction) ProviderID() string          { return t.providerID }
func (t *Transaction) ExternalID() string          { return t.externalID }
func (t *Transaction) IdempotencyKey() string      { return t.idempotencyKey }
func (t *Transaction) PayloadHash() string         { return t.payloadHash }
func (t *Transaction) RoundID() string             { return t.roundID }
func (t *Transaction) GameID() string              { return t.gameID }
func (t *Transaction) ReferenceExtID() string      { return t.referenceExtID }
func (t *Transaction) ReferenceID() *uuid.UUID     { return t.referenceID }
func (t *Transaction) FailureCode() FailureCode    { return t.failureCode }
func (t *Transaction) ResultBalance() *money.Money { return t.resultBalance }
func (t *Transaction) Attempts() int               { return t.attempts }
func (t *Transaction) NextAttemptAt() *time.Time   { return t.nextAttemptAt }
func (t *Transaction) CorrelationID() string       { return t.correlationID }
func (t *Transaction) CreatedAt() time.Time        { return t.createdAt }
func (t *Transaction) UpdatedAt() time.Time        { return t.updatedAt }
