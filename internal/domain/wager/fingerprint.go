package wager

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

// FingerprintInput são os campos de negócio que entram no hash.
//
// O que NÃO entra, e por quê: a chave de idempotência (o enunciado proíbe
// explicitamente confundir chave com conteúdo), os headers HTTP e o envelope
// do SQS — messageId, occurredAt, type — porque são metadados de transporte.
// Se entrassem, a mesma operação chegando pelos dois caminhos geraria hashes
// diferentes e a deduplicação cruzada, que o enunciado exige, nunca casaria.
type FingerprintInput struct {
	ProviderID     string
	ExternalID     string
	PlayerID       string
	WalletID       string
	RoundID        string
	GameID         string
	Kind           Kind
	Amount         money.Money
	ReferenceExtID string
}

// canonical é a forma serializada do hash.
//
// As tags definem a ordem alfabética das chaves, que é como o JSON canônico é
// produzido: o encoder do Go respeita a ordem dos campos do struct, então
// declará-los em ordem alfabética torna a canonicalização explícita e
// verificável, em vez de depender de ordenação de mapa.
type canonical struct {
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	ExternalID string `json:"externalTransactionId"`
	GameID     string `json:"gameId"`
	Kind       string `json:"kind"`
	PlayerID   string `json:"playerId"`
	ProviderID string `json:"providerId"`
	Reference  string `json:"referenceExternalTransactionId,omitempty"`
	RoundID    string `json:"roundId"`
	WalletID   string `json:"walletId"`
}

// Fingerprint devolve o SHA-256 hexadecimal do JSON canônico dos campos de
// negócio.
//
// Normalização aplicada antes do hash, conforme o enunciado pede que seja
// documentada:
//   - UUIDs em minúsculas, porque "0192F2…" e "0192f2…" são o mesmo jogador;
//   - o valor monetário entra como a string canônica de duas casas, que é a
//     única forma que o parser aceita — não há outra grafia a normalizar;
//   - referência ausente é omitida, e não serializada como string vazia.
func Fingerprint(in FingerprintInput) string {
	c := canonical{
		Amount:     in.Amount.String(),
		Currency:   string(in.Amount.Currency()),
		ExternalID: in.ExternalID,
		GameID:     in.GameID,
		Kind:       string(in.Kind),
		PlayerID:   strings.ToLower(in.PlayerID),
		ProviderID: in.ProviderID,
		Reference:  in.ReferenceExtID,
		RoundID:    in.RoundID,
		WalletID:   strings.ToLower(in.WalletID),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Sem escape de HTML: o encoder do Go converte <, > e & em < e
	// companhia por padrão. Isso não muda o significado, mas mudaria o hash
	// de um gameId que contivesse esses caracteres.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(c) // canonical não tem campo que possa falhar na serialização

	// Encode acrescenta \n; removê-lo mantém o hash sobre o JSON exato.
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return hex.EncodeToString(sum[:])
}
