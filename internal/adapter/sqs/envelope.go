// Package sqs implementa o consumidor e o publicador de mensagens.
package sqs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/kevinmatos/wallet-service/internal/contract"
)

// TipoOperacao é o único tipo de mensagem que o consumidor aceita.
const TipoOperacao = "WagerTransactionRequested"

// Envelope é a moldura da mensagem de entrada.
type Envelope struct {
	MessageID  string             `json:"messageId"`
	Type       string             `json:"type"`
	OccurredAt string             `json:"occurredAt"`
	Data       contract.Operation `json:"data"`
}

// Validate confere a moldura e o conteúdo.
//
// Mensagem malformada é erro PERMANENTE: reentregar dá o mesmo resultado, e
// o lugar dela é a DLQ, não a fila.
func (e Envelope) Validate() (contract.Parsed, error) {
	if e.MessageID == "" {
		return contract.Parsed{}, fmt.Errorf("%w: messageId obrigatório", contract.ErrDecode)
	}
	if e.Type != TipoOperacao {
		return contract.Parsed{}, fmt.Errorf("%w: tipo %q não é tratado por este consumidor", contract.ErrDecode, e.Type)
	}
	if e.OccurredAt != "" {
		if _, err := time.Parse(time.RFC3339, e.OccurredAt); err != nil {
			return contract.Parsed{}, fmt.Errorf("%w: occurredAt não é RFC 3339", contract.ErrDecode)
		}
	}
	// Pelo SQS a chave vem do corpo; por HTTP vem do header. Nos dois casos
	// ela é do cliente, e o servidor não a substitui por uma calculada.
	if e.Data.IdempotencyKey == "" {
		return contract.Parsed{}, fmt.Errorf("%w: data.idempotencyKey obrigatório", contract.ErrDecode)
	}
	return e.Data.Validate()
}

// HashDaMensagem identifica o conteúdo do envelope para a inbox.
//
// Serve a uma pergunta diferente da do fingerprint de negócio: aqui a questão
// é se o MESMO messageId voltou com outro conteúdo, o que é adulteração ou
// bug do produtor — e erro permanente. Por isso inclui o tipo e a chave.
func HashDaMensagem(e Envelope, fingerprintDeNegocio string) string {
	h := sha256.New()
	h.Write([]byte(e.Type))
	h.Write([]byte{0})
	h.Write([]byte(e.Data.IdempotencyKey))
	h.Write([]byte{0})
	h.Write([]byte(fingerprintDeNegocio))
	return hex.EncodeToString(h.Sum(nil))
}
