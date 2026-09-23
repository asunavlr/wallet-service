// Package money implementa o value object Money.
//
// Money é um inteiro com sinal de unidades mínimas (int64) mais um código de
// moeda ISO 4217. Todas as moedas suportadas têm escala 2, então "25.00 BRL"
// vale 2500 unidades mínimas.
//
// NENHUM float aparece aqui — nem no parsing, nem na aritmética, nem na
// formatação. O campo é privado e não existe construtor que aceite float64,
// então não há caminho para um ponto flutuante entrar no domínio.
//
// Limites: o intervalo representável é [MinInt64+1, MaxInt64] unidades
// mínimas, cerca de ±92 quatrilhões de unidades maiores. MinInt64 é proibido
// para que Neg nunca estoure. Toda operação que possa sair do intervalo
// devolve ErrOverflow em vez de dar a volta.
package money

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Scale é o número fixo de casas decimais das moedas suportadas.
// amountPattern e String assumem Scale == 2 e mudam junto com ele.
const Scale = 2

// minorPerMajor é 10^Scale.
const minorPerMajor int64 = 100

var (
	// ErrInvalidAmount indica um decimal malformado.
	ErrInvalidAmount = errors.New("money: valor inválido")
	// ErrNegativeAmount indica valor negativo numa entrada externa.
	ErrNegativeAmount = errors.New("money: valor negativo")
	// ErrInvalidCurrency indica código de moeda desconhecido ou malformado.
	ErrInvalidCurrency = errors.New("money: moeda inválida")
	// ErrCurrencyMismatch indica aritmética entre moedas diferentes.
	ErrCurrencyMismatch = errors.New("money: moedas incompatíveis")
	// ErrOverflow indica estouro de int64.
	ErrOverflow = errors.New("money: estouro")
	// ErrUninitialized indica uso do valor zero de Money.
	ErrUninitialized = errors.New("money: valor não inicializado")
)

// amountPattern aceita apenas decimais não negativos com exatamente duas casas
// e sem zeros à esquerda supérfluos: "0.50", "25.00", "1000.00".
//
// Exatamente duas casas é uma escolha deliberada. O enunciado manda rejeitar
// escala excedente e pede que qualquer normalização seja documentada antes do
// hash de idempotência. Aceitar só a forma canônica elimina a normalização:
// o hash usa o texto recebido, e "25.0" não é uma segunda grafia de "25.00",
// é uma entrada inválida que o cliente corrige e reenvia.
var amountPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.([0-9]{2})$`)

// Currency é um código alfabético ISO 4217.
type Currency string

// supported lista os códigos ISO 4217 cuja unidade mínima é 10^-2.
// Moedas de escala diferente (JPY, KWD) ficam de fora porque Scale é fixo.
var supported = map[Currency]struct{}{
	"BRL": {}, "USD": {}, "EUR": {}, "GBP": {}, "ARS": {}, "MXN": {}, "CAD": {},
}

// ParseCurrency valida um código ISO 4217.
func ParseCurrency(code string) (Currency, error) {
	c := Currency(code)
	if _, ok := supported[c]; !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
	}
	return c, nil
}

// Money é um valor imutável de uma moeda. O valor zero é inválido.
type Money struct {
	minor    int64
	currency Currency
}

// Parse constrói Money a partir de um decimal externo como "25.00".
//
// Rejeita vazio, NaN, Infinity, notação científica, sinal, escala diferente de
// dois, zeros à esquerda e valores fora do intervalo. Nada é arredondado: uma
// entrada inválida vira erro, nunca um valor aproximado.
func Parse(amount, currency string) (Money, error) {
	c, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	// O sinal é checado antes do regex para separar "negativo" (que é uma
	// entrada compreensível mas proibida) de "malformado".
	if strings.HasPrefix(amount, "-") {
		return Money{}, fmt.Errorf("%w: %q", ErrNegativeAmount, amount)
	}
	m := amountPattern.FindStringSubmatch(amount)
	if m == nil {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
	}
	minor, err := combine(m[1], m[2])
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", err, amount)
	}
	return Money{minor: minor, currency: c}, nil
}

// MustParse é Parse que entra em pânico no erro. Só para literais de teste e
// constantes de inicialização — nunca em caminho que processe entrada externa.
func MustParse(amount, currency string) Money {
	m, err := Parse(amount, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// combine junta parte inteira e fracionária em unidades mínimas.
//
// O limite é exato: o maior valor aceito é MaxInt64. Verificar antes da
// multiplicação é o que evita o estouro em vez de detectá-lo tarde demais.
func combine(integer, fraction string) (int64, error) {
	major, err := strconv.ParseInt(integer, 10, 64)
	if err != nil {
		return 0, ErrOverflow
	}
	frac, _ := strconv.ParseInt(fraction, 10, 64) // o regex garante dois dígitos
	maxMajor, maxFrac := math.MaxInt64/minorPerMajor, math.MaxInt64%minorPerMajor
	if major > maxMajor || (major == maxMajor && frac > maxFrac) {
		return 0, ErrOverflow
	}
	return major*minorPerMajor + frac, nil
}

// FromMinor constrói Money a partir de unidades mínimas.
//
// Valores negativos são permitidos: diferenças internas, como a da
// reconciliação, precisam deles. MinInt64 é rejeitado para que Neg seja
// sempre seguro.
func FromMinor(minor int64, currency Currency) (Money, error) {
	if _, err := ParseCurrency(string(currency)); err != nil {
		return Money{}, err
	}
	if minor == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero devolve o valor nulo da moeda.
func Zero(currency Currency) (Money, error) {
	return FromMinor(0, currency)
}

// Minor devolve as unidades mínimas. É o que vai para a coluna BIGINT.
func (m Money) Minor() int64 { return m.minor }

// Currency devolve o código ISO 4217.
func (m Money) Currency() Currency { return m.currency }

// IsZero informa se o valor é nulo.
func (m Money) IsZero() bool { return m.minor == 0 }

// IsPositive informa se o valor é maior que zero.
func (m Money) IsPositive() bool { return m.minor > 0 }

// IsNegative informa se o valor é menor que zero.
func (m Money) IsNegative() bool { return m.minor < 0 }

// Valid informa se o Money foi construído por um dos construtores.
// O valor zero do struct tem moeda vazia e é sempre inválido.
func (m Money) Valid() bool {
	_, ok := supported[m.currency]
	return ok
}

// check devolve ErrUninitialized para o valor zero do struct.
func (m Money) check() error {
	if !m.Valid() {
		return ErrUninitialized
	}
	return nil
}

// sameCurrency valida que os dois lados existem e compartilham a moeda.
func sameCurrency(a, b Money) error {
	if err := a.check(); err != nil {
		return err
	}
	if err := b.check(); err != nil {
		return err
	}
	if a.currency != b.currency {
		return fmt.Errorf("%w: %s e %s", ErrCurrencyMismatch, a.currency, b.currency)
	}
	return nil
}

// Add soma dois valores da mesma moeda.
func (m Money) Add(other Money) (Money, error) {
	if err := sameCurrency(m, other); err != nil {
		return Money{}, err
	}
	if (other.minor > 0 && m.minor > math.MaxInt64-other.minor) ||
		(other.minor < 0 && m.minor < math.MinInt64+1-other.minor) {
		return Money{}, ErrOverflow
	}
	return Money{minor: m.minor + other.minor, currency: m.currency}, nil
}

// Sub subtrai dois valores da mesma moeda.
func (m Money) Sub(other Money) (Money, error) {
	if err := sameCurrency(m, other); err != nil {
		return Money{}, err
	}
	if (other.minor < 0 && m.minor > math.MaxInt64+other.minor) ||
		(other.minor > 0 && m.minor < math.MinInt64+1+other.minor) {
		return Money{}, ErrOverflow
	}
	return Money{minor: m.minor - other.minor, currency: m.currency}, nil
}

// Neg devolve o simétrico. Nunca estoura porque MinInt64 é proibido na
// construção.
func (m Money) Neg() (Money, error) {
	if err := m.check(); err != nil {
		return Money{}, err
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Cmp compara dois valores da mesma moeda: -1, 0 ou 1.
func (m Money) Cmp(other Money) (int, error) {
	if err := sameCurrency(m, other); err != nil {
		return 0, err
	}
	switch {
	case m.minor < other.minor:
		return -1, nil
	case m.minor > other.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal informa se valor e moeda coincidem. Não devolve erro: moedas
// diferentes simplesmente não são iguais.
func (m Money) Equal(other Money) bool {
	return m.minor == other.minor && m.currency == other.currency
}

// String devolve o decimal com escala fixa, como vai para o JSON: "25.00".
//
// A formatação é inteira. Para negativos, o sinal é extraído antes da divisão
// porque o quociente inteiro de Go trunca em direção ao zero, e -1 / 100 == 0
// perderia o sinal de "-0.01".
func (m Money) String() string {
	if !m.Valid() {
		return ""
	}
	minor := m.minor
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	return fmt.Sprintf("%s%d.%0*d", sign, minor/minorPerMajor, Scale, minor%minorPerMajor)
}

// GoString ajuda a leitura de falhas de teste.
func (m Money) GoString() string {
	return fmt.Sprintf("money.Money(%s %s)", m.String(), m.currency)
}
