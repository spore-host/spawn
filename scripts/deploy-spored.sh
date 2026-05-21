#!/bin/bash
# deploy-spored.sh - Deploy spored binaries to regional S3 buckets
#
# Usage:
#   ./deploy-spored.sh [VERSION] [PROJECT]
#
# Examples:
#   ./deploy-spored.sh                  # deploy spawn/latest
#   ./deploy-spored.sh v0.32.0          # deploy spawn/v0.32.0
#   ./deploy-spored.sh latest prism     # deploy prism/latest
#   PROJECT=prism ./deploy-spored.sh    # deploy prism/latest via env var
set -e

VERSION=${1:-latest}
PROJECT=${2:-${PROJECT:-spawn}}
REGIONS=(
    "us-east-1"
    "us-east-2"
    "us-west-1"
    "us-west-2"
    "eu-west-1"
    "eu-west-2"
    "eu-central-1"
    "ap-southeast-1"
    "ap-southeast-2"
    "ap-northeast-1"
)

echo "╔════════════════════════════════════════════════════════╗"
echo "║  Deploying spored v${VERSION} (project: ${PROJECT}) to S3"
echo "╚════════════════════════════════════════════════════════╝"
echo ""

# Check binaries exist
if [ ! -f "bin/spored-linux-amd64" ]; then
    echo "❌ bin/spored-linux-amd64 not found. Run 'make build-all' first."
    exit 1
fi

if [ ! -f "bin/spored-linux-arm64" ]; then
    echo "❌ bin/spored-linux-arm64 not found. Run 'make build-all' first."
    exit 1
fi

echo "✅ Found binaries:"
echo "   • spored-linux-amd64 ($(du -h bin/spored-linux-amd64 | cut -f1))"
echo "   • spored-linux-arm64 ($(du -h bin/spored-linux-arm64 | cut -f1))"
echo ""

# Function to create bucket if it doesn't exist
create_bucket_if_needed() {
    local bucket=$1
    local region=$2
    
    if aws s3 ls "s3://${bucket}" --region "$region" 2>/dev/null; then
        echo "   ✓ Bucket exists: ${bucket}"
    else
        echo "   Creating bucket: ${bucket}"
        if [ "$region" = "us-east-1" ]; then
            # us-east-1 doesn't use LocationConstraint
            aws s3api create-bucket \
                --bucket "$bucket" \
                --region "$region" 2>/dev/null || echo "   (bucket may already exist)"
        else
            aws s3api create-bucket \
                --bucket "$bucket" \
                --region "$region" \
                --create-bucket-configuration LocationConstraint="$region" 2>/dev/null || echo "   (bucket may already exist)"
        fi
        
        # Enable versioning
        aws s3api put-bucket-versioning \
            --bucket "$bucket" \
            --versioning-configuration Status=Enabled \
            --region "$region"
        
        echo "   ✓ Created: ${bucket}"
    fi
}

# Function to upload to a region
upload_to_region() {
    local region=$1
    local bucket="spawn-binaries-${region}"
    
    echo "📦 Deploying to ${region}..."
    
    # Create bucket if needed
    create_bucket_if_needed "$bucket" "$region"
    
    # Upload AMD64
    echo "   Uploading spored-linux-amd64..."
    aws s3 cp bin/spored-linux-amd64 \
        "s3://${bucket}/${PROJECT}/spored-linux-amd64" \
        --region "$region" \
        --metadata version="${VERSION}" \
        --quiet

    # Upload ARM64
    echo "   Uploading spored-linux-arm64..."
    aws s3 cp bin/spored-linux-arm64 \
        "s3://${bucket}/${PROJECT}/spored-linux-arm64" \
        --region "$region" \
        --metadata version="${VERSION}" \
        --quiet

    # Upload to versioned path
    echo "   Uploading versioned copies..."
    aws s3 cp bin/spored-linux-amd64 \
        "s3://${bucket}/${PROJECT}/versions/${VERSION}/spored-linux-amd64" \
        --region "$region" \
        --quiet

    aws s3 cp bin/spored-linux-arm64 \
        "s3://${bucket}/${PROJECT}/versions/${VERSION}/spored-linux-arm64" \
        --region "$region" \
        --quiet
    
    echo "   ✅ Deployed to ${region}"
    echo ""
}

# Deploy to all regions
for region in "${REGIONS[@]}"; do
    upload_to_region "$region"
done

echo "╔════════════════════════════════════════════════════════╗"
echo "║  ✅ Deployment Complete!                               ║"
echo "╚════════════════════════════════════════════════════════╝"
echo ""
echo "Deployed to ${#REGIONS[@]} regions (project: ${PROJECT}):"
for region in "${REGIONS[@]}"; do
    echo "  • s3://spawn-binaries-${region}/${PROJECT}/"
done
echo ""
echo "Instances will download from their regional bucket automatically."
echo ""

# Print verification commands
echo "Verify deployment:"
echo "  aws s3 ls s3://spawn-binaries-us-east-1/${PROJECT}/ --region us-east-1"
echo ""
echo "Test download (as instance would):"
echo "  aws s3 cp s3://spawn-binaries-us-east-1/${PROJECT}/spored-linux-amd64 /tmp/test --region us-east-1"
echo ""
