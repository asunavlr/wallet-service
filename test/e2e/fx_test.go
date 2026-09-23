//go:build e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/kevinmatos/wallet-service/internal/bootstrap"
	"github.com/kevinmatos/wallet-service/internal/config"
)

// configDeTeste aponta para o ambiente do compose, visto do host.
func configDeTeste(t *testing.T) config.Config {
	t.Helper()
	// Uma porta HTTP diferente das três instâncias, para não colidir.
	t.Setenv("HTTP_ADDR", "127.0.0.1:18099")
	t.Setenv("DATABASE_URL", "postgres://wallet:dev@localhost:5432/wallet?sslmode=disable")
	t.Setenv("SQS_ENDPOINT", endpointSQS())
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("OIDC_ISSUER_URL", keycloak()+"/realms/wallet")
	t.Setenv("OIDC_DISCOVERY_URL", keycloak()+"/realms/wallet")
	t.Setenv("LOG_LEVEL", "error")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("configuração de teste inválida: %v", err)
	}
	return cfg
}

// O enunciado pede explicitamente "uma verificação da composição Fx e de seu
// início e encerramento, incluindo liberação de recursos dos workers".
//
// Este teste monta a composição REAL — a mesma função que produção usa, não
// uma montagem paralela — e a inicia e encerra de verdade. Verificar uma
// composição diferente da real não verificaria nada.
func TestComposicaoFxIniciaEEncerra(t *testing.T) {
	cfg := configDeTeste(t)

	app := fx.New(
		fx.Supply(cfg),
		bootstrap.Modulo(),
		fx.NopLogger,
	)

	// A montagem em si: se faltar um provedor ou houver dependência cíclica,
	// o erro aparece aqui, antes de qualquer conexão.
	if err := app.Err(); err != nil {
		t.Fatalf("composição inválida: %v", err)
	}

	ctxStart, cancelStart := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStart()
	if err := app.Start(ctxStart); err != nil {
		t.Fatalf("iniciando a aplicação: %v", err)
	}

	// Com tudo de pé, o servidor responde.
	esperarSaudavel(t, "http://127.0.0.1:18099", 30*time.Second)

	ctxStop, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStop()
	if err := app.Stop(ctxStop); err != nil {
		t.Fatalf("encerrando a aplicação: %v", err)
	}

	// Depois do Stop, o servidor não atende mais: novas entradas foram
	// interrompidas, que é o comportamento de shutdown que o enunciado pede.
	if _, err := cliente.Get("http://127.0.0.1:18099/health/live"); err == nil {
		t.Error("o servidor deveria ter parado de atender após o Stop")
	}
}

// O encerramento precisa LIBERAR os recursos: um segundo ciclo completo só
// funciona se o primeiro tiver devolvido a porta, fechado o pool e terminado
// os workers. Se algo vazasse, o segundo Start falharia.
func TestCicloCompletoPodeSerRepetido(t *testing.T) {
	cfg := configDeTeste(t)

	for i := 1; i <= 2; i++ {
		app := fx.New(fx.Supply(cfg), bootstrap.Modulo(), fx.NopLogger)
		if err := app.Err(); err != nil {
			t.Fatalf("ciclo %d, composição: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := app.Start(ctx); err != nil {
			cancel()
			t.Fatalf("ciclo %d, start: %v — recurso do ciclo anterior não foi liberado", i, err)
		}
		esperarSaudavel(t, "http://127.0.0.1:18099", 30*time.Second)
		if err := app.Stop(ctx); err != nil {
			cancel()
			t.Fatalf("ciclo %d, stop: %v", i, err)
		}
		cancel()
	}
}

// Configuração inválida impede a aplicação de subir, e reporta TODOS os
// problemas de uma vez em vez de um por tentativa.
//
// A validação é exercitada sobre um Config vazio, e NÃO limpando o ambiente
// do processo: os.Clearenv apaga o PATH, e os testes seguintes deixariam de
// encontrar o `docker` — pulando em silêncio, que é pior do que falhar.
func TestConfiguracaoInvalidaImpedeSubir(t *testing.T) {
	err := config.Config{}.Validate()
	if err == nil {
		t.Fatal("configuração vazia deveria ser recusada")
	}
	msg := err.Error()
	for _, esperado := range []string{"DATABASE_URL", "OIDC_ISSUER_URL"} {
		if !contemTexto(msg, esperado) {
			t.Errorf("o erro deveria citar %s; veio: %s", esperado, msg)
		}
	}
}

func contemTexto(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
