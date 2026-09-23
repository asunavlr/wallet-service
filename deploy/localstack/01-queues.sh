#!/bin/sh
# Provisiona as filas no LocalStack.
#
# O serviço também as cria na inicialização (CreateQueue é idempotente), mas
# tê-las prontas antes evita que a primeira instância corra com as outras
# duas para criá-las.
set -e

fila() {
  awslocal sqs create-queue --queue-name "$1" \
    --attributes "$2" >/dev/null
  echo "fila $1 pronta"
}

ATRIBUTOS_FIFO='{"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"60"}'

fila wager-transactions-dlq.fifo "$ATRIBUTOS_FIFO"

DLQ_ARN=$(awslocal sqs get-queue-attributes \
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo \
  --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)

# maxReceiveCount 5: uma mensagem que falhou cinco vezes não vai passar na
# sexta, e adiar a DLQ só atrasa o diagnóstico.
fila wager-transactions.fifo \
  "{\"FifoQueue\":\"true\",\"ContentBasedDeduplication\":\"false\",\"VisibilityTimeout\":\"60\",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$DLQ_ARN\\\",\\\"maxReceiveCount\\\":\\\"5\\\"}\"}"

fila wager-events.fifo "$ATRIBUTOS_FIFO"
