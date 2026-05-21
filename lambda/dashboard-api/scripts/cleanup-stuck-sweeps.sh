#!/bin/bash
set -e

# Cleanup stuck sweeps that haven't been updated in 1+ hours
# Usage: ./cleanup-stuck-sweeps.sh [aws-profile]

PROFILE=${1:-spore-host-infra}
REGION=us-east-1
TABLE=spawn-sweep-orchestration
STALE_THRESHOLD_SECONDS=3600  # 1 hour

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Cleaning Up Stuck Sweeps"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile: $PROFILE"
echo "Table:   $TABLE"
echo "Threshold: ${STALE_THRESHOLD_SECONDS}s ($(($STALE_THRESHOLD_SECONDS / 3600))h)"
echo ""

# Get all RUNNING sweeps
echo "→ Scanning for RUNNING sweeps..."
aws dynamodb scan --profile "$PROFILE" --region "$REGION" \
  --table-name "$TABLE" \
  --filter-expression "#status = :running" \
  --expression-attribute-names '{"#status":"status"}' \
  --expression-attribute-values '{":running":{"S":"RUNNING"}}' \
  --query 'Items[*].[sweep_id.S,sweep_name.S,updated_at.S]' \
  --output text | while read sweep_id sweep_name updated_at; do

  # Calculate age in seconds
  if [[ "$OSTYPE" == "darwin"* ]]; then
    # macOS
    updated_epoch=$(date -j -f "%Y-%m-%dT%H:%M:%SZ" "$updated_at" "+%s" 2>/dev/null || echo "0")
  else
    # Linux
    updated_epoch=$(date -d "$updated_at" "+%s" 2>/dev/null || echo "0")
  fi

  now_epoch=$(date +%s)
  age_seconds=$((now_epoch - updated_epoch))
  age_hours=$((age_seconds / 3600))

  if [ $age_seconds -gt $STALE_THRESHOLD_SECONDS ]; then
    echo "  → $sweep_id ($sweep_name) - stale for ${age_hours}h"

    # Mark as CANCELLED
    aws dynamodb update-item --profile "$PROFILE" --region "$REGION" \
      --table-name "$TABLE" \
      --key "{\"sweep_id\":{\"S\":\"$sweep_id\"}}" \
      --update-expression "SET #status = :cancelled, completed_at = :now, updated_at = :now" \
      --expression-attribute-names '{"#status":"status"}' \
      --expression-attribute-values "{\":cancelled\":{\"S\":\"CANCELLED\"},\":now\":{\"S\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}" \
      > /dev/null

    echo "    ✓ Marked as CANCELLED"
  fi
done

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ Cleanup Complete"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
