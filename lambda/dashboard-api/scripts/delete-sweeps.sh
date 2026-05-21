#!/bin/bash
set -e

# Delete completed/cancelled/failed sweeps
# Usage: ./delete-sweeps.sh [aws-profile] [sweep-id1] [sweep-id2] ...
#        ./delete-sweeps.sh [aws-profile] --all-completed

PROFILE=${1:-spore-host-infra}
shift  # Remove profile from args
REGION=us-east-1
TABLE=spawn-sweep-orchestration

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Delete Sweeps"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile: $PROFILE"
echo "Table:   $TABLE"
echo ""

if [ "$1" == "--all-completed" ]; then
  echo "→ Finding all completed/cancelled/failed sweeps..."

  # Get all completed/cancelled/failed sweeps
  SWEEP_IDS=$(aws dynamodb scan --profile "$PROFILE" --region "$REGION" \
    --table-name "$TABLE" \
    --filter-expression "#status IN (:completed, :cancelled, :failed)" \
    --expression-attribute-names '{"#status":"status"}' \
    --expression-attribute-values '{":completed":{"S":"COMPLETED"},":cancelled":{"S":"CANCELLED"},":failed":{"S":"FAILED"}}' \
    --query 'Items[*].sweep_id.S' \
    --output text)

  if [ -z "$SWEEP_IDS" ]; then
    echo "  No completed/cancelled/failed sweeps found."
    exit 0
  fi

  # Convert to array
  read -ra SWEEP_ARRAY <<< "$SWEEP_IDS"
  COUNT=${#SWEEP_ARRAY[@]}

  echo "  Found $COUNT sweep(s) to delete"
  echo ""
  read -p "Delete all $COUNT sweep(s)? [y/N] " -n 1 -r
  echo
  if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
  fi

  # Delete each sweep
  DELETED=0
  for sweep_id in "${SWEEP_ARRAY[@]}"; do
    aws dynamodb delete-item --profile "$PROFILE" --region "$REGION" \
      --table-name "$TABLE" \
      --key "{\"sweep_id\":{\"S\":\"$sweep_id\"}}" \
      > /dev/null
    echo "  ✓ Deleted: $sweep_id"
    DELETED=$((DELETED + 1))
  done

  echo ""
  echo "✓ Deleted $DELETED sweep(s)"
else
  # Delete specific sweep IDs
  if [ $# -eq 0 ]; then
    echo "Usage:"
    echo "  Delete specific sweeps:"
    echo "    $0 [profile] sweep-id-1 sweep-id-2 ..."
    echo ""
    echo "  Delete all completed/cancelled/failed:"
    echo "    $0 [profile] --all-completed"
    exit 1
  fi

  DELETED=0
  for sweep_id in "$@"; do
    # Check if sweep exists and get its status
    STATUS=$(aws dynamodb get-item --profile "$PROFILE" --region "$REGION" \
      --table-name "$TABLE" \
      --key "{\"sweep_id\":{\"S\":\"$sweep_id\"}}" \
      --query 'Item.status.S' \
      --output text 2>/dev/null || echo "NOT_FOUND")

    if [ "$STATUS" == "NOT_FOUND" ] || [ -z "$STATUS" ]; then
      echo "  ✗ Sweep not found: $sweep_id"
      continue
    fi

    if [ "$STATUS" == "RUNNING" ]; then
      echo "  ⚠ Sweep $sweep_id is RUNNING - skipping (cancel it first)"
      continue
    fi

    # Delete the sweep
    aws dynamodb delete-item --profile "$PROFILE" --region "$REGION" \
      --table-name "$TABLE" \
      --key "{\"sweep_id\":{\"S\":\"$sweep_id\"}}" \
      > /dev/null

    echo "  ✓ Deleted: $sweep_id (was $STATUS)"
    DELETED=$((DELETED + 1))
  done

  echo ""
  echo "✓ Deleted $DELETED sweep(s)"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ Complete"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
