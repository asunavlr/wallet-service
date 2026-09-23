package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kevinmatos/wallet-service/internal/config"
)

// minimo é a configuração que permite subir: as duas obrigatórias.
func minimo(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("OIDC_ISSUER_URL", "http://idp/realms/wallet")
}

// A validação reúne TODOS os problemas antes de falhar: reportar um por vez
// faria quem configura descobri-los em série, com um deploy a cada descoberta.
func TestValidacaoReportaTodosOsProblemas(t *testing.T) {
	err := config.Config{}.Validate()
	if err == nil {
		t.Fatal("configuração vazia deveria ser recusada")
	}
	for _, esperado := range []string{"DATABASE_URL", "OIDC_ISSUER_URL"} {
		if !strings.Contains(err.Error(), esperado) {
			t.Errorf("o erro deveria citar %s; veio: %s", esperado, err)
		}
	}
	// os dois na MESMA mensagem, e não um por vez
	if strings.Count(err.Error(), "- ") < 2 {
		t.Errorf("os problemas deveriam vir juntos: %s", err)
	}
}

// A autenticação não é opcional: sem emissor, o serviço não sobe.
func TestSemEmissorNaoSobe(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("OIDC_ISSUER_URL", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("deveria recusar: autenticação não é opcional")
	}
}

func TestSemBancoNaoSobe(t *testing.T) {
	t.Setenv("OIDC_ISSUER_URL", "http://idp/realms/wallet")
	t.Setenv("DATABASE_URL", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("deveria recusar sem DATABASE_URL")
	}
}

func TestCombinacoesIncoerentes(t *testing.T) {
	casos := []struct {
		nome string
		muta func(*config.Config)
		cita string
	}{
		{"MAX_CONNS menor que MIN_CONNS", func(c *config.Config) {
			c.DBMaxConns, c.DBMinConns = 1, 10
		}, "DB_MAX_CONNS"},
		{"MAX_DELAY menor que BASE_DELAY", func(c *config.Config) {
			c.PendingBaseDelay, c.PendingMaxDelay = time.Minute, time.Second
		}, "PENDING_MAX_DELAY"},
		{"maxReceiveCount zero", func(c *config.Config) {
			c.SQSMaxReceive = 0
		}, "SQS_MAX_RECEIVE_COUNT"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			cfg := config.Config{
				DatabaseURL: "postgres://x", OIDCIssuer: "http://idp",
				DBMaxConns: 10, DBMinConns: 1,
				PendingBaseDelay: time.Second, PendingMaxDelay: time.Minute,
				SQSMaxReceive: 5,
			}
			c.muta(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("deveria ser recusada")
			}
			if !strings.Contains(err.Error(), c.cita) {
				t.Errorf("o erro deveria citar %s; veio: %s", c.cita, err)
			}
		})
	}
}

// Os padrões precisam ser os que o serviço realmente usa: se eles mudarem sem
// querer, o comportamento em produção muda junto e ninguém percebe.
func TestPadroes(t *testing.T) {
	minimo(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	casos := []struct {
		nome string
		got  any
		quer any
	}{
		{"HTTP_ADDR", cfg.HTTPAddr, ":8080"},
		{"APP_ENV", cfg.Env, "development"},
		{"DB_LOCK_TIMEOUT", cfg.DBLockTimeout, 5 * time.Second},
		{"DB_STATEMENT_TIMEOUT", cfg.DBStatementTimeout, 30 * time.Second},
		{"SQS_MAX_RECEIVE_COUNT", cfg.SQSMaxReceive, 5},
		{"OIDC_INTERNAL_SCOPE", cfg.OIDCInternalScope, "wallets:write"},
		{"OUTBOX_BATCH", cfg.OutboxBatch, 100},
		{"PENDING_MAX_ATTEMPTS", cfg.PendingMaxAttempts, 6},
		{"CONFLICT_RETRIES", cfg.ConflictRetries, 3},
		{"SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout, 20 * time.Second},
	}
	for _, c := range casos {
		if c.got != c.quer {
			t.Errorf("padrão de %s = %v, queria %v", c.nome, c.got, c.quer)
		}
	}
	if cfg.Production() {
		t.Error("development não deveria ser produção")
	}
}

func TestProducaoEReconhecida(t *testing.T) {
	minimo(t)
	t.Setenv("APP_ENV", "production")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Production() {
		t.Error("APP_ENV=production deveria ser reconhecido")
	}
}

// Valor malformado cai no padrão em vez de derrubar o processo: um typo em
// OUTBOX_BATCH não deve impedir o serviço de subir.
func TestValorMalformadoCaiNoPadrao(t *testing.T) {
	minimo(t)
	t.Setenv("OUTBOX_BATCH", "cem")
	t.Setenv("DB_LOCK_TIMEOUT", "cinco segundos")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutboxBatch != 100 {
		t.Errorf("OUTBOX_BATCH malformado = %d, queria o padrão 100", cfg.OutboxBatch)
	}
	if cfg.DBLockTimeout != 5*time.Second {
		t.Errorf("DB_LOCK_TIMEOUT malformado = %v, queria o padrão", cfg.DBLockTimeout)
	}
}

// O mapa remetente → provedor é o que autoriza uma mensagem da fila.
func TestMapaDeRemetentes(t *testing.T) {
	minimo(t)
	t.Setenv("SQS_SENDER_PROVIDERS", "000000000000=*, arn:aws:iam::1:user/a=provider-a ,vazio")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQSSenderProviders["000000000000"] != "*" {
		t.Errorf("curinga não foi lido: %v", cfg.SQSSenderProviders)
	}
	if cfg.SQSSenderProviders["arn:aws:iam::1:user/a"] != "provider-a" {
		t.Errorf("ARN com ':' e '/' deveria ser preservado: %v", cfg.SQSSenderProviders)
	}
	// entrada sem '=' é ignorada em vez de virar chave com valor vazio,
	// que autorizaria um remetente por engano
	if _, existe := cfg.SQSSenderProviders["vazio"]; existe {
		t.Error("entrada malformada não deveria virar autorização")
	}
}

func TestMapaVazioNaoAutorizaNinguemPorEngano(t *testing.T) {
	minimo(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQSSenderProviders != nil && len(cfg.SQSSenderProviders) > 0 {
		t.Errorf("sem a variável, o mapa deveria ficar vazio: %v", cfg.SQSSenderProviders)
	}
}

// Espaço em volta do valor é ruído de arquivo .env, e não parte do valor.
func TestEspacosSaoAparados(t *testing.T) {
	t.Setenv("DATABASE_URL", "  postgres://u:p@localhost:5432/db  ")
	t.Setenv("OIDC_ISSUER_URL", " http://idp/realms/wallet ")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(cfg.DatabaseURL, " ") || strings.HasSuffix(cfg.DatabaseURL, " ") {
		t.Errorf("DATABASE_URL = %q", cfg.DatabaseURL)
	}
}
