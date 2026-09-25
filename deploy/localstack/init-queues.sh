#!/bin/sh
# Cria as filas SQS na subida do LocalStack (hook ready.d). Idempotente.
set -eu

# DLQ das operações de provedores: falhas permanentes (enviadas pelo
# consumidor) e mensagens que excederam maxReceiveCount (redrive).
awslocal sqs create-queue --queue-name wager-operations-dlq.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=false,MessageRetentionPeriod=1209600

DLQ_ARN=$(awslocal sqs get-queue-attributes \
  --queue-url "$(awslocal sqs get-queue-url --queue-name wager-operations-dlq.fifo --query QueueUrl --output text)" \
  --attribute-names QueueArn --query Attributes.QueueArn --output text)

# Operações de provedores. MessageGroupId = carteira (ordem por carteira).
awslocal sqs create-queue --queue-name wager-operations.fifo --attributes "{
  \"FifoQueue\": \"true\",
  \"ContentBasedDeduplication\": \"false\",
  \"VisibilityTimeout\": \"30\",
  \"RedrivePolicy\": \"{\\\"deadLetterTargetArn\\\":\\\"$DLQ_ARN\\\",\\\"maxReceiveCount\\\":\\\"5\\\"}\"
}"

# Eventos de integração publicados pela outbox.
awslocal sqs create-queue --queue-name wallet-events.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=false

echo "filas SQS criadas"
