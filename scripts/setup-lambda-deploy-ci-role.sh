#!/bin/bash
# Create, update or delete the role GitHub Actions assumes to deploy code-only
# changes to the spore.host-operated Lambdas after a release (#1445).
#
# Usage: ./setup-lambda-deploy-ci-role.sh [--delete]
#
# What it may do, and nothing else: replace a named function's code, read its
# configuration, and restamp its spawn:* tags. It cannot touch IAM, a function's
# configuration or environment, an event-source mapping or a CloudFormation
# stack; those stay manual deploys, listed as such by scripts/deploychecklist
# (#1444).
#
# Who may assume it: a job in spore-host/spawn running in the `production`
# GitHub environment, through the account's GitHub OIDC provider. The trust
# keys on the default subject `repo:spore-host/spawn:environment:production`,
# so it is only as strong as that environment's protection: a required reviewer
# (the owner), no admin bypass, and deployments from `v*` tags only. Set those
# up BEFORE running this script: a workflow naming an environment that does not
# exist creates it unprotected.
#
# FUNCTIONS is the operated set scripts/deploychecklist discovers;
# TestCIDeployRoleCoversTheOperatedFunctions in scripts/deploychecklist fails
# when the two differ. --delete is this role's named deleter.
set -euo pipefail

ROLE_NAME="GitHubActions-spawn-lambda-deploy"
POLICY_NAME="DeployOperatedLambdaCode"
REGION="${SPAWN_LAMBDA_REGION:-us-east-1}"
FUNCTIONS=(
  scheduler-handler
  spawn-cost-history-collector
  spawn-dashboard-api
  spawn-dashboard-websocket
  spawn-dashboard-websocket-processor
  spawn-dns-updater
  spawn-pipeline-orchestrator
  spawn-sweep-orchestrator
  spawn-ttl-reaper-production
)

EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

if [ "${1:-}" = "--delete" ]; then
  if ! aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
    echo "$ROLE_NAME does not exist; nothing to delete."
    exit 0
  fi
  for p in $(aws iam list-role-policies --role-name "$ROLE_NAME" --query PolicyNames --output text); do
    aws iam delete-role-policy --role-name "$ROLE_NAME" --policy-name "$p"
    echo "deleted inline policy $p"
  done
  aws iam delete-role --role-name "$ROLE_NAME"
  echo "deleted role $ROLE_NAME"
  exit 0
elif [ $# -gt 0 ]; then
  echo "Usage: $0 [--delete]" >&2
  exit 1
fi

PROVIDER="arn:aws:iam::${ACCOUNT_ID}:oidc-provider/token.actions.githubusercontent.com"
if ! aws iam get-open-id-connect-provider --open-id-connect-provider-arn "$PROVIDER" >/dev/null 2>&1; then
  echo "ERROR: no GitHub OIDC provider $PROVIDER; the release role uses it, so this account should have one." >&2
  exit 1
fi

TRUST_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "${PROVIDER}"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
        "token.actions.githubusercontent.com:sub": "repo:spore-host/spawn:environment:production"
      }
    }
  }]
}
EOF
)

ARNS=""
for f in "${FUNCTIONS[@]}"; do
  ARNS="${ARNS:+$ARNS, }\"arn:aws:lambda:${REGION}:${ACCOUNT_ID}:function:${f}\""
done

POLICY_DOCUMENT=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "UpdateCode",
      "Effect": "Allow",
      "Action": ["lambda:UpdateFunctionCode", "lambda:GetFunction", "lambda:GetFunctionConfiguration", "lambda:ListTags"],
      "Resource": [${ARNS}]
    },
    {
      "Sid": "StampVersion",
      "Effect": "Allow",
      "Action": ["lambda:TagResource"],
      "Resource": [${ARNS}],
      "Condition": {
        "ForAllValues:StringEquals": {
          "aws:TagKeys": ["spawn:version", "spawn:managed", "spawn:component", "spawn:created-by"]
        }
      }
    }
  ]
}
EOF
)

if aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
  aws iam update-assume-role-policy --role-name "$ROLE_NAME" --policy-document "$TRUST_POLICY"
  echo "updated trust policy of $ROLE_NAME"
else
  aws iam create-role \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document "$TRUST_POLICY" \
    --max-session-duration 3600 \
    --description "GitHub Actions (spore-host/spawn, production environment): code-only deploys of the operated Lambdas (#1445)" \
    --tags Key=spawn:managed,Value=true Key=spawn:purpose,Value=lambda-deploy-ci Key=spawn:created-by,Value=scripts/setup-lambda-deploy-ci-role.sh \
    >/dev/null
  echo "created role $ROLE_NAME"
fi

aws iam put-role-policy --role-name "$ROLE_NAME" --policy-name "$POLICY_NAME" --policy-document "$POLICY_DOCUMENT"
echo "put inline policy $POLICY_NAME (${#FUNCTIONS[@]} functions)"
echo "Role ARN: arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"
