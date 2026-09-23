// Package wager modela a operação de aposta: a transação, sua máquina de
// estados e as regras que decidem o que cada tipo faz com a carteira.
//
// A decisão vive em Decide, uma função pura. Ela não toca em banco, não
// movimenta a carteira e não emite evento — recebe o estado e devolve o que
// deveria acontecer. Isso é o que permite testar as regras dos cinco tipos
// externos sem subir nada.
package wager

import (
	"errors"
	"fmt"
)

// Kind é o tipo da operação.
type Kind string

const (
	// Opening é a abertura interna de carteira. Nunca chega por HTTP ou SQS.
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

// External informa se o tipo pode chegar de um provedor.
// OPENING é reservado à origem interna e precisa ser recusado na borda.
func (k Kind) External() bool {
	switch k {
	case Bet, Win, Loss, Refund, Rollback:
		return true
	default:
		return false
	}
}

// IsReversal informa se o tipo desfaz outra operação.
func (k Kind) IsReversal() bool { return k == Refund || k == Rollback }

// RequiresReference informa se o tipo exige referência externa declarada.
func (k Kind) RequiresReference() bool { return k.IsReversal() }

// AllowsReference informa se o tipo aceita referência.
// WIN pode citar a aposta da mesma rodada; BET e LOSS não referenciam nada.
func (k Kind) AllowsReference() bool { return k.IsReversal() || k == Win }

// Status é o estado da transação.
type Status string

const (
	// Pending é o estado inicial. Existe apenas em memória: operações sem
	// dependência são concluídas na mesma transação SQL, sem commit de
	// aceite, então nenhum PENDING é persistido — e por isso não há pendência
	// órfã para retomar depois de uma queda. Ver ARCHITECTURE.md.
	Pending Status = "PENDING"
	// PendingReference é a única espera durável.
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

// Terminal informa se o estado não admite mais transição.
func (s Status) Terminal() bool {
	return s == Processed || s == Rejected || s == Failed
}

// Origin distingue operação interna de externa.
type Origin string

const (
	Internal Origin = "INTERNAL"
	External Origin = "EXTERNAL"
)

// FailureCode é o motivo estável de uma rejeição ou falha.
//
// Os códigos são parte do contrato: o provedor decide o que fazer a partir
// deles, então precisam distinguir entrada corrigível de resultado
// definitivo. Estão documentados no README da solução.
type FailureCode string

const (
	// InsufficientFunds: a aposta não cabe no saldo. O jogador está sem
	// dinheiro — situação normal de operação.
	InsufficientFunds FailureCode = "INSUFFICIENT_FUNDS"
	// ReversalInsufficientFunds: a reversão precisaria debitar mais do que há.
	// Separado do anterior de propósito, como o enunciado exige: aqui o
	// dinheiro já saiu da carteira, e isso é incidente, não rotina.
	ReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	// ReferenceNotFound: a referência nunca chegou dentro do prazo.
	ReferenceNotFound FailureCode = "REFERENCE_NOT_FOUND"
	// ReferenceNotProcessed: a referência existe mas terminou sem sucesso.
	ReferenceNotProcessed FailureCode = "REFERENCE_NOT_PROCESSED"
	// ReferenceMismatch: a referência existe mas discorda em provedor,
	// jogador, carteira, moeda ou rodada.
	ReferenceMismatch FailureCode = "REFERENCE_MISMATCH"
	// ReferenceKindInvalid: o tipo referenciado não admite essa reversão.
	ReferenceKindInvalid FailureCode = "REFERENCE_KIND_INVALID"
	// AlreadyReversed: a referência já foi revertida uma vez.
	AlreadyReversed FailureCode = "ALREADY_REVERSED"
	// ReversalAmountMismatch: reversão parcial, que está fora do escopo.
	ReversalAmountMismatch FailureCode = "REVERSAL_AMOUNT_MISMATCH"
	// CurrencyMismatch: moeda diferente da carteira.
	CurrencyMismatch FailureCode = "CURRENCY_MISMATCH"
	// InvalidAmount: valor fora da política do tipo.
	InvalidAmount FailureCode = "INVALID_AMOUNT"
	// WalletNotFound: a carteira informada não existe.
	WalletNotFound FailureCode = "WALLET_NOT_FOUND"
	// PlayerMismatch: a carteira não é do jogador informado.
	PlayerMismatch FailureCode = "PLAYER_MISMATCH"
	// InternalFailure: falha permanente de infraestrutura, registrada para
	// auditoria. É o único código que acompanha FAILED.
	InternalFailure FailureCode = "INTERNAL_FAILURE"
)

// Erros de domínio, classificáveis com errors.Is.
var (
	ErrInvalidTransaction = errors.New("wager: transação inválida")
	ErrInvalidTransition  = errors.New("wager: transição inválida")
	ErrTerminal           = errors.New("wager: transação em estado terminal")
)

// ValidKind valida o texto de um tipo.
func ValidKind(s string) (Kind, error) {
	k := Kind(s)
	switch k {
	case Opening, Bet, Win, Loss, Refund, Rollback:
		return k, nil
	default:
		return "", fmt.Errorf("%w: tipo %q", ErrInvalidTransaction, s)
	}
}

// ValidExternalKind valida o texto de um tipo aceito na borda.
// É aqui que OPENING vindo de provedor é recusado.
func ValidExternalKind(s string) (Kind, error) {
	k, err := ValidKind(s)
	if err != nil {
		return "", err
	}
	if !k.External() {
		return "", fmt.Errorf("%w: %q não pode vir de um provedor", ErrInvalidTransaction, s)
	}
	return k, nil
}
