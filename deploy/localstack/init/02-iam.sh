#!/usr/bin/env bash
# Runs after 01-sqs.sh (mounted in /etc/localstack/init/ready.d). Creates one IAM user per
# component, each with the least-privilege policy of deploy/aws/iam (mounted in
# /etc/wagering/iam): the consumer reads the input queue and writes the dead-letter queue,
# the publisher writes the events queue.
#
# LocalStack's free edition does NOT enforce IAM (verified: a user with no policy can
# still use SQS), so locally this provisions and documents the policies; it does not
# stop a request. On AWS the same documents are attached to the role of each component.
set -euo pipefail

REGION="${AWS_DEFAULT_REGION:-us-east-1}"
POLICIES="${WAGERING_IAM_DIR:-/etc/wagering/iam}"

iam() { awslocal --region "$REGION" iam "$@"; }

for component in consumer publisher; do
  user="wagering-${component}"
  iam create-user --user-name "$user" >/dev/null
  arn=$(iam create-policy --policy-name "${user}-sqs" \
    --policy-document "file://${POLICIES}/${component}-policy.json" --query Policy.Arn --output text)
  iam attach-user-policy --user-name "$user" --policy-arn "$arn"
done

echo "IAM users and policies ready"
