package wager_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

var agora = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func brl(s string) money.Money { return money.MustParse(s, "BRL") }
func usd(s string) money.Money { return money.MustParse(s, "USD") }

func carteira(t *testing.T, saldo string) *wallet.Wallet {
	t.Helper()
	w, err := wallet.Open(uuid.New(), uuid.New(), brl(saldo), agora)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func op(k wager.Kind, valor string) wager.Operation {
	return wager.Operation{Kind: k, Amount: brl(valor), RoundID: "round-1", Provider: "provider-a"}
}

func refProcessada(k wager.Kind, valor string) *wager.Reference {
	return &wager.Reference{
		Kind: k, Status: wager.Processed, Amount: brl(valor),
		RoundID: "round-1", Provider: "provider-a",
	}
}

func TestBet(t *testing.T) {
	w := carteira(t, "100.00")

	d := wager.Decide(w, op(wager.Bet, "80.00"), nil, false)
	if d.Action != wager.ActionMove || d.Direction != wallet.Debit {
		t.Fatalf("BET = %+v, queria débito", d)
	}
	if d.Amount.String() != "80.00" {
		t.Errorf("valor = %s", d.Amount)
	}

	d = wager.Decide(w, op(wager.Bet, "100.01"), nil, false)
	if d.Action != wager.ActionReject || d.Code != wager.InsufficientFunds {
		t.Errorf("BET acima do saldo = %+v, queria INSUFFICIENT_FUNDS", d)
	}

	// débito exato cabe
	if d := wager.Decide(w, op(wager.Bet, "100.00"), nil, false); d.Action != wager.ActionMove {
		t.Errorf("BET exata = %+v, deveria caber", d)
	}
}

func TestWin(t *testing.T) {
	w := carteira(t, "10.00")
	d := wager.Decide(w, op(wager.Win, "50.00"), nil, false)
	if d.Action != wager.ActionMove || d.Direction != wallet.Credit {
		t.Fatalf("WIN = %+v, queria crédito", d)
	}
	// WIN com referência continua sendo crédito do próprio valor
	d = wager.Decide(w, op(wager.Win, "50.00"), refProcessada(wager.Bet, "20.00"), true)
	if d.Action != wager.ActionMove || d.Direction != wallet.Credit || d.Amount.String() != "50.00" {
		t.Errorf("WIN com referência = %+v", d)
	}
}

// LOSS: valor obrigatoriamente 0.00, sem lançamento e sem versionar — mas
// processa e emite evento.
func TestLoss(t *testing.T) {
	w := carteira(t, "100.00")

	if d := wager.Decide(w, op(wager.Loss, "0.00"), nil, false); d.Action != wager.ActionNoMove {
		t.Errorf("LOSS = %+v, queria NO_MOVE", d)
	}
	if d := wager.Decide(w, op(wager.Loss, "5.00"), nil, false); d.Action != wager.ActionReject || d.Code != wager.InvalidAmount {
		t.Errorf("LOSS com valor = %+v, queria INVALID_AMOUNT", d)
	}
}

func TestRefund(t *testing.T) {
	w := carteira(t, "20.00")

	d := wager.Decide(w, op(wager.Refund, "80.00"), refProcessada(wager.Bet, "80.00"), true)
	if d.Action != wager.ActionMove || d.Direction != wallet.Credit {
		t.Fatalf("REFUND = %+v, queria crédito", d)
	}

	// só estorna aposta
	d = wager.Decide(w, op(wager.Refund, "80.00"), refProcessada(wager.Win, "80.00"), true)
	if d.Action != wager.ActionReject || d.Code != wager.ReferenceKindInvalid {
		t.Errorf("REFUND de WIN = %+v, queria REFERENCE_KIND_INVALID", d)
	}

	// reversão parcial está fora do escopo
	d = wager.Decide(w, op(wager.Refund, "40.00"), refProcessada(wager.Bet, "80.00"), true)
	if d.Action != wager.ActionReject || d.Code != wager.ReversalAmountMismatch {
		t.Errorf("REFUND parcial = %+v, queria REVERSAL_AMOUNT_MISMATCH", d)
	}
}

// A direção do ROLLBACK vem do tipo revertido, e um tipo fora da tabela é
// recusado em vez de virar "processado sem mover dinheiro".
func TestRollbackDirecaoPorTipoRevertido(t *testing.T) {
	w := carteira(t, "100.00")
	casos := []struct {
		revertido wager.Kind
		direcao   wallet.Direction
	}{
		{wager.Bet, wallet.Credit},
		{wager.Win, wallet.Debit},
		{wager.Refund, wallet.Debit},
	}
	for _, c := range casos {
		d := wager.Decide(w, op(wager.Rollback, "50.00"), refProcessada(c.revertido, "50.00"), true)
		if d.Action != wager.ActionMove || d.Direction != c.direcao {
			t.Errorf("ROLLBACK de %s = %+v, queria %s", c.revertido, d, c.direcao)
		}
	}

	d := wager.Decide(w, op(wager.Rollback, "50.00"), refProcessada(wager.Loss, "50.00"), true)
	if d.Action != wager.ActionReject || d.Code != wager.ReferenceKindInvalid {
		t.Errorf("ROLLBACK de LOSS = %+v, queria REFERENCE_KIND_INVALID", d)
	}
	d = wager.Decide(w, op(wager.Rollback, "50.00"), refProcessada(wager.Rollback, "50.00"), true)
	if d.Action != wager.ActionReject || d.Code != wager.ReferenceKindInvalid {
		t.Errorf("ROLLBACK de ROLLBACK = %+v, queria REFERENCE_KIND_INVALID", d)
	}
}

// O enunciado exige código diferente do de aposta sem saldo.
func TestRollbackSemSaldoTemCodigoProprio(t *testing.T) {
	w := carteira(t, "10.00")
	d := wager.Decide(w, op(wager.Rollback, "50.00"), refProcessada(wager.Win, "50.00"), true)
	if d.Action != wager.ActionReject || d.Code != wager.ReversalInsufficientFunds {
		t.Fatalf("= %+v, queria REVERSAL_INSUFFICIENT_FUNDS", d)
	}
	if d.Code == wager.InsufficientFunds {
		t.Error("o código não pode ser o mesmo da aposta sem saldo")
	}
}

// Uma transação recebe no máximo uma reversão bem-sucedida, de qualquer tipo.
func TestReversaoDuplaERecusada(t *testing.T) {
	w := carteira(t, "100.00")
	ref := refProcessada(wager.Bet, "50.00")
	ref.Reversed = true

	for _, k := range []wager.Kind{wager.Refund, wager.Rollback} {
		d := wager.Decide(w, op(k, "50.00"), ref, true)
		if d.Action != wager.ActionReject || d.Code != wager.AlreadyReversed {
			t.Errorf("%s sobre referência já revertida = %+v, queria ALREADY_REVERSED", k, d)
		}
	}
}

func TestReferenciaAindaNaoChegou(t *testing.T) {
	w := carteira(t, "100.00")
	d := wager.Decide(w, op(wager.Refund, "50.00"), nil, true)
	if d.Action != wager.ActionAwait {
		t.Errorf("= %+v, queria AWAIT", d)
	}
}

// Referência que existe mas ainda espera a própria referência: continua
// esperando. Referência terminada sem sucesso: rejeita na hora.
func TestReferenciaNaoConcluida(t *testing.T) {
	w := carteira(t, "100.00")

	pend := refProcessada(wager.Bet, "50.00")
	pend.Status = wager.PendingReference
	if d := wager.Decide(w, op(wager.Refund, "50.00"), pend, true); d.Action != wager.ActionAwait {
		t.Errorf("referência pendente = %+v, queria AWAIT", d)
	}

	for _, s := range []wager.Status{wager.Rejected, wager.Failed} {
		r := refProcessada(wager.Bet, "50.00")
		r.Status = s
		d := wager.Decide(w, op(wager.Refund, "50.00"), r, true)
		if d.Action != wager.ActionReject || d.Code != wager.ReferenceNotProcessed {
			t.Errorf("referência %s = %+v, queria REFERENCE_NOT_PROCESSED", s, d)
		}
	}
}

func TestReferenciaDeOutroContexto(t *testing.T) {
	w := carteira(t, "100.00")

	outroProvedor := refProcessada(wager.Bet, "50.00")
	outroProvedor.Provider = "provider-b"
	if d := wager.Decide(w, op(wager.Refund, "50.00"), outroProvedor, true); d.Code != wager.ReferenceMismatch {
		t.Errorf("provedor diferente = %+v, queria REFERENCE_MISMATCH", d)
	}

	outraRodada := refProcessada(wager.Bet, "50.00")
	outraRodada.RoundID = "round-2"
	if d := wager.Decide(w, op(wager.Refund, "50.00"), outraRodada, true); d.Code != wager.ReferenceMismatch {
		t.Errorf("rodada diferente = %+v, queria REFERENCE_MISMATCH", d)
	}
}

func TestMoedaDiferenteDaCarteira(t *testing.T) {
	w := carteira(t, "100.00")
	o := wager.Operation{Kind: wager.Bet, Amount: money.MustParse("10.00", "USD"), RoundID: "round-1", Provider: "provider-a"}
	if d := wager.Decide(w, o, nil, false); d.Action != wager.ActionReject || d.Code != wager.CurrencyMismatch {
		t.Errorf("= %+v, queria CURRENCY_MISMATCH", d)
	}
}

// OPENING é interno: precisa ser recusado na borda, venha por HTTP ou SQS.
func TestOpeningNaoEntraPelaBorda(t *testing.T) {
	if _, err := wager.ValidExternalKind("OPENING"); err == nil {
		t.Error("OPENING deveria ser recusado como tipo externo")
	}
	if _, err := wager.ValidExternalKind("BET"); err != nil {
		t.Errorf("BET deveria ser aceito: %v", err)
	}
	if _, err := wager.ValidExternalKind("TRANSFER"); err == nil {
		t.Error("tipo desconhecido deveria ser recusado")
	}
	if _, err := wager.ValidKind("OPENING"); err != nil {
		t.Errorf("OPENING deveria ser um tipo válido internamente: %v", err)
	}
}

// Referência marcada como divergente pelo caso de uso é sempre recusada,
// qualquer que seja o roundId da operação — inclusive quando ele coincide
// com o da referência, que era o buraco da versão que usava string sentinela.
func TestReferenciaDivergenteERecusadaSempre(t *testing.T) {
	w := carteira(t, "100.00")
	for _, rodada := range []string{"round-1", "divergente", ""} {
		ref := refProcessada(wager.Bet, "50.00")
		ref.RoundID = rodada
		ref.Mismatch = true

		o := op(wager.Refund, "50.00")
		o.RoundID = rodada

		d := wager.Decide(w, o, ref, true)
		if d.Action != wager.ActionReject || d.Code != wager.ReferenceMismatch {
			t.Errorf("roundId %q: = %+v, queria REJECT/REFERENCE_MISMATCH", rodada, d)
		}
	}
}

// WIN só referencia aposta.
func TestWinSoReferenciaAposta(t *testing.T) {
	w := carteira(t, "10.00")

	if d := wager.Decide(w, op(wager.Win, "50.00"), refProcessada(wager.Bet, "20.00"), true); d.Action != wager.ActionMove {
		t.Errorf("WIN citando BET = %+v, deveria passar", d)
	}
	for _, k := range []wager.Kind{wager.Win, wager.Refund, wager.Rollback, wager.Loss} {
		d := wager.Decide(w, op(wager.Win, "50.00"), refProcessada(k, "20.00"), true)
		if d.Action != wager.ActionReject || d.Code != wager.ReferenceKindInvalid {
			t.Errorf("WIN citando %s = %+v, queria REFERENCE_KIND_INVALID", k, d)
		}
	}
}
