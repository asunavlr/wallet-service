package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/kevinmatos/wallet-service/internal/app"
)

// PublisherAPI é o recorte usado pelo publicador.
type PublisherAPI interface {
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// Publisher entrega os eventos da outbox à fila de saída.
type Publisher struct {
	api      PublisherAPI
	queueURL string
	fifo     bool
}

func NewPublisher(api PublisherAPI, queueURL string, fifo bool) *Publisher {
	return &Publisher{api: api, queueURL: queueURL, fifo: fifo}
}

// Publish envia um evento.
//
// Numa fila FIFO:
//   - MessageGroupId é a carteira, porque ordem por carteira é a única que
//     importa ao consumidor dos eventos, e agregados diferentes seguem em
//     paralelo;
//   - MessageDeduplicationId é o eventId, que é estável entre republicações.
//     Isso faz o SQS descartar a entrega repetida dentro da janela de 5
//     minutos — otimização, não garantia: quem garante é o eventId no
//     consumidor.
func (p *Publisher) Publish(ctx context.Context, e app.OutboxEvent) error {
	in := &sqs.SendMessageInput{
		QueueUrl:    aws.String(p.queueURL),
		MessageBody: aws.String(string(e.Payload)),
	}
	if p.fifo {
		in.MessageGroupId = aws.String(e.PartitionKey)
		in.MessageDeduplicationId = aws.String(e.EventID.String())
	}
	if _, err := p.api.SendMessage(ctx, in); err != nil {
		return fmt.Errorf("publicando evento %s: %w", e.EventID, err)
	}
	return nil
}
