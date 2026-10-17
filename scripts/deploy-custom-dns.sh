#!/bin/bash
#
# Deploy custom DNS infrastructure for spawn (spore-host project)
#
# Usage: ./deploy-custom-dns.sh --hosted-zone-id Z123... --domain spore.example.edu
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to print colored output
info() {
    echo -e "${GREEN}✓${NC} $1"
}

warn() {
    echo -e "${YELLOW}⚠${NC} $1"
}

error() {
    echo -e "${RED}✗${NC} $1"
}

# Parse command line arguments
HOSTED_ZONE_ID=""
DOMAIN=""
ALLOW_ACCOUNTS=()
REGION=$(aws configure get region || echo "us-east-1")

while [[ $# -gt 0 ]]; do
  case $1 in
    --hosted-zone-id)
      HOSTED_ZONE_ID="$2"
      shift 2
      ;;
    --domain)
      DOMAIN="$2"
      shift 2
      ;;
    --region)
      REGION="$2"
      shift 2
      ;;
    --allow-account)
      ALLOW_ACCOUNTS+=("$2")
      shift 2
      ;;
    --help)
      echo "Usage: $0 --hosted-zone-id Z123... --domain spore.example.edu [--region us-east-1]"
      echo ""
      echo "Options:"
      echo "  --hosted-zone-id    Route53 hosted zone ID (required)"
      echo "  --domain            DNS domain (required)"
      echo "  --region            AWS region (default: from AWS CLI config)"
      echo "  --allow-account ID  AWS account whose spores may register (repeatable;"
      echo "                      default: the account this script deploys into)"
      echo "  --help              Show this help message"
      exit 0
      ;;
    *)
      error "Unknown option: $1"
      echo "Use --help for usage information"
      exit 1
      ;;
  esac
done

# Validate required parameters
if [ -z "$HOSTED_ZONE_ID" ] || [ -z "$DOMAIN" ]; then
  error "Missing required parameters"
  echo ""
  echo "Usage: $0 --hosted-zone-id Z123... --domain spore.example.edu"
  echo "Use --help for more information"
  exit 1
fi

echo "========================================="
echo "  Spawn Custom DNS Deployment"
echo "========================================="
echo ""
echo "Configuration:"
echo "  Hosted Zone: $HOSTED_ZONE_ID"
echo "  Domain:      $DOMAIN"
echo "  Region:      $REGION"
echo ""

# Make REGION actually bind, rather than being parsed, printed and then ignored.
#
# Most `aws` calls below carried no --region, so they used whatever was ambient
# while this script printed the region it believed it was deploying to. That is
# how a second scheduler-handler came to exist in us-west-2 while the stale one
# kept serving traffic from us-east-1 (#716). Exporting it fixes every call at
# once, including the ones added later.
export AWS_REGION="$REGION" AWS_DEFAULT_REGION="$REGION"

# Get AWS account ID
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
info "AWS Account: $ACCOUNT_ID"

# PRINTING the account is not checking it. This script read ACCOUNT_ID and only
# echoed it, so ambient credentials silently decided where a Lambda, an IAM role
# and Route53 records were created. Found when the deploy-script gates were
# widened past their hardcoded two-file list (#799). Override only for a fork.
EXPECTED_ACCOUNT="${SPAWN_LAMBDA_ACCOUNT:-966362334030}"
if [ "$ACCOUNT_ID" != "$EXPECTED_ACCOUNT" ]; then
  echo "ERROR: credentials are for account $ACCOUNT_ID, expected $EXPECTED_ACCOUNT." >&2
  echo "       Set AWS_PROFILE, or SPAWN_LAMBDA_ACCOUNT=$ACCOUNT_ID to deploy here deliberately." >&2
  exit 1
fi
echo ""

# Accounts whose spores may call the updater (#891). Each gets an
# lambda:InvokeFunctionUrl grant below and is added to DNS_ALLOWED_ACCOUNTS, the
# updater's own allowlist. Default: this account only.
if [ ${#ALLOW_ACCOUNTS[@]} -eq 0 ]; then
  ALLOW_ACCOUNTS=("$ACCOUNT_ID")
fi
for a in "${ALLOW_ACCOUNTS[@]}"; do
  if ! [[ "$a" =~ ^[0-9]{12}$ ]]; then
    error "--allow-account $a is not a 12-digit AWS account ID"
    exit 1
  fi
done
info "Allowed accounts: ${ALLOW_ACCOUNTS[*]}"

# Step 1: Build Lambda function
echo "Step 1: Building Lambda function..."
cd lambda/dns-updater

if [ ! -f "main.go" ]; then
  error "Lambda source not found: lambda/dns-updater/main.go"
  exit 1
fi

info "Building for Linux/AMD64..."
# Build the whole package, not just main.go — the handler is split across
# main.go + signature.go, so `go build main.go` fails with
# "undefined: verifyInstanceIdentitySignature" (#248).
GOOS=linux GOARCH=amd64 go build -o bootstrap .

info "Creating deployment package..."
zip -q function.zip bootstrap
cd ../..

info "Lambda function built"
echo ""

# Step 2: Create IAM role
echo "Step 2: Creating IAM role..."

# The role the LIVE function runs under, read off the function rather than named
# from memory.
#
# This said SpawnDNSLambdaRole, which does not exist in the infra account at all.
# spawn-dns-updater runs under SpawnDNSLambdaExecutionRole. So every IAM
# permission this script granted went to a role it created for the purpose and
# nothing used, while the function kept running under a different role whose
# policies live only in the account — and the script reported success either way.
# A grant added here for a new feature would simply not have taken effect.
#
# Overridable, because an operator deploying their own custom-DNS stack in their
# own account has no reason to inherit spore.host's role name.
ROLE_NAME="${SPAWN_DNS_ROLE_NAME:-SpawnDNSLambdaExecutionRole}"

# Check if role exists
if aws iam get-role --role-name "$ROLE_NAME" &>/dev/null; then
  warn "IAM role $ROLE_NAME already exists"
else
  info "Creating IAM role: $ROLE_NAME"
  aws iam create-role \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document file://deployment/trust-policy.json \
    --description "Execution role for spawn DNS Lambda function" \
    > /dev/null

  info "IAM role created"
fi

# Get the role ARN, retrying for IAM's eventual consistency (#752).
#
# This was `sleep 3` followed by a bare get-role with no retry and no failure
# check. Three seconds is well inside IAM's consistency tail, so a slow
# propagation left ROLE_ARN EMPTY and the script went on to create a Lambda with
# an empty role — a failure that surfaces much later and nowhere near its cause.
ROLE_ARN=""
DNS_WAITED=0
while [ "$DNS_WAITED" -lt 40 ]; do
  # A failed attempt is expected while IAM propagates; the loop's deadline below
  # is what reports the real failure.
  # swallow-ok: this IS the poll, so an individual get-role failure is the normal case
  ROLE_ARN=$(aws iam get-role --role-name "$ROLE_NAME" --query 'Role.Arn' --output text 2>/dev/null || true)
  case "$ROLE_ARN" in
    arn:aws:iam::*) break ;;
  esac
  ROLE_ARN=""
  sleep 2
  DNS_WAITED=$((DNS_WAITED + 2))
done
if [ -z "$ROLE_ARN" ]; then
  echo "ERROR: could not read the ARN of IAM role $ROLE_NAME after ${DNS_WAITED}s." >&2
  echo "       Refusing to continue: the Lambda would be created with an empty role." >&2
  exit 1
fi
info "Role ARN: $ROLE_ARN"
echo ""

# Build the environment by MERGING into the deployed one, never replacing it.
#
# `--environment` replaces the entire variable map. The update path below passed
# Variables={HOSTED_ZONE_ID=...,DOMAIN=...}, so running this script against the
# live function would have deleted DOMAIN_ZONES — the only variable it actually
# has, and the one main.go prefers — leaving it on the legacy single-zone
# fallback. It would also have silently dropped DNS_AUTHZ_MODE, i.e. turned
# #835's enforcement off as a side effect of shipping unrelated code. That is the
# #716 shape: a deploy quietly reverting configuration it never meant to touch.
#
# DOMAIN_ZONES is merged key-by-key rather than overwritten, so deploying one
# domain cannot unregister the others in a multi-domain map. DOMAIN is no longer
# written: main.go reads DEFAULT_DOMAIN, never DOMAIN, so the old variable was
# inert.
merge_lambda_env() {
  # stdin: deployed Variables map as JSON. args: KEY=VALUE to set.
  python3 -c '
import json, sys
try:
    cur = json.load(sys.stdin) or {}
except Exception:
    cur = {}
if not isinstance(cur, dict):
    cur = {}
for kv in sys.argv[1:]:
    k, _, v = kv.partition("=")
    if k.startswith("?"):
        # Set only if absent: DEFAULT_DOMAIN and HOSTED_ZONE_ID belong to the first
        # zone deployed. Overwriting them meant adding a zone to a
        # shared updater made that zone the default (#891).
        cur.setdefault(k[1:], v)
    elif k.startswith("+"):
        # Union of comma-separated values (DNS_ALLOWED_ACCOUNTS).
        have = [x for x in (cur.get(k[1:]) or "").split(",") if x]
        for x in v.split(","):
            if x and x not in have:
                have.append(x)
        cur[k[1:]] = ",".join(have)
    elif k == "DOMAIN_ZONES":
        zones = {}
        for e in (cur.get("DOMAIN_ZONES") or "").split(","):
            d, _, z = e.strip().partition("=")
            if d and z:
                zones[d] = z
        for e in v.split(","):
            d, _, z = e.strip().partition("=")
            if d and z:
                zones[d] = z
        cur["DOMAIN_ZONES"] = ",".join(f"{d}={z}" for d, z in sorted(zones.items()))
    else:
        cur[k] = v
print(json.dumps({"Variables": cur}))
' "$@"
}

# Only variables the operator actually set are passed through, so an unset
# DNS_AUTHZ_MODE leaves the deployed value alone instead of clearing it.
ENV_ARGS=(
  "DOMAIN_ZONES=${DOMAIN}=${HOSTED_ZONE_ID}"
  "?DEFAULT_DOMAIN=${DOMAIN}"
  "?HOSTED_ZONE_ID=${HOSTED_ZONE_ID}"
  "+DNS_ALLOWED_ACCOUNTS=$(printf '%s,' "${ALLOW_ACCOUNTS[@]}")"
)
for passthrough in DNS_AUTHZ_MODE DNS_ACCOUNTS_TABLE; do
  if [ -n "${!passthrough:-}" ]; then
    ENV_ARGS+=("${passthrough}=${!passthrough}")
  fi
done

FUNCTION_NAME="spawn-dns-updater"
DEPLOYED_ENV='{}'
if aws lambda get-function --function-name "$FUNCTION_NAME" &>/dev/null; then
  DEPLOYED_ENV=$(aws lambda get-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --query 'Environment.Variables' --output json)
  if [ -z "$DEPLOYED_ENV" ] || [ "$DEPLOYED_ENV" = "null" ]; then
    DEPLOYED_ENV='{}'
  fi
fi
MERGED_ENV=$(printf '%s' "$DEPLOYED_ENV" | merge_lambda_env "${ENV_ARGS[@]}")

# The Route53 grant covers EVERY zone the updater serves, read from the merged
# DOMAIN_ZONES — not just this run's --hosted-zone-id. put-role-policy replaces
# the whole document, so granting only the new zone revoked every zone already
# deployed on a role this script created (#891). (spore.host's live role also has
# a hand-made Route53DNSUpdate policy for its zone, which would have masked it
# there; a grant that is only correct because of another one is not correct.)
ZONE_ARNS=$(printf '%s' "$MERGED_ENV" | python3 -c '
import json, sys
zones = json.load(sys.stdin)["Variables"].get("DOMAIN_ZONES", "")
ids = sorted({e.partition("=")[2].strip() for e in zones.split(",") if "=" in e})
print(json.dumps(["arn:aws:route53:::hostedzone/" + z for z in ids if z]))')
info "Route53 zones served: $ZONE_ARNS"

# Step 3: Create/update IAM policy
echo "Step 3: Creating IAM policy..."

# Create policy document
cat > /tmp/spawn-lambda-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Route53Permissions",
      "Effect": "Allow",
      "Action": [
        "route53:ChangeResourceRecordSets",
        "route53:ListResourceRecordSets"
      ],
      "Resource": $ZONE_ARNS
    },
    {
      "Sid": "EC2Permissions",
      "Effect": "Allow",
      "Action": "ec2:DescribeInstances",
      "Resource": "*"
    },
    {
      "Sid": "CloudWatchLogsPermissions",
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogGroup",
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "arn:aws:logs:*:*:*"
    }
  ]
}
EOF

info "Attaching inline policy to role..."
aws iam put-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-name "SpawnDNSPermissions" \
  --policy-document file:///tmp/spawn-lambda-policy.json

rm /tmp/spawn-lambda-policy.json
info "IAM policy attached"

# The account-registry read, for caller AUTHORIZATION (#835).
#
# Its own inline policy rather than another statement in the one above, so that
# the single grant allowing this function to read customer records can be audited
# and revoked on its own.
#
# Scoped three ways, and the third one is load-bearing:
#
#   - to the one table;
#   - to the one attribute, via the dynamodb:Attributes condition key (a
#     DynamoDB key on a dynamodb: action — the pairing that #665 got wrong by
#     putting ec2:ResourceTag on an ssm: action and earning a silent implicit
#     deny);
#   - to Select=SPECIFIC_ATTRIBUTES, WITHOUT which the attribute scoping does
#     nothing. ForAllValues:StringEquals is true for an EMPTY set, so a Scan that
#     names no attributes at all satisfies it and returns every field of every
#     item. This is the same trap as ForAllValues:StringNotEquals on aws:TagKeys
#     being defeated by one innocuous tag. Pinning Select is what makes a
#     full-table read a denial rather than a loophole.
DNS_ACCOUNTS_TABLE="${DNS_ACCOUNTS_TABLE:-spawn-user-accounts}"
cat > /tmp/spawn-dns-registry-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadAccountRegistryForCallerAuthorization",
      "Effect": "Allow",
      "Action": "dynamodb:Scan",
      "Resource": "arn:aws:dynamodb:${REGION}:${ACCOUNT_ID}:table/${DNS_ACCOUNTS_TABLE}",
      "Condition": {
        "StringEquals": { "dynamodb:Select": "SPECIFIC_ATTRIBUTES" },
        "ForAllValues:StringEquals": { "dynamodb:Attributes": ["aws_account_id"] }
      }
    }
  ]
}
EOF
aws iam put-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-name "SpawnDNSAccountRegistryRead" \
  --policy-document file:///tmp/spawn-dns-registry-policy.json
rm /tmp/spawn-dns-registry-policy.json
info "Account-registry read policy attached (table: $DNS_ACCOUNTS_TABLE)"
echo ""

# Step 4: Create or update Lambda function
echo "Step 4: Deploying Lambda function..."

# Check if function exists
if aws lambda get-function --function-name "$FUNCTION_NAME" &>/dev/null; then
  warn "Lambda function $FUNCTION_NAME already exists, updating code..."

  aws lambda update-function-code \
    --function-name "$FUNCTION_NAME" \
    --zip-file fileb://lambda/dns-updater/function.zip \
    > /dev/null
  aws lambda wait function-updated --function-name "$FUNCTION_NAME"

  # --runtime on every deploy, not only on create.
  #
  # update-function-code does not change the runtime and --runtime appears only
  # in the create path, which never runs for an existing function. So a script
  # can declare provided.al2023 and redeploy forever while the function stays on
  # provided.al2 — exactly what happened to scheduler-handler (#716).
  info "Updating configuration (runtime + merged environment)..."
  aws lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" \
    --runtime provided.al2023 \
    --environment "$MERGED_ENV" \
    > /dev/null
  aws lambda wait function-updated --function-name "$FUNCTION_NAME"

  info "Lambda function updated"
else
  info "Creating Lambda function: $FUNCTION_NAME"

  aws lambda create-function \
    --function-name "$FUNCTION_NAME" \
    --runtime provided.al2023 \
    --role "$ROLE_ARN" \
    --handler bootstrap \
    --zip-file fileb://lambda/dns-updater/function.zip \
    --timeout 30 \
    --memory-size 256 \
    --environment "$MERGED_ENV" \
    --description "DNS updater for spawn managed instances" \
    > /dev/null

  info "Lambda function created"
fi

# Get Lambda ARN
LAMBDA_ARN=$(aws lambda get-function \
  --function-name "$FUNCTION_NAME" \
  --query 'Configuration.FunctionArn' \
  --output text)

info "Lambda ARN: $LAMBDA_ARN"

# Stamp the version so an operator can read it in ONE API call (#654). This
# function is not CloudFormation-managed, so a tag here is not stack drift.
#
# This script was the one the version census found: it deploys spawn-dns-updater
# from lambda/dns-updater/, but the script's own name says "custom-dns", so an
# enumeration keyed on the lambda directory name missed it entirely — the same
# shape as the runtime census finding github-oauth-bridge.
SPAWN_VERSION=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo unknown)
aws lambda tag-resource \
  --resource "$LAMBDA_ARN" \
  --tags "spawn:managed=true,spawn:component=dns-updater,spawn:created-by=deploy-custom-dns.sh,spawn:version=$SPAWN_VERSION" \
  > /dev/null
# Set log retention (#653). A log group Lambda auto-creates on first invocation
# has NO retention and keeps logs forever: a footprint audit found 10 such groups
# holding 51 MB, the only control-plane artifact measured to grow without bound.
# The three SAM-deployed functions declare a group with a retention; the
# script-deployed ones did not, which is exactly why theirs were among the ten.
#
# put-retention-policy is idempotent and creates nothing: if the group does not
# exist yet (first deploy, never invoked) it fails, which is why the failure is
# tolerated rather than fatal — the next deploy after a first invocation sets it.
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-30}"
aws logs put-retention-policy \
  --log-group-name "/aws/lambda/$FUNCTION_NAME" \
  --retention-in-days "$LOG_RETENTION_DAYS" \
  --region "$REGION"  2>/dev/null \
  && echo "log retention: ${LOG_RETENTION_DAYS}d on /aws/lambda/$FUNCTION_NAME" \
  || echo "log retention: could not set it yet (group not created until first invocation)"

info "Stamped spawn:version=$SPAWN_VERSION"
echo ""

# The invocation path is the Lambda FUNCTION URL, not an API Gateway.
#
# Steps 5 and 6 used to create a REST API here — `spawn-dns-api`, with
# `POST /update-dns` at `--authorization-type NONE` — plus an
# `apigateway-invoke` permission on the function. All of it is removed (#870).
#
# It predated #173, which moved the updater to a Function URL with
# `AuthType: AWS_IAM` and rewrote the handler to take a
# LambdaFunctionURLRequest. The API Gateway section was never deleted, so this
# script went on building infrastructure for an architecture the function no
# longer has:
#
#   - the integration could not work. API Gateway REST sends an
#     APIGatewayProxyRequest and expects a matching response shape, so a POST
#     returned 502 rather than a handler response. Verified against the resource
#     this script created on 2026-10-12, before it was deleted again.
#   - it was a PUBLIC, UNAUTHENTICATED endpoint into the DNS updater. It could
#     not write DNS (the 502 above), but it did invoke a billable function and
#     was one handler-signature change away from being live.
#   - nothing consumed it. Neither /spawn/dns/* SSM parameter was set, and
#     spored registers through the Function URL.
#
# Ensure the Function URL — the endpoint spored signs its requests for (#891).
#
# This used to only READ the URL and warn when there was none, so on a fresh
# deployment every spore's registration had nowhere to go. It now creates one,
# and only ever with AuthType AWS_IAM: the updater namespaces each record under
# the SigV4-verified caller account and refuses a request with no IAM authorizer,
# so a URL with AuthType NONE accepts nothing (#173). An existing URL with any
# other AuthType is refused, not changed — rewriting a live endpoint's auth is
# not something a deploy should do as a side effect.
FUNCTION_URL=$(aws lambda get-function-url-config --function-name "$FUNCTION_NAME" \
  --query 'FunctionUrl' --output text 2>/dev/null) || FUNCTION_URL=""
if [ -z "$FUNCTION_URL" ] || [ "$FUNCTION_URL" = "None" ]; then
  info "Creating Function URL (AuthType AWS_IAM)..."
  FUNCTION_URL=$(aws lambda create-function-url-config \
    --function-name "$FUNCTION_NAME" \
    --auth-type AWS_IAM \
    --query 'FunctionUrl' --output text)
fi
FUNCTION_URL_AUTH=$(aws lambda get-function-url-config --function-name "$FUNCTION_NAME" \
  --query 'AuthType' --output text)
if [ "$FUNCTION_URL_AUTH" != "AWS_IAM" ]; then
  error "Function URL $FUNCTION_URL has AuthType $FUNCTION_URL_AUTH, not AWS_IAM."
  error "The updater refuses every request without an IAM authorizer, so no spore can"
  error "register through it. Fix it deliberately:"
  error "  aws lambda update-function-url-config --function-name $FUNCTION_NAME --auth-type AWS_IAM"
  exit 1
fi
info "Function URL: $FUNCTION_URL (AuthType: AWS_IAM)"

# Let each allowed account invoke the URL — lambda:InvokeFunctionUrl, conditioned
# on the AWS_IAM auth type, one statement per account. The same shape as the
# spore.host deployment.
#
# Deliberately NOT lambda:InvokeFunction. A direct invocation lets the caller
# write the whole event, including requestContext.authorizer.iam.accountId — the
# value the updater trusts as "who is asking". Granting it would let any allowed
# account register records under any other account's subdomain.
for a in "${ALLOW_ACCOUNTS[@]}"; do
  SID="FunctionURLAllow${a}"
  if aws lambda get-policy --function-name "$FUNCTION_NAME" --query Policy --output text 2>/dev/null \
       | grep -q "\"Sid\":\"${SID}\""; then
    info "Invoke grant for $a already present"
    continue
  fi
  aws lambda add-permission \
    --function-name "$FUNCTION_NAME" \
    --statement-id "$SID" \
    --action lambda:InvokeFunctionUrl \
    --principal "$a" \
    --function-url-auth-type AWS_IAM \
    > /dev/null
  info "Granted lambda:InvokeFunctionUrl to $a"
done
echo ""

# Summary
echo "========================================="
echo "  Deployment Complete!"
echo "========================================="
echo ""
echo "Configuration:"
echo "  Domain:        $DOMAIN"
echo "  Hosted Zone:   $HOSTED_ZONE_ID"
echo "  Function URL:  $FUNCTION_URL"
echo ""
echo "Next Steps:"
echo ""
echo "1. Verify DNS delegation (if not already done):"
echo "   dig NS $DOMAIN"
echo ""
echo "2. Point launches at this deployment — any ONE of:"
echo "   - per launch:"
echo "       spawn launch <name> --dns-domain $DOMAIN --dns-api-endpoint $FUNCTION_URL ..."
echo "   - for this workstation, in ~/.spawn/config.yaml:"
echo "       dns:"
echo "         domain: $DOMAIN"
echo "         api_endpoint: $FUNCTION_URL"
echo "   - for every launch in an account and region (run in THAT account):"
echo "       aws ssm put-parameter --region <region> --type String --name /spawn/dns/domain --value $DOMAIN"
echo "       aws ssm put-parameter --region <region> --type String --name /spawn/dns/api_endpoint --value $FUNCTION_URL"
echo "   spored on the instance registers through it; --dry-run shows the target."
echo ""
echo "3. Test the deployment:"
echo "   spawn launch dns-test --ttl 1h --dns-domain $DOMAIN --dns-api-endpoint $FUNCTION_URL"
echo "   spawn status dns-test      # spawn:dns-status should read 'registered'"
echo ""
echo "4. (Optional) Enable DNSSEC:"
echo "   ./scripts/enable-dnssec.sh --hosted-zone-id $HOSTED_ZONE_ID --domain $DOMAIN"
echo ""
echo "Records in $DOMAIN are NOT covered by spore.host's TTL reaper DNS lease expiry;"
echo "see docs/custom-dns.md."
echo ""
echo "For more information, see docs/custom-dns.md"
echo ""
