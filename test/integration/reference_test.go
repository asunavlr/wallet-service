//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinmatos/wallet-service/internal/adapter/postgres"
	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
	"github.com/kevinmatos/wallet-service/internal/worker"
	"github.com/kevinmatos/wallet-service/test/testenv"
)

// relogioFalso permite adiantar o tempo sem esperar o backoff de verdade.
type relogioFalso struct{ t time.Time }

func (r *relogioFalso) Now() time.Time          { return r.t }
func (r *relogioFalso) Avancar(d time.Duration) { r.t = r.t.Add(d) }

func servicosCom(pool *pgxpool.Pool, clock app.Clock, p app.PendingPolicy) (*app.WalletService, *app.WagerService) {
	uow := postgres.NewUnitOfWork(pool)
	ids, m := app.UUIDGenerator{}, app.NoMetrics{}
	return app.NewWalletService(uow, clock, ids, m), app.NewWagerService(uow, clock, ids, m, p, 5)
}

func operacao(walletID, playerID uuid.UUID, extID string, k wager.Kind, valor, refExt string) app.Submit {
	return app.Submit{
		ProviderID: "provider-a", ExternalID: extID, IdempotencyKey: "provider-a:" + extID,
		PlayerID: playerID, WalletID: walletID, RoundID: "round-1", GameID: "fortune-chimp",
		Kind: k, Money: brl(valor), ReferenceExtID: refExt, Source: "http",
	}
}

// O cenário 7 do enunciado: a reversão chega ANTES da transação que reverte.
func TestReversaoChegandoAntesDaReferencia(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)

	clock := &relogioFalso{t: time.Now().UTC()}
	politica := app.PendingPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 5}
	ws, ts := servicosCom(pool, clock, politica)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}

	// O REFUND chega primeiro, citando uma aposta que ainda não existe.
	refund, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-refund", wager.Refund, "30.00", "tx-bet"))
	if err != nil {
		t.Fatal(err)
	}
	if refund.Status != wager.PendingReference {
		t.Fatalf("REFUND sem referência = %s, queria PENDING_REFERENCE", refund.Status)
	}
	// e o evento de espera foi enfileirado
	var pendentes int
	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='WagerTransactionPendingReference'`).Scan(&pendentes)
	if pendentes != 1 {
		t.Errorf("eventos de espera = %d, queria 1", pendentes)
	}

	// Agora a aposta chega.
	bet, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-bet", wager.Bet, "30.00", ""))
	if err != nil {
		t.Fatal(err)
	}
	if bet.Status != wager.Processed {
		t.Fatalf("BET = %s, queria PROCESSED", bet.Status)
	}

	// O worker retoma a pendência.
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	resolvedor := worker.NewReferenceResolver(postgres.NewUnitOfWork(pool), ts, clock, app.NoMetrics{}, log,
		worker.ReferenceConfig{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 5})

	clock.Avancar(2 * time.Second)
	if _, err := resolvedor.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// O REFUND agora concluiu e o saldo voltou aos 100.00.
	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "100.00" {
		t.Errorf("saldo = %s, queria 100.00 (30 debitados e 30 devolvidos)", final.Balance())
	}
	var estado string
	pool.QueryRow(ctx, `SELECT status FROM wager_transactions WHERE id=$1`, refund.TransactionID).Scan(&estado)
	if estado != string(wager.Processed) {
		t.Errorf("REFUND ficou em %s, queria PROCESSED", estado)
	}

	rec, _ := ws.Reconcile(ctx, w.ID())
	if !rec.Consistent {
		t.Errorf("reconciliação divergente: %+v", rec)
	}
}

// Referência que nunca chega: a pendência expira com REFERENCE_NOT_FOUND.
func TestPendenciaExpiraQuandoAReferenciaNaoChega(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)

	clock := &relogioFalso{t: time.Now().UTC()}
	politica := app.PendingPolicy{BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxAttempts: 3}
	ws, ts := servicosCom(pool, clock, politica)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	r, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-orfa", wager.Rollback, "10.00", "nunca-existiu"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != wager.PendingReference {
		t.Fatalf("= %s, queria PENDING_REFERENCE", r.Status)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	resolvedor := worker.NewReferenceResolver(postgres.NewUnitOfWork(pool), ts, clock, app.NoMetrics{}, log,
		worker.ReferenceConfig{BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxAttempts: 3})

	// roda até esgotar as tentativas
	for i := 0; i < 6; i++ {
		clock.Avancar(time.Minute)
		if _, err := resolvedor.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var estado, codigo string
	if err := pool.QueryRow(ctx,
		`SELECT status, COALESCE(failure_code,'') FROM wager_transactions WHERE id=$1`,
		r.TransactionID).Scan(&estado, &codigo); err != nil {
		t.Fatal(err)
	}
	if estado != string(wager.Rejected) {
		t.Errorf("estado = %s, queria REJECTED", estado)
	}
	if codigo != string(wager.ReferenceNotFound) {
		t.Errorf("código = %s, queria REFERENCE_NOT_FOUND", codigo)
	}

	// o saldo nunca se moveu
	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "100.00" {
		t.Errorf("saldo = %s, queria 100.00 intacto", final.Balance())
	}
	// e a rejeição foi para a outbox
	var rejeitados int
	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='WagerTransactionRejected'`).Scan(&rejeitados)
	if rejeitados != 1 {
		t.Errorf("eventos de rejeição = %d, queria 1", rejeitados)
	}
}

// Uma aposta recebe no máximo UMA reversão bem-sucedida, de qualquer tipo:
// REFUND e ROLLBACK sobre a mesma BET devolveriam o mesmo débito duas vezes.
func TestReversaoDuplaERecusadaNoBanco(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)

	clock := &relogioFalso{t: time.Now().UTC()}
	politica := app.PendingPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 3}
	ws, ts := servicosCom(pool, clock, politica)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-bet", wager.Bet, "40.00", "")); err != nil {
		t.Fatal(err)
	}
	primeira, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-refund", wager.Refund, "40.00", "tx-bet"))
	if err != nil {
		t.Fatal(err)
	}
	if primeira.Status != wager.Processed {
		t.Fatalf("primeiro REFUND = %s", primeira.Status)
	}

	// um ROLLBACK da mesma aposta precisa ser recusado
	segunda, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-rollback", wager.Rollback, "40.00", "tx-bet"))
	if err != nil {
		t.Fatal(err)
	}
	if segunda.Status != wager.Rejected || segunda.FailureCode != wager.AlreadyReversed {
		t.Errorf("segunda reversão = %s/%s, queria REJECTED/ALREADY_REVERSED", segunda.Status, segunda.FailureCode)
	}

	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "100.00" {
		t.Errorf("saldo = %s, queria 100.00 — o débito voltou uma vez só", final.Balance())
	}
}

// Rollback que precisaria debitar além do saldo tem código próprio, diferente
// do de aposta sem saldo.
func TestRollbackSemSaldoTemCodigoProprio(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)

	clock := &relogioFalso{t: time.Now().UTC()}
	politica := app.PendingPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 3}
	ws, ts := servicosCom(pool, clock, politica)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("0.00")})
	if err != nil {
		t.Fatal(err)
	}
	// ganha 50, gasta 50, e então tentam desfazer o ganho
	if _, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-win", wager.Win, "50.00", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-bet", wager.Bet, "50.00", "")); err != nil {
		t.Fatal(err)
	}

	r, err := ts.Submit(ctx, operacao(w.ID(), w.PlayerID(), "tx-rb", wager.Rollback, "50.00", "tx-win"))
	if err != nil {
		t.Fatal(err)
	}
	if r.FailureCode != wager.ReversalInsufficientFunds {
		t.Errorf("código = %s, queria REVERSAL_INSUFFICIENT_FUNDS", r.FailureCode)
	}
	if r.FailureCode == wager.InsufficientFunds {
		t.Error("não pode ser o mesmo código da aposta sem saldo")
	}
}
