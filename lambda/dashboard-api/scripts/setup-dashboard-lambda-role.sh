#!/bin/bash
set -e

# Setup IAM Role for Dashboard Lambda
# Usage: ./setup-dashboard-lambda-role.sh [aws-profile]

PROFILE=${1:-default}
ROLE_NAME="SpawnDashboardLambdaRole"
POLICY_NAME="SpawnDashboardLambdaPolicy"

# Assert the target account (#799 follow-up). The deploy-script gates originally
# discovered only `aws lambda`/`aws cloudformation` verbs, so the IAM-mutating
# scripts — the class that creates cross-account trust — were the one class
# outside the account assertion. Two of them had committed a grant into an
# account the project forbids. Override only to deploy a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Setting up IAM Role for Dashboard Lambda"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile: $PROFILE"
echo "Role:    $ROLE_NAME"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Get AWS account ID
ACCOUNT_ID=$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)
echo "Account: $ACCOUNT_ID"
echo ""

# Create trust policy
echo "→ Creating trust policy..."
cat > /tmp/trust-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "lambda.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF

# Create IAM role
echo "→ Creating IAM role..."
ROLE_ARN=$(aws iam create-role \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document file:///tmp/trust-policy.json \
    --description "Execution role of the spawn dashboard Lambdas (dashboard-api, websocket, websocket-processor, cost-history-collector)" \
    --tags Key=spawn:managed,Value=true Key=spawn:purpose,Value=dashboard-api \
    --query 'Role.Arn' \
    --output text 2>/dev/null || aws iam get-role --profile "$PROFILE" --role-name "$ROLE_NAME" --query 'Role.Arn' --output text)

echo -e "  ${GREEN}✓${NC} Role created: $ROLE_ARN"

# Attach basic Lambda execution policy
echo "→ Attaching Lambda basic execution policy..."
aws iam attach-role-policy \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --policy-arn "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"

echo -e "  ${GREEN}✓${NC} Basic execution policy attached"

# The role's own policy, as one inline document written with put-role-policy,
# so a re-run converges the role on exactly this (#1344). It used to be a
# customer-managed policy made with create-policy, which a re-run never updated,
# and it had fallen far behind the code: the four Lambdas on this role also
# write five tables, read a Slack secret, resolve strata lockfiles in S3, read
# two DynamoDB streams and post to WebSocket connections. Each statement below
# is what the IAM coverage gate (internal/iamcoverage) discovers the code
# calling, scoped to the tables, indexes, bucket prefixes and API it calls them
# on. Table names follow the same env overrides the Lambdas read.
REGION="us-east-1"
USER_ACCOUNTS_TABLE="${SPAWN_USER_ACCOUNTS_TABLE:-spawn-user-accounts}"
WS_TICKETS_TABLE="${SPAWN_WS_TICKETS_TABLE:-spawn-ws-tickets}"
WATCHES_TABLE="${LAGOTTO_WATCHES_TABLE:-lagotto-watches}"
HISTORY_TABLE="${LAGOTTO_HISTORY_TABLE:-lagotto-match-history}"
WORKSPACES_TABLE="${SPORE_BOT_WORKSPACES_TABLE:-spore-bot-workspaces}"
STRATA_BUCKET="${STRATA_REGISTRY_BUCKET:-strata-registry}"
T="arn:aws:dynamodb:${REGION}:${ACCOUNT_ID}:table"

echo "→ Writing the role policy..."
POLICY_DOCUMENT=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DescribeInstances",
      "Effect": "Allow",
      "Action": ["ec2:DescribeInstances"],
      "Resource": "*"
    },
    {
      "Sid": "UserAccounts",
      "Effect": "Allow",
      "Action": ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:Query", "dynamodb:Scan"],
      "Resource": ["${T}/${USER_ACCOUNTS_TABLE}", "${T}/${USER_ACCOUNTS_TABLE}/index/email-index"]
    },
    {
      "Sid": "Sweeps",
      "Effect": "Allow",
      "Action": ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:Scan", "dynamodb:UpdateItem", "dynamodb:DeleteItem"],
      "Resource": ["${T}/spawn-sweep-orchestration", "${T}/spawn-sweep-orchestration/index/team_id-index"]
    },
    {
      "Sid": "Teams",
      "Effect": "Allow",
      "Action": ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem"],
      "Resource": ["${T}/spawn-teams"]
    },
    {
      "Sid": "TeamMemberships",
      "Effect": "Allow",
      "Action": ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:Query", "dynamodb:DeleteItem"],
      "Resource": ["${T}/spawn-team-memberships", "${T}/spawn-team-memberships/index/member_arn-index"]
    },
    {
      "Sid": "WebSocketTickets",
      "Effect": "Allow",
      "Action": ["dynamodb:PutItem", "dynamodb:DeleteItem"],
      "Resource": ["${T}/${WS_TICKETS_TABLE}"]
    },
    {
      "Sid": "WebSocketConnections",
      "Effect": "Allow",
      "Action": ["dynamodb:PutItem", "dynamodb:DeleteItem", "dynamodb:Scan"],
      "Resource": ["${T}/spawn-websocket-connections"]
    },
    {
      "Sid": "CostHistory",
      "Effect": "Allow",
      "Action": ["dynamodb:Query", "dynamodb:PutItem"],
      "Resource": ["${T}/spawn-cost-history"]
    },
    {
      "Sid": "LagottoWatches",
      "Effect": "Allow",
      "Action": ["dynamodb:GetItem", "dynamodb:Query"],
      "Resource": ["${T}/${WATCHES_TABLE}", "${T}/${WATCHES_TABLE}/index/user_id-index", "${T}/${HISTORY_TABLE}/index/user_id-index"]
    },
    {
      "Sid": "SlackWorkspaces",
      "Effect": "Allow",
      "Action": ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:Scan", "dynamodb:UpdateItem"],
      "Resource": ["${T}/${WORKSPACES_TABLE}"]
    },
    {
      "Sid": "ProcessorStreams",
      "Effect": "Allow",
      "Action": ["dynamodb:DescribeStream", "dynamodb:GetRecords", "dynamodb:GetShardIterator"],
      "Resource": ["${T}/spawn-sweep-orchestration/stream/*", "${T}/spawn-autoscale-groups-production/stream/*"]
    },
    {
      "Sid": "ProcessorListStreams",
      "Effect": "Allow",
      "Action": ["dynamodb:ListStreams"],
      "Resource": "*"
    },
    {
      "Sid": "StrataRegistryRead",
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": ["arn:aws:s3:::${STRATA_BUCKET}/*"]
    },
    {
      "Sid": "StrataRegistryList",
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": ["arn:aws:s3:::${STRATA_BUCKET}"]
    },
    {
      "Sid": "StrataLockfiles",
      "Effect": "Allow",
      "Action": ["s3:PutObject"],
      "Resource": ["arn:aws:s3:::${STRATA_BUCKET}/locks/*"]
    }
  ]
}
EOF
)
aws iam put-role-policy \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --policy-name "$POLICY_NAME" \
    --policy-document "$POLICY_DOCUMENT"
echo -e "  ${GREEN}✓${NC} Role policy written"

# The Slack secret (#446). Its ARN is the dashboard-api function's
# SLACK_SECRETS_ARN; with none set the Lambda reads the legacy env vars and
# needs no grant, so the statement is written only when there is an ARN.
SLACK_ARN="${SLACK_SECRETS_ARN:-}"
if [ -z "$SLACK_ARN" ]; then
    SLACK_ARN=$(aws lambda get-function-configuration --profile "$PROFILE" --region "$REGION" \
        --function-name spawn-dashboard-api \
        --query 'Environment.Variables.SLACK_SECRETS_ARN' --output text 2>/dev/null || echo "") # swallow-ok: the function may not exist yet
    [ "$SLACK_ARN" = "None" ] && SLACK_ARN=""
fi
if [ -n "$SLACK_ARN" ]; then
    SLACK_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SlackSecret",
      "Effect": "Allow",
      "Action": ["secretsmanager:GetSecretValue"],
      "Resource": "${SLACK_ARN}"
    }
  ]
}
EOF
)
    aws iam put-role-policy --profile "$PROFILE" --role-name "$ROLE_NAME" \
        --policy-name "${POLICY_NAME}Slack" --policy-document "$SLACK_POLICY"
    echo -e "  ${GREEN}✓${NC} Slack secret grant written: $SLACK_ARN"
else
    echo -e "  ${YELLOW}!${NC} No SLACK_SECRETS_ARN (env or spawn-dashboard-api config): no Slack secret grant."
    echo "    Re-run with SLACK_SECRETS_ARN=<arn> once the secret exists."
fi

# PostToConnection on the dashboard's WebSocket API (dashboard-websocket-processor).
# The API ID is known only once setup-websocket-api.sh has created it.
WS_API="${WS_API_ID:-}"
if [ -z "$WS_API" ]; then
    WS_API=$(aws apigatewayv2 get-apis --profile "$PROFILE" --region "$REGION" \
        --query "Items[?Name=='spawn-dashboard-websocket'].ApiId" --output text)
fi
if [ -n "$WS_API" ] && [ "$WS_API" != "None" ]; then
    WS_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PostToConnections",
      "Effect": "Allow",
      "Action": ["execute-api:ManageConnections"],
      "Resource": "arn:aws:execute-api:${REGION}:${ACCOUNT_ID}:${WS_API}/production/POST/@connections/*"
    }
  ]
}
EOF
)
    aws iam put-role-policy --profile "$PROFILE" --role-name "$ROLE_NAME" \
        --policy-name "${POLICY_NAME}WebSocket" --policy-document "$WS_POLICY"
    echo -e "  ${GREEN}✓${NC} WebSocket grant written: $WS_API"
else
    echo -e "  ${YELLOW}!${NC} No spawn-dashboard-websocket API yet: no WebSocket grant."
    echo "    Re-run this script after lambda/dashboard-websocket/scripts/setup-websocket-api.sh."
fi

# The customer-managed policy earlier versions created is superseded by the
# inline policy above, which grants everything it did except the unused
# ec2:DescribeRegions and ec2:DescribeTags. Detach it, and delete it once
# nothing uses it.
LEGACY_ARN="arn:aws:iam::${ACCOUNT_ID}:policy/${POLICY_NAME}"
if aws iam list-attached-role-policies --profile "$PROFILE" --role-name "$ROLE_NAME" \
    --query "AttachedPolicies[?PolicyArn=='$LEGACY_ARN'].PolicyArn" --output text | grep -q .; then
    aws iam detach-role-policy --profile "$PROFILE" --role-name "$ROLE_NAME" --policy-arn "$LEGACY_ARN"
    echo -e "  ${GREEN}✓${NC} Detached legacy managed policy $LEGACY_ARN"
fi
ATTACHMENTS=$(aws iam get-policy --profile "$PROFILE" --policy-arn "$LEGACY_ARN" \
    --query 'Policy.AttachmentCount' --output text 2>/dev/null || echo "absent") # swallow-ok: absent is the expected state
if [ "$ATTACHMENTS" = "0" ]; then
    if aws iam delete-policy --profile "$PROFILE" --policy-arn "$LEGACY_ARN"; then
        echo -e "  ${GREEN}✓${NC} Deleted legacy managed policy $LEGACY_ARN"
    else
        echo -e "  ${YELLOW}!${NC} Could not delete $LEGACY_ARN (it may have non-default versions); delete it by hand."
    fi
fi

# Workspace credentials in spore-bot-workspaces are KMS-encrypted (#868), one
# encryption-context "purpose" per attribute (pkg/botsecret.Fields). The Slack
# OAuth install (slack_oauth.go) encrypts all four it stores; it decrypts the
# signing secret it preserves when Secrets Manager holds none, and the token
# refresh paths (rotateTokenByCurrentToken, getFreshBotToken) decrypt the bot
# and refresh tokens. Nothing here reads the webhook URL, so it is not
# decryptable. Scoped to the key behind alias/spore-bot-secrets
# (pkg/botsecret.DefaultKeyAlias). put-role-policy replaces the inline policy,
# so a re-run updates an existing role.
echo "→ Granting use of the spore-bot workspace-credentials KMS key..."
KMS_POLICY=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SporeBotCredentialsEncrypt",
      "Effect": "Allow",
      "Action": ["kms:Encrypt"],
      "Resource": "arn:aws:kms:*:${ACCOUNT_ID}:key/*",
      "Condition": {
        "StringEquals": {
          "kms:EncryptionContext:purpose": [
            "spore-bot-signing-secret",
            "spore-bot-bot-token",
            "spore-bot-refresh-token",
            "spore-bot-incoming-webhook-url"
          ]
        },
        "ForAnyValue:StringEquals": {
          "kms:ResourceAliases": "alias/spore-bot-secrets"
        }
      }
    },
    {
      "Sid": "SporeBotCredentialsDecrypt",
      "Effect": "Allow",
      "Action": ["kms:Decrypt"],
      "Resource": "arn:aws:kms:*:${ACCOUNT_ID}:key/*",
      "Condition": {
        "StringEquals": {
          "kms:EncryptionContext:purpose": [
            "spore-bot-signing-secret",
            "spore-bot-bot-token",
            "spore-bot-refresh-token"
          ]
        },
        "ForAnyValue:StringEquals": {
          "kms:ResourceAliases": "alias/spore-bot-secrets"
        }
      }
    }
  ]
}
EOF
)
aws iam put-role-policy \
    --profile "$PROFILE" \
    --role-name "$ROLE_NAME" \
    --policy-name "SporeBotCredentialsKMS" \
    --policy-document "$KMS_POLICY"

echo -e "  ${GREEN}✓${NC} KMS grant attached (inline policy SporeBotCredentialsKMS)"

# Clean up temp files
rm -f /tmp/trust-policy.json

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ IAM Role Setup Complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Role ARN:"
echo "  $ROLE_ARN"
echo ""
echo "Next Steps:"
echo "  1. Deploy Lambda function: cd .. && ./deploy.sh $PROFILE"
echo "  2. Set up API Gateway: ./scripts/setup-dashboard-api-gateway.sh $PROFILE"
echo ""
