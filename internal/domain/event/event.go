// Package event define os eventos de integração publicados pelo serviço.
//
// Tipo e versão são definidos pelo construtor de cada evento, não pelo
// chamador: é o que impede um publicador de inventar um tipo. Timestamps em
// RFC 3339 UTC e valores monetários em string decimal, como o contrato exige.
package event

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

// Tipos dos eventos exigidos pelo enunciado.
const (
	TypeProcessed        = "WagerTransactionProcessed"
	TypeRejected         = "WagerTransactionRejected"
	TypeBalanceChanged   = "WalletBalanceChanged"
	TypePendingReference = "WagerTransactionPendingReference"
)

// Version é a versão do contrato dos eventos.
const Version = 1

// Amount é a forma de um valor monetário no contrato externo.
type Amount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Of converte Money para a forma do contrato.
func Of(m money.Money) Amount {
	return Amount{Amount: m.String(), Currency: string(m.Currency())}
}

// Envelope é a moldura comum a todo evento.
type Envelope struct {
	EventID       uuid.UUID       `json:"eventId"`
	EventType     string          `json:"eventType"`
	AggregateID   uuid.UUID       `json:"aggregateId"`
	CorrelationID string          `json:"correlationId"`
	CausationID   string          `json:"causationId,omitempty"`
	OccurredAt    string          `json:"occurredAt"`
	Version       int             `json:"version"`
	Data          json.RawMessage `json:"data"`
}

// ProcessedData acompanha a conclusão de uma operação, incluindo LOSS.
type ProcessedData struct {
	TransactionID uuid.UUID `json:"transactionId"`
	WalletID      uuid.UUID `json:"walletId"`
	PlayerID      uuid.UUID `json:"playerId"`
	ProviderID    string    `json:"providerId,omitempty"`
	ExternalID    string    `json:"externalTransactionId,omitempty"`
	RoundID       string    `json:"roundId,omitempty"`
	GameID        string    `json:"gameId,omitempty"`
	Kind          string    `json:"kind"`
	Money         Amount    `json:"money"`
	Balance       Amount    `json:"balance"`
}

// RejectedData acompanha a recusa definitiva por regra de negócio.
type RejectedData struct {
	TransactionID uuid.UUID `json:"transactionId"`
	WalletID      uuid.UUID `json:"walletId"`
	ProviderID    string    `json:"providerId,omitempty"`
	ExternalID    string    `json:"externalTransactionId,omitempty"`
	Kind          string    `json:"kind"`
	Money         Amount    `json:"money"`
	FailureCode   string    `json:"failureCode"`
}

// BalanceChangedData acompanha toda alteração efetiva de saldo.
// Os campos são os exigidos nominalmente pelo enunciado.
type BalanceChangedData struct {
	WalletID      uuid.UUID `json:"walletId"`
	TransactionID uuid.UUID `json:"transactionId"`
	Direction     string    `json:"direction"`
	Money         Amount    `json:"money"`
	BalanceBefore Amount    `json:"balanceBefore"`
	BalanceAfter  Amount    `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
}

// PendingReferenceData acompanha o registro de espera por referência.
type PendingReferenceData struct {
	TransactionID  uuid.UUID `json:"transactionId"`
	WalletID       uuid.UUID `json:"walletId"`
	ProviderID     string    `json:"providerId"`
	ExternalID     string    `json:"externalTransactionId"`
	ReferenceExtID string    `json:"referenceExternalTransactionId"`
	Kind           string    `json:"kind"`
	Attempts       int       `json:"attempts"`
	NextAttemptAt  string    `json:"nextAttemptAt"`
}

// build monta o envelope com tipo e versão fixados pelo construtor.
func build(eventID uuid.UUID, tipo string, agg uuid.UUID, correlation, causation string, at time.Time, data any) (Envelope, error) {
	bruto, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		EventID:       eventID,
		EventType:     tipo,
		AggregateID:   agg,
		CorrelationID: correlation,
		CausationID:   causation,
		OccurredAt:    at.UTC().Format(time.RFC3339Nano),
		Version:       Version,
		Data:          bruto,
	}, nil
}

func NewProcessed(id, agg uuid.UUID, correlation, causation string, at time.Time, d ProcessedData) (Envelope, error) {
	return build(id, TypeProcessed, agg, correlation, causation, at, d)
}

func NewRejected(id, agg uuid.UUID, correlation, causation string, at time.Time, d RejectedData) (Envelope, error) {
	return build(id, TypeRejected, agg, correlation, causation, at, d)
}

func NewBalanceChanged(id, agg uuid.UUID, correlation, causation string, at time.Time, d BalanceChangedData) (Envelope, error) {
	return build(id, TypeBalanceChanged, agg, correlation, causation, at, d)
}

func NewPendingReference(id, agg uuid.UUID, correlation, causation string, at time.Time, d PendingReferenceData) (Envelope, error) {
	return build(id, TypePendingReference, agg, correlation, causation, at, d)
}

// Marshal serializa o envelope para o snapshot da outbox.
func (e Envelope) Marshal() ([]byte, error) { return json.Marshal(e) }
