#!/bin/bash
set -e

BUCKET_NAME="spawn-sweeps-us-east-1"
REGION="us-east-1"

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

echo "✅ S3 bucket $BUCKET_NAME configured successfully"
