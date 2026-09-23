// Package wallet modela a carteira, que é a raiz do agregado financeiro.
//
// Toda mudança de saldo passa por Apply, e Apply devolve o lançamento que a
// acompanha. Não existe setter de saldo: é isso que torna impossível, no
// domínio, mexer no dinheiro sem produzir o registro correspondente.
package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

var (
	// ErrInsufficientFunds indica débito maior que o saldo.
	ErrInsufficientFunds = errors.New("wallet: saldo insuficiente")
	// ErrCurrencyMismatch indica movimentação em moeda diferente da carteira.
	ErrCurrencyMismatch = errors.New("wallet: moeda diferente da carteira")
	// ErrInvalidMovement indica valor de movimentação inválido.
	ErrInvalidMovement = errors.New("wallet: movimentação inválida")
	// ErrInvalidWallet indica dados inconsistentes na construção.
	ErrInvalidWallet = errors.New("wallet: carteira inválida")
)

// Direction é o sentido de um lançamento.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Valid informa se a direção é conhecida.
func (d Direction) Valid() bool { return d == Debit || d == Credit }

// Wallet é a raiz do agregado. Os campos são privados: o saldo só muda por
// Apply, e a versão só anda junto com ele.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// Open cria uma carteira nova.
//
// A versão inicial é 1 e permanece 1 mesmo quando há saldo inicial: o
// enunciado é explícito nisso. O crédito de abertura não é uma movimentação
// sobre uma carteira que já existia — ele nasce junto com ela.
func Open(id, playerID uuid.UUID, initial money.Money, now time.Time) (*Wallet, error) {
	if id == uuid.Nil || playerID == uuid.Nil {
		return nil, fmt.Errorf("%w: identificadores obrigatórios", ErrInvalidWallet)
	}
	if !initial.Valid() {
		return nil, fmt.Errorf("%w: saldo inicial inválido", ErrInvalidWallet)
	}
	if initial.IsNegative() {
		return nil, fmt.Errorf("%w: saldo inicial negativo", ErrInvalidWallet)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: instante obrigatório", ErrInvalidWallet)
	}
	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  initial.Currency(),
		balance:   initial,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// Rehydrate reconstrói uma carteira lida do banco.
//
// Não reaplica movimentação nem produz lançamento: é leitura de estado já
// consolidado. As validações existem para que uma linha corrompida vire erro
// aqui, e não um saldo errado lá adiante.
func Rehydrate(
	id, playerID uuid.UUID,
	balance money.Money,
	version int64,
	createdAt, updatedAt time.Time,
) (*Wallet, error) {
	if id == uuid.Nil || playerID == uuid.Nil {
		return nil, fmt.Errorf("%w: identificadores obrigatórios", ErrInvalidWallet)
	}
	if !balance.Valid() {
		return nil, fmt.Errorf("%w: saldo inválido", ErrInvalidWallet)
	}
	if balance.IsNegative() {
		return nil, fmt.Errorf("%w: saldo negativo persistido", ErrInvalidWallet)
	}
	if version < 1 {
		return nil, fmt.Errorf("%w: versão %d", ErrInvalidWallet, version)
	}
	if createdAt.IsZero() || updatedAt.IsZero() {
		return nil, fmt.Errorf("%w: instantes obrigatórios", ErrInvalidWallet)
	}
	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  balance.Currency(),
		balance:   balance,
		version:   version,
		createdAt: createdAt,
		updatedAt: updatedAt,
	}, nil
}

func (w *Wallet) ID() uuid.UUID            { return w.id }
func (w *Wallet) PlayerID() uuid.UUID      { return w.playerID }
func (w *Wallet) Currency() money.Currency { return w.currency }
func (w *Wallet) Balance() money.Money     { return w.balance }
func (w *Wallet) Version() int64           { return w.version }
func (w *Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.updatedAt }

// Apply movimenta o saldo e devolve o lançamento correspondente.
//
// É o único caminho que muda o saldo. O valor é sempre positivo — o sentido
// vem de `direction` —, e um débito nunca pode deixar o saldo negativo.
func (w *Wallet) Apply(
	entryID, transactionID uuid.UUID,
	direction Direction,
	amount money.Money,
	now time.Time,
) (*LedgerEntry, error) {
	if !direction.Valid() {
		return nil, fmt.Errorf("%w: direção %q", ErrInvalidMovement, direction)
	}
	if !amount.Valid() {
		return nil, fmt.Errorf("%w: valor inválido", ErrInvalidMovement)
	}
	if !amount.IsPositive() {
		return nil, fmt.Errorf("%w: valor precisa ser positivo", ErrInvalidMovement)
	}
	if amount.Currency() != w.currency {
		return nil, fmt.Errorf("%w: %s em carteira %s", ErrCurrencyMismatch, amount.Currency(), w.currency)
	}

	before := w.balance
	var after money.Money
	var err error
	if direction == Credit {
		after, err = before.Add(amount)
	} else {
		after, err = before.Sub(amount)
	}
	if err != nil {
		return nil, err
	}
	if after.IsNegative() {
		return nil, fmt.Errorf("%w: saldo %s, débito %s", ErrInsufficientFunds, before, amount)
	}

	entry, err := NewLedgerEntry(entryID, w.id, transactionID, direction, amount, before, after, now)
	if err != nil {
		return nil, err
	}

	w.balance = after
	w.version++
	w.updatedAt = now
	return entry, nil
}

// CanDebit informa se um débito cabe no saldo, sem aplicá-lo.
//
// Serve para decidir a rejeição antes de mexer no agregado: uma aposta sem
// saldo é rejeitada e registrada, não é um erro que aborta o fluxo.
func (w *Wallet) CanDebit(amount money.Money) bool {
	if !amount.Valid() || amount.Currency() != w.currency {
		return false
	}
	c, err := w.balance.Cmp(amount)
	return err == nil && c >= 0
}
