#!/usr/bin/env bash
# Runs when LocalStack is ready (mounted in /etc/localstack/init/ready.d).
# Creates the FIFO queues, the dead-letter queue and the redrive policy.
set -euo pipefail

REGION="${AWS_DEFAULT_REGION:-us-east-1}"
MAX_RECEIVE_COUNT=5
VISIBILITY_TIMEOUT=30

sqs() { awslocal --region "$REGION" sqs "$@"; }

sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=false,MessageRetentionPeriod=1209600

DLQ_URL=$(sqs get-queue-url --queue-name wager-transactions-dlq.fifo --query QueueUrl --output text)
DLQ_ARN=$(sqs get-queue-attributes --queue-url "$DLQ_URL" \
  --attribute-names QueueArn --query Attributes.QueueArn --output text)

# RedrivePolicy is a JSON document encoded as a string attribute.
ATTRIBUTES_FILE=$(mktemp)
trap 'rm -f "$ATTRIBUTES_FILE"' EXIT
cat > "$ATTRIBUTES_FILE" <<EOF
{
  "FifoQueue": "true",
  "ContentBasedDeduplication": "false",
  "VisibilityTimeout": "${VISIBILITY_TIMEOUT}",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${DLQ_ARN}\",\"maxReceiveCount\":\"${MAX_RECEIVE_COUNT}\"}"
}
EOF

sqs create-queue --queue-name wager-transactions.fifo --attributes "file://${ATTRIBUTES_FILE}"

# Outgoing integration events published by the outbox worker.
sqs create-queue \
  --queue-name wager-events.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=false

echo "SQS queues ready"
