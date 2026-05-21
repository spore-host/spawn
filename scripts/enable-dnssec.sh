#!/bin/bash
#
# Enable DNSSEC for spawn custom DNS domain
#
# Usage: ./enable-dnssec.sh --hosted-zone-id Z123... --domain spore.example.edu
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
    --help)
      echo "Usage: $0 --hosted-zone-id Z123... --domain spore.example.edu [--region us-east-1]"
      echo ""
      echo "Options:"
      echo "  --hosted-zone-id    Route53 hosted zone ID (required)"
      echo "  --domain            DNS domain (required)"
      echo "  --region            AWS region (default: from AWS CLI config)"
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
echo "  DNSSEC Enablement for Spawn DNS"
echo "========================================="
echo ""
echo "Configuration:"
echo "  Hosted Zone: $HOSTED_ZONE_ID"
echo "  Domain:      $DOMAIN"
echo "  Region:      $REGION"
echo ""

warn "This script will enable DNSSEC for your hosted zone."
warn "You will need to add the DS record to your parent domain."
echo ""
read -p "Continue? (y/N) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 0
fi
echo ""

# Get AWS account ID
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
info "AWS Account: $ACCOUNT_ID"
echo ""

# Step 1: Create KMS key for DNSSEC
echo "Step 1: Creating KMS key for DNSSEC..."

KMS_KEY_ALIAS="alias/spawn-dnssec-$DOMAIN"

# Check if key already exists
KMS_KEY_ID=$(aws kms list-aliases \
  --query "Aliases[?AliasName=='$KMS_KEY_ALIAS'].TargetKeyId" \
  --output text)

if [ -n "$KMS_KEY_ID" ]; then
  warn "KMS key already exists: $KMS_KEY_ID"
else
  info "Creating KMS key for DNSSEC signing..."

  KMS_KEY_ID=$(aws kms create-key \
    --description "DNSSEC signing key for $DOMAIN" \
    --key-spec ECC_NIST_P256 \
    --key-usage SIGN_VERIFY \
    --region "$REGION" \
    --query 'KeyMetadata.KeyId' \
    --output text)

  info "KMS key created: $KMS_KEY_ID"

  # Create alias
  aws kms create-alias \
    --alias-name "$KMS_KEY_ALIAS" \
    --target-key-id "$KMS_KEY_ID"

  info "KMS alias created: $KMS_KEY_ALIAS"
fi

KMS_KEY_ARN="arn:aws:kms:$REGION:$ACCOUNT_ID:key/$KMS_KEY_ID"
info "KMS Key ARN: $KMS_KEY_ARN"
echo ""

# Step 2: Update KMS key policy
echo "Step 2: Updating KMS key policy..."

# Create policy document
cat > /tmp/kms-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Enable IAM User Permissions",
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::$ACCOUNT_ID:root"
      },
      "Action": "kms:*",
      "Resource": "*"
    },
    {
      "Sid": "Allow Route 53 DNSSEC Service",
      "Effect": "Allow",
      "Principal": {
        "Service": "dnssec-route53.amazonaws.com"
      },
      "Action": [
        "kms:DescribeKey",
        "kms:GetPublicKey",
        "kms:Sign"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "aws:SourceAccount": "$ACCOUNT_ID"
        },
        "ArnLike": {
          "aws:SourceArn": "arn:aws:route53:::hostedzone/*"
        }
      }
    },
    {
      "Sid": "Allow Route 53 DNSSEC to CreateGrant",
      "Effect": "Allow",
      "Principal": {
        "Service": "dnssec-route53.amazonaws.com"
      },
      "Action": "kms:CreateGrant",
      "Resource": "*",
      "Condition": {
        "Bool": {
          "kms:GrantIsForAWSResource": true
        }
      }
    }
  ]
}
EOF

info "Updating KMS key policy..."
aws kms put-key-policy \
  --key-id "$KMS_KEY_ID" \
  --policy-name default \
  --policy file:///tmp/kms-policy.json

rm /tmp/kms-policy.json
info "KMS key policy updated"
echo ""

# Step 3: Enable DNSSEC on hosted zone
echo "Step 3: Enabling DNSSEC on hosted zone..."

# Check if DNSSEC is already enabled
DNSSEC_STATUS=$(aws route53 get-dnssec \
  --hosted-zone-id "$HOSTED_ZONE_ID" \
  --query 'Status.ServeSignature' \
  --output text 2>/dev/null || echo "NOT_ENABLED")

if [ "$DNSSEC_STATUS" = "SIGNING" ]; then
  warn "DNSSEC is already enabled on this hosted zone"
else
  info "Enabling DNSSEC..."

  aws route53 enable-hosted-zone-dnssec \
    --hosted-zone-id "$HOSTED_ZONE_ID" \
    > /dev/null

  info "DNSSEC enabled"
fi
echo ""

# Step 4: Create Key Signing Key (KSK)
echo "Step 4: Creating Key Signing Key..."

KSK_NAME="${DOMAIN//./-}-ksk"

# Check if KSK already exists
KSK_STATUS=$(aws route53 get-dnssec \
  --hosted-zone-id "$HOSTED_ZONE_ID" \
  --query "KeySigningKeys[?Name=='$KSK_NAME'].Status" \
  --output text 2>/dev/null || echo "")

if [ -n "$KSK_STATUS" ]; then
  warn "Key Signing Key already exists: $KSK_NAME (Status: $KSK_STATUS)"
else
  info "Creating Key Signing Key: $KSK_NAME"

  aws route53 create-key-signing-key \
    --hosted-zone-id "$HOSTED_ZONE_ID" \
    --key-management-service-arn "$KMS_KEY_ARN" \
    --name "$KSK_NAME" \
    --status ACTIVE \
    > /dev/null

  info "Key Signing Key created"

  # Wait for KSK to become active
  info "Waiting for KSK to become active..."
  sleep 5
fi
echo ""

# Step 5: Get DS record
echo "Step 5: Retrieving DS record..."

# Wait for DS record to be available
for i in {1..10}; do
  DS_RECORD=$(aws route53 get-dnssec \
    --hosted-zone-id "$HOSTED_ZONE_ID" \
    --query "KeySigningKeys[?Name=='$KSK_NAME'].DSRecord" \
    --output text 2>/dev/null || echo "")

  if [ -n "$DS_RECORD" ]; then
    break
  fi

  if [ $i -lt 10 ]; then
    warn "DS record not yet available, waiting..."
    sleep 5
  else
    error "DS record not available after 50 seconds"
    exit 1
  fi
done

info "DS record retrieved"
echo ""

# Summary
echo "========================================="
echo "  DNSSEC Enabled Successfully!"
echo "========================================="
echo ""
echo "Configuration:"
echo "  Domain:       $DOMAIN"
echo "  Hosted Zone:  $HOSTED_ZONE_ID"
echo "  KMS Key:      $KMS_KEY_ARN"
echo "  KSK Name:     $KSK_NAME"
echo ""
echo "DS Record:"
echo "───────────────────────────────────────"
echo "$DS_RECORD"
echo "───────────────────────────────────────"
echo ""
echo "Next Steps:"
echo ""
echo "1. Add the DS record to your parent domain's DNS:"
echo "   - Log in to your DNS provider (registrar or parent domain manager)"
echo "   - Add a DS record for: $DOMAIN"
echo "   - Paste the DS record shown above"
echo ""
echo "2. Wait for DNS propagation (up to 48 hours)"
echo ""
echo "3. Verify DNSSEC after propagation:"
echo "   dig +dnssec $DOMAIN SOA"
echo "   delv $DOMAIN"
echo ""
echo "4. Online validators:"
echo "   https://dnssec-debugger.verisignlabs.com/$DOMAIN"
echo "   https://dnsviz.net/d/$DOMAIN/dnssec/"
echo ""
echo "For more information, see SECURITY.md and CUSTOM_DNS.md"
echo ""
