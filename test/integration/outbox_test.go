//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/adapter/postgres"
	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/worker"
	"github.com/kevinmatos/wallet-service/test/testenv"
)

// publicadorEspiao registra o que recebeu e pode falhar sob comando.
type publicadorEspiao struct {
	mu       sync.Mutex
	recebido []app.OutboxEvent
	falhar   bool
}

func (p *publicadorEspiao) Publish(_ context.Context, e app.OutboxEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.falhar {
		return errors.New("destino indisponível")
	}
	p.recebido = append(p.recebido, e)
	return nil
}

func (p *publicadorEspiao) IDs() []uuid.UUID {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]uuid.UUID, len(p.recebido))
	for i, e := range p.recebido {
		ids[i] = e.EventID
	}
	return ids
}

// O evento só existe depois do commit da transação que o originou: é por isso
// que publicar antes de confirmar é impossível nesta arquitetura.
func TestEventoSoApareceDepoisDoCommit(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Submit(ctx, aposta(w.ID(), w.PlayerID(), "tx-1", "25.00")); err != nil {
		t.Fatal(err)
	}

	// abertura: Processed + BalanceChanged; aposta: Processed + BalanceChanged
	var pendentes int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pendentes); err != nil {
		t.Fatal(err)
	}
	if pendentes != 4 {
		t.Errorf("eventos pendentes = %d, queria 4", pendentes)
	}

	espiao := &publicadorEspiao{}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := worker.NewOutboxRelay(postgres.NewUnitOfWork(pool), espiao, app.SystemClock{}, app.NoMetrics{}, log,
		worker.OutboxConfig{BatchSize: 10})

	n, err := r.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("publicados = %d, queria 4", n)
	}

	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pendentes)
	if pendentes != 0 {
		t.Errorf("restaram %d pendentes", pendentes)
	}
}

// O cenário 6 do enunciado: dois publishers disputando a mesma outbox.
// SKIP LOCKED garante que cada evento saia uma vez só por ciclo.
func TestDoisPublishersNaoDuplicamTrabalho(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("1000.00")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := ts.Submit(ctx, aposta(w.ID(), w.PlayerID(), "tx-"+uuid.NewString(), "1.00")); err != nil {
			t.Fatal(err)
		}
	}

	var total int
	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&total)

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	a, b := &publicadorEspiao{}, &publicadorEspiao{}
	r1 := worker.NewOutboxRelay(postgres.NewUnitOfWork(pool), a, app.SystemClock{}, app.NoMetrics{}, log,
		worker.OutboxConfig{BatchSize: 5})
	r2 := worker.NewOutboxRelay(postgres.NewUnitOfWork(pool), b, app.SystemClock{}, app.NoMetrics{}, log,
		worker.OutboxConfig{BatchSize: 5})

	// Os dois publishers rodam até a outbox esvaziar, e não um número fixo
	// de ciclos: sob disputa, um ciclo pode encontrar tudo travado pelo
	// outro e voltar de mãos vazias. O que se verifica é a PROPRIEDADE —
	// nenhum evento sai duas vezes e todos acabam saindo —, não uma
	// contagem de rodadas, que dependeria de quem ganhou cada corrida.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var erros []error
	partida := make(chan struct{})
	const maxCiclos = 200

	for _, r := range []*worker.OutboxRelay{r1, r2} {
		wg.Add(1)
		go func(r *worker.OutboxRelay) {
			defer wg.Done()
			<-partida
			for i := 0; i < maxCiclos; i++ {
				if _, err := r.Once(ctx); err != nil {
					mu.Lock()
					erros = append(erros, err)
					mu.Unlock()
					return
				}
				var restam int
				if err := pool.QueryRow(ctx,
					`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&restam); err != nil {
					return
				}
				if restam == 0 {
					return
				}
			}
		}(r)
	}
	close(partida)
	wg.Wait()
	for _, err := range erros {
		t.Errorf("publisher falhou: %v", err)
	}

	// Nenhum evento pode ter sido entregue duas vezes.
	contagem := map[uuid.UUID]int{}
	for _, id := range append(a.IDs(), b.IDs()...) {
		contagem[id]++
	}
	vistos := map[uuid.UUID]bool{}
	for id, n := range contagem {
		vistos[id] = true
		if n != 1 {
			t.Errorf("evento %s publicado %d vezes, queria 1", id, n)
		}
	}
	if len(vistos) != total {
		t.Errorf("eventos distintos publicados = %d, queria %d", len(vistos), total)
		// Diz QUAIS faltaram, e em que estado ficaram no banco.
		linhas, err := pool.Query(ctx,
			`SELECT event_id, event_type, published_at IS NOT NULL, attempts
			   FROM outbox_events ORDER BY seq`)
		if err == nil {
			defer linhas.Close()
			for linhas.Next() {
				var id uuid.UUID
				var tipo string
				var publicado bool
				var tentativas int
				if err := linhas.Scan(&id, &tipo, &publicado, &tentativas); err == nil && !vistos[id] {
					t.Logf("não entregue ao espião: %s %s publicado=%v tentativas=%d",
						id, tipo, publicado, tentativas)
				}
			}
		}
	}

	var pendentes int
	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pendentes)
	if pendentes != 0 {
		t.Errorf("restaram %d eventos pendentes", pendentes)
	}
}

// Falha na publicação reagenda com backoff; esgotadas as tentativas, o evento
// vai para a dead-letter — e o eventId é preservado em toda republicação.
func TestFalhaNaPublicacaoReagendaEDepoisDeadLetter(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, _ := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	var eventoID uuid.UUID
	pool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id=$1 LIMIT 1`, w.ID()).Scan(&eventoID)

	espiao := &publicadorEspiao{falhar: true}
	clock := &relogioFalso{t: time.Now().UTC()}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := worker.NewOutboxRelay(postgres.NewUnitOfWork(pool), espiao, clock, app.NoMetrics{}, log,
		worker.OutboxConfig{BatchSize: 10, BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 3})

	for i := 0; i < 5; i++ {
		clock.Avancar(2 * time.Minute)
		if _, err := r.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var mortos int
	pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE dead_lettered_at IS NOT NULL`).Scan(&mortos)
	if mortos == 0 {
		t.Error("nenhum evento chegou à dead-letter depois de esgotar as tentativas")
	}

	// o identificador do evento nunca mudou
	var aindaExiste bool
	pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_events WHERE event_id=$1)`, eventoID).Scan(&aindaExiste)
	if !aindaExiste {
		t.Error("o eventId deveria ser preservado")
	}
}

// Republicação preserva o eventId: o consumidor deduplica por ele, então a
// identidade precisa sobreviver à segunda tentativa.
func TestRepublicacaoPreservaOEventID(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, _ := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	var antes []uuid.UUID
	rows, _ := pool.Query(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id=$1 ORDER BY seq`, w.ID())
	for rows.Next() {
		var id uuid.UUID
		rows.Scan(&id)
		antes = append(antes, id)
	}
	rows.Close()

	// primeira tentativa falha
	falho := &publicadorEspiao{falhar: true}
	clock := &relogioFalso{t: time.Now().UTC()}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	uow := postgres.NewUnitOfWork(pool)
	cfg := worker.OutboxConfig{BatchSize: 10, BaseDelay: time.Second, MaxDelay: time.Minute, MaxAttempts: 5}

	if _, err := worker.NewOutboxRelay(uow, falho, clock, app.NoMetrics{}, log, cfg).Once(ctx); err != nil {
		t.Fatal(err)
	}

	// segunda tentativa dá certo
	ok := &publicadorEspiao{}
	clock.Avancar(2 * time.Minute)
	if _, err := worker.NewOutboxRelay(uow, ok, clock, app.NoMetrics{}, log, cfg).Once(ctx); err != nil {
		t.Fatal(err)
	}

	entregues := map[uuid.UUID]bool{}
	for _, id := range ok.IDs() {
		entregues[id] = true
	}
	for _, id := range antes {
		if !entregues[id] {
			t.Errorf("evento %s não foi republicado com o mesmo id", id)
		}
	}
}
