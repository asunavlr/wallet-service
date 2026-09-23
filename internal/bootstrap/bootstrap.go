// Package bootstrap é o ÚNICO pacote que conhece o Uber Fx.
//
// Isso não é estilo: o enunciado exige que o domínio permaneça independente
// de Fx, HTTP, SQS e persistência. Concentrar o Fx num pacote de borda torna
// essa independência verificável — basta olhar os imports.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/kevinmatos/wallet-service/internal/adapter/auth"
	"github.com/kevinmatos/wallet-service/internal/adapter/httpapi"
	"github.com/kevinmatos/wallet-service/internal/adapter/postgres"
	"github.com/kevinmatos/wallet-service/internal/adapter/sqs"
	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/config"
	"github.com/kevinmatos/wallet-service/internal/observability"
	"github.com/kevinmatos/wallet-service/internal/worker"
)

// New monta a aplicação.
func New(cfg config.Config) *fx.App {
	return fx.New(
		fx.Supply(cfg),
		Modulo(),
		fx.WithLogger(func(log *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: log.With(slog.String("component", "fx"))}
		}),
	)
}

// Modulo reúne todos os provedores. Exportado para que os testes montem a
// mesma composição que a produção — verificar uma montagem diferente da real
// não verificaria nada.
func Modulo() fx.Option {
	return fx.Options(
		modPlataforma,
		modPersistencia,
		modMensageria,
		modAutenticacao,
		modCasosDeUso,
		modHTTP,
		modWorkers,
	)
}

// ─── plataforma ─────────────────────────────────────────────────────────────

var modPlataforma = fx.Module("plataforma",
	fx.Provide(
		func(c config.Config) *slog.Logger { return observability.NewLogger(c.LogLevel) },
		observability.NewMetrics,
		func(m *observability.Metrics) app.Metrics { return m },
		func() app.Clock { return app.SystemClock{} },
		func() app.IDGenerator { return app.UUIDGenerator{} },
	),
)

// ─── persistência ───────────────────────────────────────────────────────────

var modPersistencia = fx.Module("persistencia",
	fx.Provide(
		novoPool,
		func(p *pgxpool.Pool) app.UnitOfWork { return postgres.NewUnitOfWork(p) },
		postgres.NewQueries,
		func(q *postgres.Queries) httpapi.Queries { return q },
	),
)

// novoPool cria o pool e registra o fechamento no ciclo de vida.
//
// O Fx executa os OnStop na ORDEM INVERSA dos OnStart, então o pool fecha
// depois dos workers e do servidor — que é o requisito de "fechamento das
// dependências após a finalização dos componentes que as utilizam".
func novoPool(lc fx.Lifecycle, c config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(context.Background(), postgres.Config{
		URL: c.DatabaseURL, MaxConns: c.DBMaxConns, MinConns: c.DBMinConns,
		LockTimeout: c.DBLockTimeout, StatementTimeout: c.DBStatementTimeout,
	})
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Validar a dependência na inicialização: subir com banco
			// inacessível só adia a descoberta para a primeira aposta.
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres inacessível: %w", err)
			}
			log.Info("postgres conectado")
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			log.Info("pool do postgres fechado")
			return nil
		},
	})
	return pool, nil
}

// ─── mensageria ─────────────────────────────────────────────────────────────

var modMensageria = fx.Module("mensageria",
	fx.Provide(
		novoClienteSQS,
		provisionarFilas,
		func(api *awssqs.Client, f sqs.Filas) app.Publisher {
			return sqs.NewPublisher(api, f.Saida, true)
		},
	),
)

func novoClienteSQS(c config.Config) (*awssqs.Client, error) {
	return sqs.NewClient(context.Background(), sqs.ClientConfig{
		Region: c.AWSRegion, Endpoint: c.SQSEndpoint,
		AccessKey: c.AWSAccessKey, SecretKey: c.AWSSecretKey,
	})
}

// provisionarFilas cria as filas na inicialização.
//
// Em desenvolvimento isso faz o `docker compose up` bastar. Em produção as
// filas já existem, e CreateQueue com os mesmos atributos é idempotente.
func provisionarFilas(api *awssqs.Client, c config.Config, log *slog.Logger) (sqs.Filas, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f, err := sqs.Provisionar(ctx, api, c.QueueIn, c.QueueDLQ, c.QueueOut, c.SQSMaxReceive)
	if err != nil {
		return sqs.Filas{}, fmt.Errorf("provisionando filas: %w", err)
	}
	log.Info("filas prontas",
		slog.String("entrada", f.Entrada), slog.String("dlq", f.DLQ), slog.String("saida", f.Saida))
	return f, nil
}

// ─── autenticação ───────────────────────────────────────────────────────────

var modAutenticacao = fx.Module("autenticacao",
	fx.Provide(
		func(c config.Config) (*auth.Verifier, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return auth.New(ctx, auth.Config{
				IssuerURL: c.OIDCIssuer, DiscoveryURL: c.OIDCDiscovery,
				Audience: c.OIDCAudience, InternalScope: c.OIDCInternalScope,
			})
		},
		func(v *auth.Verifier) httpapi.Autenticador { return v },
	),
)

// ─── casos de uso ───────────────────────────────────────────────────────────

var modCasosDeUso = fx.Module("casos-de-uso",
	fx.Provide(
		app.NewWalletService,
		func(uow app.UnitOfWork, c app.Clock, ids app.IDGenerator, m app.Metrics, cfg config.Config) *app.WagerService {
			return app.NewWagerService(uow, c, ids, m, app.PendingPolicy{
				BaseDelay: cfg.PendingBaseDelay, MaxDelay: cfg.PendingMaxDelay,
				MaxAttempts: cfg.PendingMaxAttempts,
			}, cfg.ConflictRetries)
		},
	),
)

// ─── http ───────────────────────────────────────────────────────────────────

var modHTTP = fx.Module("http",
	fx.Provide(
		httpapi.NewHandlers,
		novaSaude,
		novoRouter,
		novoServidor,
	),
	fx.Invoke(func(*httpapi.Server) {}), // força a construção do servidor
)

func novoRouter(h *httpapi.Handlers, a httpapi.Autenticador, s httpapi.Health, log *slog.Logger, m *observability.Metrics) http.Handler {
	return httpapi.Router(h, a, s, log, m.Handler())
}

func novoServidor(lc fx.Lifecycle, c config.Config, h http.Handler, log *slog.Logger) *httpapi.Server {
	srv := httpapi.NewServer(c.HTTPAddr, h, log)
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { return srv.Start() },
		OnStop: func(ctx context.Context) error {
			// Interrompe novas entradas e conclui o que está em andamento
			// dentro do prazo.
			log.Info("encerrando servidor http")
			return srv.Stop(ctx)
		},
	})
	return srv
}

// ─── workers ────────────────────────────────────────────────────────────────

var modWorkers = fx.Module("workers",
	fx.Provide(novoConsumidor, novoRelay, novoResolvedor),
	fx.Invoke(
		func(*sqs.Consumer) {},
		func(*worker.OutboxRelay) {},
		func(*worker.ReferenceResolver) {},
	),
)

// rodarWorker registra um processo de fundo no ciclo de vida.
//
// O contexto do worker é PRÓPRIO, não o do OnStart: o contexto do OnStart é
// cancelado assim que a inicialização termina, e um worker atrelado a ele
// morreria no instante em que a aplicação ficasse pronta. O cancelamento
// acontece no OnStop, e ali se espera o término observável — sem isso, o
// shutdown devolve o controle antes de o trabalho em andamento acabar.
func rodarWorker(lc fx.Lifecycle, nome string, log *slog.Logger, timeout time.Duration, run func(context.Context)) {
	ctx, cancelar := context.WithCancel(context.Background())
	terminou := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(terminou)
				run(ctx)
			}()
			log.Info("worker iniciado", slog.String("worker", nome))
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancelar()
			select {
			case <-terminou:
				log.Info("worker encerrado", slog.String("worker", nome))
				return nil
			case <-stopCtx.Done():
				log.Warn("worker não terminou no prazo", slog.String("worker", nome))
				return stopCtx.Err()
			}
		},
	})
}

func novoConsumidor(lc fx.Lifecycle, api *awssqs.Client, f sqs.Filas, uow app.UnitOfWork,
	w *app.WagerService, c app.Clock, m app.Metrics, log *slog.Logger, cfg config.Config) *sqs.Consumer {
	cons := sqs.NewConsumer(api, uow, w, c, m, log, sqs.ConsumerConfig{
		QueueURL: f.Entrada, DLQURL: f.DLQ, SenderProviders: cfg.SQSSenderProviders,
	})
	rodarWorker(lc, "sqs-consumer", log, cfg.ShutdownTimeout, cons.Run)
	return cons
}

func novoRelay(lc fx.Lifecycle, uow app.UnitOfWork, p app.Publisher, c app.Clock,
	m app.Metrics, log *slog.Logger, cfg config.Config) *worker.OutboxRelay {
	r := worker.NewOutboxRelay(uow, p, c, m, log, worker.OutboxConfig{
		Interval: cfg.OutboxInterval, BatchSize: cfg.OutboxBatch, MaxAttempts: cfg.OutboxMaxAttempts,
	})
	rodarWorker(lc, "outbox-relay", log, cfg.ShutdownTimeout, r.Run)
	return r
}

func novoResolvedor(lc fx.Lifecycle, uow app.UnitOfWork, w *app.WagerService, c app.Clock,
	m app.Metrics, log *slog.Logger, cfg config.Config) *worker.ReferenceResolver {
	r := worker.NewReferenceResolver(uow, w, c, m, log, worker.ReferenceConfig{
		Interval: cfg.PendingInterval, BatchSize: cfg.PendingBatch,
		BaseDelay: cfg.PendingBaseDelay, MaxDelay: cfg.PendingMaxDelay,
		MaxAttempts: cfg.PendingMaxAttempts,
	})
	rodarWorker(lc, "reference-resolver", log, cfg.ShutdownTimeout, r.Run)
	return r
}

// ─── saúde ──────────────────────────────────────────────────────────────────

type saude struct {
	pool *pgxpool.Pool
	api  *awssqs.Client
	fila string
}

func novaSaude(p *pgxpool.Pool, api *awssqs.Client, f sqs.Filas) httpapi.Health {
	return &saude{pool: p, api: api, fila: f.Entrada}
}

// Live não toca em dependência: um liveness que depende do banco derruba o
// processo toda vez que o banco pisca, o que é o oposto do que se quer.
func (s *saude) Live(context.Context) error { return nil }

// Ready confere Postgres e SQS, como o enunciado pede.
func (s *saude) Ready(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if _, err := s.api.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl: &s.fila,
	}); err != nil {
		return fmt.Errorf("sqs: %w", err)
	}
	return nil
}
