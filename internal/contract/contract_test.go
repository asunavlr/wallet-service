package contract_test

import (
	"strings"
	"testing"

	"github.com/kevinmatos/wallet-service/internal/contract"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

// O decoder é COMPARTILHADO por HTTP e SQS: o que ele recusa, as duas
// entradas recusam igual. Testá-lo aqui é testar as duas de uma vez.
func TestDecodeRecusaCorpoAmbiguo(t *testing.T) {
	casos := map[string]string{
		"vazio":                 ``,
		"json quebrado":         `{`,
		"campo desconhecido":    `{"providerId":"a","inexistente":1}`,
		"lixo depois do objeto": `{"providerId":"a"} lixo`,
		"dois objetos":          `{"providerId":"a"}{"providerId":"b"}`,
		"array no lugar":        `[{"providerId":"a"}]`,
		"número no lugar":       `42`,
	}
	for nome, corpo := range casos {
		t.Run(nome, func(t *testing.T) {
			var op contract.Operation
			if err := contract.Decode(strings.NewReader(corpo), &op, 1<<16); err == nil {
				t.Error("deveria ser recusado")
			}
		})
	}
}

// DisallowUnknownFields protege contra o erro de digitação que passaria
// batido: "amout" viraria valor zero em silêncio em vez de 400.
func TestTypoEmCampoNaoPassaEmSilencio(t *testing.T) {
	var op contract.Operation
	err := contract.DecodeBytes([]byte(`{"providerId":"a","amout":{"amount":"1.00","currency":"BRL"}}`), &op)
	if err == nil {
		t.Fatal("campo com typo deveria ser recusado")
	}
}

// O limite de corpo existe para que um payload gigante não vire consumo de
// memória antes de qualquer validação.
func TestLimiteDeCorpo(t *testing.T) {
	grande := `{"providerId":"` + strings.Repeat("x", 10000) + `"}`
	var op contract.Operation
	if err := contract.Decode(strings.NewReader(grande), &op, 512); err == nil {
		t.Error("corpo acima do limite deveria ser recusado")
	}
}

// amount é string, sempre. Um número JSON no lugar é a porta de entrada do
// float, e precisa ser barrada na borda.
func TestAmountComoNumeroERecusado(t *testing.T) {
	var op contract.Operation
	err := contract.DecodeBytes([]byte(
		`{"providerId":"a","externalTransactionId":"t","playerId":"p","walletId":"w",`+
			`"roundId":"r","gameId":"g","kind":"BET","money":{"amount":25.00,"currency":"BRL"}}`), &op)
	if err == nil {
		t.Fatal("amount numérico deveria ser recusado no decoder")
	}
}

func operacaoValida() contract.Operation {
	return contract.Operation{
		ProviderID: "provider-a", ExternalID: "tx-1",
		PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
		WalletID: "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID:  "round-1", GameID: "fortune-chimp", Kind: "BET",
		Money: &contract.Amount{Amount: "25.00", Currency: "BRL"},
	}
}

func TestValidateAceitaOperacaoCompleta(t *testing.T) {
	p, err := operacaoValida().Validate()
	if err != nil {
		t.Fatalf("operação válida recusada: %v", err)
	}
	if p.Money.String() != "25.00" || p.Kind != "BET" {
		t.Errorf("= %s %s", p.Money, p.Kind)
	}
}

func TestValidateExigeCadaCampo(t *testing.T) {
	casos := map[string]func(*contract.Operation){
		"providerId":            func(o *contract.Operation) { o.ProviderID = "" },
		"externalTransactionId": func(o *contract.Operation) { o.ExternalID = "" },
		"roundId":               func(o *contract.Operation) { o.RoundID = "" },
		"gameId":                func(o *contract.Operation) { o.GameID = "" },
		"money":                 func(o *contract.Operation) { o.Money = nil },
		"playerId não é UUID":   func(o *contract.Operation) { o.PlayerID = "eu" },
		"walletId não é UUID":   func(o *contract.Operation) { o.WalletID = "minha" },
		"kind desconhecido":     func(o *contract.Operation) { o.Kind = "TRANSFER" },
		"kind vazio":            func(o *contract.Operation) { o.Kind = "" },
	}
	for nome, muta := range casos {
		t.Run(nome, func(t *testing.T) {
			o := operacaoValida()
			muta(&o)
			if _, err := o.Validate(); err == nil {
				t.Error("deveria ser recusado")
			}
		})
	}
}

// OPENING é interno e precisa ser recusado na borda, venha por onde vier.
func TestOpeningRecusadoNaBorda(t *testing.T) {
	o := operacaoValida()
	o.Kind = "OPENING"
	if _, err := o.Validate(); err == nil {
		t.Fatal("OPENING não pode entrar por provedor")
	}
}

// Of e Money são inversos: o valor atravessa o contrato sem perder nada.
func TestAmountIdaEVolta(t *testing.T) {
	for _, s := range []string{"0.00", "0.01", "25.00", "1000.00", "92233720368547758.07"} {
		m := money.MustParse(s, "BRL")
		a := contract.Of(m)
		if a.Amount != s || a.Currency != "BRL" {
			t.Errorf("Of(%s) = %+v", s, a)
		}
		volta, err := a.Money()
		if err != nil || !volta.Equal(m) {
			t.Errorf("ida e volta de %s = %v, %v", s, volta, err)
		}
	}
}
