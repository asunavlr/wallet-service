package money_test

import (
	"math"
	"math/big"
	"testing"

	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

// Fuzz da aritmética: soma, subtração e negação nunca podem dar a volta em
// silêncio. Ou devolvem o resultado exato, ou ErrOverflow.
func FuzzAritmeticaNuncaDaAVolta(f *testing.F) {
	f.Add(int64(0), int64(0))
	f.Add(int64(math.MaxInt64), int64(1))
	f.Add(int64(math.MinInt64+1), int64(-1))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64))
	f.Add(int64(math.MinInt64+1), int64(math.MinInt64+1))
	f.Add(int64(-1), int64(math.MinInt64+1))

	f.Fuzz(func(t *testing.T, a, b int64) {
		if a == math.MinInt64 || b == math.MinInt64 {
			t.Skip("proibido na construção")
		}
		ma, err := money.FromMinor(a, "BRL")
		if err != nil {
			t.Skip()
		}
		mb, err := money.FromMinor(b, "BRL")
		if err != nil {
			t.Skip()
		}

		// soma
		if soma, err := ma.Add(mb); err == nil {
			if estouraSoma(a, b) {
				t.Fatalf("Add(%d,%d) devolveu %d sem erro, mas estoura int64", a, b, soma.Minor())
			}
			if soma.Minor() != a+b {
				t.Fatalf("Add(%d,%d) = %d", a, b, soma.Minor())
			}
		}

		// subtração
		if dif, err := ma.Sub(mb); err == nil {
			if estouraSub(a, b) {
				t.Fatalf("Sub(%d,%d) devolveu %d sem erro, mas estoura int64", a, b, dif.Minor())
			}
			if dif.Minor() != a-b {
				t.Fatalf("Sub(%d,%d) = %d", a, b, dif.Minor())
			}
		}

		// negação nunca falha para valor construível, e é involutiva
		neg, err := ma.Neg()
		if err != nil {
			t.Fatalf("Neg(%d) falhou", a)
		}
		volta, err := neg.Neg()
		if err != nil || volta.Minor() != a {
			t.Fatalf("Neg(Neg(%d)) = %d, %v", a, volta.Minor(), err)
		}

		// ida e volta pela string preserva o valor exato
		if s := ma.String(); s != "" {
			// só para não negativos, que é o que Parse aceita
			if a >= 0 {
				p, err := money.Parse(s, "BRL")
				if err != nil || p.Minor() != a {
					t.Fatalf("Parse(%q) = %d, %v; queria %d", s, p.Minor(), err, a)
				}
			}
		}
	})
}

// estouraSoma e estouraSub decidem o estouro com math/big, independente da
// lógica que está sendo testada — senão o teste repetiria o eventual erro da
// implementação e concordaria com ele.
func estouraSoma(a, b int64) bool {
	r := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	return !r.IsInt64() || r.Int64() == math.MinInt64
}

func estouraSub(a, b int64) bool {
	r := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	return !r.IsInt64() || r.Int64() == math.MinInt64
}
