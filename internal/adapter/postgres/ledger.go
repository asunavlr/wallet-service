package postgres

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

type LedgerRepo struct{ q Querier }

func (r *LedgerRepo) Insert(ctx context.Context, e *wallet.LedgerEntry) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO ledger_entries
		   (id, wallet_id, transaction_id, direction, amount_minor, currency,
		    balance_before_minor, balance_after_minor, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID(), e.WalletID(), e.TransactionID(), string(e.Direction()),
		e.Amount().Minor(), string(e.Amount().Currency()),
		e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.CreatedAt())
	return classificar(err)
}

// Page devolve uma página do ledger.
//
// O cursor é opaco para o cliente: é o `seq` codificado em base64url. Usar
// `seq` — e não created_at — é o que dá ordenação estável, porque dois
// lançamentos podem compartilhar o mesmo instante mas nunca a mesma sequência.
func (r *LedgerRepo) Page(ctx context.Context, walletID uuid.UUID, cursor string, limit int) ([]app.LedgerRow, string, error) {
	depois := int64(0)
	if cursor != "" {
		v, err := decodificarCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: cursor inválido", app.ErrInvalidInput)
		}
		depois = v
	}

	rows, err := r.q.Query(ctx,
		`SELECT seq, id, wallet_id, transaction_id, direction, amount_minor, currency,
		        balance_before_minor, balance_after_minor, created_at
		   FROM ledger_entries
		  WHERE wallet_id = $1 AND seq > $2
		  ORDER BY seq
		  LIMIT $3`, walletID, depois, limit+1) // +1 para saber se há próxima página
	if err != nil {
		return nil, "", classificar(err)
	}
	defer rows.Close()

	var linhas []app.LedgerRow
	for rows.Next() {
		var (
			seq                  int64
			id, w, tx            uuid.UUID
			direcao, moeda       string
			valor, antes, depois int64
		)
		criado := new(timestamp)
		if err := rows.Scan(&seq, &id, &w, &tx, &direcao, &valor, &moeda, &antes, &depois, &criado.t); err != nil {
			return nil, "", classificar(err)
		}
		m, err := montarLancamento(id, w, tx, direcao, moeda, valor, antes, depois, criado)
		if err != nil {
			return nil, "", err
		}
		linhas = append(linhas, app.LedgerRow{Entry: m, Cursor: codificarCursor(seq)})
	}
	if err := rows.Err(); err != nil {
		return nil, "", classificar(err)
	}

	proximo := ""
	if len(linhas) > limit {
		linhas = linhas[:limit]
		proximo = linhas[len(linhas)-1].Cursor
	}
	return linhas, proximo, nil
}

// Balance reconstrói o saldo somando o ledger inteiro.
//
// A soma acontece em BIGINT no próprio banco: trazer as linhas para somar em
// Go seria mais lento e abriria espaço para conversão indevida.
func (r *LedgerRepo) Balance(ctx context.Context, walletID uuid.UUID) (money.Money, int, error) {
	var (
		total int64
		n     int
		moeda *string
	)
	err := r.q.QueryRow(ctx,
		`SELECT COALESCE(SUM(CASE WHEN direction='CREDIT' THEN amount_minor ELSE -amount_minor END), 0),
		        COUNT(*), MIN(currency)
		   FROM ledger_entries WHERE wallet_id = $1`, walletID).Scan(&total, &n, &moeda)
	if err != nil {
		return money.Money{}, 0, classificar(err)
	}

	// Ledger vazio não tem moeda própria: usa a da carteira.
	if moeda == nil {
		var daCarteira string
		if err := r.q.QueryRow(ctx, `SELECT currency FROM wallets WHERE id=$1`, walletID).Scan(&daCarteira); err != nil {
			return money.Money{}, 0, classificar(err)
		}
		moeda = &daCarteira
	}
	m, err := money.FromMinor(total, money.Currency(strings.TrimSpace(*moeda)))
	return m, n, err
}

func montarLancamento(
	id, w, tx uuid.UUID, direcao, moeda string,
	valor, antes, depois int64, criado *timestamp,
) (*wallet.LedgerEntry, error) {
	c := money.Currency(strings.TrimSpace(moeda))
	v, err := money.FromMinor(valor, c)
	if err != nil {
		return nil, err
	}
	a, err := money.FromMinor(antes, c)
	if err != nil {
		return nil, err
	}
	d, err := money.FromMinor(depois, c)
	if err != nil {
		return nil, err
	}
	return wallet.NewLedgerEntry(id, w, tx, wallet.Direction(direcao), v, a, d, criado.t)
}

func codificarCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seq, 10)))
}

func decodificarCursor(c string) (int64, error) {
	bruto, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(bruto), 10, 64)
}
