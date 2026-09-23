package app_test

import (
	"testing"
	"time"

	"github.com/kevinmatos/wallet-service/internal/app"
)

// O backoff das pendências: cresce, respeita o teto e nunca produz uma
// próxima tentativa no passado — que faria o worker girar em falso queimando
// o banco.
func TestBackoffDaPendencia(t *testing.T) {
	p := app.PendingPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 6}

	casos := map[int]time.Duration{
		0: time.Second, 1: 2 * time.Second, 2: 4 * time.Second,
		5: 32 * time.Second, 6: time.Minute, 20: time.Minute,
	}
	for tentativas, quer := range casos {
		if got := p.Backoff(tentativas); got != quer {
			t.Errorf("Backoff(%d) = %v, queria %v", tentativas, got, quer)
		}
	}
	if got := p.Backoff(-3); got != time.Second {
		t.Errorf("Backoff(-3) = %v, queria a base", got)
	}
	for _, n := range []int{31, 63, 64, 5000} {
		got := p.Backoff(n)
		if got <= 0 || got > p.MaxDelay {
			t.Errorf("Backoff(%d) = %v: fora do intervalo (0, %v]", n, got, p.MaxDelay)
		}
	}
}

// Transient decide se a operação é refeita e se a mensagem volta para a fila.
func TestTransient(t *testing.T) {
	transitorios := []error{app.ErrConflict, app.ErrUnavailable}
	permanentes := []error{
		app.ErrNotFound, app.ErrIdempotencyConflict,
		app.ErrDuplicateExternalTransaction, app.ErrForbidden, app.ErrInvalidInput,
	}
	for _, err := range transitorios {
		if !app.Transient(err) {
			t.Errorf("%v deveria ser transitório", err)
		}
	}
	for _, err := range permanentes {
		if app.Transient(err) {
			t.Errorf("%v NÃO deveria ser transitório: repetir dá o mesmo resultado", err)
		}
	}
	if app.Transient(nil) {
		t.Error("nil não é transitório")
	}
}
