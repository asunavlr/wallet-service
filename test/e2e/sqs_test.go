//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/kevinmatos/wallet-service/internal/adapter/sqs"
)

// prazoFila é o quanto esperamos por uma operação vinda da fila.
//
// Precisa ser MAIOR que o VisibilityTimeout (60s): depois que uma instância
// é derrubada, as mensagens que ela tinha em mãos só voltam a ficar visíveis
// quando esse prazo expira — e, numa fila FIFO, isso atrasa a entrega. Não é
// defeito, é a reentrega segura funcionando; o teste só precisa reconhecer
// que ela custa até um ciclo de visibilidade.
const prazoFila = 90 * time.Second

func endpointSQS() string {
	if v := os.Getenv("E2E_SQS_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:4566"
}

func clienteSQS(t *testing.T) *awssqs.Client {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	c, err := sqs.NewClient(context.Background(), sqs.ClientConfig{
		Region: "us-east-1", Endpoint: endpointSQS(),
		AccessKey: "test", SecretKey: "test",
	})
	if err != nil {
		t.Skipf("SQS indisponível: %v", err)
	}
	return c
}

func urlFila(t *testing.T, c *awssqs.Client, nome string) string {
	t.Helper()
	out, err := c.GetQueueUrl(context.Background(), &awssqs.GetQueueUrlInput{QueueName: aws.String(nome)})
	if err != nil {
		t.Skipf("fila %s indisponível: %v", nome, err)
	}
	return aws.ToString(out.QueueUrl)
}

func enviar(t *testing.T, c *awssqs.Client, fila, grupo, dedup string, corpo any) {
	t.Helper()
	b, _ := json.Marshal(corpo)
	_, err := c.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:               aws.String(fila),
		MessageBody:            aws.String(string(b)),
		MessageGroupId:         aws.String(grupo),
		MessageDeduplicationId: aws.String(dedup),
	})
	if err != nil {
		t.Fatalf("enviando mensagem: %v", err)
	}
}

func mensagem(messageID, walletID, player, extID, kind, valor string) map[string]any {
	return map[string]any{
		"messageId":  messageID,
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"providerId": "provider-a", "externalTransactionId": extID,
			"idempotencyKey": "provider-a:" + extID,
			"playerId":       player, "walletId": walletID,
			"roundId": "round-1", "gameId": "fortune-chimp",
			"kind": kind, "money": dinheiro(valor),
		},
	}
}

// Uma operação entrando pela fila move o saldo como a de HTTP.
func TestOperacaoPelaFila(t *testing.T) {
	c := clienteSQS(t)
	fila := urlFila(t, c, "wager-transactions.fifo")
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	ext := "sqs-" + sfx
	enviar(t, c, fila, walletID, "msg-"+ext, mensagem("msg-"+ext, walletID, player, ext, "BET", "40.00"))

	esperar(t, func() bool {
		r := chamar(t, base, http.MethodGet, "/wallets/"+walletID, tokA, nil, nil)
		s, _ := r.Corpo["balance"].(map[string]any)
		return s != nil && s["amount"] == "60.00"
	}, prazoFila, "saldo 60.00 após a aposta vinda da fila")

	rec := chamar(t, base, http.MethodPost, "/wallets/"+walletID+"/reconciliation", tokA, nil, nil)
	if rec.Corpo["consistent"] != true {
		t.Errorf("reconciliação = %s", rec.Bruto)
	}
}

// A MESMA operação chegando por HTTP e pela fila precisa ser deduplicada: as
// duas entradas compartilham a idempotência financeira.
func TestMesmaOperacaoPorHTTPEPorFila(t *testing.T) {
	c := clienteSQS(t)
	fila := urlFila(t, c, "wager-transactions.fifo")
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	ext := "cross-" + sfx

	// primeiro por HTTP
	r := apostar(t, base, tokA, walletID, player, ext, "25.00")
	if r.Status != http.StatusOK {
		t.Fatalf("HTTP = %d: %s", r.Status, r.Bruto)
	}

	// agora a MESMA operação pela fila
	enviar(t, c, fila, walletID, "msg-"+ext, mensagem("msg-"+ext, walletID, player, ext, "BET", "25.00"))

	// o saldo não pode se mover de novo
	time.Sleep(3 * time.Second)
	w := chamar(t, base, http.MethodGet, "/wallets/"+walletID, tokA, nil, nil)
	if s := w.Corpo["balance"].(map[string]any); s["amount"] != "75.00" {
		t.Errorf("saldo = %v, queria 75.00 — a operação foi aplicada duas vezes", s["amount"])
	}

	var lancamentos int
	ledger := chamar(t, base, http.MethodGet, "/wallets/"+walletID+"/ledger?limit=50", tokA, nil, nil)
	if itens, ok := ledger.Corpo["items"].([]any); ok {
		lancamentos = len(itens)
	}
	if lancamentos != 2 { // abertura + a aposta, uma vez só
		t.Errorf("lançamentos = %d, queria 2", lancamentos)
	}
}

// A mesma mensagem reentregue não reprocessa: é a inbox absorvendo.
func TestMensagemRepetidaNaoReprocessa(t *testing.T) {
	c := clienteSQS(t)
	fila := urlFila(t, c, "wager-transactions.fifo")
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	ext := "rep-" + sfx
	corpo := mensagem("msg-"+ext, walletID, player, ext, "BET", "10.00")

	// enviada três vezes, com MessageDeduplicationId distinto para furar a
	// janela do FIFO e exercitar a deduplicação da APLICAÇÃO
	for i := 0; i < 3; i++ {
		enviar(t, c, fila, walletID, fmt.Sprintf("dedup-%s-%d", ext, i), corpo)
	}

	esperar(t, func() bool {
		r := chamar(t, base, http.MethodGet, "/wallets/"+walletID, tokA, nil, nil)
		s, _ := r.Corpo["balance"].(map[string]any)
		return s != nil && s["amount"] == "90.00"
	}, prazoFila, "saldo 90.00")

	// e continua 90.00 depois de tudo assentar
	time.Sleep(3 * time.Second)
	w := chamar(t, base, http.MethodGet, "/wallets/"+walletID, tokA, nil, nil)
	if s := w.Corpo["balance"].(map[string]any); s["amount"] != "90.00" {
		t.Errorf("saldo = %v, queria 90.00 — houve movimentação duplicada", s["amount"])
	}
}

// OPENING pela fila é recusado e vai para a DLQ de imediato, sem ficar
// reciclando na fila de entrada até esgotar o maxReceiveCount.
func TestOpeningPelaFilaVaiParaDLQ(t *testing.T) {
	c := clienteSQS(t)
	fila := urlFila(t, c, "wager-transactions.fifo")
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	ext := "op-" + sfx
	enviar(t, c, fila, walletID, "msg-"+ext, mensagem("msg-"+ext, walletID, player, ext, "OPENING", "50.00"))

	// chega à DLQ sem esperar as cinco reentregas
	dlq := urlFila(t, c, "wager-transactions-dlq.fifo")
	achou := false
	limite := time.Now().Add(prazoFila)
	for time.Now().Before(limite) && !achou {
		out, err := c.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl: aws.String(dlq), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			if strings.Contains(aws.ToString(m.Body), ext) {
				achou = true
			}
			c.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl: aws.String(dlq), ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
	if !achou {
		t.Error("a mensagem com OPENING não chegou à DLQ")
	}

	w := chamar(t, base, http.MethodGet, "/wallets/"+walletID, tokA, nil, nil)
	if s := w.Corpo["balance"].(map[string]any); s["amount"] != "100.00" {
		t.Errorf("saldo = %v, queria 100.00 — OPENING externo foi aplicado", s["amount"])
	}
}

// Os eventos da outbox chegam à fila de saída depois do commit.
func TestEventosChegamNaFilaDeSaida(t *testing.T) {
	c := clienteSQS(t)
	saida := urlFila(t, c, "wager-events.fifo")
	base := instancias()[0]

	// drena o que estiver pendente de testes anteriores
	for i := 0; i < 5; i++ {
		out, err := c.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl: aws.String(saida), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		})
		if err != nil || len(out.Messages) == 0 {
			break
		}
		for _, m := range out.Messages {
			c.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl: aws.String(saida), ReceiptHandle: m.ReceiptHandle,
			})
		}
	}

	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")
	if r := apostar(t, base, tokA, walletID, player, "ev-"+sfx, "10.00"); r.Status != http.StatusOK {
		t.Fatalf("aposta = %d: %s", r.Status, r.Bruto)
	}

	tipos := map[string]int{}
	limite := time.Now().Add(prazoFila)
	for time.Now().Before(limite) && len(tipos) < 2 {
		out, err := c.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl: aws.String(saida), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			var env struct {
				EventID   string `json:"eventId"`
				EventType string `json:"eventType"`
				Version   int    `json:"version"`
			}
			if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &env); err == nil {
				tipos[env.EventType]++
				if env.EventID == "" || env.Version == 0 {
					t.Errorf("envelope incompleto: %s", aws.ToString(m.Body))
				}
			}
			c.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl: aws.String(saida), ReceiptHandle: m.ReceiptHandle,
			})
		}
	}

	if tipos["WagerTransactionProcessed"] == 0 {
		t.Error("nenhum WagerTransactionProcessed chegou à fila de saída")
	}
	if tipos["WalletBalanceChanged"] == 0 {
		t.Error("nenhum WalletBalanceChanged chegou à fila de saída")
	}
}
