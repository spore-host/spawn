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

# Only a clean release tag is deployed (#1368). This function renders the spored
# bootstrap for members of sweeps and schedules made by a spawn CLI at v0.146.0
# or older, and that bootstrap's stub fetches the body published for the
# release the function was built from, refusing any body but the one compiled
# into it. A dev, pseudo-version or dirty build names no published body, so its
# members would get the body inline from code no release signed; and a
# version stamped from anything but the tag being built would name a body
# that differs from the compiled one, so every such member would retire itself
# at boot.
SPAWN_VERSION=$(git describe --tags --exact-match --match 'v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null | sed 's/^v//' || true)  # swallow-ok: not on a tag; refused just below
if ! [[ "$SPAWN_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "ERROR: HEAD is not a spawn release tag (vX.Y.Z); deploy $FUNCTION_NAME only from a release checkout." >&2
  exit 1
fi
if [ -n "$(git status --porcelain)" ]; then
  echo "ERROR: the working tree has uncommitted changes; $FUNCTION_NAME would not be v$SPAWN_VERSION." >&2
  exit 1
fi

# The body the stub will fetch must be published for this release and be the
# one this build compiles in: the release workflow publishes exactly what
# `spawn bootstrap-body` prints, from the same pkg/launcher this function builds
# against. Checked in the us-east-1 bucket, the stub's last fallback.
BODY_URL="https://spawn-binaries-us-east-1.s3.amazonaws.com/spawn/versions/${SPAWN_VERSION}/bootstrap.sh"
if ! WANT_BODY_SHA=$( (cd ../.. && SPORE_NO_UPDATE_CHECK=1 CGO_ENABLED=0 go run . bootstrap-body) | openssl dgst -sha256 | awk '{print $NF}'); then
  echo "ERROR: could not render v$SPAWN_VERSION's bootstrap body (spawn bootstrap-body)." >&2
  exit 1
fi
if ! GOT_BODY_SHA=$(curl -fsS --max-time 60 "$BODY_URL" | openssl dgst -sha256 | awk '{print $NF}'); then
  echo "ERROR: no bootstrap body is published at $BODY_URL; legacy sweep members could not boot spored." >&2
  exit 1
fi
if [ "$GOT_BODY_SHA" != "$WANT_BODY_SHA" ]; then
  echo "ERROR: the body published at $BODY_URL (sha256 $GOT_BODY_SHA) is not the one v$SPAWN_VERSION compiles in ($WANT_BODY_SHA)." >&2
  exit 1
fi
echo "published bootstrap body for v$SPAWN_VERSION verified ($WANT_BODY_SHA)"

# amd64 because the live function is x86_64. Changing architecture is a separate
# decision from shipping a code fix, and doing both at once would make a failure
# ambiguous.
#
# The package, not main.go: the function is several files, and `go build
# main.go` compiles only the one named.
echo "Building Go binary..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -ldflags "-X main.Version=${SPAWN_VERSION}" -o bootstrap .

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

# Stamp the version so an operator can read it in ONE API call (#654).
#
# This function is NOT CloudFormation-managed, so a tag added here is not stack
# drift — unlike the SAM-deployed lambdas, where the tag is declared in the
# template for exactly that reason.
#
# SPAWN_VERSION is the release tag checked above, the version compiled in, so
# the stamp and the bootstrap the function renders name the same release.
# Before stamping, answering "is the deployed sweep orchestrator current?"
# meant comparing LastModified against `git log -- lambda/sweep-orchestrator/`.
FUNCTION_ARN=$(aws lambda get-function-configuration \
  --function-name "$FUNCTION_NAME" --region "$REGION" \
  --query FunctionArn --output text)
aws lambda tag-resource \
  --resource "$FUNCTION_ARN" \
  --tags "spawn:managed=true,spawn:component=sweep-orchestrator,spawn:created-by=deploy-sweep-orchestrator.sh,spawn:version=$SPAWN_VERSION" \
  --region "$REGION" >/dev/null
echo "stamped spawn:version=$SPAWN_VERSION"

# Set log retention (#653). A log group Lambda auto-creates on first invocation
# has NO retention and keeps logs forever: a footprint audit found 10 such groups
# holding 51 MB, the only control-plane artifact measured to grow without bound.
# The three SAM-deployed functions declare a group with a retention; the
# script-deployed ones did not, which is exactly why theirs were among the ten.
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-30}"
# Create the group BEFORE setting its retention (#827). put-retention-policy
# creates nothing, and the group Lambda makes on first invocation has no
# retention, so a deploy that only set retention left every never-invoked
# function to start with none. Lambda writes to an existing group rather than
# making its own, so this is safe and idempotent.
aws logs create-log-group \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --region "$REGION" 2>/dev/null || true  # swallow-ok: already exists; any other failure fails put-retention-policy next
# Fatal now: with the group created above, a failure here is real.
aws logs put-retention-policy \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --retention-in-days "$LOG_RETENTION_DAYS" \
  --region "$REGION"
echo "log retention: ${LOG_RETENTION_DAYS}d on /aws/lambda/$FUNCTION_NAME"

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
