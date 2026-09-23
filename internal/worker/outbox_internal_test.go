package worker

import (
	"testing"
	"time"
)

// O backoff precisa crescer, respeitar o teto e nunca dar a volta: um
// deslocamento grande demais em int64 produz duração negativa, e uma próxima
// tentativa no passado faz o worker girar em falso queimando o banco.
func TestBackoff(t *testing.T) {
	base, max := time.Second, time.Minute

	casos := []struct {
		tentativas int
		quer       time.Duration
	}{
		{0, time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{5, 32 * time.Second},
		{6, time.Minute},  // 64s passa do teto
		{10, time.Minute}, // muito além, continua no teto
	}
	for _, c := range casos {
		if got := backoff(base, max, c.tentativas); got != c.quer {
			t.Errorf("backoff(%d) = %v, queria %v", c.tentativas, got, c.quer)
		}
	}

	// negativo é tratado como zero em vez de virar duração estranha
	if got := backoff(base, max, -5); got != base {
		t.Errorf("backoff(-5) = %v, queria %v", got, base)
	}

	// deslocamento absurdo não pode dar a volta
	for _, n := range []int{31, 62, 64, 1000} {
		got := backoff(base, max, n)
		if got <= 0 {
			t.Errorf("backoff(%d) = %v: duração não positiva", n, got)
		}
		if got > max {
			t.Errorf("backoff(%d) = %v, acima do teto %v", n, got, max)
		}
	}
}

// Com base muito grande, a primeira tentativa já estoura o teto — e o teto
// precisa ganhar.
func TestBackoffComBaseMaiorQueOTeto(t *testing.T) {
	if got := backoff(time.Hour, time.Minute, 0); got != time.Minute {
		t.Errorf("= %v, queria o teto de 1 minuto", got)
	}
}
