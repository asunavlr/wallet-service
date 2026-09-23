package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

// Queries são as leituras que não precisam de transação nem de lock.
// Vão direto ao pool, para não segurar conexão de escrita à toa.
type Queries struct{ pool *pgxpool.Pool }

func NewQueries(pool *pgxpool.Pool) *Queries { return &Queries{pool: pool} }

func (q *Queries) TransactionByID(ctx context.Context, id uuid.UUID) (*wager.Transaction, error) {
	return (&TransactionRepo{q: q.pool}).ByID(ctx, id)
}

func (q *Queries) TransactionByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error) {
	return (&TransactionRepo{q: q.pool}).ByExternalID(ctx, providerID, externalID)
}
