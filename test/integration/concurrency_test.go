//go:build integration

package integration_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinmatos/wallet-service/internal/adapter/postgres"
	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
	"github.com/kevinmatos/wallet-service/test/testenv"
)

func brl(s string) money.Money { return money.MustParse(s, "BRL") }

// servicos monta o par de casos de uso sobre um pool.
//
// Cada chamada cria um UnitOfWork próprio — é isso que simula instâncias
// independentes: memória separada e conexões distintas do pool.
func servicos(pool *pgxpool.Pool) (*app.WalletService, *app.WagerService) {
	uow := postgres.NewUnitOfWork(pool)
	clock, ids, m := app.SystemClock{}, app.UUIDGenerator{}, app.NoMetrics{}
	politica := app.PendingPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 5}
	return app.NewWalletService(uow, clock, ids, m, nil), app.NewWagerService(uow, clock, ids, m, nil, politica, 5)
}

func abrir(t *testing.T, ws *app.WalletService, saldo string) uuid.UUID {
	t.Helper()
	w, err := ws.Open(context.Background(), app.OpenWallet{
		PlayerID: uuid.New(), Initial: brl(saldo), CorrelationID: "teste",
	})
	if err != nil {
		t.Fatalf("abrindo carteira: %v", err)
	}
	return w.ID()
}

func aposta(walletID, playerID uuid.UUID, extID, valor string) app.Submit {
	return app.Submit{
		ProviderID: "provider-a", ExternalID: extID,
		IdempotencyKey: "provider-a:" + extID,
		PlayerID:       playerID, WalletID: walletID,
		RoundID: "round-1", GameID: "fortune-chimp",
		Kind: wager.Bet, Money: brl(valor), Source: "http",
	}
}

// O cenário obrigatório do enunciado: uma carteira com 100.00 recebe, ao
// mesmo tempo, duas apostas distintas de 80.00.
func TestDuasApostasDisputandoOMesmoSaldo(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)

	ctx := context.Background()
	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}

	// Cada aposta vai por um serviço próprio, com UnitOfWork próprio.
	_, ts1 := servicos(pool)
	_, ts2 := servicos(pool)
	_ = ts

	var wg sync.WaitGroup
	res := make([]app.Result, 2)
	errs := make([]error, 2)
	partida := make(chan struct{})

	for i, svc := range []*app.WagerService{ts1, ts2} {
		wg.Add(1)
		go func(i int, s *app.WagerService) {
			defer wg.Done()
			<-partida // largada simultânea
			res[i], errs[i] = s.Submit(ctx, aposta(w.ID(), w.PlayerID(), "tx-"+string(rune('a'+i)), "80.00"))
		}(i, svc)
	}
	close(partida)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("aposta %d falhou com erro: %v", i, err)
		}
	}

	var processadas, rejeitadas int
	for _, r := range res {
		switch r.Status {
		case wager.Processed:
			processadas++
		case wager.Rejected:
			rejeitadas++
			if r.FailureCode != wager.InsufficientFunds {
				t.Errorf("rejeição com código %s, queria INSUFFICIENT_FUNDS", r.FailureCode)
			}
		}
	}
	if processadas != 1 || rejeitadas != 1 {
		t.Fatalf("resultado = %d processadas, %d rejeitadas; queria 1 e 1", processadas, rejeitadas)
	}

	// saldo final 20.00
	final, err := ws.Get(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if final.Balance().String() != "20.00" {
		t.Errorf("saldo final = %s, queria 20.00", final.Balance())
	}

	// exatamente um débito no ledger (além do crédito de abertura)
	var debitos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`,
		w.ID()).Scan(&debitos); err != nil {
		t.Fatal(err)
	}
	if debitos != 1 {
		t.Errorf("débitos no ledger = %d, queria 1", debitos)
	}

	// e a reconciliação fecha
	rec, err := ws.Reconcile(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Consistent || rec.Difference.String() != "0.00" {
		t.Errorf("reconciliação = %+v, queria consistente com diferença 0.00", rec)
	}
}

// Reenviar as mesmas apostas não pode alterar o resultado.
func TestReenvioNaoAlteraResultado(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	primeira := aposta(w.ID(), w.PlayerID(), "tx-a", "80.00")
	segunda := aposta(w.ID(), w.PlayerID(), "tx-b", "80.00")

	r1, err := ts.Submit(ctx, primeira)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := ts.Submit(ctx, segunda)
	if err != nil {
		t.Fatal(err)
	}

	// reenvia as duas várias vezes
	for i := 0; i < 5; i++ {
		rr1, err := ts.Submit(ctx, primeira)
		if err != nil {
			t.Fatal(err)
		}
		if !rr1.Replay || rr1.Status != r1.Status || rr1.TransactionID != r1.TransactionID {
			t.Errorf("reenvio da primeira = %+v, queria replay de %+v", rr1, r1)
		}
		rr2, err := ts.Submit(ctx, segunda)
		if err != nil {
			t.Fatal(err)
		}
		if !rr2.Replay || rr2.Status != r2.Status {
			t.Errorf("reenvio da segunda = %+v, queria replay de %+v", rr2, r2)
		}
	}

	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "20.00" {
		t.Errorf("saldo após reenvios = %s, queria 20.00", final.Balance())
	}
}

// A mesma aposta enviada 50 vezes em paralelo produz um único débito.
func TestCinquentaEnviosDaMesmaApostaDebitamUmaVez(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, _ := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("1000.00")})
	if err != nil {
		t.Fatal(err)
	}
	pedido := aposta(w.ID(), w.PlayerID(), "tx-unica", "25.00")

	const n = 50
	var wg sync.WaitGroup
	res := make([]app.Result, n)
	errs := make([]error, n)
	partida := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, svc := servicos(pool) // instância própria por requisição
			<-partida
			res[i], errs[i] = svc.Submit(ctx, pedido)
		}(i)
	}
	close(partida)
	wg.Wait()

	var replays, novas int
	for i, err := range errs {
		if err != nil {
			t.Fatalf("envio %d falhou: %v", i, err)
		}
		if res[i].Replay {
			replays++
		} else {
			novas++
		}
		if res[i].TransactionID != res[0].TransactionID {
			t.Errorf("envio %d devolveu outra transação", i)
		}
	}
	if novas != 1 || replays != n-1 {
		t.Errorf("= %d novas e %d replays; queria 1 e %d", novas, replays, n-1)
	}

	var debitos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`,
		w.ID()).Scan(&debitos); err != nil {
		t.Fatal(err)
	}
	if debitos != 1 {
		t.Fatalf("débitos = %d, queria 1", debitos)
	}

	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "975.00" {
		t.Errorf("saldo = %s, queria 975.00", final.Balance())
	}
}

// Carteiras diferentes precisam avançar em paralelo: locks globais são
// proibidos pelo enunciado.
func TestCarteirasDistintasAvancamEmParalelo(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, _ := servicos(pool)
	ctx := context.Background()

	const n = 8
	ids := make([]uuid.UUID, n)
	jogadores := make([]uuid.UUID, n)
	for i := range ids {
		w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
		if err != nil {
			t.Fatal(err)
		}
		ids[i], jogadores[i] = w.ID(), w.PlayerID()
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	partida := make(chan struct{})
	inicio := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, svc := servicos(pool)
			<-partida
			_, errs[i] = svc.Submit(ctx, aposta(ids[i], jogadores[i], "tx-par-"+uuid.NewString(), "10.00"))
		}(i)
	}
	close(partida)
	wg.Wait()
	decorrido := time.Since(inicio)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("carteira %d: %v", i, err)
		}
	}
	for i, id := range ids {
		w, _ := ws.Get(ctx, id)
		if w.Balance().String() != "90.00" {
			t.Errorf("carteira %d com saldo %s, queria 90.00", i, w.Balance())
		}
	}
	t.Logf("%d carteiras em paralelo levaram %s", n, decorrido)
}

// Idempotência precisa sobreviver ao reinício de todos os processos: ela é
// persistente, não de memória. Aqui, um pool inteiramente novo — conexões e
// caches zerados — precisa enxergar o resultado gravado.
func TestIdempotenciaSobreviveAoReinicio(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	pedido := aposta(w.ID(), w.PlayerID(), "tx-persistente", "30.00")
	primeira, err := ts.Submit(ctx, pedido)
	if err != nil {
		t.Fatal(err)
	}

	// "reinicia": pool novo, serviços novos, nada compartilhado em memória
	outroPool := testenv.Pool(t)
	_, outroServico := servicos(outroPool)

	depois, err := outroServico.Submit(ctx, pedido)
	if err != nil {
		t.Fatal(err)
	}
	if !depois.Replay {
		t.Error("o reenvio após reinício deveria ser replay")
	}
	if depois.TransactionID != primeira.TransactionID {
		t.Errorf("transação = %s, queria %s", depois.TransactionID, primeira.TransactionID)
	}
}

// A mesma chave com conteúdo diferente é conflito; a mesma operação
// financeira com outra chave também é barrada.
func TestConflitosDeIdempotencia(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("500.00")})
	if err != nil {
		t.Fatal(err)
	}
	original := aposta(w.ID(), w.PlayerID(), "tx-1", "25.00")
	if _, err := ts.Submit(ctx, original); err != nil {
		t.Fatal(err)
	}

	// mesma chave, valor diferente
	divergente := original
	divergente.Money = brl("30.00")
	if _, err := ts.Submit(ctx, divergente); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Errorf("chave reutilizada com outro conteúdo = %v, queria ErrIdempotencyConflict", err)
	}

	// mesma operação financeira, outra chave
	outraChave := original
	outraChave.IdempotencyKey = "provider-a:outra-chave"
	if _, err := ts.Submit(ctx, outraChave); !errors.Is(err, app.ErrDuplicateExternalTransaction) {
		t.Errorf("mesma operação com outra chave = %v, queria ErrDuplicateExternalTransaction", err)
	}

	// o saldo não se moveu por causa das tentativas recusadas
	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "475.00" {
		t.Errorf("saldo = %s, queria 475.00", final.Balance())
	}
}

// O replay devolve o saldo do PROCESSAMENTO ORIGINAL, não o saldo atual — o
// enunciado é explícito. Recalcular na hora do replay daria o número errado.
func TestReplayDevolveOSaldoOriginal(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	primeira := aposta(w.ID(), w.PlayerID(), "tx-1", "10.00")
	r1, err := ts.Submit(ctx, primeira)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Balance.String() != "90.00" {
		t.Fatalf("saldo após a primeira = %s, queria 90.00", r1.Balance)
	}

	// a carteira segue se movendo
	if _, err := ts.Submit(ctx, aposta(w.ID(), w.PlayerID(), "tx-2", "20.00")); err != nil {
		t.Fatal(err)
	}
	atual, _ := ws.Get(ctx, w.ID())
	if atual.Balance().String() != "70.00" {
		t.Fatalf("saldo atual = %s, queria 70.00", atual.Balance())
	}

	// o replay da primeira ainda devolve 90.00
	replay, err := ts.Submit(ctx, primeira)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay {
		t.Error("deveria ser replay")
	}
	if replay.Balance.String() != "90.00" {
		t.Errorf("replay devolveu %s, queria o saldo original 90.00", replay.Balance)
	}
}

// LOSS processa, não cria lançamento e não versiona a carteira — mas emite
// WagerTransactionProcessed e nenhum WalletBalanceChanged.
func TestLossNaoMoveNemVersiona(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	antes, _ := ws.Get(ctx, w.ID())

	perda := aposta(w.ID(), w.PlayerID(), "tx-loss", "0.00")
	perda.Kind = wager.Loss
	r, err := ts.Submit(ctx, perda)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != wager.Processed {
		t.Fatalf("LOSS = %s, queria PROCESSED", r.Status)
	}

	depois, _ := ws.Get(ctx, w.ID())
	if depois.Version() != antes.Version() {
		t.Errorf("versão mudou de %d para %d; LOSS não versiona", antes.Version(), depois.Version())
	}
	if depois.Balance().String() != antes.Balance().String() {
		t.Errorf("saldo mudou; LOSS não move dinheiro")
	}

	var lancamentos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE transaction_id=$1`, r.TransactionID).Scan(&lancamentos); err != nil {
		t.Fatal(err)
	}
	if lancamentos != 0 {
		t.Errorf("LOSS gerou %d lançamentos, queria 0", lancamentos)
	}

	var processados, saldoAlterado int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE event_type='WagerTransactionProcessed'),
		        count(*) FILTER (WHERE event_type='WalletBalanceChanged')
		   FROM outbox_events WHERE payload::text LIKE '%'||$1||'%'`,
		r.TransactionID.String()).Scan(&processados, &saldoAlterado); err != nil {
		t.Fatal(err)
	}
	if processados != 1 {
		t.Errorf("eventos Processed = %d, queria 1", processados)
	}
	if saldoAlterado != 0 {
		t.Errorf("eventos BalanceChanged = %d, queria 0", saldoAlterado)
	}
}

// Abertura com saldo zero não cria OPENING, nem ledger, nem evento
// financeiro. Com saldo positivo, cria os três no mesmo commit.
func TestAberturaComESemSaldoInicial(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, _ := servicos(pool)
	ctx := context.Background()

	zero, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("0.00")})
	if err != nil {
		t.Fatal(err)
	}
	if zero.Version() != 1 {
		t.Errorf("versão = %d, queria 1", zero.Version())
	}
	var aberturas, lancamentos, eventos int
	pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1`, zero.ID()).Scan(&aberturas)
	pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE wallet_id=$1`, zero.ID()).Scan(&lancamentos)
	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, zero.ID()).Scan(&eventos)
	if aberturas != 0 || lancamentos != 0 || eventos != 0 {
		t.Errorf("saldo zero gerou %d OPENING, %d lançamentos, %d eventos; queria 0, 0, 0",
			aberturas, lancamentos, eventos)
	}

	comSaldo, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("1000.00")})
	if err != nil {
		t.Fatal(err)
	}
	if comSaldo.Version() != 1 {
		t.Errorf("versão com saldo inicial = %d, queria 1", comSaldo.Version())
	}
	pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='OPENING'`, comSaldo.ID()).Scan(&aberturas)
	pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE wallet_id=$1`, comSaldo.ID()).Scan(&lancamentos)
	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, comSaldo.ID()).Scan(&eventos)
	if aberturas != 1 || lancamentos != 1 || eventos != 2 {
		t.Errorf("saldo positivo gerou %d OPENING, %d lançamentos, %d eventos; queria 1, 1, 2",
			aberturas, lancamentos, eventos)
	}

	// segunda carteira do mesmo jogador e moeda é conflito
	if _, err := ws.Open(ctx, app.OpenWallet{PlayerID: comSaldo.PlayerID(), Initial: brl("10.00")}); !errors.Is(err, app.ErrConflict) {
		t.Errorf("segunda carteira = %v, queria ErrConflict", err)
	}
}
