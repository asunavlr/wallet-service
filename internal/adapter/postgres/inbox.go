package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type InboxRepo struct{ q Querier }

// Register grava a chegada da mensagem.
//
// Devolve false quando a mensagem já estava registrada. É esse retorno que
// absorve a reentrega depois de uma queda entre o commit e o DeleteMessage:
// o consumidor reconhece a duplicata e apaga a mensagem sem reprocessar.
//
// ON CONFLICT DO NOTHING em vez de consultar antes de inserir: consultar
// primeiro abriria uma janela entre a leitura e a escrita, e duas instâncias
// poderiam decidir processar a mesma mensagem.
func (r *InboxRepo) Register(ctx context.Context, consumer, messageID, payloadHash string, now time.Time) (bool, error) {
	tag, err := r.q.Exec(ctx,
		`INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		 VALUES ($1,$2,$3,$4) ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		consumer, messageID, payloadHash, now)
	if err != nil {
		return false, classificar(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *InboxRepo) Complete(ctx context.Context, consumer, messageID string, txID *uuid.UUID, now time.Time) error {
	_, err := r.q.Exec(ctx,
		`UPDATE inbox_messages SET processed_at=$3, transaction_id=$4
		  WHERE consumer_name=$1 AND message_id=$2`, consumer, messageID, now, txID)
	return classificar(err)
}

// Hash devolve o hash registrado, para detectar o mesmo messageId chegando
// com conteúdo diferente — que é erro permanente e vai para a DLQ.
func (r *InboxRepo) Hash(ctx context.Context, consumer, messageID string) (string, error) {
	var h string
	err := r.q.QueryRow(ctx,
		`SELECT payload_hash FROM inbox_messages WHERE consumer_name=$1 AND message_id=$2`,
		consumer, messageID).Scan(&h)
	if err != nil {
		return "", classificar(err)
	}
	return h, nil
}
