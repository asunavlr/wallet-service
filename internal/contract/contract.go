// Package contract guarda as formas de wire compartilhadas por HTTP e SQS.
//
// As duas entradas decodificam com o mesmo código, então rejeitam exatamente
// as mesmas coisas — o enunciado exige que compartilhem as garantias, e
// dividir o decoder é o jeito de isso não divergir com o tempo.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

// ErrDecode indica corpo malformado.
var ErrDecode = errors.New("contract: corpo inválido")

// Amount é o objeto monetário do contrato: {"amount":"25.00","currency":"BRL"}.
//
// `amount` é string, nunca número. Um número JSON no lugar é recusado com 400
// — é a barreira que impede um float de entrar pela borda.
type Amount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Money converte para o value object, validando.
func (a Amount) Money() (money.Money, error) { return money.Parse(a.Amount, a.Currency) }

// Of converte do value object para a forma do contrato.
func Of(m money.Money) Amount {
	return Amount{Amount: m.String(), Currency: string(m.Currency())}
}

// Operation é o corpo de uma operação de aposta, idêntico por HTTP e por SQS.
type Operation struct {
	ProviderID     string  `json:"providerId"`
	ExternalID     string  `json:"externalTransactionId"`
	IdempotencyKey string  `json:"idempotencyKey,omitempty"` // só no SQS
	PlayerID       string  `json:"playerId"`
	WalletID       string  `json:"walletId"`
	RoundID        string  `json:"roundId"`
	GameID         string  `json:"gameId"`
	Kind           string  `json:"kind"`
	Money          *Amount `json:"money"`
	ReferenceExtID string  `json:"referenceExternalTransactionId,omitempty"`
}

// Parsed é a operação com os tipos do domínio já resolvidos.
type Parsed struct {
	PlayerID uuid.UUID
	WalletID uuid.UUID
	Kind     wager.Kind
	Money    money.Money
}

// Validate confere os campos obrigatórios e converte os tipos.
//
// Entrada inválida é permanente e corrigível: não é persistida, o cliente
// corrige e reenvia com a mesma chave de idempotência.
func (o Operation) Validate() (Parsed, error) {
	var p Parsed
	if o.ProviderID == "" {
		return p, fmt.Errorf("%w: providerId obrigatório", ErrDecode)
	}
	if o.ExternalID == "" {
		return p, fmt.Errorf("%w: externalTransactionId obrigatório", ErrDecode)
	}
	if o.RoundID == "" {
		return p, fmt.Errorf("%w: roundId obrigatório", ErrDecode)
	}
	if o.GameID == "" {
		return p, fmt.Errorf("%w: gameId obrigatório", ErrDecode)
	}
	if o.Money == nil {
		return p, fmt.Errorf("%w: money obrigatório", ErrDecode)
	}

	player, err := uuid.Parse(o.PlayerID)
	if err != nil {
		return p, fmt.Errorf("%w: playerId não é um UUID", ErrDecode)
	}
	w, err := uuid.Parse(o.WalletID)
	if err != nil {
		return p, fmt.Errorf("%w: walletId não é um UUID", ErrDecode)
	}
	// ValidExternalKind é o que recusa OPENING vindo de provedor.
	kind, err := wager.ValidExternalKind(o.Kind)
	if err != nil {
		return p, fmt.Errorf("%w: %s", ErrDecode, err)
	}
	valor, err := o.Money.Money()
	if err != nil {
		return p, fmt.Errorf("%w: %s", ErrDecode, err)
	}
	return Parsed{PlayerID: player, WalletID: w, Kind: kind, Money: valor}, nil
}

// Decode lê um único objeto JSON, recusando campo desconhecido e lixo depois
// do objeto.
//
// DisallowUnknownFields protege contra erro de digitação que passaria batido:
// "amout" em vez de "amount" viraria valor zero silencioso em vez de 400.
func Decode(r io.Reader, destino any, limite int64) error {
	dec := json.NewDecoder(io.LimitReader(r, limite))
	dec.DisallowUnknownFields()
	if err := dec.Decode(destino); err != nil {
		return fmt.Errorf("%w: %s", ErrDecode, err)
	}
	// nada pode vir depois do objeto
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("%w: conteúdo extra após o objeto JSON", ErrDecode)
	}
	return nil
}

// DecodeBytes é Decode sobre um slice, para o consumidor do SQS.
func DecodeBytes(b []byte, destino any) error {
	return Decode(bytes.NewReader(b), destino, int64(len(b))+1)
}
