package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

func TestParseAceitaFormaCanonica(t *testing.T) {
	casos := []struct {
		entrada string
		minor   int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.50", 50},
		{"25.00", 2500},
		{"100.00", 10000},
		{"1000.00", 100000},
		{"92233720368547758.07", math.MaxInt64}, // o maior valor representável
	}
	for _, c := range casos {
		m, err := money.Parse(c.entrada, "BRL")
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.entrada, err)
		}
		if m.Minor() != c.minor {
			t.Errorf("Parse(%q).Minor() = %d, queria %d", c.entrada, m.Minor(), c.minor)
		}
		if got := m.String(); got != c.entrada {
			t.Errorf("String() = %q, queria %q (ida e volta)", got, c.entrada)
		}
	}
}

// As entradas que o enunciado manda rejeitar, uma por linha.
func TestParseRejeita(t *testing.T) {
	casos := []struct {
		nome    string
		entrada string
		erro    error
	}{
		{"vazio", "", money.ErrInvalidAmount},
		{"só espaço", " ", money.ErrInvalidAmount},
		{"NaN", "NaN", money.ErrInvalidAmount},
		{"Infinity", "Infinity", money.ErrInvalidAmount},
		{"-Infinity", "-Infinity", money.ErrNegativeAmount},
		{"notação científica", "2.5e2", money.ErrInvalidAmount},
		{"notação científica maiúscula", "2.5E2", money.ErrInvalidAmount},
		{"escala excedente", "25.001", money.ErrInvalidAmount},
		{"escala insuficiente", "25.0", money.ErrInvalidAmount},
		{"sem casas decimais", "25", money.ErrInvalidAmount},
		{"zero à esquerda", "025.00", money.ErrInvalidAmount},
		{"negativo", "-25.00", money.ErrNegativeAmount},
		{"negativo zero", "-0.00", money.ErrNegativeAmount},
		{"sinal de mais", "+25.00", money.ErrInvalidAmount},
		{"separador de milhar", "1,000.00", money.ErrInvalidAmount},
		{"vírgula decimal", "25,00", money.ErrInvalidAmount},
		{"espaço no meio", "25 .00", money.ErrInvalidAmount},
		{"hexadecimal", "0x19", money.ErrInvalidAmount},
		{"só ponto", ".", money.ErrInvalidAmount},
		{"sem parte inteira", ".50", money.ErrInvalidAmount},
		{"sem parte fracionária", "25.", money.ErrInvalidAmount},
		{"dois pontos", "25.00.00", money.ErrInvalidAmount},
		{"acima do máximo", "92233720368547758.08", money.ErrOverflow},
		{"muito acima do máximo", "99999999999999999999.00", money.ErrOverflow},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if _, err := money.Parse(c.entrada, "BRL"); !errors.Is(err, c.erro) {
				t.Errorf("Parse(%q) = %v, queria %v", c.entrada, err, c.erro)
			}
		})
	}
}

func TestParseRejeitaMoedaInvalida(t *testing.T) {
	for _, code := range []string{"", "BR", "BRLL", "brl", "XXX", "123"} {
		if _, err := money.Parse("25.00", code); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("Parse com moeda %q = %v, queria ErrInvalidCurrency", code, err)
		}
	}
}

func TestValorZeroDoStructEhInvalido(t *testing.T) {
	var m money.Money
	if m.Valid() {
		t.Fatal("o valor zero de Money não pode ser válido")
	}
	if _, err := m.Add(money.MustParse("1.00", "BRL")); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Add sobre valor não inicializado = %v, queria ErrUninitialized", err)
	}
	if _, err := m.Neg(); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Neg sobre valor não inicializado = %v, queria ErrUninitialized", err)
	}
	if got := m.String(); got != "" {
		t.Errorf("String() do valor zero = %q, queria vazio", got)
	}
}

func TestAritmeticaExigeMesmaMoeda(t *testing.T) {
	brl := money.MustParse("25.00", "BRL")
	usd := money.MustParse("25.00", "USD")

	if _, err := brl.Add(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Add entre moedas = %v, queria ErrCurrencyMismatch", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Sub entre moedas = %v, queria ErrCurrencyMismatch", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Cmp entre moedas = %v, queria ErrCurrencyMismatch", err)
	}
	if brl.Equal(usd) {
		t.Error("valores de moedas diferentes não podem ser iguais")
	}
}

func TestAritmetica(t *testing.T) {
	a := money.MustParse("100.00", "BRL")
	b := money.MustParse("80.00", "BRL")

	soma, err := a.Add(b)
	if err != nil || soma.String() != "180.00" {
		t.Errorf("Add = %v, %v; queria 180.00", soma, err)
	}
	dif, err := a.Sub(b)
	if err != nil || dif.String() != "20.00" {
		t.Errorf("Sub = %v, %v; queria 20.00", dif, err)
	}
	// diferença negativa: permitida em cálculo interno, como a reconciliação
	neg, err := b.Sub(a)
	if err != nil || neg.String() != "-20.00" {
		t.Errorf("Sub invertido = %v, %v; queria -20.00", neg, err)
	}
	if !neg.IsNegative() {
		t.Error("IsNegative() deveria ser verdadeiro")
	}
	sim, err := neg.Neg()
	if err != nil || sim.String() != "20.00" {
		t.Errorf("Neg = %v, %v; queria 20.00", sim, err)
	}
}

// O sinal precisa sobreviver à formatação de valores menores que uma unidade
// maior: o quociente inteiro trunca em direção ao zero e comeria o "-".
func TestFormatacaoDeNegativoPequeno(t *testing.T) {
	m, err := money.FromMinor(-1, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.String(); got != "-0.01" {
		t.Errorf("String() = %q, queria -0.01", got)
	}
}

func TestOverflow(t *testing.T) {
	max, err := money.FromMinor(math.MaxInt64, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	um := money.MustParse("0.01", "BRL")

	if _, err := max.Add(um); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("Add no topo = %v, queria ErrOverflow", err)
	}

	min, err := money.FromMinor(math.MinInt64+1, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := min.Sub(um); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("Sub no fundo = %v, queria ErrOverflow", err)
	}

	// MinInt64 é proibido na construção justamente para que Neg nunca estoure.
	if _, err := money.FromMinor(math.MinInt64, "BRL"); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("FromMinor(MinInt64) = %v, queria ErrOverflow", err)
	}
	if _, err := min.Neg(); err != nil {
		t.Errorf("Neg do menor valor construível: %v", err)
	}
}

func TestCmp(t *testing.T) {
	a := money.MustParse("10.00", "BRL")
	b := money.MustParse("20.00", "BRL")
	if c, _ := a.Cmp(b); c != -1 {
		t.Errorf("Cmp(10,20) = %d, queria -1", c)
	}
	if c, _ := b.Cmp(a); c != 1 {
		t.Errorf("Cmp(20,10) = %d, queria 1", c)
	}
	if c, _ := a.Cmp(a); c != 0 {
		t.Errorf("Cmp(10,10) = %d, queria 0", c)
	}
}

func TestZero(t *testing.T) {
	z, err := money.Zero("BRL")
	if err != nil {
		t.Fatal(err)
	}
	if !z.IsZero() || z.String() != "0.00" {
		t.Errorf("Zero(BRL) = %q", z.String())
	}
	if _, err := money.Zero("XXX"); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Error("Zero com moeda inválida deveria falhar")
	}
}
