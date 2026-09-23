package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

type WalletRepo struct{ q Querier }

const colunasCarteira = `id, player_id, balance_minor, currency, version, created_at, updated_at`

// Lock trava a linha da carteira.
//
// É o ponto de serialização por carteira. O lock é de linha: duas apostas na
// mesma carteira se enfileiram aqui, e apostas em carteiras diferentes não se
// tocam. Como cada transação trava exatamente uma carteira, não há ordem de
// aquisição e, portanto, não há deadlock possível.
func (r *WalletRepo) Lock(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return r.ler(ctx, `SELECT `+colunasCarteira+` FROM wallets WHERE id = $1 FOR UPDATE`, id)
}

func (r *WalletRepo) ByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return r.ler(ctx, `SELECT `+colunasCarteira+` FROM wallets WHERE id = $1`, id)
}

func (r *WalletRepo) ByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, c money.Currency) (*wallet.Wallet, error) {
	return r.ler(ctx, `SELECT `+colunasCarteira+` FROM wallets WHERE player_id = $1 AND currency = $2`, playerID, string(c))
}

func (r *WalletRepo) ler(ctx context.Context, sql string, args ...any) (*wallet.Wallet, error) {
	var (
		id, player uuid.UUID
		minor      int64
		moeda      string
		versao     int64
	)
	var criado, atualizado = new(timestamp), new(timestamp)
	err := r.q.QueryRow(ctx, sql, args...).
		Scan(&id, &player, &minor, &moeda, &versao, &criado.t, &atualizado.t)
	if err != nil {
		return nil, classificar(err)
	}
	saldo, err := money.FromMinor(minor, money.Currency(moeda))
	if err != nil {
		return nil, err
	}
	return wallet.Rehydrate(id, player, saldo, versao, criado.t, atualizado.t)
}

func (r *WalletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		w.ID(), w.PlayerID(), string(w.Currency()), w.Balance().Minor(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	return classificar(err)
}

// UpdateBalance grava o novo saldo condicionado à versão anterior.
//
// A condição na cláusula WHERE é a segunda linha de defesa contra lost
// update: o lock já serializa os escritores, mas se algum caminho futuro
// esquecer o FOR UPDATE, um escritor atrasado atualiza zero linhas e recebe
// conflito em vez de sobrescrever silenciosamente trabalho confirmado.
func (r *WalletRepo) UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE wallets SET balance_minor = $1, version = $2, updated_at = $3
		  WHERE id = $4 AND version = $5`,
		w.Balance().Minor(), w.Version(), w.UpdatedAt(), w.ID(), expectedVersion)
	if err != nil {
		return classificar(err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrConflict
	}
	return nil
}
