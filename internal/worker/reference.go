package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
)

// ReferenceResolver reavalia as operações que esperam por uma referência.
//
// Existe porque uma reversão pode chegar antes da transação que ela reverte —
// o enunciado exige suportar isso. O estado inteiro vive no banco, então uma
// pendência sobrevive ao reinício e pode ser retomada por qualquer instância.
type ReferenceResolver struct {
	uow     app.UnitOfWork
	wagers  *app.WagerService
	clock   app.Clock
	metrics app.Metrics
	log     *slog.Logger
	cfg     ReferenceConfig
}

// ReferenceConfig governa a espera.
type ReferenceConfig struct {
	Interval    time.Duration
	BatchSize   int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

func NewReferenceResolver(uow app.UnitOfWork, w *app.WagerService, c app.Clock, m app.Metrics, log *slog.Logger, cfg ReferenceConfig) *ReferenceResolver {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
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
		cfg.MaxAttempts = 6
	}
	return &ReferenceResolver{uow: uow, wagers: w, clock: c, metrics: m, log: log, cfg: cfg}
}

// Run roda até o contexto ser cancelado.
func (r *ReferenceResolver) Run(ctx context.Context) {
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("resolvedor de pendências encerrado")
			return
		case <-tick.C:
			if _, err := r.Once(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("ciclo do resolvedor falhou", slog.String("erro", err.Error()))
			}
		}
	}
}

// Once processa um lote de pendências vencidas.
func (r *ReferenceResolver) Once(ctx context.Context) (int, error) {
	agora := r.clock.Now()

	// A listagem acontece numa transação curta e o processamento de cada
	// pendência em outra. Segurar o lote inteiro travado enquanto reavalia
	// bloquearia as carteiras envolvidas por mais tempo que o necessário.
	var ids []uuid.UUID
	err := r.uow.Do(ctx, func(ctx context.Context, repos *app.Repos) error {
		lote, err := repos.Transactions.DuePending(ctx, agora, r.cfg.BatchSize)
		ids = lote
		return err
	})
	if err != nil {
		return 0, err
	}

	resolvidas := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return resolvidas, ctx.Err()
		}
		if err := r.resolverUma(ctx, id); err != nil {
			r.log.Error("pendência não resolvida",
				slog.String("transactionId", id.String()),
				slog.String("erro", err.Error()))
			continue
		}
		resolvidas++
	}
	return resolvidas, nil
}

// resolverUma reavalia uma pendência.
//
// A decisão em si mora no caso de uso, para que as regras não divirjam entre
// quem chegou na hora e quem chegou antes da referência. Aqui fica só a
// transação SQL que delimita a retomada.
func (r *ReferenceResolver) resolverUma(ctx context.Context, id uuid.UUID) error {
	return r.uow.Do(ctx, func(ctx context.Context, repos *app.Repos) error {
		return r.wagers.ResolvePending(ctx, repos, id)
	})
}
