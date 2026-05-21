#!/bin/bash
# Example: How to use spawn collect-results with parameter sweeps
#
# This script demonstrates the full workflow:
# 1. Run parameter sweep with result generation
# 2. Instances upload results to S3
# 3. Collect and aggregate results
# 4. Identify best parameters

set -e

# Step 1: Launch parameter sweep with result generation
# Each instance should upload results to S3 in this format:
# s3://spawn-results-<account>-<region>/sweeps/<sweep-id>/<index>/results.json

cat > ml-training-sweep.yaml <<'EOF'
defaults:
  region: us-west-2
  ttl: 4h
  spot: true

params:
  # Try different learning rates and batch sizes
  - step: training-run-1
    instance_type: g4dn.xlarge
    learning_rate: 0.001
    batch_size: 32
    command: |
      # Train model
      python3 train.py \
        --learning-rate $PARAM_learning_rate \
        --batch-size $PARAM_batch_size \
        --output /models/output

      # Evaluate and save metrics
      python3 evaluate.py --model /models/output --output /tmp/metrics.json

      # Upload results to S3
      ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
      REGION=$(curl -s http://169.254.169.254/latest/meta-data/placement/region)

      # Create result file with parameters and metrics
      cat > /tmp/results.json <<RESULTS
      {
        "parameters": {
          "learning_rate": $PARAM_learning_rate,
          "batch_size": $PARAM_batch_size
        },
        "accuracy": 0.92,
        "loss": 0.15,
        "f1_score": 0.91,
        "training_time": 1234.5
      }
      RESULTS

      aws s3 cp /tmp/results.json \
        s3://spawn-results-${ACCOUNT_ID}-${REGION}/sweeps/${SWEEP_ID}/${SWEEP_INDEX}/results.json

  - step: training-run-2
    instance_type: g4dn.xlarge
    learning_rate: 0.0001
    batch_size: 32
    command: |
      # Same as above but with different parameters
      python3 train.py --learning-rate $PARAM_learning_rate --batch-size $PARAM_batch_size --output /models/output
      python3 evaluate.py --model /models/output --output /tmp/metrics.json

      ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
      REGION=$(curl -s http://169.254.169.254/latest/meta-data/placement/region)

      cat > /tmp/results.json <<RESULTS
      {
        "parameters": {
          "learning_rate": $PARAM_learning_rate,
          "batch_size": $PARAM_batch_size
        },
        "accuracy": 0.94,
        "loss": 0.12,
        "f1_score": 0.93,
        "training_time": 1456.7
      }
      RESULTS

      aws s3 cp /tmp/results.json \
        s3://spawn-results-${ACCOUNT_ID}-${REGION}/sweeps/${SWEEP_ID}/${SWEEP_INDEX}/results.json

  - step: training-run-3
    instance_type: g4dn.xlarge
    learning_rate: 0.001
    batch_size: 64
    command: |
      # Same as above but with different parameters
      python3 train.py --learning-rate $PARAM_learning_rate --batch-size $PARAM_batch_size --output /models/output
      python3 evaluate.py --model /models/output --output /tmp/metrics.json

      ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
      REGION=$(curl -s http://169.254.169.254/latest/meta-data/placement/region)

      cat > /tmp/results.json <<RESULTS
      {
        "parameters": {
          "learning_rate": $PARAM_learning_rate,
          "batch_size": $PARAM_batch_size
        },
        "accuracy": 0.96,
        "loss": 0.08,
        "f1_score": 0.95,
        "training_time": 1678.9
      }
      RESULTS

      aws s3 cp /tmp/results.json \
        s3://spawn-results-${ACCOUNT_ID}-${REGION}/sweeps/${SWEEP_ID}/${SWEEP_INDEX}/results.json
EOF

# Step 2: Launch the sweep
echo "Launching parameter sweep..."
SWEEP_ID=$(spawn sweep --file ml-training-sweep.yaml --detach | grep "Sweep ID:" | awk '{print $3}')

echo "Sweep ID: $SWEEP_ID"
echo "Waiting for sweep to complete... (this may take a while)"

# Step 3: Wait for sweep to complete (poll status)
while true; do
  STATUS=$(spawn status --sweep-id $SWEEP_ID --json | jq -r '.status')
  if [ "$STATUS" = "COMPLETED" ] || [ "$STATUS" = "FAILED" ] || [ "$STATUS" = "CANCELLED" ]; then
    break
  fi
  echo "Status: $STATUS - waiting 30s..."
  sleep 30
done

echo "Sweep completed with status: $STATUS"

# Step 4: Collect results
echo ""
echo "Collecting results..."

# Collect all results to JSON
spawn collect-results --sweep-id $SWEEP_ID --output results.json

# Collect to CSV for spreadsheet analysis
spawn collect-results --sweep-id $SWEEP_ID --output results.csv --format csv

# Find top 3 runs by accuracy
spawn collect-results --sweep-id $SWEEP_ID --output best-accuracy.json --metric accuracy --best 3

# Find top 3 runs by f1_score
spawn collect-results --sweep-id $SWEEP_ID --output best-f1.json --metric f1_score --best 3

echo ""
echo "Results collected:"
echo "  - results.json: All results"
echo "  - results.csv: CSV format for analysis"
echo "  - best-accuracy.json: Top 3 by accuracy"
echo "  - best-f1.json: Top 3 by F1 score"

echo ""
echo "Best parameters by accuracy:"
cat best-accuracy.json | jq '.[0].parameters'

echo ""
echo "Done! You can now analyze the results or re-run with the best parameters."
