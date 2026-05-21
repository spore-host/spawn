#!/usr/bin/env bash
# Layer 2 smoke test for spore-bot Lambda.
# Tests the HTTP endpoints without a real Slack workspace by using httpbin
# as the response_url — httpbin echoes back whatever the Lambda POSTs to it.
#
# Prerequisites:
#   - Lambda deployed (make deploy from spawn/lambda/spore-bot/)
#   - Workspace registered (spawn bot workspace-add --platform slack ...)
#   - Instance registered (spawn bot register --platform slack ...)
#
# Usage:
#   LAMBDA_URL=https://abc123.lambda-url.us-east-1.on.aws \
#   SIGNING_SECRET=your-slack-signing-secret \
#   WORKSPACE_ID=T03NE3GTY \
#   USER_ID=UTEST123 \
#   ./smoke_test.sh

set -euo pipefail

LAMBDA_URL="${LAMBDA_URL:?set LAMBDA_URL to the Lambda Function URL}"
SIGNING_SECRET="${SIGNING_SECRET:?set SIGNING_SECRET from workspace-add}"
WORKSPACE_ID="${WORKSPACE_ID:-TTEST456}"
USER_ID="${USER_ID:-UTEST123}"
COMMAND="${COMMAND:-status}"
NICKNAME="${NICKNAME:-}"

# Color output
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}✓ PASS${NC}: $1"; }
fail() { echo -e "${RED}✗ FAIL${NC}: $1"; exit 1; }
info() { echo -e "${YELLOW}→${NC} $1"; }

# --- Helper: build valid Slack HMAC signature ---
slack_sig() {
    local ts="$1" body="$2"
    local base="v0:${ts}:${body}"
    printf 'v0=%s' "$(printf '%s' "$base" | openssl dgst -sha256 -hmac "$SIGNING_SECRET" | awk '{print $2}')"
}

echo ""
echo "=========================================="
echo " spore-bot Layer 2 Smoke Test"
echo "=========================================="
echo "  Lambda URL:   $LAMBDA_URL"
echo "  Workspace:    $WORKSPACE_ID"
echo "  User:         $USER_ID"
echo ""

# ── Test 1: URL Verification Challenge ────────────────────────────────────────
info "Test 1: Slack URL verification challenge"
CHALLENGE_RESP=$(curl -sf -X POST "${LAMBDA_URL}/slack" \
    -H "Content-Type: application/json" \
    -d '{"type":"url_verification","challenge":"abc123xyz"}')

if echo "$CHALLENGE_RESP" | grep -q '"challenge":"abc123xyz"'; then
    pass "URL verification echoes challenge"
else
    fail "URL verification failed. Response: $CHALLENGE_RESP"
fi

# ── Test 2: Missing signature → 401 ──────────────────────────────────────────
info "Test 2: Missing signature headers → 401"
HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${LAMBDA_URL}/slack" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "command=/prism&text=status&user_id=${USER_ID}&team_id=${WORKSPACE_ID}")
if [ "$HTTP_STATUS" = "401" ]; then
    pass "Missing signature → 401"
else
    fail "Expected 401, got $HTTP_STATUS"
fi

# ── Test 3: Invalid signature → 401 ──────────────────────────────────────────
info "Test 3: Invalid signature → 401"
TIMESTAMP=$(date +%s)
BODY="command=/prism&text=status&user_id=${USER_ID}&team_id=${WORKSPACE_ID}&response_url=https://httpbin.org/post"
HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${LAMBDA_URL}/slack" \
    -H "X-Slack-Signature: v0=invalidsignature" \
    -H "X-Slack-Request-Timestamp: ${TIMESTAMP}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "$(echo $BODY)")
if [ "$HTTP_STATUS" = "401" ]; then
    pass "Invalid signature → 401"
else
    fail "Expected 401, got $HTTP_STATUS"
fi

# ── Test 4: Valid status command → 200 ACK ───────────────────────────────────
info "Test 4: Valid ${COMMAND} command → 200 ACK (response_url = httpbin)"
TIMESTAMP=$(date +%s)
TEXT="${COMMAND}"
[ -n "$NICKNAME" ] && TEXT="${COMMAND} ${NICKNAME}"
BODY="command=/prism&text=${TEXT}&user_id=${USER_ID}&team_id=${WORKSPACE_ID}&response_url=https://httpbin.org/post"
SIG=$(slack_sig "$TIMESTAMP" "$BODY")

RESP=$(curl -sf -X POST "${LAMBDA_URL}/slack" \
    -H "X-Slack-Signature: ${SIG}" \
    -H "X-Slack-Request-Timestamp: ${TIMESTAMP}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "${BODY}")

if echo "$RESP" | grep -qE '"text"'; then
    pass "Valid command → 200 with ACK text"
    echo "  ACK: $(echo "$RESP" | grep -o '"text":"[^"]*"')"
else
    fail "Expected 200 with text, got: $RESP"
fi

# ── Test 5: help command → 200 (no async needed) ────────────────────────────
info "Test 5: /prism help → 200 ACK"
TIMESTAMP=$(date +%s)
BODY="command=/prism&text=help&user_id=${USER_ID}&team_id=${WORKSPACE_ID}&response_url=https://httpbin.org/post"
SIG=$(slack_sig "$TIMESTAMP" "$BODY")

HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${LAMBDA_URL}/slack" \
    -H "X-Slack-Signature: ${SIG}" \
    -H "X-Slack-Request-Timestamp: ${TIMESTAMP}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "${BODY}")
if [ "$HTTP_STATUS" = "200" ]; then
    pass "/prism help → 200"
else
    fail "Expected 200, got $HTTP_STATUS"
fi

echo ""
echo "=========================================="
echo " All smoke tests passed!"
echo ""
echo " Note: For the status/start/stop commands, check httpbin.org"
echo " or CloudWatch /aws/lambda/spore-bot for the Phase 2 response."
echo " The Lambda posts the EC2 result to response_url asynchronously."
echo "=========================================="
