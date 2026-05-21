#!/bin/bash
set -e

# Setup custom domain (api.spore.host) for Dashboard API
# Usage: ./setup-dashboard-domain.sh [aws-profile]

PROFILE=${1:-default}
API_NAME="spawn-dashboard-api"
REGION="us-east-1"
DOMAIN_NAME="api.spore.host"
HOSTED_ZONE_NAME="spore.host"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Setting up Custom Domain for Dashboard API"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Profile: $PROFILE"
echo "Domain:  $DOMAIN_NAME"
echo ""

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
RED='\033[0;31m'
NC='\033[0m'

# Get API Gateway ID
echo "→ Finding API Gateway..."
API_ID=$(aws apigatewayv2 get-apis \
    --profile "$PROFILE" \
    --region "$REGION" \
    --query "Items[?Name=='${API_NAME}'].ApiId" \
    --output text)

if [ -z "$API_ID" ]; then
    echo -e "${RED}✗ API Gateway not found${NC}"
    echo "Run: ./setup-dashboard-api-gateway.sh $PROFILE"
    exit 1
fi

API_ENDPOINT=$(aws apigatewayv2 get-api --profile "$PROFILE" --region "$REGION" --api-id "$API_ID" --query 'ApiEndpoint' --output text)
echo -e "  ${GREEN}✓${NC} API found: $API_ID"
echo -e "  ${BLUE}→${NC} Current endpoint: $API_ENDPOINT"

# Get hosted zone ID
echo "→ Finding Route53 hosted zone..."
HOSTED_ZONE_ID=$(aws route53 list-hosted-zones \
    --profile "$PROFILE" \
    --query "HostedZones[?Name=='${HOSTED_ZONE_NAME}.'].Id" \
    --output text | sed 's|/hostedzone/||')

if [ -z "$HOSTED_ZONE_ID" ]; then
    echo -e "${RED}✗ Hosted zone not found: ${HOSTED_ZONE_NAME}${NC}"
    exit 1
fi

echo -e "  ${GREEN}✓${NC} Hosted zone: $HOSTED_ZONE_ID"

# Check if ACM certificate exists for *.spore.host
echo "→ Checking for ACM certificate..."
CERT_ARN=$(aws acm list-certificates \
    --profile "$PROFILE" \
    --region "$REGION" \
    --query "CertificateSummaryList[?DomainName=='*.spore.host' || DomainName=='api.spore.host'].CertificateArn" \
    --output text | head -n1)

if [ -z "$CERT_ARN" ]; then
    echo -e "${YELLOW}! No certificate found for *.spore.host or api.spore.host${NC}"
    echo ""
    echo "You need to request an ACM certificate:"
    echo "  1. Go to: https://console.aws.amazon.com/acm/home?region=${REGION}"
    echo "  2. Request a public certificate"
    echo "  3. Domain name: *.spore.host (wildcard) or api.spore.host"
    echo "  4. Validation method: DNS validation"
    echo "  5. Add CNAME records to Route53 for validation"
    echo ""
    echo "Or run this command:"
    echo "  aws acm request-certificate \\"
    echo "    --profile $PROFILE \\"
    echo "    --region $REGION \\"
    echo "    --domain-name api.spore.host \\"
    echo "    --validation-method DNS"
    echo ""
    exit 1
fi

echo -e "  ${GREEN}✓${NC} Certificate: $CERT_ARN"

# Create custom domain in API Gateway
echo "→ Creating custom domain in API Gateway..."
DOMAIN_NAME_ID=$(aws apigatewayv2 create-domain-name \
    --profile "$PROFILE" \
    --region "$REGION" \
    --domain-name "$DOMAIN_NAME" \
    --domain-name-configurations CertificateArn="$CERT_ARN" \
    --tags spawn:managed=true,spawn:purpose=dashboard-api \
    --query 'DomainName' \
    --output text 2>/dev/null || echo "$DOMAIN_NAME")

echo -e "  ${GREEN}✓${NC} Custom domain created: $DOMAIN_NAME_ID"

# Get the API Gateway domain name and hosted zone ID for DNS
API_GW_INFO=$(aws apigatewayv2 get-domain-name \
    --profile "$PROFILE" \
    --region "$REGION" \
    --domain-name "$DOMAIN_NAME" \
    --query 'DomainNameConfigurations[0].[ApiGatewayDomainName,HostedZoneId]' \
    --output text)

API_GW_DOMAIN=$(echo "$API_GW_INFO" | awk '{print $1}')
API_GW_HOSTED_ZONE=$(echo "$API_GW_INFO" | awk '{print $2}')

echo -e "  ${BLUE}→${NC} Target domain: $API_GW_DOMAIN"
echo -e "  ${BLUE}→${NC} Target zone: $API_GW_HOSTED_ZONE"

# Create API mapping
echo "→ Creating API mapping..."
aws apigatewayv2 create-api-mapping \
    --profile "$PROFILE" \
    --region "$REGION" \
    --domain-name "$DOMAIN_NAME" \
    --api-id "$API_ID" \
    --stage '$default' \
    &>/dev/null || true

echo -e "  ${GREEN}✓${NC} API mapping created"

# Create Route53 A record
echo "→ Creating Route53 A record..."
cat > /tmp/route53-change.json <<EOF
{
  "Changes": [
    {
      "Action": "UPSERT",
      "ResourceRecordSet": {
        "Name": "$DOMAIN_NAME",
        "Type": "A",
        "AliasTarget": {
          "HostedZoneId": "$API_GW_HOSTED_ZONE",
          "DNSName": "$API_GW_DOMAIN",
          "EvaluateTargetHealth": false
        }
      }
    }
  ]
}
EOF

CHANGE_ID=$(aws route53 change-resource-record-sets \
    --profile "$PROFILE" \
    --hosted-zone-id "$HOSTED_ZONE_ID" \
    --change-batch file:///tmp/route53-change.json \
    --query 'ChangeInfo.Id' \
    --output text)

echo -e "  ${GREEN}✓${NC} DNS record created: $CHANGE_ID"

# Clean up
rm -f /tmp/route53-change.json

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅ Custom Domain Setup Complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Domain Details:"
echo "  Custom Domain: https://$DOMAIN_NAME"
echo "  API Gateway:   $API_GW_DOMAIN"
echo "  Certificate:   $CERT_ARN"
echo ""
echo "DNS propagation may take a few minutes."
echo ""
echo "Test the API:"
echo "  curl https://$DOMAIN_NAME/api/user/profile"
echo ""
