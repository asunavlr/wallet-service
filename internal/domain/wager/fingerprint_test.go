package wager_test

import (
	"strings"
	"testing"

	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

func base() wager.FingerprintInput {
	return wager.FingerprintInput{
		ProviderID: "provider-a",
		ExternalID: "transaction-123",
		PlayerID:   "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
		WalletID:   "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID:    "round-987",
		GameID:     "fortune-chimp",
		Kind:       wager.Bet,
		Amount:     brl("25.00"),
	}
}

func TestFingerprintEstavel(t *testing.T) {
	a := wager.Fingerprint(base())
	b := wager.Fingerprint(base())
	if a != b {
		t.Fatal("o mesmo conteúdo precisa gerar o mesmo hash")
	}
	if len(a) != 64 {
		t.Errorf("hash com %d caracteres, queria 64", len(a))
	}
}

// Cada campo de negócio precisa mudar o hash: se algum não muda, dois pedidos
// diferentes passariam como replay um do outro.
func TestTodoCampoDeNegocioAfetaOHash(t *testing.T) {
	original := wager.Fingerprint(base())

	mutacoes := map[string]func(*wager.FingerprintInput){
		"providerId": func(i *wager.FingerprintInput) { i.ProviderID = "provider-b" },
		"externalId": func(i *wager.FingerprintInput) { i.ExternalID = "transaction-124" },
		"playerId":   func(i *wager.FingerprintInput) { i.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" },
		"walletId":   func(i *wager.FingerprintInput) { i.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef38" },
		"roundId":    func(i *wager.FingerprintInput) { i.RoundID = "round-988" },
		"gameId":     func(i *wager.FingerprintInput) { i.GameID = "outro-jogo" },
		"kind":       func(i *wager.FingerprintInput) { i.Kind = wager.Win },
		"valor":      func(i *wager.FingerprintInput) { i.Amount = brl("25.01") },
		"moeda":      func(i *wager.FingerprintInput) { i.Amount = usd("25.00") },
		"referência": func(i *wager.FingerprintInput) { i.ReferenceExtID = "transaction-122" },
	}
	for nome, muta := range mutacoes {
		in := base()
		muta(&in)
		if wager.Fingerprint(in) == original {
			t.Errorf("mudar %s não mudou o hash", nome)
		}
	}
}

// UUID em maiúscula é o mesmo identificador. Sem essa normalização, o mesmo
// pedido com grafia diferente passaria como conteúdo divergente e viraria 409.
func TestUUIDNormalizaParaMinusculas(t *testing.T) {
	in := base()
	original := wager.Fingerprint(in)

	in.PlayerID = strings.ToUpper(in.PlayerID)
	in.WalletID = strings.ToUpper(in.WalletID)
	if wager.Fingerprint(in) != original {
		t.Error("a caixa do UUID não deveria mudar o hash")
	}
}

// Referência ausente é omitida; string vazia e ausência são o mesmo pedido.
func TestReferenciaAusenteEVaziaSaoIguais(t *testing.T) {
	in := base()
	in.ReferenceExtID = ""
	if wager.Fingerprint(in) != wager.Fingerprint(base()) {
		t.Error("referência vazia deveria equivaler a ausente")
	}
}
