package sqs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/contract"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

// NomeConsumidor identifica esta aplicação na inbox. Fixo de propósito: se
// variasse por instância, a deduplicação deixaria de funcionar entre elas.
const NomeConsumidor = "wallet-service"

// API é o recorte do cliente SQS que o consumidor usa.
type API interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, opts ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

// Consumer lê operações da fila.
type Consumer struct {
	api     API
	uow     app.UnitOfWork
	wagers  *app.WagerService
	clock   app.Clock
	metrics app.Metrics
	log     *slog.Logger
	cfg     ConsumerConfig
}

// ConsumerConfig governa o consumo.
type ConsumerConfig struct {
	QueueURL string
	// MaxMessages por ReceiveMessage. O SQS aceita até 10.
	MaxMessages int32
	// WaitTime habilita long polling: sem ele o consumidor gira em falso.
	WaitTime int32
	// VisibilityTimeout precisa ser maior que o p99 de processamento, senão a
	// mensagem reaparece enquanto ainda está sendo tratada.
	VisibilityTimeout int32
	// SenderProviders mapeia o remetente IAM ao provedor que ele pode
	// declarar. É o equivalente, na fila, ao providerId derivado do token.
	SenderProviders map[string]string
}

func NewConsumer(api API, uow app.UnitOfWork, w *app.WagerService, c app.Clock, m app.Metrics, log *slog.Logger, cfg ConsumerConfig) *Consumer {
	if cfg.MaxMessages <= 0 || cfg.MaxMessages > 10 {
		cfg.MaxMessages = 10
	}
	if cfg.WaitTime <= 0 {
		cfg.WaitTime = 20
	}
	if cfg.VisibilityTimeout <= 0 {
		cfg.VisibilityTimeout = 60
	}
	return &Consumer{api: api, uow: uow, wagers: w, clock: c, metrics: m, log: log, cfg: cfg}
}

// Run consome até o contexto ser cancelado.
//
// Em SIGTERM o contexto é cancelado: o laço para de BUSCAR trabalho novo e o
// que já está em processamento termina ou tem a visibilidade liberada para
// reentrega segura — que é exatamente o que o enunciado pede.
func (c *Consumer) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			c.log.Info("consumidor sqs encerrado")
			return
		}
		if err := c.Once(ctx); err != nil && ctx.Err() == nil {
			c.log.Error("ciclo do consumidor falhou", slog.String("erro", err.Error()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// Once busca e trata um lote.
func (c *Consumer) Once(ctx context.Context) error {
	saida, err := c.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(c.cfg.QueueURL),
		MaxNumberOfMessages: c.cfg.MaxMessages,
		WaitTimeSeconds:     c.cfg.WaitTime,
		VisibilityTimeout:   c.cfg.VisibilityTimeout,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameSenderId,
			types.MessageSystemAttributeNameMessageGroupId,
		},
	})
	if err != nil {
		return err
	}

	// Grupos já adiados neste lote. Um ReceiveMessage pode trazer várias
	// mensagens do mesmo MessageGroupId, e elas vêm em ordem: se a primeira
	// precisa voltar para a fila, as seguintes do mesmo grupo não podem ser
	// processadas antes dela, senão a ordem por carteira se perde.
	adiados := map[string]bool{}

	for _, m := range saida.Messages {
		if ctx.Err() != nil {
			c.liberar(context.WithoutCancel(ctx), m)
			continue
		}
		grupo := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
		if grupo != "" && adiados[grupo] {
			c.liberar(ctx, m)
			continue
		}
		if err := c.tratar(ctx, m); err != nil {
			if grupo != "" {
				adiados[grupo] = true
			}
			c.liberar(ctx, m)
		}
	}
	return nil
}

// tratar processa uma mensagem. Devolver erro significa "devolva para a
// fila"; nil significa que a mensagem foi removida.
func (c *Consumer) tratar(ctx context.Context, m types.Message) error {
	corpo := aws.ToString(m.Body)

	var env Envelope
	if err := contract.DecodeBytes([]byte(corpo), &env); err != nil {
		// Malformada: nada a tentar de novo. Deixar chegar à DLQ pelo
		// maxReceiveCount é o caminho previsto pelo enunciado.
		c.log.Error("mensagem malformada", slog.String("erro", err.Error()))
		c.metrics.DeadLetter("sqs")
		return nil // remove: reentregar não muda nada
	}
	dados, err := env.Validate()
	if err != nil {
		c.log.Error("envelope inválido",
			slog.String("messageId", env.MessageID), slog.String("erro", err.Error()))
		c.metrics.DeadLetter("sqs")
		return nil
	}

	// Vínculo remetente → provedor: o equivalente, na fila, ao providerId
	// derivado do token no HTTP. Sem isso, qualquer produtor com acesso à
	// fila poderia operar em nome de qualquer provedor.
	if !c.remetenteAutorizado(m, env.Data.ProviderID) {
		c.log.Error("remetente não autorizado para o provedor",
			slog.String("providerId", env.Data.ProviderID))
		c.metrics.DeadLetter("sqs")
		return nil
	}

	fingerprint := wager.Fingerprint(wager.FingerprintInput{
		ProviderID: env.Data.ProviderID, ExternalID: env.Data.ExternalID,
		PlayerID: dados.PlayerID.String(), WalletID: dados.WalletID.String(),
		RoundID: env.Data.RoundID, GameID: env.Data.GameID,
		Kind: dados.Kind, Amount: dados.Money, ReferenceExtID: env.Data.ReferenceExtID,
	})
	hashMensagem := HashDaMensagem(env, fingerprint)

	// A inbox e o processamento compartilham UMA transação SQL, junto com as
	// mudanças de domínio, o ledger e os eventos. É isso que faz a reentrega
	// depois de uma queda entre o commit e o DeleteMessage ser absorvida.
	var duplicada bool
	var resultado app.Result
	err = c.uow.Do(ctx, func(ctx context.Context, r *app.Repos) error {
		agora := c.clock.Now()
		nova, err := r.Inbox.Register(ctx, NomeConsumidor, env.MessageID, hashMensagem, agora)
		if err != nil {
			return err
		}
		if !nova {
			// Já tratada. Confere se o conteúdo bate: o mesmo messageId com
			// outro conteúdo é adulteração ou bug do produtor, e permanente.
			anterior, err := r.Inbox.Hash(ctx, NomeConsumidor, env.MessageID)
			if err != nil {
				return err
			}
			if anterior != hashMensagem {
				return errConteudoDivergente
			}
			duplicada = true
			return nil
		}

		res, err := c.wagers.SubmitInTx(ctx, r, app.Submit{
			ProviderID: env.Data.ProviderID, ExternalID: env.Data.ExternalID,
			IdempotencyKey: env.Data.IdempotencyKey,
			PlayerID:       dados.PlayerID, WalletID: dados.WalletID,
			RoundID: env.Data.RoundID, GameID: env.Data.GameID,
			Kind: dados.Kind, Money: dados.Money, ReferenceExtID: env.Data.ReferenceExtID,
			CorrelationID: env.MessageID, Source: "sqs",
		})
		if err != nil {
			return err
		}
		resultado = res
		return r.Inbox.Complete(ctx, NomeConsumidor, env.MessageID, &res.TransactionID, agora)
	})

	switch {
	case errors.Is(err, errConteudoDivergente):
		c.log.Error("mesmo messageId com conteúdo diferente",
			slog.String("messageId", env.MessageID))
		c.metrics.DeadLetter("sqs")
		return nil

	case errors.Is(err, app.ErrIdempotencyConflict),
		errors.Is(err, app.ErrDuplicateExternalTransaction),
		errors.Is(err, app.ErrInvalidInput),
		errors.Is(err, contract.ErrDecode):
		// Permanentes: reentregar dá o mesmo resultado.
		c.log.Warn("mensagem recusada em definitivo",
			slog.String("messageId", env.MessageID), slog.String("erro", err.Error()))
		c.metrics.DeadLetter("sqs")
		return nil

	case err != nil && app.Transient(err):
		c.metrics.Retry("sqs")
		return err // volta para a fila

	case err != nil:
		c.log.Error("falha ao tratar mensagem",
			slog.String("messageId", env.MessageID), slog.String("erro", err.Error()))
		return err
	}

	if duplicada {
		c.metrics.Duplicate("sqs")
	} else {
		c.log.Info("operação processada",
			slog.String("messageId", env.MessageID),
			slog.String("transactionId", resultado.TransactionID.String()),
			slog.String("walletId", dados.WalletID.String()),
			slog.String("providerId", env.Data.ProviderID),
			slog.String("status", string(resultado.Status)))
	}

	// DeleteMessage só DEPOIS do commit. Morrer aqui no meio é seguro: a
	// mensagem volta e a inbox reconhece a duplicata.
	_, delErr := c.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: m.ReceiptHandle,
	})
	if delErr != nil {
		c.log.Warn("mensagem tratada mas não removida da fila",
			slog.String("messageId", env.MessageID), slog.String("erro", delErr.Error()))
	}
	return nil
}

var errConteudoDivergente = errors.New("sqs: mesmo messageId com conteúdo diferente")

// remetenteAutorizado confere o mapa remetente → provedor.
func (c *Consumer) remetenteAutorizado(m types.Message, providerID string) bool {
	if len(c.cfg.SenderProviders) == 0 {
		return true // sem política configurada, a checagem fica a cargo do broker
	}
	remetente := m.Attributes[string(types.MessageSystemAttributeNameSenderId)]
	permitido, ok := c.cfg.SenderProviders[remetente]
	return ok && (permitido == "*" || permitido == providerID)
}

// liberar devolve a mensagem para a fila imediatamente, em vez de esperar o
// visibility timeout inteiro.
func (c *Consumer) liberar(ctx context.Context, m types.Message) {
	_, err := c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 0,
	})
	if err != nil {
		c.log.Debug("não foi possível liberar a visibilidade", slog.String("erro", err.Error()))
	}
}

var _ = uuid.Nil
