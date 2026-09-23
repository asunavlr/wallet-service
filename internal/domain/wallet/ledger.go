package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

// ErrInvalidLedgerEntry indica lançamento inconsistente.
var ErrInvalidLedgerEntry = errors.New("wallet: lançamento inválido")

// LedgerEntry é um lançamento imutável do ledger.
//
// O construtor valida balanceAfter = balanceBefore ± amount. A mesma conta é
// verificada de novo por CHECK no banco: o domínio protege o processo, a
// constraint protege o dado.
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// NewLedgerEntry constrói um lançamento validado.
func NewLedgerEntry(
	id, walletID, transactionID uuid.UUID,
	direction Direction,
	amount, before, after money.Money,
	now time.Time,
) (*LedgerEntry, error) {
	if id == uuid.Nil || walletID == uuid.Nil || transactionID == uuid.Nil {
		return nil, fmt.Errorf("%w: identificadores obrigatórios", ErrInvalidLedgerEntry)
	}
	if !direction.Valid() {
		return nil, fmt.Errorf("%w: direção %q", ErrInvalidLedgerEntry, direction)
	}
	if !amount.Valid() || !before.Valid() || !after.Valid() {
		return nil, fmt.Errorf("%w: valores obrigatórios", ErrInvalidLedgerEntry)
	}
	if !amount.IsPositive() {
		return nil, fmt.Errorf("%w: valor precisa ser positivo", ErrInvalidLedgerEntry)
	}
	if before.IsNegative() || after.IsNegative() {
		return nil, fmt.Errorf("%w: saldo negativo", ErrInvalidLedgerEntry)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: instante obrigatório", ErrInvalidLedgerEntry)
	}

	var esperado money.Money
	var err error
	if direction == Credit {
		esperado, err = before.Add(amount)
	} else {
		esperado, err = before.Sub(amount)
	}
	if err != nil {
		return nil, err
	}
	if !esperado.Equal(after) {
		return nil, fmt.Errorf("%w: %s %s %s daria %s, mas veio %s",
			ErrInvalidLedgerEntry, before, direction, amount, esperado, after)
	}

	return &LedgerEntry{
		id:            id,
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		amount:        amount,
		balanceBefore: before,
		balanceAfter:  after,
		createdAt:     now,
	}, nil
}

func (e *LedgerEntry) ID() uuid.UUID              { return e.id }
func (e *LedgerEntry) WalletID() uuid.UUID        { return e.walletID }
func (e *LedgerEntry) TransactionID() uuid.UUID   { return e.transactionID }
func (e *LedgerEntry) Direction() Direction       { return e.direction }
func (e *LedgerEntry) Amount() money.Money        { return e.amount }
func (e *LedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }
func (e *LedgerEntry) BalanceAfter() money.Money  { return e.balanceAfter }
func (e *LedgerEntry) CreatedAt() time.Time       { return e.createdAt }
