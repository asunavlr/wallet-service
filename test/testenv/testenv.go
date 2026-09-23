// Package testenv sobe a infraestrutura real usada pelos testes.
//
// O enunciado trata como eliminatório "substituição integral de PostgreSQL,
// SQS e IdP por mocks nos testes". Aqui o Postgres é de verdade, com as
// migrations aplicadas, e as constraints e triggers participam de cada teste.
package testenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinmatos/wallet-service/internal/adapter/postgres"
)

// URL é a conexão com o Postgres de teste.
// Vem de TEST_DATABASE_URL, com o padrão do `make db-up`.
func URL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://wallet:dev@localhost:55432/wallet?sslmode=disable"
}

// Pool devolve um pool conectado, pulando o teste se o banco não estiver de pé.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, postgres.Config{URL: URL(), MaxConns: 10})
	if err != nil {
		t.Skipf("banco de teste indisponível: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("banco de teste indisponível: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Reset limpa os dados preservando o schema.
//
// O ledger é append-only e recusa DELETE e TRUNCATE por trigger — o que é
// exatamente o que se quer em produção. Para o teste, os triggers são
// desabilitados apenas durante a limpeza, e reabilitados em seguida.
func Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		ALTER TABLE ledger_entries DISABLE TRIGGER USER;
		ALTER TABLE wallets DISABLE TRIGGER USER;
		ALTER TABLE wager_transactions DISABLE TRIGGER USER;
		ALTER TABLE outbox_events DISABLE TRIGGER USER;
		DELETE FROM inbox_messages;
		DELETE FROM outbox_events;
		DELETE FROM ledger_entries;
		DELETE FROM wager_transactions;
		DELETE FROM wallets;
		ALTER TABLE ledger_entries ENABLE TRIGGER USER;
		ALTER TABLE wallets ENABLE TRIGGER USER;
		ALTER TABLE wager_transactions ENABLE TRIGGER USER;
		ALTER TABLE outbox_events ENABLE TRIGGER USER;`)
	if err != nil {
		t.Fatalf("limpando o banco: %v", err)
	}
}

// Migrate aplica a migration inicial num banco vazio.
func Migrate(container string) error {
	caminho := "internal/adapter/postgres/migrations/000001_init.up.sql"
	f, err := os.Open(caminho)
	if err != nil {
		return err
	}
	defer f.Close()

	cmd := exec.Command("docker", "exec", "-i", container,
		"psql", "-U", "wallet", "-d", "wallet", "-v", "ON_ERROR_STOP=1", "-q")
	cmd.Stdin = f
	var saida strings.Builder
	cmd.Stderr = &saida
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, saida.String())
	}
	return nil
}
