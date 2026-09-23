package sqs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/adapter/sqs"
	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/contract"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

func envelopeValido() sqs.Envelope {
	return sqs.Envelope{
		MessageID:  "msg-1",
		Type:       sqs.TipoOperacao,
		OccurredAt: "2026-09-23T12:00:00Z",
		Data: contract.Operation{
			ProviderID: "provider-a", ExternalID: "tx-1",
			IdempotencyKey: "provider-a:tx-1",
			PlayerID:       "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
			WalletID:       "0192f291-27dd-7d3f-8071-5f8685deef37",
			RoundID:        "round-1", GameID: "fortune-chimp", Kind: "BET",
			Money: &contract.Amount{Amount: "25.00", Currency: "BRL"},
		},
	}
}

func TestEnvelopeValido(t *testing.T) {
	p, err := envelopeValido().Validate()
	if err != nil {
		t.Fatalf("envelope válido recusado: %v", err)
	}
	if p.Kind != wager.Bet || p.Money.String() != "25.00" {
		t.Errorf("= %v %s", p.Kind, p.Money)
	}
}

// Mensagem malformada é PERMANENTE: reentregar dá o mesmo resultado, e o
// lugar dela é a DLQ.
func TestEnvelopeRecusado(t *testing.T) {
	casos := map[string]func(*sqs.Envelope){
		"sem messageId":         func(e *sqs.Envelope) { e.MessageID = "" },
		"tipo desconhecido":     func(e *sqs.Envelope) { e.Type = "OutraCoisa" },
		"tipo vazio":            func(e *sqs.Envelope) { e.Type = "" },
		"occurredAt malformado": func(e *sqs.Envelope) { e.OccurredAt = "ontem" },
		"sem idempotencyKey":    func(e *sqs.Envelope) { e.Data.IdempotencyKey = "" },
		"sem providerId":        func(e *sqs.Envelope) { e.Data.ProviderID = "" },
		"sem roundId":           func(e *sqs.Envelope) { e.Data.RoundID = "" },
		"sem gameId":            func(e *sqs.Envelope) { e.Data.GameID = "" },
		"playerId não é UUID":   func(e *sqs.Envelope) { e.Data.PlayerID = "eu" },
		"walletId não é UUID":   func(e *sqs.Envelope) { e.Data.WalletID = "minha" },
		"sem money":             func(e *sqs.Envelope) { e.Data.Money = nil },
		"money com escala ruim": func(e *sqs.Envelope) { e.Data.Money.Amount = "25.001" },
		"money negativo":        func(e *sqs.Envelope) { e.Data.Money.Amount = "-25.00" },
		"moeda inválida":        func(e *sqs.Envelope) { e.Data.Money.Currency = "XXX" },
		"OPENING pela fila":     func(e *sqs.Envelope) { e.Data.Kind = "OPENING" },
		"tipo de operação ruim": func(e *sqs.Envelope) { e.Data.Kind = "TRANSFER" },
	}
	for nome, muta := range casos {
		t.Run(nome, func(t *testing.T) {
			e := envelopeValido()
			muta(&e)
			if _, err := e.Validate(); err == nil {
				t.Error("deveria ser recusado")
			}
		})
	}
}

// O hash da mensagem responde uma pergunta diferente do fingerprint de
// negócio: o MESMO messageId voltou com outro conteúdo? Isso é adulteração
// ou bug do produtor, e é permanente.
func TestHashDaMensagem(t *testing.T) {
	e := envelopeValido()
	base := sqs.HashDaMensagem(e, "fingerprint-x")

	if sqs.HashDaMensagem(e, "fingerprint-x") != base {
		t.Error("o mesmo conteúdo precisa dar o mesmo hash")
	}
	if sqs.HashDaMensagem(e, "fingerprint-y") == base {
		t.Error("mudar o negócio precisa mudar o hash")
	}

	outraChave := envelopeValido()
	outraChave.Data.IdempotencyKey = "provider-a:outra"
	if sqs.HashDaMensagem(outraChave, "fingerprint-x") == base {
		t.Error("mudar a chave precisa mudar o hash da mensagem")
	}

	// o messageId NÃO entra: ele é a identidade, não o conteúdo
	outroID := envelopeValido()
	outroID.MessageID = "msg-999"
	if sqs.HashDaMensagem(outroID, "fingerprint-x") != base {
		t.Error("o messageId não deveria entrar no hash do conteúdo")
	}
}

// filaFalsa registra o que foi enviado.
type filaFalsa struct {
	enviadas []*awssqs.SendMessageInput
	erro     error
}

func (f *filaFalsa) SendMessage(_ context.Context, in *awssqs.SendMessageInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
	if f.erro != nil {
		return nil, f.erro
	}
	f.enviadas = append(f.enviadas, in)
	return &awssqs.SendMessageOutput{}, nil
}

// Numa fila FIFO o grupo é a carteira, e a deduplicação é o eventId — que é
// estável entre republicações, e é o que faz o SQS descartar a entrega
// repetida dentro da janela dele.
func TestPublisherFIFO(t *testing.T) {
	f := &filaFalsa{}
	p := sqs.NewPublisher(f, "https://fila/eventos.fifo", true)

	carteira := uuid.New()
	evento := app.OutboxEvent{
		EventID: uuid.New(), EventType: "WalletBalanceChanged",
		AggregateID: carteira, PartitionKey: carteira.String(),
		Payload: []byte(`{"eventId":"x"}`),
	}
	if err := p.Publish(context.Background(), evento); err != nil {
		t.Fatal(err)
	}
	if len(f.enviadas) != 1 {
		t.Fatalf("enviadas = %d", len(f.enviadas))
	}
	env := f.enviadas[0]
	if aws.ToString(env.MessageGroupId) != carteira.String() {
		t.Errorf("MessageGroupId = %q, queria a carteira", aws.ToString(env.MessageGroupId))
	}
	if aws.ToString(env.MessageDeduplicationId) != evento.EventID.String() {
		t.Errorf("MessageDeduplicationId = %q, queria o eventId", aws.ToString(env.MessageDeduplicationId))
	}
	if aws.ToString(env.MessageBody) != `{"eventId":"x"}` {
		t.Errorf("corpo alterado: %q", aws.ToString(env.MessageBody))
	}
}

// Numa fila comum, grupo e deduplicação não se aplicam — mandá-los seria
// erro da API.
func TestPublisherNaoFIFO(t *testing.T) {
	f := &filaFalsa{}
	p := sqs.NewPublisher(f, "https://fila/eventos", false)
	if err := p.Publish(context.Background(), app.OutboxEvent{
		EventID: uuid.New(), PartitionKey: "x", Payload: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if f.enviadas[0].MessageGroupId != nil || f.enviadas[0].MessageDeduplicationId != nil {
		t.Error("fila comum não leva grupo nem deduplicação")
	}
}

// Falha ao publicar precisa subir com o eventId: sem ele, o log não diz qual
// evento ficou para trás.
func TestPublisherPropagaFalha(t *testing.T) {
	id := uuid.New()
	f := &filaFalsa{erro: errors.New("fila indisponível")}
	err := sqs.NewPublisher(f, "https://fila", true).Publish(context.Background(),
		app.OutboxEvent{EventID: id, PartitionKey: "x", Payload: []byte("{}")})
	if err == nil {
		t.Fatal("deveria propagar a falha")
	}
	if !contemTexto(err.Error(), id.String()) {
		t.Errorf("o erro deveria citar o eventId: %v", err)
	}
}

var _ = money.MustParse

func contemTexto(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
