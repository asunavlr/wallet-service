// Package worker reúne os processos de fundo: o publicador da outbox e o
// resolvedor de referências pendentes.
//
// Os dois seguem a mesma forma: um laço com intervalo, cancelável por
// contexto, cujo estado inteiro vive no banco. Reiniciar não perde nada, e
// várias instâncias competem sem coordenação externa.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/kevinmatos/wallet-service/internal/app"
)

// OutboxRelay publica os eventos gravados pela transação de negócio.
type OutboxRelay struct {
	uow       app.UnitOfWork
	publisher app.Publisher
	clock     app.Clock
	metrics   app.Metrics
	log       *slog.Logger
	cfg       OutboxConfig
}

// OutboxConfig governa o laço de publicação.
type OutboxConfig struct {
	Interval    time.Duration
	BatchSize   int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
	// PublishTimeout limita a chamada ao destino. Importa porque o lock do
	// registro é mantido enquanto a publicação acontece: sem timeout, um
	// destino pendurado seguraria o lote indefinidamente.
	PublishTimeout time.Duration
}

func NewOutboxRelay(uow app.UnitOfWork, p app.Publisher, c app.Clock, m app.Metrics, log *slog.Logger, cfg OutboxConfig) *OutboxRelay {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = time.Minute
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.PublishTimeout <= 0 {
		cfg.PublishTimeout = 5 * time.Second
	}
	return &OutboxRelay{uow: uow, publisher: p, clock: c, metrics: m, log: log, cfg: cfg}
}

// Run roda até o contexto ser cancelado.
func (r *OutboxRelay) Run(ctx context.Context) {
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("relay da outbox encerrado")
			return
		case <-tick.C:
			if n, err := r.Once(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("ciclo do relay falhou", slog.String("erro", err.Error()))
			} else if n > 0 {
				r.log.Debug("eventos publicados", slog.Int("quantidade", n))
			}
		}
	}
}

// Once executa um ciclo: reivindica um lote, publica e confirma.
//
// A sequência é deliberada — reivindicar travando, publicar, marcar e só
// então commitar. Se o processo morrer entre a publicação e o commit, o lock
// cai junto com a transação e outro publisher assume o MESMO evento, com o
// mesmo eventId. O resultado é entrega ao menos uma vez com identidade
// estável, que é o que o enunciado pede ao exigir que republicações preservem
// o eventId.
func (r *OutboxRelay) Once(ctx context.Context) (int, error) {
	agora := r.clock.Now()
	publicados := 0

	err := r.uow.Do(ctx, func(ctx context.Context, repos *app.Repos) error {
		eventos, err := repos.Outbox.Claim(ctx, agora, r.cfg.BatchSize)
		if err != nil {
			return err
		}
		for _, e := range eventos {
			pubCtx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
			err := r.publisher.Publish(pubCtx, e)
			cancel()

			if err == nil {
				if err := repos.Outbox.MarkPublished(ctx, e.EventID, r.clock.Now()); err != nil {
					return err
				}
				publicados++
				continue
			}

			// Falha na publicação: reagenda com backoff, ou manda para a
			// dead-letter quando as tentativas se esgotam.
			if e.Attempts+1 >= r.cfg.MaxAttempts {
				r.metrics.DeadLetter("outbox")
				r.log.Error("evento esgotou as tentativas de publicação",
					slog.String("eventId", e.EventID.String()),
					slog.String("eventType", e.EventType),
					slog.Int("tentativas", e.Attempts+1))
				if err := repos.Outbox.DeadLetter(ctx, e.EventID, r.clock.Now(), err.Error()); err != nil {
					return err
				}
				continue
			}
			r.metrics.Retry("outbox")
			proxima := r.clock.Now().Add(backoff(r.cfg.BaseDelay, r.cfg.MaxDelay, e.Attempts))
			if err := repos.Outbox.Reschedule(ctx, e.EventID, proxima, err.Error()); err != nil {
				return err
			}
		}

		// A métrica de atraso é o sinal de que a publicação parou mesmo
		// quando o resto parece saudável.
		if atraso, err := repos.Outbox.OldestPendingAge(ctx, r.clock.Now()); err == nil {
			r.metrics.OutboxLag(atraso)
		}
		return nil
	})
	return publicados, err
}

// backoff devolve base × 2^tentativas, limitado ao máximo e sem estourar o
// deslocamento.
func backoff(base, max time.Duration, tentativas int) time.Duration {
	if tentativas < 0 {
		tentativas = 0
	}
	if tentativas > 30 {
		return max
	}
	d := base << uint(tentativas)
	if d <= 0 || d > max {
		return max
	}
	return d
}
