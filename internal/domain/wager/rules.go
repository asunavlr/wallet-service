package wager

import (
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

// Action é o que Decide manda fazer.
type Action string

const (
	// ActionMove aplica uma movimentação na carteira.
	ActionMove Action = "MOVE"
	// ActionNoMove conclui sem tocar no saldo. É o caso do LOSS: processa,
	// emite WagerTransactionProcessed, mas não cria lançamento nem versiona.
	ActionNoMove Action = "NO_MOVE"
	// ActionReject recusa por regra de negócio, com código auditável.
	ActionReject Action = "REJECT"
	// ActionAwait registra espera por uma referência que ainda não chegou.
	ActionAwait Action = "AWAIT"
)

// Decision é o resultado da avaliação das regras.
type Decision struct {
	Action    Action
	Direction wallet.Direction // só em ActionMove
	Amount    money.Money      // só em ActionMove
	Code      FailureCode      // só em ActionReject
}

// Operation é o pedido já validado sintaticamente, pronto para decisão.
type Operation struct {
	Kind     Kind
	Amount   money.Money
	RoundID  string
	Provider string
}

// Reference é a operação referenciada, quando ela existe.
type Reference struct {
	Kind     Kind
	Status   Status
	Amount   money.Money
	RoundID  string
	Provider string
	PlayerID string
	WalletID string
	// Reversed informa se essa referência já recebeu uma reversão
	// bem-sucedida, de qualquer tipo.
	Reversed bool
}

// rollbackDirection diz para que lado um ROLLBACK move, por tipo revertido.
//
// A tabela é explícita de propósito. Um tipo fora dela é rejeitado com
// REFERENCE_KIND_INVALID em vez de virar "processado sem mover dinheiro",
// que é o desfecho silencioso e errado.
var rollbackDirection = map[Kind]wallet.Direction{
	Bet:    wallet.Credit, // desfazer um débito é creditar
	Win:    wallet.Debit,  // desfazer um crédito é debitar
	Refund: wallet.Debit,  // desfazer um estorno é debitar de novo
}

// Decide aplica as regras de negócio dos cinco tipos externos.
//
// É uma função pura: mesmo estado, mesma decisão. Não movimenta a carteira,
// não escreve no banco e não emite evento — quem faz isso é o caso de uso, a
// partir do que ela devolve.
//
// `ref` é nil quando a operação não referencia nada ou quando a referência
// ainda não chegou; `refDeclared` distingue os dois casos.
func Decide(w *wallet.Wallet, op Operation, ref *Reference, refDeclared bool) Decision {
	if op.Amount.Currency() != w.Currency() {
		return Decision{Action: ActionReject, Code: CurrencyMismatch}
	}

	// LOSS não move dinheiro. O valor zero já foi exigido na construção;
	// aqui a regra é só não produzir lançamento.
	if op.Kind == Loss {
		if !op.Amount.IsZero() {
			return Decision{Action: ActionReject, Code: InvalidAmount}
		}
		return Decision{Action: ActionNoMove}
	}

	if !op.Amount.IsPositive() {
		return Decision{Action: ActionReject, Code: InvalidAmount}
	}

	// A referência foi declarada mas ainda não existe: espera durável.
	if refDeclared && ref == nil {
		return Decision{Action: ActionAwait}
	}

	if ref != nil {
		if d, ok := checkReference(op, *ref); !ok {
			return d
		}
	}

	switch op.Kind {
	case Bet:
		if !w.CanDebit(op.Amount) {
			return Decision{Action: ActionReject, Code: InsufficientFunds}
		}
		return Decision{Action: ActionMove, Direction: wallet.Debit, Amount: op.Amount}

	case Win:
		// A referência do WIN é informativa: ela amarra o ganho à aposta da
		// rodada, mas não muda o sentido nem o valor do crédito.
		return Decision{Action: ActionMove, Direction: wallet.Credit, Amount: op.Amount}

	case Refund:
		// REFUND devolve integralmente uma BET processada.
		if ref.Kind != Bet {
			return Decision{Action: ActionReject, Code: ReferenceKindInvalid}
		}
		return Decision{Action: ActionMove, Direction: wallet.Credit, Amount: op.Amount}

	case Rollback:
		dir, ok := rollbackDirection[ref.Kind]
		if !ok {
			return Decision{Action: ActionReject, Code: ReferenceKindInvalid}
		}
		// Um rollback que debita pode não caber no saldo. O código é
		// diferente do de aposta sem saldo, como o enunciado exige: aqui o
		// dinheiro já foi embora, e isso precisa ser distinguível na auditoria.
		if dir == wallet.Debit && !w.CanDebit(op.Amount) {
			return Decision{Action: ActionReject, Code: ReversalInsufficientFunds}
		}
		return Decision{Action: ActionMove, Direction: dir, Amount: op.Amount}
	}

	return Decision{Action: ActionReject, Code: InvalidAmount}
}

// checkReference valida a coerência entre a operação e sua referência.
// Devolve ok=false e a rejeição correspondente quando algo não bate.
func checkReference(op Operation, ref Reference) (Decision, bool) {
	// Uma referência só serve depois de ter sido efetivamente processada.
	if ref.Status != Processed {
		if ref.Status == PendingReference {
			// Ainda pode dar certo: continua esperando em vez de rejeitar.
			return Decision{Action: ActionAwait}, false
		}
		return Decision{Action: ActionReject, Code: ReferenceNotProcessed}, false
	}
	// Provedor e rodada precisam concordar. Jogador, carteira e moeda são
	// conferidos pelo caso de uso, que tem os identificadores resolvidos.
	if ref.Provider != op.Provider || ref.RoundID != op.RoundID {
		return Decision{Action: ActionReject, Code: ReferenceMismatch}, false
	}
	if ref.Amount.Currency() != op.Amount.Currency() {
		return Decision{Action: ActionReject, Code: ReferenceMismatch}, false
	}

	if op.Kind.IsReversal() {
		// Uma transação recebe no máximo uma reversão bem-sucedida, de
		// qualquer tipo: REFUND e ROLLBACK de uma mesma BET devolveriam o
		// mesmo débito duas vezes. O índice parcial no banco impõe o mesmo.
		if ref.Reversed {
			return Decision{Action: ActionReject, Code: AlreadyReversed}, false
		}
		// Reversão parcial está fora do escopo do desafio.
		if !ref.Amount.Equal(op.Amount) {
			return Decision{Action: ActionReject, Code: ReversalAmountMismatch}, false
		}
	}
	return Decision{}, true
}
