# How-To: Multi-Account Setup

Configure spawn across multiple AWS accounts using AWS Organizations.

## Overview

### Why Multi-Account?

**Security Benefits:**
- Blast radius containment (compromised account doesn't affect others)
- Separate billing and cost tracking
- Environment isolation (dev/staging/prod)
- Compliance boundaries (PCI, HIPAA workloads isolated)

**Organizational Benefits:**
- Team/project separation
- Resource quotas per account
- Independent IAM policies
- Simplified auditing

### Architecture

```
Management Account (123456789012)
├── Infrastructure Account (966362334030)
│   ├── S3 buckets (spawn-binaries-*)
│   ├── Lambda functions (DNS updater)
│   ├── Route53 (spore.host)
│   └── CloudFront
├── Development Account (435415984226)
│   ├── EC2 instances (dev/test)
│   └── DynamoDB tables (dev)
├── Production Account (789012345678)
│   ├── EC2 instances (prod)
│   └── DynamoDB tables (prod)
└── Data Science Account (345678901234)
    └── EC2 instances (ML workloads)
```

---

## AWS Organizations Setup

### Create Organization

**In management account:**
```bash
# Create organization
aws organizations create-organization \
  --feature-set ALL

# Get organization ID
ORG_ID=$(aws organizations describe-organization \
  --query 'Organization.Id' \
  --output text)

echo "Organization ID: $ORG_ID"
```

### Create Member Accounts

```bash
# Create infrastructure account
INFRA_ACCOUNT=$(aws organizations create-account \
  --email infra@example.com \
  --account-name "spawn-infrastructure" \
  --query 'CreateAccountStatus.AccountId' \
  --output text)

# Create development account
DEV_ACCOUNT=$(aws organizations create-account \
  --email dev@example.com \
  --account-name "spawn-development" \
  --query 'CreateAccountStatus.AccountId' \
  --output text)

# Create production account
PROD_ACCOUNT=$(aws organizations create-account \
  --email prod@example.com \
  --account-name "spawn-production" \
  --query 'CreateAccountStatus.AccountId' \
  --output text)

echo "Infrastructure: $INFRA_ACCOUNT"
echo "Development: $DEV_ACCOUNT"
echo "Production: $PROD_ACCOUNT"
```

### Create Organizational Units

```bash
# Create OUs for organization
WORKLOADS_OU=$(aws organizations create-organizational-unit \
  --parent-id $ROOT_ID \
  --name "Workloads" \
  --query 'OrganizationalUnit.Id' \
  --output text)

INFRASTRUCTURE_OU=$(aws organizations create-organizational-unit \
  --parent-id $ROOT_ID \
  --name "Infrastructure" \
  --query 'OrganizationalUnit.Id' \
  --output text)

# Move accounts to OUs
aws organizations move-account \
  --account-id $INFRA_ACCOUNT \
  --source-parent-id $ROOT_ID \
  --destination-parent-id $INFRASTRUCTURE_OU

aws organizations move-account \
  --account-id $DEV_ACCOUNT \
  --source-parent-id $ROOT_ID \
  --destination-parent-id $WORKLOADS_OU
```

---

## Cross-Account IAM Roles

### Problem
Instances in development account need to access S3 buckets in infrastructure account.

### Solution: Cross-Account IAM Roles

**In infrastructure account:**
```bash
# Create trust policy allowing development account
cat > trust-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "AWS": "arn:aws:iam::435415984226:root",
      "Service": "ec2.amazonaws.com"
    },
    "Action": "sts:AssumeRole",
    "Condition": {
      "StringEquals": {
        "sts:ExternalId": "spawn-cross-account-access"
      }
    }
  }]
}
EOF

# Create cross-account role
aws iam create-role \
  --role-name spawn-cross-account-s3-access \
  --assume-role-policy-document file://trust-policy.json \
  --description "Allow spawn instances from dev account to access S3"

# Attach policy for S3 access
cat > s3-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "s3:GetObject",
      "s3:PutObject"
    ],
    "Resource": [
      "arn:aws:s3:::spawn-binaries-*/*",
      "arn:aws:s3:::spawn-results-*/*"
    ]
  }]
}
EOF

aws iam put-role-policy \
  --role-name spawn-cross-account-s3-access \
  --policy-name s3-access \
  --policy-document file://s3-policy.json

# Get role ARN
ROLE_ARN=$(aws iam get-role \
  --role-name spawn-cross-account-s3-access \
  --query 'Role.Arn' \
  --output text)

echo "Cross-account role ARN: $ROLE_ARN"
```

**In development account:**
```bash
# Create instance role that can assume cross-account role
cat > instance-role-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": "sts:AssumeRole",
    "Resource": "$ROLE_ARN"
  }]
}
EOF

aws iam create-role \
  --role-name spawn-instance-role \
  --assume-role-policy-document file://ec2-trust-policy.json

aws iam put-role-policy \
  --role-name spawn-instance-role \
  --policy-name cross-account-assume \
  --policy-document file://instance-role-policy.json

# Attach instance profile
aws iam create-instance-profile \
  --instance-profile-name spawn-instance-profile

aws iam add-role-to-instance-profile \
  --instance-profile-name spawn-instance-profile \
  --role-name spawn-instance-role
```

**Use in user-data:**
```bash
spawn launch \
  --instance-type c7i.xlarge \
  --iam-instance-profile spawn-instance-profile \
  --user-data "
    # Assume cross-account role
    ROLE_ARN='arn:aws:iam::966362334030:role/spawn-cross-account-s3-access'

    CREDS=\$(aws sts assume-role \
      --role-arn \$ROLE_ARN \
      --role-session-name spawn-instance-\$INSTANCE_ID \
      --external-id spawn-cross-account-access \
      --query 'Credentials' \
      --output json)

    export AWS_ACCESS_KEY_ID=\$(echo \$CREDS | jq -r '.AccessKeyId')
    export AWS_SECRET_ACCESS_KEY=\$(echo \$CREDS | jq -r '.SecretAccessKey')
    export AWS_SESSION_TOKEN=\$(echo \$CREDS | jq -r '.SessionToken')

    # Now can access S3 in infrastructure account
    aws s3 cp s3://spawn-binaries-us-east-1/spored /usr/local/bin/spored
  "
```

---

## Centralized Billing

### Problem
Track costs across all accounts.

### Solution: Consolidated Billing with Cost Allocation Tags

**Enable cost allocation tags (management account):**
```bash
# Activate cost allocation tags
aws ce activate-cost-allocation-tag \
  --tag-keys spawn project owner environment

# Create cost report
aws cur put-report-definition \
  --report-definition '{
    "ReportName": "spawn-daily-costs",
    "TimeUnit": "DAILY",
    "Format": "Parquet",
    "Compression": "Parquet",
    "AdditionalSchemaElements": ["RESOURCES"],
    "S3Bucket": "spawn-billing-reports",
    "S3Prefix": "reports",
    "S3Region": "us-east-1",
    "AdditionalArtifacts": ["ATHENA"],
    "RefreshClosedReports": true,
    "ReportVersioning": "OVERWRITE_REPORT"
  }'
```

**Query costs by account:**
```bash
# Get costs by account (last 30 days)
aws ce get-cost-and-usage \
  --time-period Start=$(date -d '30 days ago' +%Y-%m-%d),End=$(date +%Y-%m-%d) \
  --granularity DAILY \
  --metrics BlendedCost \
  --group-by Type=DIMENSION,Key=LINKED_ACCOUNT \
  --filter '{
    "Tags": {
      "Key": "spawn",
      "Values": ["true"]
    }
  }'
```

**Create cost dashboard:**
```python
#!/usr/bin/env python3
# create-cost-dashboard.py

import boto3
import json

cloudwatch = boto3.client('cloudwatch')

dashboard = {
    "widgets": [
        {
            "type": "metric",
            "properties": {
                "title": "Daily Cost by Account",
                "metrics": [
                    ["AWS/Billing", "EstimatedCharges", {"stat": "Maximum"}]
                ],
                "period": 86400,
                "stat": "Maximum",
                "region": "us-east-1",
                "yAxis": {"left": {"label": "USD"}}
            }
        }
    ]
}

cloudwatch.put_dashboard(
    DashboardName='spawn-multi-account-costs',
    DashboardBody=json.dumps(dashboard)
)

print("Cost dashboard created")
```

---

## Service Control Policies (SCPs)

### Problem
Prevent accidental resource creation in wrong regions or accounts.

### Solution: Organization-wide SCPs

**Restrict to allowed regions:**
```bash
# Create SCP to restrict regions
cat > region-restriction.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Deny",
    "Action": "*",
    "Resource": "*",
    "Condition": {
      "StringNotEquals": {
        "aws:RequestedRegion": [
          "us-east-1",
          "us-west-2"
        ]
      },
      "ForAllValues:StringNotEquals": {
        "aws:RequestedRegion": [
          "us-east-1",
          "us-west-2"
        ]
      }
    }
  }]
}
EOF

# Attach to workloads OU
aws organizations create-policy \
  --content file://region-restriction.json \
  --description "Restrict to us-east-1 and us-west-2" \
  --name region-restriction \
  --type SERVICE_CONTROL_POLICY

POLICY_ID=$(aws organizations list-policies --filter SERVICE_CONTROL_POLICY \
  --query 'Policies[?Name==`region-restriction`].Id' \
  --output text)

aws organizations attach-policy \
  --policy-id $POLICY_ID \
  --target-id $WORKLOADS_OU
```

**Enforce tagging:**
```bash
# Require spawn tag on all EC2 instances
cat > tagging-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Deny",
    "Action": "ec2:RunInstances",
    "Resource": "arn:aws:ec2:*:*:instance/*",
    "Condition": {
      "StringNotEquals": {
        "aws:RequestTag/spawn": "true"
      }
    }
  }]
}
EOF

aws organizations create-policy \
  --content file://tagging-policy.json \
  --description "Require spawn tag" \
  --name spawn-tagging \
  --type SERVICE_CONTROL_POLICY

POLICY_ID=$(aws organizations list-policies --filter SERVICE_CONTROL_POLICY \
  --query 'Policies[?Name==`spawn-tagging`].Id' \
  --output text)

aws organizations attach-policy \
  --policy-id $POLICY_ID \
  --target-id $WORKLOADS_OU
```

---

## Centralized DNS

### Problem
Instances in different accounts need consistent DNS naming.

### Solution: Shared Route53 hosted zone

**In infrastructure account (create hosted zone):**
```bash
# Create private hosted zone
HOSTED_ZONE_ID=$(aws route53 create-hosted-zone \
  --name spore.host \
  --caller-reference $(date +%s) \
  --hosted-zone-config PrivateZone=true \
  --vpc VPCRegion=us-east-1,VPCId=vpc-infra \
  --query 'HostedZone.Id' \
  --output text)

echo "Hosted zone: $HOSTED_ZONE_ID"
```

**Associate VPC from other accounts:**
```bash
# In infrastructure account, authorize association
aws route53 create-vpc-association-authorization \
  --hosted-zone-id $HOSTED_ZONE_ID \
  --vpc VPCRegion=us-east-1,VPCId=vpc-dev-account

# In development account, complete association
aws route53 associate-vpc-with-hosted-zone \
  --hosted-zone-id $HOSTED_ZONE_ID \
  --vpc VPCRegion=us-east-1,VPCId=vpc-dev-account

# Delete authorization (no longer needed)
# Back in infrastructure account:
aws route53 delete-vpc-association-authorization \
  --hosted-zone-id $HOSTED_ZONE_ID \
  --vpc VPCRegion=us-east-1,VPCId=vpc-dev-account
```

**Update spored to use shared DNS:**
```bash
# In development account, update spored config
spawn launch \
  --user-data "
    # Configure spored with DNS Lambda in infrastructure account
    sudo spored config set dns.lambda_arn arn:aws:lambda:us-east-1:966362334030:function:spawn-dns-updater
    sudo spored config set dns.hosted_zone_id $HOSTED_ZONE_ID
  "
```

---

## VPC Peering

### Problem
Instances in different accounts need to communicate privately.

### Solution: Cross-Account VPC Peering

**Create peering connection:**
```bash
# In development account, create peering request
PEERING_CONNECTION_ID=$(aws ec2 create-vpc-peering-connection \
  --vpc-id vpc-dev \
  --peer-vpc-id vpc-prod \
  --peer-owner-id 789012345678 \
  --peer-region us-east-1 \
  --query 'VpcPeeringConnection.VpcPeeringConnectionId' \
  --output text)

echo "Peering connection: $PEERING_CONNECTION_ID"

# In production account, accept peering request
aws ec2 accept-vpc-peering-connection \
  --vpc-peering-connection-id $PEERING_CONNECTION_ID

# In development account, add route to prod VPC
aws ec2 create-route \
  --route-table-id rtb-dev \
  --destination-cidr-block 10.1.0.0/16 \
  --vpc-peering-connection-id $PEERING_CONNECTION_ID

# In production account, add route to dev VPC
aws ec2 create-route \
  --route-table-id rtb-prod \
  --destination-cidr-block 10.0.0.0/16 \
  --vpc-peering-connection-id $PEERING_CONNECTION_ID
```

**Update security groups:**
```bash
# In development account, allow traffic from prod
aws ec2 authorize-security-group-ingress \
  --group-id sg-dev \
  --protocol tcp \
  --port 22 \
  --cidr 10.1.0.0/16

# In production account, allow traffic from dev
aws ec2 authorize-security-group-ingress \
  --group-id sg-prod \
  --protocol tcp \
  --port 22 \
  --cidr 10.0.0.0/16
```

---

## AWS CLI Profile Configuration

### Problem
Manage credentials for multiple accounts.

### Solution: Named profiles in ~/.aws/config

**Configure profiles:**
```ini
# ~/.aws/config

[profile management]
region = us-east-1
output = json

[profile infra]
region = us-east-1
output = json
role_arn = arn:aws:iam::966362334030:role/OrganizationAccountAccessRole
source_profile = management

[profile dev]
region = us-east-1
output = json
role_arn = arn:aws:iam::435415984226:role/OrganizationAccountAccessRole
source_profile = management

[profile prod]
region = us-east-1
output = json
role_arn = arn:aws:iam::789012345678:role/OrganizationAccountAccessRole
source_profile = management
mfa_serial = arn:aws:iam::123456789012:mfa/username
```

**Usage:**
```bash
# Launch in development account
AWS_PROFILE=dev spawn launch --instance-type c7i.xlarge

# Upload binary to infrastructure account
AWS_PROFILE=infra aws s3 cp spored s3://spawn-binaries-us-east-1/

# Launch in production account (requires MFA)
AWS_PROFILE=prod spawn launch --instance-type c7i.xlarge
```

---

## spawn CLI Configuration

### Problem
Simplify multi-account usage in spawn.

### Solution: spawn config with account aliases

**Configure spawn:**
```bash
# Add account aliases
spawn config set accounts.dev.profile dev
spawn config set accounts.dev.account_id 435415984226
spawn config set accounts.dev.regions us-east-1,us-west-2

spawn config set accounts.prod.profile prod
spawn config set accounts.prod.account_id 789012345678
spawn config set accounts.prod.regions us-east-1

# Set default account
spawn config set default_account dev
```

**Usage:**
```bash
# Launch in default account (dev)
spawn launch --instance-type c7i.xlarge

# Launch in specific account
spawn launch --account prod --instance-type c7i.xlarge

# List instances across all accounts
spawn list --all-accounts
```

---

## Automated Account Provisioning

### Problem
Onboard new teams with consistent account setup.

### Solution: Terraform/CloudFormation for account baseline

**Terraform module:**
```hcl
# modules/spawn-account/main.tf

resource "aws_organizations_account" "member" {
  name  = var.account_name
  email = var.account_email
}

resource "aws_iam_role" "spawn_instance_role" {
  provider = aws.member

  name = "spawn-instance-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "spawn_instance_s3" {
  provider = aws.member

  role       = aws_iam_role.spawn_instance_role.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"
}

resource "aws_dynamodb_table" "spawn_metadata" {
  provider = aws.member

  name           = "spawn-instances"
  billing_mode   = "PAY_PER_REQUEST"
  hash_key       = "instance_id"

  attribute {
    name = "instance_id"
    type = "S"
  }

  tags = {
    spawn = "true"
  }
}

# VPC setup
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "3.0"

  providers = {
    aws = aws.member
  }

  name = "spawn-vpc"
  cidr = var.vpc_cidr

  azs             = var.availability_zones
  private_subnets = var.private_subnet_cidrs
  public_subnets  = var.public_subnet_cidrs

  enable_nat_gateway = true
  enable_vpn_gateway = false

  tags = {
    spawn = "true"
  }
}
```

**Usage:**
```hcl
# main.tf

module "data_science_account" {
  source = "./modules/spawn-account"

  account_name  = "spawn-data-science"
  account_email = "data-science@example.com"
  vpc_cidr      = "10.2.0.0/16"

  private_subnet_cidrs = ["10.2.1.0/24", "10.2.2.0/24"]
  public_subnet_cidrs  = ["10.2.101.0/24", "10.2.102.0/24"]

  availability_zones = ["us-east-1a", "us-east-1b"]
}
```

---

## Cost Optimization

### Shared Resources

**Infrastructure account (shared):**
- S3 buckets (spawn-binaries-*)
- Lambda functions (DNS, automation)
- AMIs (shared via permissions)
- NAT Gateway (if using VPC peering)

**Workload accounts (per-account):**
- EC2 instances
- EBS volumes
- DynamoDB tables

**Savings:**
- Single NAT Gateway shared via VPC peering: $32/month savings per account
- Single S3 bucket instead of per-account: Simplified management
- Shared AMIs: No storage duplication

### Reserved Instances/Savings Plans

**In management account:**
```bash
# Purchase Savings Plan (applies to all accounts)
aws savingsplans create-savings-plan \
  --savings-plan-offering-id offering-xxx \
  --commitment 100.0 \
  --upfront-payment-amount 0.0 \
  --purchase-time $(date -u +%Y-%m-%dT%H:%M:%S.000Z) \
  --savingsplan-type EC2InstanceSavingsPlan \
  --term OneYear \
  --payment-option NoUpfront
```

---

## Compliance and Governance

### AWS Config Rules

**Enforce compliance across accounts:**
```bash
# Deploy config rule to all accounts
aws configservice put-config-rule \
  --config-rule '{
    "ConfigRuleName": "required-spawn-tag",
    "Description": "Checks that EC2 instances have spawn tag",
    "Scope": {
      "ComplianceResourceTypes": ["AWS::EC2::Instance"]
    },
    "Source": {
      "Owner": "AWS",
      "SourceIdentifier": "REQUIRED_TAGS"
    },
    "InputParameters": "{\"tag1Key\":\"spawn\"}"
  }'
```

### CloudTrail Logging

**Centralized logging in management account:**
```bash
# Create organization trail
aws cloudtrail create-trail \
  --name spawn-organization-trail \
  --s3-bucket-name spawn-cloudtrail-logs \
  --is-organization-trail \
  --is-multi-region-trail

# Enable logging
aws cloudtrail start-logging \
  --name spawn-organization-trail
```

---

## Troubleshooting

### Cross-Account Access Denied

**Problem:** Instance can't assume cross-account role.

**Solution:**
```bash
# Verify trust policy allows source account
aws iam get-role --role-name spawn-cross-account-s3-access

# Verify external ID matches
# Check instance role has sts:AssumeRole permission
aws iam get-role-policy --role-name spawn-instance-role --policy-name cross-account-assume

# Test manually
aws sts assume-role \
  --role-arn arn:aws:iam::966362334030:role/spawn-cross-account-s3-access \
  --role-session-name test \
  --external-id spawn-cross-account-access
```

### VPC Peering Connection Fails

**Problem:** Can't connect between peered VPCs.

**Causes:**
1. CIDR blocks overlap
2. Routes not configured
3. Security groups blocking traffic

**Solution:**
```bash
# Check peering status
aws ec2 describe-vpc-peering-connections \
  --vpc-peering-connection-ids $PEERING_CONNECTION_ID

# Verify routes exist
aws ec2 describe-route-tables --filters "Name=vpc-id,Values=vpc-dev"

# Check security group rules
aws ec2 describe-security-groups --group-ids sg-dev
```

---

## See Also

- [How-To: Security & IAM](security-iam.md) - IAM best practices
- [How-To: Custom Networking](custom-networking.md) - VPC setup
- [How-To: Cost Optimization](cost-optimization.md) - Multi-account savings
- [AWS Organizations Documentation](https://docs.aws.amazon.com/organizations/)
