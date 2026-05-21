# IAM Instance Profiles for Spawn

## Overview

Add IAM instance profile support to spawn, enabling instances to access AWS services securely without embedding credentials. Instances can read from S3, write to DynamoDB, invoke Lambda, etc. using temporary credentials managed by AWS.

## Problem

**Current State:**
- Instances have no AWS permissions by default
- Users must embed credentials in user-data (insecure)
- No way to grant least-privilege access
- Credential rotation is manual

**With Instance Profiles:**
- Instances automatically get temporary credentials
- Credentials auto-rotate every 6 hours
- Fine-grained permissions via IAM policies
- No secrets in code or user-data
- Audit trail via CloudTrail

## Use Cases

1. **ML Training**: Read datasets from S3, write results back
2. **Batch Processing**: Process SQS messages, write to DynamoDB
3. **CI/CD**: Pull from ECR, push to S3, deploy to Lambda
4. **Data Processing**: Read/write S3, query Athena, write to Redshift
5. **Monitoring**: Push metrics to CloudWatch, logs to CloudWatch Logs
6. **Genomics**: Access data in S3 glacier, write results to S3

## User Interface

### Basic Usage

```bash
# Use existing IAM role
spawn launch --instance-type m7i.large \
  --iam-role my-s3-reader-role

# Create and attach role inline
spawn launch --instance-type m7i.large \
  --iam-policy s3:ReadOnly,dynamodb:WriteOnly

# Use managed policies
spawn launch --instance-type m7i.large \
  --iam-managed-policies AmazonS3ReadOnlyAccess,AmazonDynamoDBFullAccess

# Custom policy from file
spawn launch --instance-type m7i.large \
  --iam-policy-file ./policy.json
```

### Common Patterns

```bash
# S3 access
spawn launch --instance-type m7i.large \
  --iam-policy s3:FullAccess \
  --user-data "aws s3 cp s3://my-bucket/data.txt ."

# DynamoDB + S3
spawn launch --instance-type m7i.large \
  --iam-policy s3:ReadOnly,dynamodb:WriteOnly

# ECR + S3 (for Docker workloads)
spawn launch --instance-type m7i.large \
  --iam-policy ecr:ReadOnly,s3:WriteOnly \
  --user-data "docker pull 123456789012.dkr.ecr.us-east-1.amazonaws.com/my-app"

# CloudWatch logging
spawn launch --instance-type m7i.large \
  --iam-policy logs:WriteOnly \
  --user-data "aws logs create-log-stream --log-group-name spawn ..."

# SQS queue processing
spawn launch --instance-type m7i.large \
  --iam-policy sqs:ReadWrite \
  --user-data "python process_queue.py --queue-url https://sqs.us-east-1...."
```

### Job Arrays with IAM

```bash
# All instances get same role
spawn launch --count 8 --job-array-name training \
  --iam-policy s3:FullAccess,dynamodb:WriteOnly

# Per-instance credentials (different permissions)
# Not supported in v1 - all instances share role
```

### Flags

```
--iam-role NAME                      Use existing IAM role (if exists, reuse; else create)
--iam-policy SERVICE:LEVEL,...       Create role with service-level policies
--iam-managed-policies ARN,...       Attach AWS managed policies
--iam-policy-file PATH               Custom IAM policy JSON file
--iam-trust-services SERVICE,...     Additional services that can assume role (default: ec2)
--iam-role-tags KEY=VALUE,...        Tags for created IAM role
```

**Service Levels:**
- `FullAccess` - Read and write
- `ReadOnly` - Read only
- `WriteOnly` - Write only (where applicable)

**Supported Services:**
- s3, dynamodb, sqs, sns, cloudwatch, logs, ecr, secretsmanager, ssm, lambda, sts

## Architecture

### IAM Role Lifecycle

```
1. User: spawn launch --iam-policy s3:ReadOnly
2. Check if role exists: spawn-instance-{hash-of-policies}
3. If not exists:
   a. Create IAM role with trust policy (ec2.amazonaws.com)
   b. Attach inline policies or managed policies
   c. Tag role: spawn:managed=true, spawn:policies={hash}
4. Create instance profile (if not exists)
5. Attach instance profile to role
6. Launch EC2 instance with instance profile
7. Instance gets credentials via metadata service
8. Cleanup: On instance termination, optionally delete role (if no other instances)
```

### Role Naming Convention

```
spawn-instance-{hash}
```

**Hash:** SHA256 of policy JSON (first 8 chars)

**Examples:**
- `spawn-instance-a3f2b8c1` (S3 read-only)
- `spawn-instance-9d4e7f3a` (S3 + DynamoDB)
- Custom: `my-ml-training-role` (user provided via --iam-role)

**Rationale:** Reuse roles with identical policies across instances.

### Policy Templates

**Built-in templates for `--iam-policy` flag:**

```go
var PolicyTemplates = map[string]string{
	"s3:FullAccess": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["s3:*"],
			"Resource": "*"
		}]
	}`,

	"s3:ReadOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"s3:GetObject",
				"s3:GetObjectVersion",
				"s3:ListBucket",
				"s3:GetBucketLocation"
			],
			"Resource": "*"
		}]
	}`,

	"s3:WriteOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"s3:PutObject",
				"s3:PutObjectAcl",
				"s3:DeleteObject"
			],
			"Resource": "*"
		}]
	}`,

	"dynamodb:FullAccess": `{...}`,
	"dynamodb:ReadOnly": `{...}`,
	"dynamodb:WriteOnly": `{...}`,

	"sqs:FullAccess": `{...}`,
	"sqs:ReadOnly": `{...}`,
	"sqs:WriteOnly": `{...}`,

	"logs:WriteOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"logs:CreateLogGroup",
				"logs:CreateLogStream",
				"logs:PutLogEvents"
			],
			"Resource": "*"
		}]
	}`,

	"ecr:ReadOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"ecr:GetAuthorizationToken",
				"ecr:BatchCheckLayerAvailability",
				"ecr:GetDownloadUrlForLayer",
				"ecr:BatchGetImage"
			],
			"Resource": "*"
		}]
	}`,

	"secretsmanager:ReadOnly": `{...}`,
	"ssm:ReadOnly": `{...}`,
}
```

## Implementation Details

### 1. Launch Command Changes

**File:** `cmd/launch.go`

**New Flags:**
```go
var (
	launchIAMRole            string
	launchIAMPolicy          []string
	launchIAMManagedPolicies []string
	launchIAMPolicyFile      string
	launchIAMTrustServices   []string
	launchIAMRoleTags        []string
)

func init() {
	launchCmd.Flags().StringVar(&launchIAMRole, "iam-role", "", "IAM role name")
	launchCmd.Flags().StringSliceVar(&launchIAMPolicy, "iam-policy", []string{}, "Service-level policies (e.g., s3:ReadOnly)")
	launchCmd.Flags().StringSliceVar(&launchIAMManagedPolicies, "iam-managed-policies", []string{}, "AWS managed policy ARNs")
	launchCmd.Flags().StringVar(&launchIAMPolicyFile, "iam-policy-file", "", "Custom policy JSON file")
	launchCmd.Flags().StringSliceVar(&launchIAMTrustServices, "iam-trust-services", []string{"ec2"}, "Services that can assume role")
	launchCmd.Flags().StringSliceVar(&launchIAMRoleTags, "iam-role-tags", []string{}, "Tags for IAM role")
}
```

**Launch Logic:**
```go
func runLaunch(cmd *cobra.Command, args []string) error {
	// ... existing code

	// Handle IAM role
	var instanceProfile string
	if launchIAMRole != "" || len(launchIAMPolicy) > 0 || len(launchIAMManagedPolicies) > 0 || launchIAMPolicyFile != "" {
		iamConfig := aws.IAMRoleConfig{
			RoleName:        launchIAMRole,
			Policies:        launchIAMPolicy,
			ManagedPolicies: launchIAMManagedPolicies,
			PolicyFile:      launchIAMPolicyFile,
			TrustServices:   launchIAMTrustServices,
			Tags:            parseIAMRoleTags(launchIAMRoleTags),
		}

		profile, err := client.CreateOrGetInstanceProfile(ctx, iamConfig)
		if err != nil {
			return fmt.Errorf("failed to setup IAM role: %w", err)
		}

		instanceProfile = profile
		fmt.Fprintf(os.Stderr, "IAM instance profile: %s\n", instanceProfile)
	}

	config.InstanceProfile = instanceProfile

	// Launch instance with profile
	instanceID, err := client.LaunchInstance(ctx, config)
	// ...
}
```

### 2. AWS Client - IAM Management

**File:** `pkg/aws/iam.go` (new)

```go
package aws

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

type IAMRoleConfig struct {
	RoleName        string            // User-provided or auto-generated
	Policies        []string          // Service-level policies (s3:ReadOnly, etc.)
	ManagedPolicies []string          // AWS managed policy ARNs
	PolicyFile      string            // Path to custom policy JSON
	TrustServices   []string          // Services that can assume role
	Tags            map[string]string // Tags for role
}

// CreateOrGetInstanceProfile creates or retrieves an IAM instance profile
func (c *Client) CreateOrGetInstanceProfile(ctx context.Context, config IAMRoleConfig) (string, error) {
	iamClient := iam.NewFromConfig(c.cfg)

	// Generate role name if not provided
	roleName := config.RoleName
	if roleName == "" {
		roleName = c.generateRoleName(config)
	}

	// Check if role exists
	roleExists, err := c.roleExists(ctx, iamClient, roleName)
	if err != nil {
		return "", err
	}

	if !roleExists {
		// Create new role
		if err := c.createIAMRole(ctx, iamClient, roleName, config); err != nil {
			return "", fmt.Errorf("failed to create IAM role: %w", err)
		}
	}

	// Ensure instance profile exists
	profileName := roleName // Use same name for profile
	profileExists, err := c.instanceProfileExists(ctx, iamClient, profileName)
	if err != nil {
		return "", err
	}

	if !profileExists {
		// Create instance profile
		_, err := iamClient.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
			Tags:                c.buildIAMTags(config.Tags),
		})
		if err != nil {
			return "", fmt.Errorf("failed to create instance profile: %w", err)
		}

		// Attach role to profile
		_, err = iamClient.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
			RoleName:            aws.String(roleName),
		})
		if err != nil {
			return "", fmt.Errorf("failed to attach role to instance profile: %w", err)
		}
	}

	return profileName, nil
}

func (c *Client) generateRoleName(config IAMRoleConfig) string {
	// Hash policies to generate deterministic name
	policyHash := c.hashPolicies(config)
	return fmt.Sprintf("spawn-instance-%s", policyHash[:8])
}

func (c *Client) hashPolicies(config IAMRoleConfig) string {
	// Combine all policy sources for hashing
	data := fmt.Sprintf("%v|%v|%s",
		config.Policies,
		config.ManagedPolicies,
		config.PolicyFile)

	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", hash)
}

func (c *Client) createIAMRole(ctx context.Context, iamClient *iam.Client, roleName string, config IAMRoleConfig) error {
	// Build trust policy
	trustPolicy := c.buildTrustPolicy(config.TrustServices)
	trustPolicyJSON, _ := json.Marshal(trustPolicy)

	// Create role
	_, err := iamClient.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(string(trustPolicyJSON)),
		Description:              aws.String("Created by spawn for EC2 instance access"),
		Tags:                     c.buildIAMTags(config.Tags),
	})
	if err != nil {
		return fmt.Errorf("failed to create role: %w", err)
	}

	// Attach inline policies
	if len(config.Policies) > 0 {
		policy := c.buildInlinePolicy(config.Policies)
		policyJSON, _ := json.Marshal(policy)

		_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spawn-inline-policy"),
			PolicyDocument: aws.String(string(policyJSON)),
		})
		if err != nil {
			return fmt.Errorf("failed to attach inline policy: %w", err)
		}
	}

	// Attach custom policy from file
	if config.PolicyFile != "" {
		policyDoc, err := os.ReadFile(config.PolicyFile)
		if err != nil {
			return fmt.Errorf("failed to read policy file: %w", err)
		}

		_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spawn-custom-policy"),
			PolicyDocument: aws.String(string(policyDoc)),
		})
		if err != nil {
			return fmt.Errorf("failed to attach custom policy: %w", err)
		}
	}

	// Attach managed policies
	for _, policyArn := range config.ManagedPolicies {
		_, err = iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(policyArn),
		})
		if err != nil {
			return fmt.Errorf("failed to attach managed policy %s: %w", policyArn, err)
		}
	}

	return nil
}

func (c *Client) buildTrustPolicy(services []string) map[string]interface{} {
	principals := make([]map[string]string, len(services))
	for i, service := range services {
		principals[i] = map[string]string{
			"Service": fmt.Sprintf("%s.amazonaws.com", service),
		}
	}

	return map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []map[string]interface{}{
			{
				"Effect": "Allow",
				"Principal": map[string]interface{}{
					"Service": principals,
				},
				"Action": "sts:AssumeRole",
			},
		},
	}
}

func (c *Client) buildInlinePolicy(policies []string) map[string]interface{} {
	statements := []map[string]interface{}{}

	for _, policyStr := range policies {
		// Parse: "s3:ReadOnly" -> template
		template := PolicyTemplates[policyStr]
		if template == "" {
			continue
		}

		var policy map[string]interface{}
		json.Unmarshal([]byte(template), &policy)

		if stmts, ok := policy["Statement"].([]interface{}); ok {
			for _, stmt := range stmts {
				statements = append(statements, stmt.(map[string]interface{}))
			}
		}
	}

	return map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": statements,
	}
}

func (c *Client) buildIAMTags(tags map[string]string) []types.Tag {
	// Add spawn tags
	tags["spawn:managed"] = "true"
	tags["spawn:created"] = time.Now().UTC().Format(time.RFC3339)

	iamTags := make([]types.Tag, 0, len(tags))
	for key, value := range tags {
		iamTags = append(iamTags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	return iamTags
}

func (c *Client) roleExists(ctx context.Context, iamClient *iam.Client, roleName string) (bool, error) {
	_, err := iamClient.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		// Check if NoSuchEntity error
		return false, nil
	}
	return true, nil
}

func (c *Client) instanceProfileExists(ctx context.Context, iamClient *iam.Client, profileName string) (bool, error) {
	_, err := iamClient.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	})
	if err != nil {
		return false, nil
	}
	return true, nil
}

// CleanupUnusedRoles removes IAM roles with no attached instances
func (c *Client) CleanupUnusedRoles(ctx context.Context) error {
	// List all spawn-managed roles
	// Check if any instances use them
	// Delete unused roles and profiles
	return nil
}
```

### 3. EC2 Launch with Instance Profile

**File:** `pkg/aws/client.go`

**Modify LaunchInstance:**
```go
func (c *Client) LaunchInstance(ctx context.Context, config LaunchConfig) (string, error) {
	// ... existing code

	input := &ec2.RunInstancesInput{
		ImageId:           aws.String(config.AMI),
		InstanceType:      types.InstanceType(config.InstanceType),
		KeyName:           aws.String(config.KeyName),
		SecurityGroupIds:  config.SecurityGroups,
		SubnetId:          aws.String(config.SubnetID),
		UserData:          aws.String(config.UserData),
		TagSpecifications: buildTagSpecs(config),
		MinCount:          aws.Int32(1),
		MaxCount:          aws.Int32(1),
	}

	// Add instance profile if specified
	if config.InstanceProfile != "" {
		input.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Name: aws.String(config.InstanceProfile),
		}
	}

	result, err := ec2Client.RunInstances(ctx, input)
	// ...
}
```

### 4. List Command - Show IAM Role

**File:** `cmd/list.go`

```go
func outputTable(instances []aws.InstanceInfo) error {
	// Add IAM ROLE column
	fmt.Fprintf(w, "NAME\tINSTANCE ID\tTYPE\tSTATE\tIAM ROLE\tIP\tREGION\n")

	for _, instance := range instances {
		iamRole := instance.IAMRole
		if iamRole == "" {
			iamRole = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			instance.Name,
			instance.InstanceID,
			instance.InstanceType,
			instance.State,
			iamRole,
			instance.PublicIP,
			instance.Region,
		)
	}
}
```

### 5. Testing Instance Permissions

**Add helper command:**

```bash
# Test if instance can access S3
spawn exec my-instance "aws s3 ls"

# Test DynamoDB access
spawn exec my-instance "aws dynamodb list-tables"

# Show instance credentials
spawn exec my-instance "aws sts get-caller-identity"
```

## Testing Strategy

### Unit Tests

```go
func TestRoleGeneration(t *testing.T) {
	tests := []struct {
		name     string
		config   IAMRoleConfig
		wantName string
	}{
		{
			name:     "custom name",
			config:   IAMRoleConfig{RoleName: "my-role"},
			wantName: "my-role",
		},
		{
			name: "auto-generated from policies",
			config: IAMRoleConfig{
				Policies: []string{"s3:ReadOnly"},
			},
			wantName: "spawn-instance-*", // Hash-based
		},
	}
}

func TestPolicyBuilding(t *testing.T) {
	// Test template expansion
	// Test policy merging
	// Test custom policy file loading
}

func TestRoleReuse(t *testing.T) {
	// Launch 2 instances with same policies
	// Verify they share same role
}
```

### Integration Tests

```bash
# 1. Launch with S3 access
spawn launch --instance-type t3.micro --iam-policy s3:ReadOnly --name test-iam

# 2. Verify role attached
aws ec2 describe-instances --instance-ids i-xxx \
  --query 'Reservations[0].Instances[0].IamInstanceProfile'

# 3. SSH and test S3 access
spawn connect test-iam
aws s3 ls  # Should work
aws s3 cp test.txt s3://my-bucket/  # Should fail (read-only)

# 4. Launch with write access
spawn launch --instance-type t3.micro --iam-policy s3:FullAccess --name test-write
spawn connect test-write
echo "test" > file.txt
aws s3 cp file.txt s3://my-bucket/  # Should work

# 5. Test role reuse
spawn launch --instance-type t3.micro --iam-policy s3:ReadOnly --name test-reuse
# Should reuse existing spawn-instance-* role

# 6. Cleanup
spawn terminate test-iam test-write test-reuse
```

### Edge Cases

1. **Role name conflicts** - User provides name that exists
2. **Policy attachment failures** - IAM permissions issues
3. **Instance profile propagation delay** - Wait for IAM eventual consistency
4. **Invalid policy JSON** - Validate before sending to AWS
5. **Managed policy ARN typos** - Catch and suggest corrections
6. **Multiple instances sharing role** - Ensure concurrent access works
7. **Role deletion with active instances** - Prevent accidental deletion

## Security Considerations

### Least Privilege

**Always scope policies minimally:**

```json
// ❌ Bad - Too broad
{
  "Effect": "Allow",
  "Action": "*",
  "Resource": "*"
}

// ✅ Good - Specific actions and resources
{
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:PutObject"],
  "Resource": "arn:aws:s3:::my-specific-bucket/*"
}
```

**spawn should warn on overly broad policies:**
```bash
spawn launch --iam-policy s3:FullAccess
⚠️  Warning: s3:FullAccess grants access to ALL S3 buckets in your account.
   Consider scoping to specific buckets if possible.
   Continue? (y/N)
```

### Trust Policies

**Only allow EC2 by default:**
```json
{
  "Principal": {
    "Service": "ec2.amazonaws.com"
  }
}
```

**User can add other services if needed:**
```bash
spawn launch --iam-policy s3:ReadOnly \
  --iam-trust-services ec2,lambda
```

### Audit Trail

**All IAM operations logged to CloudTrail:**
- Role creation
- Policy attachments
- Instance profile associations
- Role assumptions

**Tag roles for visibility:**
```
spawn:managed = true
spawn:created = 2026-01-14T10:30:00Z
spawn:creator = alice@example.com
```

## Documentation

### User Guide Section

```markdown
## IAM Instance Profiles

### Why Use Instance Profiles?

✅ **Secure** - No credentials in code or user-data
✅ **Automatic** - Credentials rotate every 6 hours
✅ **Auditable** - CloudTrail logs all access
✅ **Flexible** - Fine-grained permissions

### Quick Start

```bash
# Read from S3
spawn launch --instance-type m7i.large \
  --iam-policy s3:ReadOnly \
  --user-data "aws s3 cp s3://my-bucket/data.txt ."

# Write to S3 and DynamoDB
spawn launch --instance-type m7i.large \
  --iam-policy s3:WriteOnly,dynamodb:WriteOnly

# Use AWS managed policies
spawn launch --instance-type m7i.large \
  --iam-managed-policies arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

### Available Service Policies

| Policy | Permissions |
|--------|-------------|
| `s3:FullAccess` | Read and write all S3 buckets |
| `s3:ReadOnly` | Read from all S3 buckets |
| `s3:WriteOnly` | Write to all S3 buckets |
| `dynamodb:FullAccess` | Full DynamoDB access |
| `dynamodb:ReadOnly` | Read from DynamoDB |
| `dynamodb:WriteOnly` | Write to DynamoDB |
| `sqs:FullAccess` | Full SQS access |
| `logs:WriteOnly` | Write logs to CloudWatch |
| `ecr:ReadOnly` | Pull Docker images from ECR |
| `secretsmanager:ReadOnly` | Read secrets |

### Custom Policies

**From file:**
```bash
spawn launch --iam-policy-file ./my-policy.json
```

**policy.json:**
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["s3:GetObject"],
    "Resource": "arn:aws:s3:::my-specific-bucket/*"
  }]
}
```

### Reusing Roles

spawn automatically reuses roles with identical policies:

```bash
# First launch creates role
spawn launch --iam-policy s3:ReadOnly --name instance1

# Second launch reuses same role
spawn launch --iam-policy s3:ReadOnly --name instance2
```

### Testing Permissions

```bash
# Launch instance
spawn launch --iam-policy s3:ReadOnly --name test

# SSH and test
spawn connect test
aws sts get-caller-identity  # Shows assumed role
aws s3 ls  # Lists buckets (should work)
aws s3 cp test.txt s3://bucket/  # Fails (read-only)
```

### Best Practices

1. **Least privilege** - Only grant needed permissions
2. **Scope to resources** - Limit to specific buckets/tables when possible
3. **Use managed policies** - For common patterns (S3ReadOnly, etc.)
4. **Test permissions** - Verify access before production use
5. **Tag roles** - For cost tracking and organization
6. **Cleanup unused roles** - Use `spawn cleanup-iam`
```

## Verification Checklist

- [ ] Can launch with existing IAM role
- [ ] Can create role with inline policies
- [ ] Can attach managed policies
- [ ] Can use custom policy file
- [ ] Role name auto-generated correctly
- [ ] Role reuse works (same policies = same role)
- [ ] Instance can access S3 with ReadOnly policy
- [ ] Instance cannot write with ReadOnly policy
- [ ] Instance can write with FullAccess policy
- [ ] Job arrays work with IAM roles
- [ ] `spawn list` shows IAM role
- [ ] Tags applied to roles
- [ ] Trust policy includes only ec2 by default
- [ ] CloudTrail logs role assumptions
- [ ] Role cleanup removes unused roles

## Future Enhancements

1. **Resource-scoped policies** - Scope to specific buckets/tables
   ```bash
   spawn launch --iam-policy s3:ReadOnly --iam-resources s3://my-bucket/*
   ```

2. **Policy builder UI** - Interactive policy creation
   ```bash
   spawn iam-wizard
   # Prompts for services, actions, resources
   ```

3. **Permission testing** - Pre-flight permission checks
   ```bash
   spawn iam-test --role my-role --action s3:GetObject --resource s3://bucket/key
   ```

4. **Role sharing** - Share roles across team
   ```bash
   spawn iam-export --role my-role > role.json
   spawn launch --iam-import role.json
   ```

5. **Compliance checking** - Detect overly permissive policies
   ```bash
   spawn iam-audit
   # Lists roles with wildcard permissions
   ```

6. **Cross-account roles** - Access resources in other accounts
   ```bash
   spawn launch --iam-cross-account 123456789012 --iam-role shared-role
   ```

## Summary

IAM instance profile support enables secure, credential-free AWS access for instances. The implementation provides:

- Simple flag-based interface (`--iam-policy`, `--iam-role`)
- Automatic role creation and reuse
- Built-in policy templates for common services
- Custom policy file support
- AWS managed policy attachment
- Least-privilege defaults
- Audit trail via CloudTrail

This feature eliminates the insecure practice of embedding credentials and enables proper cloud-native security patterns for spawn workloads.
