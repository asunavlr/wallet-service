// Package postgres implementa as portas de persistência com pgx e SQL
// explícito.
//
// Nenhum repositório abre transação: todos recebem um executor, que tanto
// pode ser o pool quanto uma pgx.Tx. Quem decide o limite da transação é o
// caso de uso — é o que permite confirmar saldo, ledger, estado e eventos
// no mesmo commit.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinmatos/wallet-service/internal/app"
)

// Querier é o mínimo que um repositório precisa. Pool e Tx satisfazem os dois.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UnitOfWork abre a transação e entrega os repositórios ligados a ela.
type UnitOfWork struct{ pool *pgxpool.Pool }

func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork { return &UnitOfWork{pool: pool} }

// Do executa fn dentro de uma transação READ COMMITTED.
//
// READ COMMITTED e não SERIALIZABLE: toda coordenação financeira passa pelo
// SELECT ... FOR UPDATE da carteira, então o nível serializável não
// acrescentaria garantia — só acrescentaria erro de serialização sob disputa,
// exatamente no caso das duas apostas concorrentes, e exigiria uma camada de
// retry para resolver um problema criado por ele mesmo.
func (u *UnitOfWork) Do(ctx context.Context, fn func(context.Context, *app.Repos) error) error {
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return classificar(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op depois de um commit bem-sucedido

	if err := fn(ctx, Repos(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return classificar(err)
	}
	return nil
}

// Snapshot roda a função numa transação REPEATABLE READ somente-leitura.
//
// REPEATABLE READ fixa o snapshot no primeiro statement e o mantém até o fim:
// saldo e ledger passam a ser lidos do mesmo instante. Somente-leitura deixa
// isso explícito para o banco e impede que um caminho de leitura escreva por
// engano.
//
// Não bloqueia escritores: eles seguem normalmente, e esta transação apenas
// continua vendo o mundo como ele era quando começou — que é exatamente o que
// uma conferência precisa.
func (u *UnitOfWork) Snapshot(ctx context.Context, fn func(context.Context, *app.Repos) error) error {
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return classificar(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, Repos(tx)); err != nil {
		return err
	}
	return classificar(tx.Commit(ctx))
}

// Repos monta os repositórios sobre um executor.
func Repos(q Querier) *app.Repos {
	return &app.Repos{
		Wallets:      &WalletRepo{q: q},
		Transactions: &TransactionRepo{q: q},
		Ledger:       &LedgerRepo{q: q},
		Inbox:        &InboxRepo{q: q},
		Outbox:       &OutboxRepo{q: q},
	}
}

// classificar traduz erro do Postgres para a taxonomia da aplicação.
//
// É esta função que decide se a operação é refeita, se a mensagem volta para
// a fila ou se vai para a DLQ. Errar aqui manda aposta sem saldo para a DLQ
// ou insiste eternamente num payload malformado.
func classificar(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ErrNotFound
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err // cancelamento é repassado sem reclassificar
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		// unicidade, exclusão, deadlock e falha de serialização: transitórios,
		// porque refazer enxerga o vencedor da corrida
		case "23505", "23P01", "40001", "40P01":
			return fmt.Errorf("%w: %s", app.ErrConflict, pgErr.Message)
		// lock_timeout e statement_timeout
		case "55P03", "57014":
			return fmt.Errorf("%w: %s", app.ErrUnavailable, pgErr.Message)
		}
		switch {
		// classe 08 (conexão), 53 (recursos), 57 (intervenção do operador),
		// 58 (sistema): a dependência caiu, tentar de novo pode dar certo
		case len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "08" || pgErr.Code[:2] == "53" ||
			pgErr.Code[:2] == "57" || pgErr.Code[:2] == "58"):
			return fmt.Errorf("%w: %s", app.ErrUnavailable, pgErr.Message)
		}
		// violação de CHECK, FK ou trigger: permanente. Insistir dá o mesmo
		// resultado, então o lugar é a DLQ, não a fila.
		return fmt.Errorf("%s (SQLSTATE %s)", pgErr.Message, pgErr.Code)
	}
	// pool fechado, rede: transitório
	return fmt.Errorf("%w: %s", app.ErrUnavailable, err)
}

// Config são os parâmetros do pool.
type Config struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

// NewPool cria o pool já com os timeouts por conexão.
//
// lock_timeout evita que uma instância fique presa indefinidamente atrás de
// uma carteira travada por outra que não responde; sem ele, uma transação
// pendurada arrastaria as demais.
func NewPool(ctx context.Context, c Config) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL inválida: %w", err)
	}
	if c.MaxConns > 0 {
		cfg.MaxConns = c.MaxConns
	}
	if c.MinConns > 0 {
		cfg.MinConns = c.MinConns
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	lock := c.LockTimeout
	if lock <= 0 {
		lock = 5 * time.Second
	}
	stmt := c.StatementTimeout
	if stmt <= 0 {
		stmt = 30 * time.Second
	}
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = fmt.Sprintf("%d", lock.Milliseconds())
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", stmt.Milliseconds())

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, classificar(err)
	}
	return pool, nil
}
