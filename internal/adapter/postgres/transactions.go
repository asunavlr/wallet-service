package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

type TransactionRepo struct{ q Querier }

const colunasTx = `id, origin, kind, status, wallet_id, player_id, amount_minor, currency,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id, failure_code,
	result_balance_minor, result_currency, attempts, next_attempt_at, correlation_id,
	created_at, updated_at`

func (r *TransactionRepo) Insert(ctx context.Context, t *wager.Transaction) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO wager_transactions (`+colunasTx+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		t.ID(), string(t.Origin()), string(t.Kind()), string(t.Status()),
		t.WalletID(), t.PlayerID(), t.Amount().Minor(), string(t.Amount().Currency()),
		nulo(t.ProviderID()), nulo(t.ExternalID()), nulo(t.IdempotencyKey()), nulo(t.PayloadHash()),
		nulo(t.RoundID()), nulo(t.GameID()), nulo(t.ReferenceExtID()), t.ReferenceID(),
		nulo(string(t.FailureCode())), saldoMinor(t), saldoMoeda(t),
		t.Attempts(), t.NextAttemptAt(), t.CorrelationID(), t.CreatedAt(), t.UpdatedAt())
	return classificar(err)
}

// Save atualiza o que pode mudar. A identidade e o payload ficam de fora
// porque o trigger do banco os recusa de qualquer forma.
func (r *TransactionRepo) Save(ctx context.Context, t *wager.Transaction) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE wager_transactions
		    SET status=$1, failure_code=$2, result_balance_minor=$3, result_currency=$4,
		        reference_transaction_id=$5, attempts=$6, next_attempt_at=$7, updated_at=$8
		  WHERE id=$9`,
		string(t.Status()), nulo(string(t.FailureCode())), saldoMinor(t), saldoMoeda(t),
		t.ReferenceID(), t.Attempts(), t.NextAttemptAt(), t.UpdatedAt(), t.ID())
	if err != nil {
		return classificar(err)
	}
	if tag.RowsAffected() == 0 {
		// Linha inexistente é bug de fluxo, não ausência benigna: falhar aqui
		// evita que um caminho errado passe em silêncio.
		return app.ErrNotFound
	}
	return nil
}

func (r *TransactionRepo) ByID(ctx context.Context, id uuid.UUID) (*wager.Transaction, error) {
	return r.ler(ctx, `SELECT `+colunasTx+` FROM wager_transactions WHERE id=$1`, id)
}

func (r *TransactionRepo) ByIdempotencyKey(ctx context.Context, providerID, key string) (*wager.Transaction, error) {
	return r.ler(ctx, `SELECT `+colunasTx+` FROM wager_transactions
		WHERE provider_id=$1 AND idempotency_key=$2`, providerID, key)
}

func (r *TransactionRepo) ByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error) {
	return r.ler(ctx, `SELECT `+colunasTx+` FROM wager_transactions
		WHERE provider_id=$1 AND external_transaction_id=$2`, providerID, externalID)
}

// HasSuccessfulReversal informa se a transação já foi revertida com sucesso.
// O índice parcial impõe a mesma regra; esta consulta permite responder com
// ALREADY_REVERSED em vez de deixar estourar a violação de unicidade.
func (r *TransactionRepo) HasSuccessfulReversal(ctx context.Context, referenceID uuid.UUID) (bool, error) {
	var existe bool
	err := r.q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM wager_transactions
		   WHERE reference_transaction_id=$1 AND status='PROCESSED'
		     AND kind IN ('REFUND','ROLLBACK'))`, referenceID).Scan(&existe)
	if err != nil {
		return false, classificar(err)
	}
	return existe, nil
}

// DuePending trava um lote de pendências vencidas com SKIP LOCKED, para que
// várias instâncias trabalhem em paralelo sem pegar a mesma linha.
func (r *TransactionRepo) DuePending(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := r.q.Query(ctx,
		`SELECT id FROM wager_transactions
		  WHERE status='PENDING_REFERENCE' AND next_attempt_at <= $1
		  ORDER BY next_attempt_at
		  FOR UPDATE SKIP LOCKED
		  LIMIT $2`, now, limit)
	if err != nil {
		return nil, classificar(err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, classificar(err)
		}
		ids = append(ids, id)
	}
	return ids, classificar(rows.Err())
}

// WakeWaitingFor antecipa as pendências que esperam por esta referência.
//
// Sem isso, uma reversão que chegou antes da aposta ficaria esperando o
// backoff inteiro depois que a aposta já chegou — tempo morto sem motivo.
func (r *TransactionRepo) WakeWaitingFor(ctx context.Context, providerID, externalID string, now time.Time) error {
	_, err := r.q.Exec(ctx,
		`UPDATE wager_transactions SET next_attempt_at=$3, updated_at=$3
		  WHERE status='PENDING_REFERENCE'
		    AND provider_id=$1 AND reference_external_transaction_id=$2
		    AND next_attempt_at > $3`, providerID, externalID, now)
	return classificar(err)
}

func (r *TransactionRepo) ler(ctx context.Context, sql string, args ...any) (*wager.Transaction, error) {
	var (
		in                          wager.RehydrateInput
		origem, tipo, estado, moeda string
		provider, extID, chave      *string
		hash, rodada, jogo, refExt  *string
		codigo, moedaResultado      *string
		minorResultado              *int64
		amountMinor                 int64
	)
	err := r.q.QueryRow(ctx, sql, args...).Scan(
		&in.ID, &origem, &tipo, &estado, &in.WalletID, &in.PlayerID, &amountMinor, &moeda,
		&provider, &extID, &chave, &hash, &rodada, &jogo,
		&refExt, &in.ReferenceID, &codigo,
		&minorResultado, &moedaResultado, &in.Attempts, &in.NextAttemptAt, &in.CorrelationID,
		&in.CreatedAt, &in.UpdatedAt)
	if err != nil {
		return nil, classificar(err)
	}

	valor, err := money.FromMinor(amountMinor, money.Currency(moeda))
	if err != nil {
		return nil, err
	}
	in.Origin, in.Kind, in.Status, in.Amount = wager.Origin(origem), wager.Kind(tipo), wager.Status(estado), valor
	in.ProviderID, in.ExternalID = texto(provider), texto(extID)
	in.IdempotencyKey, in.PayloadHash = texto(chave), texto(hash)
	in.RoundID, in.GameID, in.ReferenceExtID = texto(rodada), texto(jogo), texto(refExt)
	in.FailureCode = wager.FailureCode(texto(codigo))

	if minorResultado != nil && moedaResultado != nil {
		saldo, err := money.FromMinor(*minorResultado, money.Currency(*moedaResultado))
		if err != nil {
			return nil, err
		}
		in.ResultBalance = &saldo
	}
	return wager.Rehydrate(in)
}

// nulo converte string vazia em NULL, para que os CHECKs de formato do schema
// distingam ausência de string vazia.
func nulo(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func texto(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func saldoMinor(t *wager.Transaction) *int64 {
	if b := t.ResultBalance(); b != nil {
		m := b.Minor()
		return &m
	}
	return nil
}

func saldoMoeda(t *wager.Transaction) *string {
	if b := t.ResultBalance(); b != nil {
		c := string(b.Currency())
		return &c
	}
	return nil
}
