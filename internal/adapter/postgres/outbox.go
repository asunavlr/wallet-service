package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
)

type OutboxRepo struct{ q Querier }

// Append grava os eventos no mesmo commit da mudança que os originou.
//
// É a metade que torna impossível publicar antes de confirmar: se a transação
// não commitar, o evento não existe; se commitar, ele existe e o publisher o
// encontrará, mesmo que o processo morra no instante seguinte.
func (r *OutboxRepo) Append(ctx context.Context, events ...app.OutboxEvent) error {
	for _, e := range events {
		_, err := r.q.Exec(ctx,
			`INSERT INTO outbox_events
			   (event_id, event_type, event_version, aggregate_type, aggregate_id,
			    partition_key, payload, correlation_id, causation_id, occurred_at, next_attempt_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`,
			e.EventID, e.EventType, e.EventVersion, e.AggregateType, e.AggregateID,
			e.PartitionKey, string(e.Payload), e.CorrelationID, nulo(e.CausationID), e.OccurredAt)
		if err != nil {
			return classificar(err)
		}
	}
	return nil
}

// Claim trava um lote de eventos pendentes.
//
// FOR UPDATE SKIP LOCKED é a resposta exata ao que o enunciado pede de
// "múltiplos publishers, disputa por registros e recuperação de trabalho
// abandonado": cada publisher leva um lote diferente sem coordenação
// externa, e um publisher que morre com a transação aberta simplesmente
// devolve o lock ao banco — outro assume o mesmo evento, com o mesmo eventId.
func (r *OutboxRepo) Claim(ctx context.Context, now time.Time, limit int) ([]app.OutboxEvent, error) {
	rows, err := r.q.Query(ctx,
		`SELECT event_id, event_type, event_version, aggregate_type, aggregate_id,
		        partition_key, payload, correlation_id, COALESCE(causation_id,''),
		        occurred_at, attempts
		   FROM outbox_events
		  WHERE published_at IS NULL AND dead_lettered_at IS NULL AND next_attempt_at <= $1
		  ORDER BY seq
		  FOR UPDATE SKIP LOCKED
		  LIMIT $2`, now, limit)
	if err != nil {
		return nil, classificar(err)
	}
	defer rows.Close()

	var eventos []app.OutboxEvent
	for rows.Next() {
		var e app.OutboxEvent
		var payload string
		ocorreu := new(timestamp)
		if err := rows.Scan(&e.EventID, &e.EventType, &e.EventVersion, &e.AggregateType,
			&e.AggregateID, &e.PartitionKey, &payload, &e.CorrelationID, &e.CausationID,
			&ocorreu.t, &e.Attempts); err != nil {
			return nil, classificar(err)
		}
		e.Payload, e.OccurredAt = []byte(payload), ocorreu.t
		eventos = append(eventos, e)
	}
	return eventos, classificar(rows.Err())
}

func (r *OutboxRepo) MarkPublished(ctx context.Context, eventID uuid.UUID, now time.Time) error {
	_, err := r.q.Exec(ctx,
		`UPDATE outbox_events SET published_at=$2 WHERE event_id=$1 AND published_at IS NULL`,
		eventID, now)
	return classificar(err)
}

func (r *OutboxRepo) Reschedule(ctx context.Context, eventID uuid.UUID, next time.Time, lastErr string) error {
	_, err := r.q.Exec(ctx,
		`UPDATE outbox_events SET attempts=attempts+1, next_attempt_at=$2, last_error=$3
		  WHERE event_id=$1`, eventID, next, truncar(lastErr, 500))
	return classificar(err)
}

func (r *OutboxRepo) DeadLetter(ctx context.Context, eventID uuid.UUID, now time.Time, lastErr string) error {
	_, err := r.q.Exec(ctx,
		`UPDATE outbox_events SET dead_lettered_at=$2, last_error=$3 WHERE event_id=$1`,
		eventID, now, truncar(lastErr, 500))
	return classificar(err)
}

// OldestPendingAge alimenta a métrica de atraso da outbox, que é o sinal de
// que a publicação parou mesmo quando tudo o mais parece saudável.
func (r *OutboxRepo) OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, error) {
	var maisAntigo *time.Time
	err := r.q.QueryRow(ctx,
		`SELECT MIN(occurred_at) FROM outbox_events
		  WHERE published_at IS NULL AND dead_lettered_at IS NULL`).Scan(&maisAntigo)
	if err != nil {
		return 0, classificar(err)
	}
	if maisAntigo == nil {
		return 0, nil
	}
	return now.Sub(*maisAntigo), nil
}

func truncar(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
