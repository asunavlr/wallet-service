package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

var (
	agora = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	brl   = func(s string) money.Money { return money.MustParse(s, "BRL") }
)

func nova(t *testing.T, saldo string) *wallet.Wallet {
	t.Helper()
	w, err := wallet.Open(uuid.New(), uuid.New(), brl(saldo), agora)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return w
}

// O enunciado é explícito: a versão da carteira na abertura é 1, mesmo com
// saldo inicial positivo.
func TestAberturaComeçaNaVersao1(t *testing.T) {
	for _, saldo := range []string{"0.00", "1000.00"} {
		w := nova(t, saldo)
		if w.Version() != 1 {
			t.Errorf("saldo inicial %s: versão = %d, queria 1", saldo, w.Version())
		}
		if w.Balance().String() != saldo {
			t.Errorf("saldo = %s, queria %s", w.Balance(), saldo)
		}
	}
}

func TestAberturaRejeitaEntradaInvalida(t *testing.T) {
	var vazio money.Money
	casos := []struct {
		nome       string
		id, player uuid.UUID
		saldo      money.Money
		instante   time.Time
	}{
		{"id nulo", uuid.Nil, uuid.New(), brl("0.00"), agora},
		{"jogador nulo", uuid.New(), uuid.Nil, brl("0.00"), agora},
		{"money não inicializado", uuid.New(), uuid.New(), vazio, agora},
		{"instante zero", uuid.New(), uuid.New(), brl("0.00"), time.Time{}},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if _, err := wallet.Open(c.id, c.player, c.saldo, c.instante); err == nil {
				t.Error("deveria recusar")
			}
		})
	}
}

func TestDebitoECreditoMovemSaldoEVersao(t *testing.T) {
	w := nova(t, "100.00")

	e, err := w.Apply(uuid.New(), uuid.New(), wallet.Debit, brl("80.00"), agora)
	if err != nil {
		t.Fatalf("débito: %v", err)
	}
	if w.Balance().String() != "20.00" {
		t.Errorf("saldo = %s, queria 20.00", w.Balance())
	}
	if w.Version() != 2 {
		t.Errorf("versão = %d, queria 2", w.Version())
	}
	if e.BalanceBefore().String() != "100.00" || e.BalanceAfter().String() != "20.00" {
		t.Errorf("lançamento %s → %s", e.BalanceBefore(), e.BalanceAfter())
	}

	if _, err := w.Apply(uuid.New(), uuid.New(), wallet.Credit, brl("5.00"), agora); err != nil {
		t.Fatalf("crédito: %v", err)
	}
	if w.Balance().String() != "25.00" || w.Version() != 3 {
		t.Errorf("após crédito: saldo %s versão %d", w.Balance(), w.Version())
	}
}

// O caso central do enunciado: 80.00 passa, o segundo 80.00 não cabe, e a
// carteira fica intacta depois da recusa.
func TestDebitoNaoPodeDeixarSaldoNegativo(t *testing.T) {
	w := nova(t, "100.00")
	if _, err := w.Apply(uuid.New(), uuid.New(), wallet.Debit, brl("80.00"), agora); err != nil {
		t.Fatal(err)
	}
	_, err := w.Apply(uuid.New(), uuid.New(), wallet.Debit, brl("80.00"), agora)
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("segundo débito = %v, queria ErrInsufficientFunds", err)
	}
	if w.Balance().String() != "20.00" {
		t.Errorf("saldo após recusa = %s, queria 20.00 intacto", w.Balance())
	}
	if w.Version() != 2 {
		t.Errorf("versão após recusa = %d, queria 2 (recusa não versiona)", w.Version())
	}
}

func TestDebitoExatoAteZero(t *testing.T) {
	w := nova(t, "100.00")
	if _, err := w.Apply(uuid.New(), uuid.New(), wallet.Debit, brl("100.00"), agora); err != nil {
		t.Fatalf("débito exato: %v", err)
	}
	if !w.Balance().IsZero() {
		t.Errorf("saldo = %s, queria 0.00", w.Balance())
	}
}

func TestMovimentacaoExigeMoedaDaCarteira(t *testing.T) {
	w := nova(t, "100.00")
	_, err := w.Apply(uuid.New(), uuid.New(), wallet.Credit, money.MustParse("10.00", "USD"), agora)
	if !errors.Is(err, wallet.ErrCurrencyMismatch) {
		t.Errorf("= %v, queria ErrCurrencyMismatch", err)
	}
}

func TestMovimentacaoRejeitaValorNaoPositivo(t *testing.T) {
	w := nova(t, "100.00")
	for _, v := range []string{"0.00"} {
		if _, err := w.Apply(uuid.New(), uuid.New(), wallet.Credit, brl(v), agora); !errors.Is(err, wallet.ErrInvalidMovement) {
			t.Errorf("valor %s = %v, queria ErrInvalidMovement", v, err)
		}
	}
	if _, err := w.Apply(uuid.New(), uuid.New(), "TRANSFER", brl("1.00"), agora); !errors.Is(err, wallet.ErrInvalidMovement) {
		t.Error("direção desconhecida deveria ser recusada")
	}
}

func TestCanDebit(t *testing.T) {
	w := nova(t, "100.00")
	if !w.CanDebit(brl("100.00")) {
		t.Error("débito exato deveria caber")
	}
	if w.CanDebit(brl("100.01")) {
		t.Error("débito acima do saldo não deveria caber")
	}
	if w.CanDebit(money.MustParse("1.00", "USD")) {
		t.Error("moeda diferente não deveria caber")
	}
}

// Reidratar é leitura de estado consolidado: não reaplica movimentação nem
// produz lançamento.
func TestRehydrateNaoReaplicaNada(t *testing.T) {
	id, player := uuid.New(), uuid.New()
	w, err := wallet.Rehydrate(id, player, brl("42.00"), 7, agora, agora)
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().String() != "42.00" || w.Version() != 7 {
		t.Errorf("reidratada com saldo %s versão %d", w.Balance(), w.Version())
	}
}

func TestRehydrateRejeitaLinhaCorrompida(t *testing.T) {
	neg, _ := money.FromMinor(-1, "BRL")
	casos := []struct {
		nome   string
		saldo  money.Money
		versao int64
	}{
		{"saldo negativo persistido", neg, 1},
		{"versão zero", brl("1.00"), 0},
		{"versão negativa", brl("1.00"), -3},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if _, err := wallet.Rehydrate(uuid.New(), uuid.New(), c.saldo, c.versao, agora, agora); !errors.Is(err, wallet.ErrInvalidWallet) {
				t.Error("deveria recusar")
			}
		})
	}
}

func TestLedgerEntryValidaAritmetica(t *testing.T) {
	id, w, tx := uuid.New(), uuid.New(), uuid.New()

	// crédito coerente
	if _, err := wallet.NewLedgerEntry(id, w, tx, wallet.Credit,
		brl("10.00"), brl("5.00"), brl("15.00"), agora); err != nil {
		t.Errorf("crédito coerente recusado: %v", err)
	}
	// crédito incoerente
	if _, err := wallet.NewLedgerEntry(id, w, tx, wallet.Credit,
		brl("10.00"), brl("5.00"), brl("14.00"), agora); !errors.Is(err, wallet.ErrInvalidLedgerEntry) {
		t.Error("crédito incoerente deveria ser recusado")
	}
	// débito coerente
	if _, err := wallet.NewLedgerEntry(id, w, tx, wallet.Debit,
		brl("10.00"), brl("15.00"), brl("5.00"), agora); err != nil {
		t.Errorf("débito coerente recusado: %v", err)
	}
	// débito com a conta de crédito
	if _, err := wallet.NewLedgerEntry(id, w, tx, wallet.Debit,
		brl("10.00"), brl("5.00"), brl("15.00"), agora); !errors.Is(err, wallet.ErrInvalidLedgerEntry) {
		t.Error("débito incoerente deveria ser recusado")
	}
}
