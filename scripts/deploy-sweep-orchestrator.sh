#!/bin/bash
# Deploy the spawn-sweep-orchestrator Lambda, which launches the rows of a
# DETACHED parameter sweep — the default path, since launchParameterSweep
# auto-enables detach.
#
# This function is hand-deployed (no CloudFormation stack), so this script is the
# only record of how it is configured. It had sat untouched since 2026-01-17 when
# #725 made it tag its rows, which is nine months of version skew against a CLI
# that shipped ~30 releases in the same period.
#
# set -u because an unset variable here means deploying to the wrong place, and
# pipefail because a failure on the left of a pipe must not be masked by a
# successful grep/tee on the right.
set -euo pipefail

FUNCTION_NAME="spawn-sweep-orchestrator"
ROLE_NAME="SpawnSweepOrchestratorRole"
REGION="${SPAWN_LAMBDA_REGION:-us-east-1}"
LAMBDA_DIR="../lambda/sweep-orchestrator"

# The account this function belongs to. Asserted rather than inferred: the
# account comes from whatever credentials happen to be ambient, and an earlier
# hand-deploy in this family created a duplicate function in the wrong REGION
# from exactly that shape of assumption. Override only to deploy a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi

echo "Deploying $FUNCTION_NAME to account $ACCOUNT_ID in $REGION"

cd "$(dirname "$0")/$LAMBDA_DIR"

# amd64 because the live function is x86_64. Changing architecture is a separate
# decision from shipping a code fix, and doing both at once would make a failure
# ambiguous.
echo "Building Go binary..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap main.go

echo "Creating deployment package..."
zip -q function.zip bootstrap
rm bootstrap

# The hash AWS will report if it really took this zip. Computed locally BEFORE
# the upload so the comparison afterwards is against something independent of
# the API's own answer.
LOCAL_SHA=$(openssl dgst -sha256 -binary function.zip | openssl base64)

if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$REGION" >/dev/null 2>&1; then
  echo "Updating existing Lambda function..."

  # Snapshot the live configuration before touching it, so a drift can be seen
  # rather than assumed absent.
  BEFORE=$(aws lambda get-function-configuration \
    --function-name "$FUNCTION_NAME" --region "$REGION" \
    --query '{Timeout:Timeout,Memory:MemorySize,Runtime:Runtime,Env:Environment.Variables}' --output json)
  echo "live config before: $BEFORE"

  aws lambda update-function-code \
    --function-name "$FUNCTION_NAME" \
    --zip-file fileb://function.zip \
    --region "$REGION" \
    --no-cli-pager >/dev/null

  aws lambda wait function-updated \
    --function-name "$FUNCTION_NAME" --region "$REGION"

  # Deliberately NOT passing --environment. The previous version of this script
  # sent `Variables={}` on every update, which WIPES whatever was configured —
  # and did it under `|| true`, so the wipe could not even fail loudly. There is
  # nothing set today, which is exactly when a landmine like that is cheapest to
  # remove. Timeout/memory are re-asserted because they are this script's record.
  aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --timeout 900 \
    --memory-size 512 \
    --region "$REGION" \
    --no-cli-pager >/dev/null

  aws lambda wait function-updated \
    --function-name "$FUNCTION_NAME" --region "$REGION"
else
  echo "Creating new Lambda function..."
  aws lambda create-function \
    --function-name "$FUNCTION_NAME" \
    --runtime provided.al2023 \
    --role "arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}" \
    --handler bootstrap \
    --zip-file fileb://function.zip \
    --timeout 900 \
    --memory-size 512 \
    --region "$REGION" \
    --no-cli-pager >/dev/null

  aws lambda wait function-active \
    --function-name "$FUNCTION_NAME" --region "$REGION"
fi

rm function.zip

# Verify the function is actually running the binary just built. An HTTP 200 from
# update-function-code is not evidence of that: it says the call was accepted.
REMOTE_SHA=$(aws lambda get-function-configuration \
  --function-name "$FUNCTION_NAME" --region "$REGION" \
  --query CodeSha256 --output text)
if [ "$REMOTE_SHA" != "$LOCAL_SHA" ]; then
  echo "ERROR: deployed CodeSha256 does not match the zip that was built." >&2
  echo "       local:  $LOCAL_SHA" >&2
  echo "       remote: $REMOTE_SHA" >&2
  exit 1
fi

echo "live config after:  $(aws lambda get-function-configuration \
  --function-name "$FUNCTION_NAME" --region "$REGION" \
  --query '{Timeout:Timeout,Memory:MemorySize,Runtime:Runtime,Env:Environment.Variables}' --output json)"
echo "✅ $FUNCTION_NAME deployed and CodeSha256 verified ($REMOTE_SHA)"
echo "Function ARN: arn:aws:lambda:${REGION}:${ACCOUNT_ID}:function:${FUNCTION_NAME}"
