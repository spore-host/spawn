# Custom DNS Deployment Guide

This guide helps institutions deploy their own spawn DNS infrastructure with custom domains (e.g., `spore.ucla.edu` instead of `spore.host`).

## Table of Contents

- [Overview](#overview)
- [Use Cases](#use-cases)
- [Architecture](#architecture)
- [Prerequisites](#prerequisites)
- [Deployment Steps](#deployment-steps)
- [Configuration Methods](#configuration-methods)
- [Testing](#testing)
- [Troubleshooting](#troubleshooting)
- [Security Considerations](#security-considerations)
- [Example: UCLA Deployment](#example-ucla-deployment)

## Overview

By default, spawn uses the public `spore.host` domain for automatic DNS registration. Institutions may want to use their own domains for several reasons:

- **Compliance** - Organization policies require using institutional domains
- **Branding** - Prefer `*.spore.ucla.edu` over `*.spore.host`
- **Control** - Full ownership and control of DNS infrastructure
- **Privacy** - DNS records not visible in public zone

This guide walks through deploying a complete DNS infrastructure in your AWS account.

## Use Cases

### Academic Institutions

**Example**: UCLA wants `spore.ucla.edu` for their spawn instances

**Benefits**:
- DNS records clearly identify UCLA resources
- Compliance with university IT policies
- Centralized management by IT department
- Integration with existing `.edu` domain

### Enterprise Organizations

**Example**: Acme Corp wants `spore.acme.com` for their development environments

**Benefits**:
- Corporate branding on all development resources
- Integration with internal DNS systems
- Compliance with corporate security policies
- Cost allocation and chargeback tracking

### Government Agencies

**Example**: Agency wants `spore.agency.gov` for secure development

**Benefits**:
- Compliance with FedRAMP/FISMA requirements
- Isolation from public DNS infrastructure
- Audit trail in agency AWS account
- Integration with GovCloud if needed

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Your AWS Account                         │
│                                                             │
│  ┌─────────────────┐         ┌─────────────────┐          │
│  │  Route53 Zone   │         │  Lambda Function│          │
│  │ spore.ucla.edu  │◄────────│   (Go-based)    │          │
│  │                 │         │                 │          │
│  │  Z123456789ABC  │         │  Validates:     │          │
│  └─────────────────┘         │  - Instance ID  │          │
│                              │  - spawn:managed│          │
│                              │  - IP address   │          │
│                              └────────▲────────┘          │
│                                       │                    │
│  ┌─────────────────────────────────────┐                  │
│  │     API Gateway (Public)            │                  │
│  │  https://xyz.execute-api...         │                  │
│  └────────────────▲────────────────────┘                  │
│                   │                                        │
│  ┌────────────────┴────────────────────┐                  │
│  │  SSM Parameter Store (Optional)     │                  │
│  │  /spawn/dns/domain                  │                  │
│  │  /spawn/dns/api_endpoint            │                  │
│  └─────────────────────────────────────┘                  │
└─────────────────────────────────────────────────────────────┘
                    │
                    │ DNS Delegation (NS records)
                    ▼
┌─────────────────────────────────────────────────────────────┐
│         Parent Domain (ucla.edu)                            │
│                                                             │
│  NS records:                                                │
│  spore.ucla.edu → ns-1234.awsdns-12.com                    │
│                → ns-5678.awsdns-34.net                    │
│                → ns-9012.awsdns-56.org                    │
│                → ns-3456.awsdns-78.co.uk                  │
└─────────────────────────────────────────────────────────────┘
```

### Component Overview

1. **Route53 Hosted Zone** - DNS authority for your subdomain
2. **Lambda Function** - Validates requests and updates DNS
3. **API Gateway** - Public HTTPS endpoint for DNS updates
4. **IAM Role** - Permissions for Lambda to access Route53 and EC2
5. **SSM Parameters** (Optional) - Auto-discovery configuration for users
6. **DNSSEC** (Optional) - Enhanced security with cryptographic signatures

## Prerequisites

### Domain Requirements

You must have:
- Ownership of a domain (e.g., `ucla.edu`)
- Access to update DNS records in the parent domain
- Ability to create NS (nameserver) records

### AWS Account

You need:
- AWS account for hosting DNS infrastructure
- Permission to create:
  - Route53 hosted zones
  - Lambda functions
  - API Gateway endpoints
  - IAM roles and policies
  - SSM parameters

### Tools

Install these tools:
- AWS CLI (`aws`)
- Go 1.21+ (for building Lambda)
- `jq` (for JSON processing)
- `dig` or `nslookup` (for testing)

## Deployment Steps

### Step 1: Create Route53 Hosted Zone

Create a hosted zone for your subdomain:

```bash
# Set your subdomain
SUBDOMAIN="spore.ucla.edu"

# Create hosted zone
aws route53 create-hosted-zone \
  --name "$SUBDOMAIN" \
  --caller-reference "spawn-$(date +%s)" \
  --hosted-zone-config Comment="Spawn DNS for $SUBDOMAIN"
```

**Save the Hosted Zone ID and nameservers from the output:**

```json
{
  "HostedZone": {
    "Id": "/hostedzone/Z123456789ABC",
    "Name": "spore.ucla.edu.",
    ...
  },
  "DelegationSet": {
    "NameServers": [
      "ns-1234.awsdns-12.com",
      "ns-5678.awsdns-34.net",
      "ns-9012.awsdns-56.org",
      "ns-3456.awsdns-78.co.uk"
    ]
  }
}
```

### Step 2: Delegate Subdomain

Add NS records to your parent domain pointing to the hosted zone nameservers:

**In Route53 (if parent domain is in Route53):**

```bash
PARENT_ZONE_ID="Z987654321XYZ"  # Your ucla.edu zone ID

aws route53 change-resource-record-sets \
  --hosted-zone-id "$PARENT_ZONE_ID" \
  --change-batch '{
    "Changes": [{
      "Action": "CREATE",
      "ResourceRecordSet": {
        "Name": "spore.ucla.edu",
        "Type": "NS",
        "TTL": 300,
        "ResourceRecords": [
          {"Value": "ns-1234.awsdns-12.com"},
          {"Value": "ns-5678.awsdns-34.net"},
          {"Value": "ns-9012.awsdns-56.org"},
          {"Value": "ns-3456.awsdns-78.co.uk"}
        ]
      }
    }]
  }'
```

**In other DNS providers:**

Add four NS records for `spore` subdomain pointing to the AWS nameservers.

### Step 3: Build Lambda Function

```bash
# Navigate to spawn repository
cd spawn/lambda/dns-updater

# Build for Linux/AMD64
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go

# Create deployment package
zip function.zip bootstrap
```

### Step 4: Create IAM Role for Lambda

```bash
# Create trust policy
cat > trust-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "lambda.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}
EOF

# Create role
aws iam create-role \
  --role-name SpawnDNSLambdaRole \
  --assume-role-policy-document file://trust-policy.json

# Create permissions policy
HOSTED_ZONE_ID="Z123456789ABC"  # Your zone ID from Step 1

cat > lambda-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "route53:ChangeResourceRecordSets",
        "route53:ListResourceRecordSets"
      ],
      "Resource": "arn:aws:route53:::hostedzone/$HOSTED_ZONE_ID"
    },
    {
      "Effect": "Allow",
      "Action": "ec2:DescribeInstances",
      "Resource": "*"
    },
    {
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

# Attach policy
aws iam put-role-policy \
  --role-name SpawnDNSLambdaRole \
  --policy-name SpawnDNSPermissions \
  --policy-document file://lambda-policy.json
```

### Step 5: Create Lambda Function

```bash
# Get role ARN
ROLE_ARN=$(aws iam get-role --role-name SpawnDNSLambdaRole --query 'Role.Arn' --output text)

# Create Lambda function
aws lambda create-function \
  --function-name spawn-dns-updater \
  --runtime provided.al2023 \
  --role "$ROLE_ARN" \
  --handler bootstrap \
  --zip-file fileb://function.zip \
  --timeout 30 \
  --memory-size 256 \
  --environment Variables="{HOSTED_ZONE_ID=$HOSTED_ZONE_ID,DOMAIN=$SUBDOMAIN}"
```

### Step 6: Create API Gateway

```bash
# Create REST API
API_ID=$(aws apigateway create-rest-api \
  --name "spawn-dns-api" \
  --description "DNS update API for spawn instances" \
  --query 'id' \
  --output text)

# Get root resource ID
ROOT_ID=$(aws apigateway get-resources \
  --rest-api-id "$API_ID" \
  --query 'items[0].id' \
  --output text)

# Create /update-dns resource
RESOURCE_ID=$(aws apigateway create-resource \
  --rest-api-id "$API_ID" \
  --parent-id "$ROOT_ID" \
  --path-part "update-dns" \
  --query 'id' \
  --output text)

# Create POST method
aws apigateway put-method \
  --rest-api-id "$API_ID" \
  --resource-id "$RESOURCE_ID" \
  --http-method POST \
  --authorization-type NONE

# Get Lambda ARN
LAMBDA_ARN=$(aws lambda get-function \
  --function-name spawn-dns-updater \
  --query 'Configuration.FunctionArn' \
  --output text)

# Get AWS account ID and region
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REGION=$(aws configure get region)

# Create Lambda integration
aws apigateway put-integration \
  --rest-api-id "$API_ID" \
  --resource-id "$RESOURCE_ID" \
  --http-method POST \
  --type AWS_PROXY \
  --integration-http-method POST \
  --uri "arn:aws:apigateway:$REGION:lambda:path/2015-03-31/functions/$LAMBDA_ARN/invocations"

# Grant API Gateway permission to invoke Lambda
aws lambda add-permission \
  --function-name spawn-dns-updater \
  --statement-id apigateway-invoke \
  --action lambda:InvokeFunction \
  --principal apigateway.amazonaws.com \
  --source-arn "arn:aws:execute-api:$REGION:$ACCOUNT_ID:$API_ID/*/*"

# Deploy API
aws apigateway create-deployment \
  --rest-api-id "$API_ID" \
  --stage-name prod

# Get API endpoint
API_ENDPOINT="https://$API_ID.execute-api.$REGION.amazonaws.com/prod/update-dns"
echo "API Endpoint: $API_ENDPOINT"
```

### Step 7: Update Lambda with Domain Configuration

Update the Lambda function code to use your custom domain:

```bash
# Edit lambda/dns-updater/main.go
# Change:
#   hostedZoneID = "Z048907324UNXKEK9KX93"
#   domain       = "spore.host"
# To:
#   hostedZoneID = "Z123456789ABC"  # Your zone ID
#   domain       = "spore.ucla.edu"  # Your domain

# Rebuild and redeploy
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go
zip function.zip bootstrap

aws lambda update-function-code \
  --function-name spawn-dns-updater \
  --zip-file fileb://function.zip
```

### Step 8: Configure SSM Parameters (Optional)

Set up auto-discovery for your users:

```bash
# Create SSM parameters
aws ssm put-parameter \
  --name "/spawn/dns/domain" \
  --value "$SUBDOMAIN" \
  --type String \
  --description "Custom DNS domain for spawn instances"

aws ssm put-parameter \
  --name "/spawn/dns/api_endpoint" \
  --value "$API_ENDPOINT" \
  --type String \
  --description "Custom DNS API endpoint for spawn instances"
```

**Update IAM policy for users:**

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": "ssm:GetParameter",
    "Resource": [
      "arn:aws:ssm:*:YOUR_ACCOUNT_ID:parameter/spawn/dns/domain",
      "arn:aws:ssm:*:YOUR_ACCOUNT_ID:parameter/spawn/dns/api_endpoint"
    ]
  }]
}
```

### Step 9: Enable DNSSEC (Optional)

Enable DNSSEC for enhanced security:

```bash
# Create KMS key for DNSSEC
KMS_KEY_ID=$(aws kms create-key \
  --description "DNSSEC signing key for $SUBDOMAIN" \
  --key-spec ECC_NIST_P256 \
  --key-usage SIGN_VERIFY \
  --query 'KeyMetadata.KeyId' \
  --output text)

# Create key policy (see SECURITY.md for full policy)
# Update KMS key policy to allow dnssec-route53.amazonaws.com

# Enable DNSSEC
aws route53 enable-hosted-zone-dnssec \
  --hosted-zone-id "$HOSTED_ZONE_ID"

# Create Key Signing Key
aws route53 create-key-signing-key \
  --hosted-zone-id "$HOSTED_ZONE_ID" \
  --key-management-service-arn "arn:aws:kms:$REGION:$ACCOUNT_ID:key/$KMS_KEY_ID" \
  --name "$SUBDOMAIN-ksk" \
  --status ACTIVE

# Get DS record
DS_RECORD=$(aws route53 get-dnssec \
  --hosted-zone-id "$HOSTED_ZONE_ID" \
  --query 'KeySigningKeys[0].DSRecord' \
  --output text)

echo "Add this DS record to your parent domain:"
echo "$DS_RECORD"
```

Add the DS record to your parent domain's DNS settings.

## Configuration Methods

Spawn supports multiple configuration methods with the following precedence (highest to lowest):

### 1. CLI Flags (Highest Priority)

Override DNS settings per command:

```bash
spawn launch \
  --dns my-instance \
  --dns-domain spore.ucla.edu \
  --dns-api-endpoint https://xyz.execute-api.us-east-1.amazonaws.com/prod/update-dns
```

**Use case**: Testing different DNS configurations

### 2. Environment Variables

Set in shell or CI/CD environment:

```bash
export SPAWN_DNS_DOMAIN="spore.ucla.edu"
export SPAWN_DNS_API_ENDPOINT="https://xyz.execute-api.us-east-1.amazonaws.com/prod/update-dns"

spawn launch --dns my-instance
```

**Use case**: CI/CD pipelines, containerized environments

### 3. Configuration File

Create `~/.spawn/config.yaml`:

```yaml
dns:
  enabled: true
  domain: spore.ucla.edu
  api_endpoint: https://xyz.execute-api.us-east-1.amazonaws.com/prod/update-dns

# Other spawn settings
default_region: us-east-1
default_instance_type: t3.micro
```

**Use case**: Personal workstation, persistent settings

### 4. SSM Parameter Store (Auto-Discovery)

Set parameters in your AWS account:

```bash
aws ssm put-parameter \
  --name "/spawn/dns/domain" \
  --value "spore.ucla.edu" \
  --type String

aws ssm put-parameter \
  --name "/spawn/dns/api_endpoint" \
  --value "https://xyz.execute-api.us-east-1.amazonaws.com/prod/update-dns" \
  --type String
```

spawn will automatically discover these settings when using the AWS account.

**Use case**: Institution-wide deployment, zero configuration for end users

### 5. Default (Lowest Priority)

Falls back to public `spore.host` infrastructure:

```bash
# No configuration needed
spawn launch --dns my-instance
# → Creates my-instance.spore.host
```

**Use case**: Quick start, testing, personal projects

### Precedence Example

```bash
# SSM: spore.ucla.edu
# Config: spore.stanford.edu
# Env: SPAWN_DNS_DOMAIN=spore.mit.edu
# Flag: --dns-domain spore.harvard.edu

spawn launch --dns test --dns-domain spore.harvard.edu
# → Uses spore.harvard.edu (flag wins)

spawn launch --dns test
# → Uses spore.mit.edu (env var wins over config and SSM)
```

## Testing

### Step 1: Verify DNS Delegation

```bash
# Check nameservers
dig NS spore.ucla.edu

# Should show AWS nameservers:
# spore.ucla.edu. 300 IN NS ns-1234.awsdns-12.com.
# spore.ucla.edu. 300 IN NS ns-5678.awsdns-34.net.
# ...
```

### Step 2: Test API Endpoint

From an EC2 instance with `spawn:managed=true` tag:

```bash
# Get instance identity
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" -s)

IDENTITY_DOC=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" \
  -s http://169.254.169.254/latest/dynamic/instance-identity/document | base64 -w0)

IDENTITY_SIG=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" \
  -s http://169.254.169.254/latest/dynamic/instance-identity/signature | tr -d '\n')

PUBLIC_IP=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" \
  -s http://169.254.169.254/latest/meta-data/public-ipv4)

# Call API
curl -X POST "$API_ENDPOINT" \
  -H "Content-Type: application/json" \
  -d "{
    \"instance_identity_document\": \"$IDENTITY_DOC\",
    \"instance_identity_signature\": \"$IDENTITY_SIG\",
    \"record_name\": \"test\",
    \"ip_address\": \"$PUBLIC_IP\",
    \"action\": \"UPSERT\"
  }"

# Expected response:
# {
#   "success": true,
#   "message": "DNS record updated: test.spore.ucla.edu -> 1.2.3.4",
#   "record": "test.spore.ucla.edu",
#   "change_id": "/change/C123...",
#   "timestamp": "2025-12-21T18:00:00Z"
# }
```

### Step 3: Verify DNS Resolution

```bash
# Wait 60 seconds for TTL
sleep 60

# Check DNS resolution
dig test.spore.ucla.edu

# Should return:
# test.spore.ucla.edu. 60 IN A 1.2.3.4
```

### Step 4: Test with spawn CLI

```bash
# With config file
spawn launch --dns my-test --ttl 1h --region us-east-1

# Should output:
# 🌐 DNS: my-test.spore.ucla.edu
#    Connect: ssh user@my-test.spore.ucla.edu

# Verify SSH
ssh user@my-test.spore.ucla.edu
```

## Troubleshooting

### DNS Delegation Not Working

**Symptom**: `dig NS spore.ucla.edu` doesn't return AWS nameservers

**Solution**:
1. Verify NS records in parent domain
2. Wait for DNS propagation (up to 48 hours)
3. Check with multiple DNS servers: `dig @8.8.8.8 NS spore.ucla.edu`

### Lambda Function Fails

**Symptom**: API returns 500 error

**Solution**:
1. Check CloudWatch Logs: `/aws/lambda/spawn-dns-updater`
2. Verify IAM role permissions
3. Check Lambda environment variables
4. Verify hosted zone ID is correct

### API Gateway 403 Forbidden

**Symptom**: API returns 403 when calling endpoint

**Solution**:
1. Verify Lambda permission for API Gateway
2. Check API Gateway deployment
3. Verify endpoint URL is correct
4. Check API Gateway logs

### Instance Validation Fails

**Symptom**: API returns "instance not found" error

**Solution**:
1. Verify instance has `spawn:managed=true` tag
2. Check instance is in running state
3. Verify IP address matches instance public IP
4. For cross-account: Ensure instance identity signature is valid

### SSM Parameters Not Found

**Symptom**: spawn still uses default spore.host

**Solution**:
1. Verify parameters exist: `aws ssm get-parameter --name /spawn/dns/domain`
2. Check IAM permissions for `ssm:GetParameter`
3. Verify AWS_PROFILE is set correctly
4. Check parameter names match exactly

### DNSSEC Validation Errors

**Symptom**: `delv` reports DNSSEC validation failure

**Solution**:
1. Verify DS record added to parent domain
2. Wait for propagation (up to 48 hours)
3. Check KMS key policy allows dnssec-route53.amazonaws.com
4. Verify Key Signing Key is ACTIVE

## Security Considerations

### Least Privilege

The Lambda function should have minimal permissions:

```json
{
  "Effect": "Allow",
  "Action": [
    "route53:ChangeResourceRecordSets",
    "route53:ListResourceRecordSets"
  ],
  "Resource": "arn:aws:route53:::hostedzone/Z123456789ABC"
}
```

**Do NOT grant**:
- `route53:*` (too broad)
- Permissions to other hosted zones
- DeleteHostedZone permission

### API Gateway Security

Consider adding:

1. **API Keys** - Require API key for requests
2. **Usage Plans** - Rate limit per API key
3. **WAF** - Web Application Firewall rules
4. **CloudWatch Alarms** - Alert on anomalies

### CloudWatch Logs

Enable encryption for CloudWatch Logs:

```bash
aws logs create-log-group \
  --log-group-name /aws/lambda/spawn-dns-updater \
  --kms-key-id arn:aws:kms:region:account:key/key-id
```

### Audit Trail

Enable CloudTrail for Route53 API calls:

```bash
aws cloudtrail create-trail \
  --name spawn-dns-audit \
  --s3-bucket-name your-audit-bucket \
  --include-global-service-events \
  --is-multi-region-trail
```

## Example: UCLA Deployment

Complete walkthrough for UCLA deploying `spore.ucla.edu`:

### Overview

- **Parent Domain**: ucla.edu (managed in Route53)
- **Subdomain**: spore.ucla.edu
- **AWS Account**: 123456789012 (UCLA IT account)
- **Users**: ~1000 researchers and students

### Step 1: Planning

UCLA IT team decides:
- Deploy in isolated AWS account (not production account)
- Use SSM Parameter Store for auto-discovery
- Enable DNSSEC for security
- Set up CloudWatch alarms for monitoring

### Step 2: Deployment

```bash
# Set variables
SUBDOMAIN="spore.ucla.edu"
PARENT_ZONE_ID="Z999888777666"  # ucla.edu zone

# Create hosted zone
ZONE_ID=$(aws route53 create-hosted-zone \
  --name "$SUBDOMAIN" \
  --caller-reference "spawn-$(date +%s)" \
  --query 'HostedZone.Id' \
  --output text | cut -d/ -f3)

# Get nameservers
NAMESERVERS=$(aws route53 get-hosted-zone \
  --id "$ZONE_ID" \
  --query 'DelegationSet.NameServers' \
  --output json)

# Delegate subdomain (manual step in UCLA's DNS)
# Add NS records for spore.ucla.edu pointing to $NAMESERVERS

# Deploy Lambda and API Gateway (using deployment script)
./scripts/deploy-custom-dns.sh \
  --hosted-zone-id "$ZONE_ID" \
  --domain "$SUBDOMAIN"

# Enable DNSSEC
./scripts/enable-dnssec.sh \
  --hosted-zone-id "$ZONE_ID" \
  --domain "$SUBDOMAIN"

# Configure SSM
aws ssm put-parameter \
  --name "/spawn/dns/domain" \
  --value "$SUBDOMAIN" \
  --type String \
  --description "UCLA spawn DNS domain"

aws ssm put-parameter \
  --name "/spawn/dns/api_endpoint" \
  --value "https://abc123.execute-api.us-east-1.amazonaws.com/prod/update-dns" \
  --type String \
  --description "UCLA spawn DNS API endpoint"
```

### Step 3: User Setup

UCLA distributes this guide to users:

```markdown
## Using spawn at UCLA

spawn is configured to automatically use UCLA's DNS domain.

1. Configure AWS credentials:
   ```bash
   aws configure --profile ucla
   ```

2. Launch instances with DNS:
   ```bash
   AWS_PROFILE=ucla spawn launch --dns my-research --ttl 8h
   ```

3. Connect:
   ```bash
   ssh $USER@my-research.spore.ucla.edu
   ```

No additional configuration needed - spawn automatically detects UCLA's DNS settings!
```

### Step 4: Monitoring

UCLA IT sets up monitoring:

```bash
# CloudWatch alarm for API errors
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-dns-errors \
  --alarm-description "Alert on DNS API errors" \
  --metric-name 5XXError \
  --namespace AWS/ApiGateway \
  --statistic Sum \
  --period 300 \
  --evaluation-periods 1 \
  --threshold 10 \
  --comparison-operator GreaterThanThreshold

# Lambda error alarm
aws cloudwatch put-metric-alarm \
  --alarm-name spawn-dns-lambda-errors \
  --alarm-description "Alert on Lambda errors" \
  --metric-name Errors \
  --namespace AWS/Lambda \
  --dimensions Name=FunctionName,Value=spawn-dns-updater \
  --statistic Sum \
  --period 300 \
  --evaluation-periods 1 \
  --threshold 5 \
  --comparison-operator GreaterThanThreshold
```

### Results

- ✅ 1000+ users automatically use `*.spore.ucla.edu`
- ✅ Zero configuration required for end users
- ✅ DNSSEC enabled for security
- ✅ Complete audit trail in CloudWatch
- ✅ Compliance with UCLA IT policies

## Deployment Scripts

### Quick Deploy Script

Save as `scripts/deploy-custom-dns.sh`:

```bash
#!/bin/bash
set -e

# Usage: ./deploy-custom-dns.sh --hosted-zone-id Z123... --domain spore.ucla.edu

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
    *)
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

if [ -z "$HOSTED_ZONE_ID" ] || [ -z "$DOMAIN" ]; then
  echo "Usage: $0 --hosted-zone-id Z123... --domain spore.ucla.edu"
  exit 1
fi

echo "Deploying spawn DNS infrastructure..."
echo "  Hosted Zone: $HOSTED_ZONE_ID"
echo "  Domain: $DOMAIN"

# Build Lambda
echo "Building Lambda function..."
cd lambda/dns-updater
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go
zip function.zip bootstrap
cd ../..

# Create IAM role
echo "Creating IAM role..."
aws iam create-role \
  --role-name SpawnDNSLambdaRole \
  --assume-role-policy-document file://deployment/trust-policy.json \
  || echo "Role already exists"

# Update IAM policy
cat > /tmp/lambda-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "route53:ChangeResourceRecordSets",
        "route53:ListResourceRecordSets"
      ],
      "Resource": "arn:aws:route53:::hostedzone/$HOSTED_ZONE_ID"
    },
    {
      "Effect": "Allow",
      "Action": "ec2:DescribeInstances",
      "Resource": "*"
    },
    {
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

aws iam put-role-policy \
  --role-name SpawnDNSLambdaRole \
  --policy-name SpawnDNSPermissions \
  --policy-document file:///tmp/lambda-policy.json

# Create/update Lambda
echo "Deploying Lambda function..."
ROLE_ARN=$(aws iam get-role --role-name SpawnDNSLambdaRole --query 'Role.Arn' --output text)

aws lambda create-function \
  --function-name spawn-dns-updater \
  --runtime provided.al2023 \
  --role "$ROLE_ARN" \
  --handler bootstrap \
  --zip-file fileb://lambda/dns-updater/function.zip \
  --timeout 30 \
  --memory-size 256 \
  --environment Variables="{HOSTED_ZONE_ID=$HOSTED_ZONE_ID,DOMAIN=$DOMAIN}" \
  2>/dev/null || \
aws lambda update-function-code \
  --function-name spawn-dns-updater \
  --zip-file fileb://lambda/dns-updater/function.zip

# Create API Gateway (implementation details omitted for brevity)
# See full script in deployment/ directory

echo "✅ Deployment complete!"
echo "Next steps:"
echo "1. Add NS records to parent domain"
echo "2. Configure SSM parameters (optional)"
echo "3. Enable DNSSEC (optional)"
echo "4. Test with: spawn launch --dns test"
```

## Additional Resources

- [DNS_SETUP.md](DNS_SETUP.md) - Original spore.host setup guide
- [SECURITY.md](SECURITY.md) - Security model and best practices
- [IAM_PERMISSIONS.md](IAM_PERMISSIONS.md) - Required IAM permissions
- [AWS Route53 Documentation](https://docs.aws.amazon.com/route53/)
- [AWS Lambda Documentation](https://docs.aws.amazon.com/lambda/)
- [DNSSEC Best Practices](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/dns-configuring-dnssec.html)

---

**Questions?** Open an issue on GitHub or contact your IT department for institution-specific deployment support.

**Last Updated**: 2025-12-21
**Version**: 1.0.0
