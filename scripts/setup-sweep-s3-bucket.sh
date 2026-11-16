#!/bin/bash
set -e

BUCKET_NAME="spawn-sweeps-us-east-1"
REGION="us-east-1"

# The spore-host:* service tags (spore-host/spore-host#750).
SPORE_SERVICE_COMPONENT=sweep-orchestrator
SPORE_SERVICE_DEPLOYED_BY=scripts/setup-sweep-s3-bucket.sh
# shellcheck source=scripts/lib/service-tags.sh
. "$(dirname "$0")/lib/service-tags.sh"

echo "Creating S3 bucket for parameter sweeps..."

# Create bucket
aws s3api create-bucket \
  --bucket "$BUCKET_NAME" \
  --region "$REGION" 2>/dev/null || echo "Bucket already exists"

# Enable versioning
aws s3api put-bucket-versioning \
  --bucket "$BUCKET_NAME" \
  --versioning-configuration Status=Enabled

# Configure lifecycle policy to delete old sweeps after 30 days
aws s3api put-bucket-lifecycle-configuration \
  --bucket "$BUCKET_NAME" \
  --lifecycle-configuration '{
    "Rules": [{
      "ID": "DeleteOldSweeps",
      "Status": "Enabled",
      "Filter": {"Prefix": "sweeps/"},
      "Expiration": {"Days": 30}
    }]
  }'

# Block public access
aws s3api put-public-access-block \
  --bucket "$BUCKET_NAME" \
  --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

# S3 takes no tags on create-bucket from this CLI path; add them, keeping any
# the bucket already has (#750).
spore_merge_bucket_tags "$BUCKET_NAME"

echo "✅ S3 bucket $BUCKET_NAME configured successfully"
