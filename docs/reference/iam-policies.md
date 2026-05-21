# IAM Policies Reference

Complete reference for IAM policies used by spawn.

## Overview

spawn uses IAM roles in two contexts:

1. **CLI User Permissions** - What your AWS user needs to run spawn
2. **Instance Profiles** - What instances can access

## CLI User Permissions

### Minimal Policy

Required permissions for spawn CLI:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeImages",
        "ec2:CreateTags",
        "ec2:TerminateInstances",
        "ec2:CreateSecurityGroup",
        "ec2:AuthorizeSecurityGroupIngress",
        "ec2:CreateKeyPair",
        "ec2:ImportKeyPair",
        "iam:CreateRole",
        "iam:AttachRolePolicy",
        "iam:CreateInstanceProfile",
        "iam:AddRoleToInstanceProfile",
        "iam:PassRole",
        "ssm:GetParameter"
      ],
      "Resource": "*"
    }
  ]
}
```

### Full Policy

Recommended for complete functionality:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EC2Management",
      "Effect": "Allow",
      "Action": [
        "ec2:*"
      ],
      "Resource": "*"
    },
    {
      "Sid": "IAMManagement",
      "Effect": "Allow",
      "Action": [
        "iam:CreateRole",
        "iam:AttachRolePolicy",
        "iam:PutRolePolicy",
        "iam:CreateInstanceProfile",
        "iam:AddRoleToInstanceProfile",
        "iam:PassRole",
        "iam:GetRole",
        "iam:GetRolePolicy",
        "iam:ListAttachedRolePolicies"
      ],
      "Resource": "*"
    },
    {
      "Sid": "SSMParameterAccess",
      "Effect": "Allow",
      "Action": [
        "ssm:GetParameter",
        "ssm:GetParameters"
      ],
      "Resource": "*"
    },
    {
      "Sid": "DynamoDBAccess",
      "Effect": "Allow",
      "Action": [
        "dynamodb:*"
      ],
      "Resource": "*"
    },
    {
      "Sid": "S3Access",
      "Effect": "Allow",
      "Action": [
        "s3:*"
      ],
      "Resource": "*"
    },
    {
      "Sid": "LambdaAccess",
      "Effect": "Allow",
      "Action": [
        "lambda:InvokeFunction",
        "lambda:CreateFunction",
        "lambda:UpdateFunctionCode"
      ],
      "Resource": "*"
    }
  ]
}
```

## Instance Profile Policies

### Built-in Policy Templates

spawn provides 13 policy templates:

#### S3 Policies

**s3:FullAccess**
```json
{
  "Effect": "Allow",
  "Action": "s3:*",
  "Resource": "*"
}
```

**s3:ReadOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "s3:GetObject",
    "s3:ListBucket"
  ],
  "Resource": "*"
}
```

**s3:WriteOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "s3:PutObject",
    "s3:PutObjectAcl"
  ],
  "Resource": "*"
}
```

#### DynamoDB Policies

**dynamodb:FullAccess**
```json
{
  "Effect": "Allow",
  "Action": "dynamodb:*",
  "Resource": "*"
}
```

**dynamodb:ReadOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "dynamodb:GetItem",
    "dynamodb:Query",
    "dynamodb:Scan",
    "dynamodb:BatchGetItem"
  ],
  "Resource": "*"
}
```

**dynamodb:WriteOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "dynamodb:PutItem",
    "dynamodb:UpdateItem",
    "dynamodb:DeleteItem",
    "dynamodb:BatchWriteItem"
  ],
  "Resource": "*"
}
```

#### SQS Policies

**sqs:FullAccess**
```json
{
  "Effect": "Allow",
  "Action": "sqs:*",
  "Resource": "*"
}
```

**sqs:ReadOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "sqs:ReceiveMessage",
    "sqs:GetQueueAttributes"
  ],
  "Resource": "*"
}
```

**sqs:WriteOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "sqs:SendMessage",
    "sqs:SendMessageBatch"
  ],
  "Resource": "*"
}
```

#### Other Services

**logs:WriteOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "logs:CreateLogGroup",
    "logs:CreateLogStream",
    "logs:PutLogEvents"
  ],
  "Resource": "*"
}
```

**ecr:ReadOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "ecr:GetAuthorizationToken",
    "ecr:BatchCheckLayerAvailability",
    "ecr:GetDownloadUrlForLayer",
    "ecr:BatchGetImage"
  ],
  "Resource": "*"
}
```

**secretsmanager:ReadOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "secretsmanager:GetSecretValue",
    "secretsmanager:DescribeSecret"
  ],
  "Resource": "*"
}
```

**ssm:ReadOnly**
```json
{
  "Effect": "Allow",
  "Action": [
    "ssm:GetParameter",
    "ssm:GetParameters",
    "ssm:GetParametersByPath"
  ],
  "Resource": "*"
}
```

### Default spored Role

Minimal permissions for spored agent:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeTags",
        "ec2:DescribeInstances",
        "ec2:TerminateInstances"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    }
  ]
}
```

## Usage Examples

### Launch with Policy Template

```bash
spawn launch --instance-type m7i.large --iam-policy s3:ReadOnly,logs:WriteOnly
```

### Launch with Custom Policy

```bash
# policy.json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "s3:*",
      "Resource": "arn:aws:s3:::my-bucket/*"
    }
  ]
}

spawn launch --instance-type m7i.large --iam-policy-file policy.json
```

### Launch with AWS Managed Policy

```bash
spawn launch --instance-type m7i.large \
  --iam-managed-policies arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

## Best Practices

### 1. Principle of Least Privilege

```bash
# Good - minimal permissions
spawn launch --iam-policy s3:ReadOnly

# Bad - excessive permissions
spawn launch --iam-policy s3:FullAccess
```

### 2. Scope to Specific Resources

```json
{
  "Effect": "Allow",
  "Action": "s3:GetObject",
  "Resource": "arn:aws:s3:::my-bucket/data/*"
}
```

### 3. Use Policy Templates

```bash
# Prefer templates over custom policies
spawn launch --iam-policy s3:ReadOnly,dynamodb:WriteOnly,logs:WriteOnly
```

### 4. Named Roles for Reuse

```bash
# Create reusable role
spawn launch --iam-role ml-training-role --iam-policy s3:FullAccess,logs:WriteOnly

# Reuse in subsequent launches
spawn launch --iam-role ml-training-role
```

## Security Considerations

### Trust Policies

All instance roles use this trust policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "ec2.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
```

### Resource Scoping

Scope policies to specific resources when possible:

```json
{
  "Effect": "Allow",
  "Action": "dynamodb:*",
  "Resource": "arn:aws:dynamodb:us-east-1:123456789012:table/my-table"
}
```

## See Also

- [spawn launch](commands/launch.md) - IAM policy flags
- [IAM_PERMISSIONS.md](../../IAM_PERMISSIONS.md) - CLI permissions
- [AWS IAM Documentation](https://docs.aws.amazon.com/iam/)
