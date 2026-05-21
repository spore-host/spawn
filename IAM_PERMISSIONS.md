# IAM Permissions Required for spawn

## Overview

To use `spawn`, your AWS IAM user or role needs specific permissions to:
1. Create and manage EC2 instances
2. Manage SSH key pairs
3. Create and attach IAM roles (for spored agent)
4. Query instance metadata

## Required Permissions

### Minimum Policy for spawn Users

Create an IAM policy with these permissions and attach it to your user/role:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EC2InstanceManagement",
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:StopInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeImages",
        "ec2:DescribeKeyPairs",
        "ec2:DescribeSecurityGroups",
        "ec2:DescribeSubnets",
        "ec2:DescribeVpcs",
        "ec2:DescribeTags",
        "ec2:CreateTags",
        "ec2:ImportKeyPair",
        "ec2:DeleteKeyPair"
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
      "Resource": "arn:aws:ssm:*::parameter/aws/service/ami-amazon-linux-latest/*"
    },
    {
      "Sid": "IAMRoleManagement",
      "Effect": "Allow",
      "Action": [
        "iam:CreateRole",
        "iam:GetRole",
        "iam:PutRolePolicy",
        "iam:CreateInstanceProfile",
        "iam:GetInstanceProfile",
        "iam:AddRoleToInstanceProfile",
        "iam:PassRole"
      ],
      "Resource": [
        "arn:aws:iam::*:role/spored-instance-role",
        "arn:aws:iam::*:instance-profile/spored-instance-profile"
      ]
    },
    {
      "Sid": "IAMTagging",
      "Effect": "Allow",
      "Action": [
        "iam:TagRole",
        "iam:TagInstanceProfile"
      ],
      "Resource": [
        "arn:aws:iam::*:role/spored-instance-role",
        "arn:aws:iam::*:instance-profile/spored-instance-profile"
      ]
    }
  ]
}
```

## What Each Permission Does

### EC2 Permissions
- **RunInstances**: Launch new EC2 instances
- **DescribeInstances**: Get instance details (public IP, status, etc.)
- **DescribeInstanceTypes**: Query instance type availability
- **DescribeImages**: Find AMI IDs
- **DescribeKeyPairs**: Check for existing SSH keys
- **ImportKeyPair**: Upload SSH public keys
- **TerminateInstances/StopInstances**: (Optional) Manually terminate instances
- **CreateTags/DescribeTags**: Tag instances with spawn metadata

### SSM Permissions
- **GetParameter**: Auto-detect latest Amazon Linux 2023 AMI IDs

### IAM Permissions
- **CreateRole**: Create the `spored-instance-role` if it doesn't exist
- **GetRole**: Check if role already exists
- **PutRolePolicy**: Attach permissions policy to the role
- **CreateInstanceProfile**: Create instance profile for EC2
- **GetInstanceProfile**: Check if profile already exists
- **AddRoleToInstanceProfile**: Associate role with profile
- **PassRole**: Allow EC2 to assume the spored role
- **TagRole/TagInstanceProfile**: Tag IAM resources

## IAM Resources Created by spawn

spawn automatically creates these IAM resources (once per account):

### 1. IAM Role: `spored-instance-role`
**Purpose**: Allows spored daemon to read its own tags and terminate the instance

**Trust Policy** (who can assume this role):
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

**Permissions Policy** (what the role can do):
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeTags",
        "ec2:DescribeInstances",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:TerminateInstances",
        "ec2:StopInstances"
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

**Security Note**: The role can only terminate/stop instances tagged with `spawn:managed=true`, preventing accidental termination of other instances.

### 2. Instance Profile: `spored-instance-profile`
**Purpose**: Attaches the IAM role to EC2 instances

This is automatically attached to every instance launched by spawn.

## Validation

Use the `validate-permissions.sh` script to check if your AWS credentials have the required permissions:

```bash
./scripts/validate-permissions.sh
```

This will test each permission and report any missing access.

## PowerUser Compatibility

The AWS-managed `PowerUser` policy grants full access to AWS services **except IAM**. This means PowerUser alone is **NOT sufficient** for spawn because spawn needs to create IAM roles.

**Solution:** Add a supplementary policy that grants spawn-specific IAM permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SpawnIAMRoleManagement",
      "Effect": "Allow",
      "Action": [
        "iam:CreateRole",
        "iam:GetRole",
        "iam:PutRolePolicy",
        "iam:CreateInstanceProfile",
        "iam:GetInstanceProfile",
        "iam:AddRoleToInstanceProfile",
        "iam:PassRole"
      ],
      "Resource": [
        "arn:aws:iam::*:role/spored-instance-role",
        "arn:aws:iam::*:instance-profile/spored-instance-profile"
      ]
    },
    {
      "Sid": "SpawnIAMTagging",
      "Effect": "Allow",
      "Action": [
        "iam:TagRole",
        "iam:TagInstanceProfile"
      ],
      "Resource": [
        "arn:aws:iam::*:role/spored-instance-role",
        "arn:aws:iam::*:instance-profile/spored-instance-profile"
      ]
    }
  ]
}
```

This policy is **highly scoped** - it only allows creating/managing the specific `spored-instance-role` and `spored-instance-profile` resources. It cannot be used to create other IAM roles or escalate privileges.

## Setup Instructions

### Option 1: Attach Policy to IAM User

1. Log in to AWS Console
2. Go to IAM → Users → [Your Username]
3. Click "Add permissions" → "Create inline policy"
4. Paste the JSON policy above
5. Name it `spawn-user-policy`
6. Click "Create policy"

### Option 2: Create Dedicated IAM Group

```bash
# Create group
aws iam create-group --group-name spawn-users

# Create policy file
cat > spawn-policy.json <<'EOF'
[paste policy JSON from above]
EOF

# Attach policy to group
aws iam put-group-policy \
  --group-name spawn-users \
  --policy-name spawn-policy \
  --policy-document file://spawn-policy.json

# Add user to group
aws iam add-user-to-group \
  --group-name spawn-users \
  --user-name your-username
```

### Option 3: Use with AWS Organizations

For multi-account setups:

1. Create a Service Control Policy (SCP) allowing spawn permissions
2. Attach to OUs where spawn will be used
3. Create local IAM policies in each member account

## Troubleshooting

### Error: "User: arn:aws:iam::123456789012:user/myuser is not authorized to perform: iam:CreateRole"

**Solution**: Add IAM permissions to your user/role (see policy above)

### Error: "User: arn:aws:iam::123456789012:user/myuser is not authorized to perform: iam:PassRole"

**Solution**: Add `iam:PassRole` permission for the `spored-instance-role` resource

### Error: "You are not authorized to perform this operation"

**Solution**: Run `./scripts/validate-permissions.sh` to identify which specific permission is missing

## Security Best Practices

1. **Least Privilege**: Only grant spawn permissions to users who need to launch instances
2. **Resource Tags**: Use the `spawn:managed=true` tag condition to limit blast radius
3. **MFA**: Consider requiring MFA for `ec2:TerminateInstances` actions
4. **CloudTrail**: Enable CloudTrail to audit spawn usage
5. **Budget Alerts**: Set up billing alerts for unexpected costs

## See Also

- [spawn README](README.md) - Main documentation
- [validate-permissions.sh](../scripts/validate-permissions.sh) - Permission validation script
- [AWS IAM Best Practices](https://docs.aws.amazon.com/IAM/latest/UserGuide/best-practices.html)
