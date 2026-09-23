package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// ClientConfig configura o cliente SQS.
type ClientConfig struct {
	Region string
	// Endpoint aponta para o LocalStack em desenvolvimento. Vazio em
	// produção, onde o SDK resolve o endereço real da AWS.
	Endpoint string
	// Credenciais estáticas só são usadas com endpoint local; em produção o
	// SDK usa a cadeia padrão (perfil, variáveis, IAM role).
	AccessKey string
	SecretKey string
}

// NewClient monta o cliente.
func NewClient(ctx context.Context, c ClientConfig) (*sqs.Client, error) {
	opcoes := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(c.Region)}
	if c.AccessKey != "" {
		opcoes = append(opcoes, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opcoes...)
	if err != nil {
		return nil, fmt.Errorf("configuração da AWS: %w", err)
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	}), nil
}

// Filas são as URLs usadas pelo serviço.
type Filas struct {
	Entrada string
	DLQ     string
	Saida   string
}

// Provisionar cria as filas e o redrive, para que o ambiente local suba com
// `docker compose up` sem passo manual.
//
// maxReceiveCount é deliberadamente baixo: uma mensagem que falhou 5 vezes
// não vai passar na sexta, e demorar para mandá-la à DLQ só atrasa o
// diagnóstico.
func Provisionar(ctx context.Context, api *sqs.Client, entrada, dlq, saida string, maxReceive int) (Filas, error) {
	if maxReceive <= 0 {
		maxReceive = 5
	}
	fifo := map[string]string{
		string(types.QueueAttributeNameFifoQueue):                 "true",
		string(types.QueueAttributeNameContentBasedDeduplication): "false",
		string(types.QueueAttributeNameVisibilityTimeout):         "60",
	}

	criar := func(nome string, attrs map[string]string) (string, string, error) {
		out, err := api.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName: aws.String(nome), Attributes: attrs,
		})
		if err != nil {
			return "", "", fmt.Errorf("criando fila %s: %w", nome, err)
		}
		url := aws.ToString(out.QueueUrl)
		arn, err := api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       out.QueueUrl,
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			return "", "", err
		}
		return url, arn.Attributes[string(types.QueueAttributeNameQueueArn)], nil
	}

	urlDLQ, arnDLQ, err := criar(dlq, fifo)
	if err != nil {
		return Filas{}, err
	}

	comRedrive := map[string]string{}
	for k, v := range fifo {
		comRedrive[k] = v
	}
	comRedrive[string(types.QueueAttributeNameRedrivePolicy)] =
		fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":"%d"}`, arnDLQ, maxReceive)

	urlEntrada, _, err := criar(entrada, comRedrive)
	if err != nil {
		return Filas{}, err
	}
	urlSaida, _, err := criar(saida, fifo)
	if err != nil {
		return Filas{}, err
	}
	return Filas{Entrada: urlEntrada, DLQ: urlDLQ, Saida: urlSaida}, nil
}
