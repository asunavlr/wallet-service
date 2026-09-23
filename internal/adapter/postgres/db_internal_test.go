package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kevinmatos/wallet-service/internal/app"
)

// classificar decide três coisas de uma vez: se a transação é refeita, se a
// mensagem volta para a fila ou vai para a DLQ, e qual HTTP o provedor
// recebe. Errar aqui manda aposta sem saldo para a DLQ, ou insiste para
// sempre num payload malformado.
func TestClassificarSQLSTATE(t *testing.T) {
	casos := []struct {
		nome   string
		codigo string
		quer   error
	}{
		// Transitórios: refazer enxerga o vencedor da corrida.
		{"unicidade", "23505", app.ErrConflict},
		{"exclusão", "23P01", app.ErrConflict},
		{"falha de serialização", "40001", app.ErrConflict},
		{"deadlock", "40P01", app.ErrConflict},

		// Transitórios: a dependência piscou.
		{"lock_timeout", "55P03", app.ErrUnavailable},
		{"statement_timeout", "57014", app.ErrUnavailable},
		{"conexão recusada (classe 08)", "08006", app.ErrUnavailable},
		{"sem conexão (classe 08)", "08003", app.ErrUnavailable},
		{"sem recurso (classe 53)", "53300", app.ErrUnavailable},
		{"intervenção do operador (classe 57)", "57P01", app.ErrUnavailable},
		{"erro de sistema (classe 58)", "58030", app.ErrUnavailable},

		// Permanentes: insistir dá o mesmo resultado.
		{"violação de CHECK", "23514", nil},
		{"violação de FK", "23503", nil},
		{"NOT NULL", "23502", nil},
		{"erro de sintaxe", "42601", nil},
		{"permissão negada", "42501", nil},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			err := classificar(&pgconn.PgError{Code: c.codigo, Message: c.nome})
			if err == nil {
				t.Fatal("classificar não deveria engolir o erro")
			}
			switch c.quer {
			case app.ErrConflict, app.ErrUnavailable:
				if !errors.Is(err, c.quer) {
					t.Errorf("SQLSTATE %s = %v, queria %v", c.codigo, err, c.quer)
				}
				if !app.Transient(err) {
					t.Errorf("SQLSTATE %s deveria ser transitório", c.codigo)
				}
			default:
				if app.Transient(err) {
					t.Errorf("SQLSTATE %s foi classificado como transitório; é permanente", c.codigo)
				}
			}
			// O SQLSTATE precisa sobreviver na mensagem: sem ele, depurar um
			// erro permanente vira adivinhação.
			if c.quer == nil && !contem(err.Error(), c.codigo) {
				t.Errorf("a mensagem deveria citar o SQLSTATE: %v", err)
			}
		})
	}
}

func TestClassificarCasosEspeciais(t *testing.T) {
	if err := classificar(nil); err != nil {
		t.Errorf("classificar(nil) = %v, queria nil", err)
	}

	if err := classificar(pgx.ErrNoRows); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("ErrNoRows = %v, queria ErrNotFound", err)
	}
	// Ausência de linha NÃO é transitória: repetir encontra a mesma ausência.
	if app.Transient(classificar(pgx.ErrNoRows)) {
		t.Error("ErrNotFound não pode ser transitório")
	}

	// Cancelamento é repassado sem reclassificar: quem cancelou sabe por quê,
	// e transformá-lo em "indisponível" faria o chamador tentar de novo algo
	// que ele mesmo abortou.
	if err := classificar(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("Canceled = %v, queria ser repassado", err)
	}
	if err := classificar(context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("DeadlineExceeded = %v, queria ser repassado", err)
	}

	// Erro de rede é transitório.
	rede := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if !errors.Is(classificar(rede), app.ErrUnavailable) {
		t.Error("erro de rede deveria ser ErrUnavailable")
	}

	// Erro embrulhado continua sendo reconhecido.
	embrulhado := fmt.Errorf("ao gravar a carteira: %w", &pgconn.PgError{Code: "23505"})
	if !errors.Is(classificar(embrulhado), app.ErrConflict) {
		t.Error("erro embrulhado deveria ser desembrulhado")
	}
}

// O cursor é opaco para o cliente, mas precisa voltar exatamente como saiu.
func TestCursorIdaEVolta(t *testing.T) {
	for _, seq := range []int64{0, 1, 42, 999999, 1 << 40} {
		c := codificarCursor(seq)
		if contem(c, "=") {
			t.Errorf("cursor %q tem padding: quebra em URL", c)
		}
		volta, err := decodificarCursor(c)
		if err != nil {
			t.Fatalf("decodificar(%q): %v", c, err)
		}
		if volta != seq {
			t.Errorf("ida e volta de %d = %d", seq, volta)
		}
	}
}

func TestCursorInvalidoEhRecusado(t *testing.T) {
	for _, c := range []string{"!!!", "não-base64", "YWJj", "%%%"} {
		if _, err := decodificarCursor(c); err == nil {
			t.Errorf("cursor %q deveria ser recusado", c)
		}
	}
}

// nulo converte string vazia em NULL, para que os CHECKs de formato do schema
// distingam ausência de string vazia.
func TestNuloETexto(t *testing.T) {
	if nulo("") != nil {
		t.Error(`nulo("") deveria ser NULL`)
	}
	if p := nulo("provider-a"); p == nil || *p != "provider-a" {
		t.Error("nulo deveria preservar valor não vazio")
	}
	if texto(nil) != "" {
		t.Error("texto(nil) deveria ser vazio")
	}
	v := "x"
	if texto(&v) != "x" {
		t.Error("texto deveria devolver o valor apontado")
	}
}

func contem(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
