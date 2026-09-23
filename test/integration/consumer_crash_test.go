//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/adapter/postgres"
	"github.com/kevinmatos/wallet-service/internal/adapter/sqs"
	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/test/testenv"
)

// filaFalsa reproduz o SQS o bastante para exercitar a janela de falha que o
// cenário 5 do enunciado descreve.
//
// Um SQS de verdade não permite provocar essa janela de forma determinística:
// seria preciso matar o processo exatamente entre o COMMIT e o
// DeleteMessage, e acertar esse instante é sorte, não teste. Aqui o controle
// é explícito — `engolirDelete` simula o processo morrendo antes de remover a
// mensagem, e a reentrega seguinte é a mesma que o SQS faria.
//
// O PostgreSQL continua sendo real: a inbox, as constraints e a transação que
// absorvem a reentrega são as de produção.
type filaFalsa struct {
	mensagens     []types.Message
	engolirDelete bool
	deletadas     []string
	enviadasDLQ   []string
	liberadas     []string
}

func (f *filaFalsa) ReceiveMessage(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	msgs := f.mensagens
	f.mensagens = nil
	return &awssqs.ReceiveMessageOutput{Messages: msgs}, nil
}

func (f *filaFalsa) DeleteMessage(_ context.Context, in *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	if f.engolirDelete {
		// O processo "morreu" aqui: o commit já aconteceu, a mensagem não foi
		// removida e voltará para a fila.
		return nil, errors.New("processo interrompido antes de remover a mensagem")
	}
	f.deletadas = append(f.deletadas, aws.ToString(in.ReceiptHandle))
	return &awssqs.DeleteMessageOutput{}, nil
}

func (f *filaFalsa) ChangeMessageVisibility(_ context.Context, in *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
	f.liberadas = append(f.liberadas, aws.ToString(in.ReceiptHandle))
	return &awssqs.ChangeMessageVisibilityOutput{}, nil
}

func (f *filaFalsa) SendMessage(_ context.Context, in *awssqs.SendMessageInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
	f.enviadasDLQ = append(f.enviadasDLQ, aws.ToString(in.MessageBody))
	return &awssqs.SendMessageOutput{}, nil
}

func mensagemDeAposta(messageID, walletID, playerID, extID, valor string) types.Message {
	corpo := `{
		"messageId": "` + messageID + `",
		"type": "WagerTransactionRequested",
		"occurredAt": "2026-09-23T12:00:00Z",
		"data": {
			"providerId": "provider-a",
			"externalTransactionId": "` + extID + `",
			"idempotencyKey": "provider-a:` + extID + `",
			"playerId": "` + playerID + `",
			"walletId": "` + walletID + `",
			"roundId": "round-1",
			"gameId": "fortune-chimp",
			"kind": "BET",
			"money": {"amount": "` + valor + `", "currency": "BRL"}
		}
	}`
	return types.Message{
		MessageId:     aws.String(messageID),
		Body:          aws.String(corpo),
		ReceiptHandle: aws.String("recibo-" + messageID),
		Attributes: map[string]string{
			string(types.MessageSystemAttributeNameMessageGroupId): walletID,
			string(types.MessageSystemAttributeNameSenderId):       "000000000000",
		},
	}
}

// CENÁRIO 5 DO ENUNCIADO: interromper o consumidor DEPOIS do commit e ANTES
// da remoção da mensagem, e validar a reentrega.
//
// O que precisa acontecer: a primeira passagem move o dinheiro e commita; a
// remoção falha; a mensagem volta; a segunda passagem reconhece a duplicata
// pela inbox, NÃO move dinheiro de novo e aí sim remove a mensagem.
func TestConsumidorInterrompidoAntesDeRemoverAMensagem(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	msg := mensagemDeAposta("msg-crash-1", w.ID().String(), w.PlayerID().String(), "tx-crash-1", "40.00")

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	fila := &filaFalsa{mensagens: []types.Message{msg}, engolirDelete: true}
	cons := sqs.NewConsumer(fila, postgres.NewUnitOfWork(pool), ts,
		app.SystemClock{}, app.NoMetrics{}, log,
		sqs.ConsumerConfig{QueueURL: "entrada", DLQURL: "dlq"})

	// 1ª passagem: processa, commita, e a remoção falha (processo morreu).
	if err := cons.Once(ctx); err != nil {
		t.Fatalf("primeira passagem: %v", err)
	}
	depois, _ := ws.Get(ctx, w.ID())
	if depois.Balance().String() != "60.00" {
		t.Fatalf("saldo após a primeira passagem = %s, queria 60.00", depois.Balance())
	}
	if len(fila.deletadas) != 0 {
		t.Fatal("a mensagem não deveria ter sido removida: o processo morreu antes")
	}

	// 2ª passagem: o SQS reentrega a MESMA mensagem a um consumidor novo.
	fila2 := &filaFalsa{mensagens: []types.Message{msg}}
	cons2 := sqs.NewConsumer(fila2, postgres.NewUnitOfWork(pool), ts,
		app.SystemClock{}, app.NoMetrics{}, log,
		sqs.ConsumerConfig{QueueURL: "entrada", DLQURL: "dlq"})
	if err := cons2.Once(ctx); err != nil {
		t.Fatalf("reentrega: %v", err)
	}

	// O dinheiro NÃO se moveu de novo.
	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "60.00" {
		t.Errorf("saldo após a reentrega = %s, queria 60.00 — houve movimentação duplicada", final.Balance())
	}
	if final.Version() != depois.Version() {
		t.Errorf("versão mudou de %d para %d na reentrega", depois.Version(), final.Version())
	}

	// Um único lançamento de débito.
	var debitos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`,
		w.ID()).Scan(&debitos); err != nil {
		t.Fatal(err)
	}
	if debitos != 1 {
		t.Errorf("débitos = %d, queria 1", debitos)
	}

	// E agora sim a mensagem saiu da fila.
	if len(fila2.deletadas) != 1 {
		t.Errorf("mensagens removidas = %d, queria 1", len(fila2.deletadas))
	}

	// A reconciliação fecha.
	rec, err := ws.Reconcile(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Consistent || rec.Difference.String() != "0.00" {
		t.Errorf("reconciliação = %+v", rec)
	}
}

// O mesmo messageId com CONTEÚDO diferente é adulteração ou bug do produtor:
// permanente, vai para a DLQ e não é reprocessado.
func TestMesmoMessageIdComConteudoDiferenteVaiParaDLQ(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("100.00")})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := sqs.ConsumerConfig{QueueURL: "entrada", DLQURL: "dlq"}

	primeira := mensagemDeAposta("msg-dup", w.ID().String(), w.PlayerID().String(), "tx-dup", "10.00")
	f1 := &filaFalsa{mensagens: []types.Message{primeira}}
	if err := sqs.NewConsumer(f1, postgres.NewUnitOfWork(pool), ts, app.SystemClock{}, app.NoMetrics{}, log, cfg).Once(ctx); err != nil {
		t.Fatal(err)
	}

	// mesmo messageId, valor diferente
	adulterada := mensagemDeAposta("msg-dup", w.ID().String(), w.PlayerID().String(), "tx-dup", "90.00")
	f2 := &filaFalsa{mensagens: []types.Message{adulterada}}
	if err := sqs.NewConsumer(f2, postgres.NewUnitOfWork(pool), ts, app.SystemClock{}, app.NoMetrics{}, log, cfg).Once(ctx); err != nil {
		t.Fatal(err)
	}

	if len(f2.enviadasDLQ) != 1 {
		t.Errorf("mensagens enviadas à DLQ = %d, queria 1", len(f2.enviadasDLQ))
	}
	final, _ := ws.Get(ctx, w.ID())
	if final.Balance().String() != "90.00" {
		t.Errorf("saldo = %s, queria 90.00 — a adulterada foi aplicada", final.Balance())
	}
}

// Falha transitória do banco devolve a mensagem para a fila, em vez de
// descartá-la ou mandá-la à DLQ.
func TestFalhaTransitoriaDevolveAMensagem(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	_, ts := servicos(pool)
	ctx := context.Background()

	// carteira inexistente: o caso de uso devolve ErrNotFound, que não é
	// transitório, mas exercita o caminho de erro sem tocar em dinheiro
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	msg := mensagemDeAposta("msg-sem-carteira", uuid.NewString(), uuid.NewString(), "tx-sem", "10.00")
	fila := &filaFalsa{mensagens: []types.Message{msg}}
	cons := sqs.NewConsumer(fila, postgres.NewUnitOfWork(pool), ts, app.SystemClock{}, app.NoMetrics{}, log,
		sqs.ConsumerConfig{QueueURL: "entrada", DLQURL: "dlq"})

	_ = cons.Once(ctx)

	// não foi removida nem mandada à DLQ: volta para a fila
	if len(fila.deletadas) != 0 {
		t.Error("mensagem com carteira inexistente não deveria ser removida")
	}
	if len(fila.liberadas) != 1 {
		t.Errorf("liberações de visibilidade = %d, queria 1", len(fila.liberadas))
	}
	_ = time.Second
}
