package aws

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"
)

// iamErrorCode returns the API error code for err, matching both the modeled
// SDK error types and a generic smithy.APIError (which is what some emulators /
// non-modeled responses surface). Falls back to substring matching on the
// message as a last resort.
func iamErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// isAlreadyExists reports whether err is an IAM "entity already exists" error —
// benign when multiple concurrent launches ensure the same shared role/profile
// (#64). The instance-profile API also reports an already-attached role via
// LimitExceededException (a profile holds at most one role), which is likewise
// benign here.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	// The SDK's modeled error codes drop the "Exception" suffix
	// (EntityAlreadyExists, LimitExceeded) while some emulators keep it, so
	// match on the prefix.
	code := iamErrorCode(err)
	if strings.HasPrefix(code, "EntityAlreadyExists") || strings.HasPrefix(code, "LimitExceeded") {
		return true
	}
	// Fallback for non-modeled errors that only carry the code in the message.
	msg := err.Error()
	return strings.Contains(msg, "EntityAlreadyExists") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "already has a role")
}

// isThrottle reports whether err is a retryable IAM throttling error.
func isThrottle(err error) bool {
	if err == nil {
		return false
	}
	code := iamErrorCode(err)
	if strings.HasPrefix(code, "Throttling") || strings.HasPrefix(code, "RequestLimitExceeded") {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Throttling") || strings.Contains(msg, "RequestLimitExceeded")
}

// retryIAM runs fn, retrying on throttling with a short backoff. "Already
// exists" is treated as success (the resource is present, which is the goal),
// so concurrent launches racing to ensure the same shared role/profile converge
// instead of failing (#64).
func retryIAM(fn func() error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = fn()
		if err == nil || isAlreadyExists(err) {
			return nil
		}
		if !isThrottle(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return err
}

// IAMRoleConfig contains configuration for creating IAM roles
type IAMRoleConfig struct {
	RoleName        string            // User-provided or auto-generated
	Policies        []string          // Service-level policies (s3:ReadOnly, etc.)
	ManagedPolicies []string          // AWS managed policy ARNs
	PolicyFile      string            // Path to custom policy JSON
	TrustServices   []string          // Services that can assume role
	Tags            map[string]string // Tags for role
}

// GenerateScopedS3Policy creates an S3 policy scoped to spawn resources
func GenerateScopedS3Policy(region, accountID string) string {
	return fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"s3:GetObject",
					"s3:GetObjectVersion"
				],
				"Resource": [
					"arn:aws:s3:::spawn-binaries-%s/*",
					"arn:aws:s3:::spawn-binaries-*/*",
					"arn:aws:s3:::spawn-schedules-*/*"
				]
			},
			{
				"Effect": "Allow",
				"Action": [
					"s3:PutObject",
					"s3:PutObjectAcl"
				],
				"Resource": [
					"arn:aws:s3:::spawn-results-%s/*",
					"arn:aws:s3:::spawn-schedules-*/*"
				]
			},
			{
				"Effect": "Allow",
				"Action": [
					"s3:ListBucket",
					"s3:GetBucketLocation"
				],
				"Resource": [
					"arn:aws:s3:::spawn-binaries-%s",
					"arn:aws:s3:::spawn-results-%s",
					"arn:aws:s3:::spawn-schedules-*"
				]
			}
		]
	}`, region, region, region, region)
}

// GenerateScopedDynamoDBPolicy creates a DynamoDB policy scoped to spawn tables
func GenerateScopedDynamoDBPolicy(region, accountID string) string {
	return fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"dynamodb:BatchGetItem",
				"dynamodb:BatchWriteItem",
				"dynamodb:DescribeTable",
				"dynamodb:GetItem",
				"dynamodb:PutItem",
				"dynamodb:Query",
				"dynamodb:Scan",
				"dynamodb:UpdateItem",
				"dynamodb:DeleteItem"
			],
			"Resource": [
				"arn:aws:dynamodb:%s:%s:table/spawn-alerts",
				"arn:aws:dynamodb:%s:%s:table/spawn-alert-history",
				"arn:aws:dynamodb:%s:%s:table/spawn-schedules",
				"arn:aws:dynamodb:%s:%s:table/spawn-queues",
				"arn:aws:dynamodb:%s:%s:table/spawn-alerts/index/*",
				"arn:aws:dynamodb:%s:%s:table/spawn-schedules/index/*"
			]
		}]
	}`, region, accountID, region, accountID, region, accountID, region, accountID, region, accountID, region, accountID)
}

// GenerateScopedCloudWatchLogsPolicy creates a CloudWatch Logs policy for audit logs
func GenerateScopedCloudWatchLogsPolicy(region, accountID string) string {
	return fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"logs:CreateLogGroup",
				"logs:CreateLogStream",
				"logs:PutLogEvents",
				"logs:DescribeLogStreams"
			],
			"Resource": [
				"arn:aws:logs:%s:%s:log-group:/aws/spawn/audit",
				"arn:aws:logs:%s:%s:log-group:/aws/spawn/audit:*"
			]
		}]
	}`, region, accountID, region, accountID)
}

// PolicyTemplates provides built-in policy templates for common services
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
	"dynamodb:FullAccess": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["dynamodb:*"],
			"Resource": "*"
		}]
	}`,
	"dynamodb:ReadOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"dynamodb:BatchGetItem",
				"dynamodb:DescribeTable",
				"dynamodb:GetItem",
				"dynamodb:Query",
				"dynamodb:Scan"
			],
			"Resource": "*"
		}]
	}`,
	"dynamodb:WriteOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"dynamodb:BatchWriteItem",
				"dynamodb:PutItem",
				"dynamodb:UpdateItem",
				"dynamodb:DeleteItem"
			],
			"Resource": "*"
		}]
	}`,
	"sqs:FullAccess": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["sqs:*"],
			"Resource": "*"
		}]
	}`,
	"sqs:ReadOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"sqs:GetQueueAttributes",
				"sqs:GetQueueUrl",
				"sqs:ListQueues",
				"sqs:ReceiveMessage"
			],
			"Resource": "*"
		}]
	}`,
	"sqs:WriteOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"sqs:SendMessage",
				"sqs:DeleteMessage",
				"sqs:ChangeMessageVisibility"
			],
			"Resource": "*"
		}]
	}`,
	"logs:WriteOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"logs:CreateLogGroup",
				"logs:CreateLogStream",
				"logs:PutLogEvents",
				"logs:DescribeLogStreams"
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
	"secretsmanager:ReadOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"secretsmanager:GetSecretValue",
				"secretsmanager:DescribeSecret"
			],
			"Resource": "*"
		}]
	}`,
	"ssm:ReadOnly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"ssm:GetParameter",
				"ssm:GetParameters",
				"ssm:GetParametersByPath"
			],
			"Resource": "*"
		}]
	}`,
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
	} else if len(config.Policies) > 0 || config.PolicyFile != "" {
		// Role exists but caller specified policies — update the inline policy so that
		// re-using a cached role (same hash) still picks up any new policy additions.
		if err := c.updateInlinePolicy(ctx, iamClient, roleName, config); err != nil {
			return "", fmt.Errorf("failed to update IAM role policy: %w", err)
		}
	}

	// Ensure instance profile exists
	profileName := roleName // Use same name for profile
	profileExists, err := c.instanceProfileExists(ctx, iamClient, profileName)
	if err != nil {
		return "", err
	}

	if !profileExists {
		// Create instance profile (idempotent under concurrency — #64).
		err := retryIAM(func() error {
			_, e := iamClient.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
				InstanceProfileName: aws.String(profileName),
				Tags:                c.buildIAMTags(config.Tags),
			})
			return e
		})
		if err != nil {
			return "", fmt.Errorf("failed to create instance profile: %w", err)
		}

		// Attach role to profile. If another launch already attached it, the
		// API reports LimitExceeded (a profile holds one role) — benign here.
		err = retryIAM(func() error {
			_, e := iamClient.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
				InstanceProfileName: aws.String(profileName),
				RoleName:            aws.String(roleName),
			})
			return e
		})
		if err != nil {
			return "", fmt.Errorf("failed to attach role to instance profile: %w", err)
		}

		// Wait for instance profile to propagate (IAM eventual consistency)
		time.Sleep(10 * time.Second)
	}

	// Retrieve the instance profile to get its ARN
	profile, err := iamClient.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	})
	if err != nil {
		return "", fmt.Errorf("failed to retrieve instance profile: %w", err)
	}

	if profile.InstanceProfile == nil {
		return "", fmt.Errorf("instance profile not available")
	}

	// Return the profile name, not the ARN — EC2 RunInstances iamInstanceProfile.name
	// expects the bare name, not "arn:aws:iam::ACCOUNT:instance-profile/NAME".
	return profileName, nil
}

// generateRoleName creates a deterministic role name based on policies
func (c *Client) generateRoleName(config IAMRoleConfig) string {
	// Hash policies to generate deterministic name
	policyHash := c.hashPolicies(config)
	return fmt.Sprintf("spawn-instance-%s", policyHash[:8])
}

// hashPolicies generates a hash of all policy sources for role naming
func (c *Client) hashPolicies(config IAMRoleConfig) string {
	// Combine all policy sources for hashing
	data := fmt.Sprintf("%v|%v|%s",
		config.Policies,
		config.ManagedPolicies,
		config.PolicyFile)

	hash := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", hash)
}

// createIAMRole creates a new IAM role with policies
func (c *Client) createIAMRole(ctx context.Context, iamClient *iam.Client, roleName string, config IAMRoleConfig) error {
	// Get current account ID for trust policy conditions
	accountID, err := c.GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get account ID: %w", err)
	}

	// Build trust policy with account condition
	trustPolicy := c.buildTrustPolicyWithAccount(config.TrustServices, accountID)
	trustPolicyJSON, err := json.Marshal(trustPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal trust policy: %w", err)
	}

	// Create role. Idempotent under concurrency: if another launch already
	// created the shared role, EntityAlreadyExists is treated as success (#64).
	err = retryIAM(func() error {
		_, e := iamClient.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(roleName),
			AssumeRolePolicyDocument: aws.String(string(trustPolicyJSON)),
			Description:              aws.String("Created by spawn for EC2 instance access"),
			Tags:                     c.buildIAMTags(config.Tags),
		})
		return e
	})
	if err != nil {
		return fmt.Errorf("failed to create role: %w", err)
	}

	// Attach inline policies
	if len(config.Policies) > 0 {
		policy := c.buildInlinePolicy(config.Policies)
		policyJSON, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("failed to marshal inline policy: %w", err)
		}

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

// updateInlinePolicy replaces the inline policy on an existing role.
func (c *Client) updateInlinePolicy(ctx context.Context, iamClient *iam.Client, roleName string, config IAMRoleConfig) error {
	policy := c.buildInlinePolicy(config.Policies)
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("failed to marshal inline policy: %w", err)
	}
	_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String("spawn-inline-policy"),
		PolicyDocument: aws.String(string(policyJSON)),
	})
	return err
}

// buildTrustPolicy creates an assume role policy document (legacy, no account condition)
func (c *Client) buildTrustPolicy(services []string) map[string]interface{} {
	// Build service principals
	servicePrincipals := make([]string, len(services))
	for i, service := range services {
		servicePrincipals[i] = fmt.Sprintf("%s.amazonaws.com", service)
	}

	return map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []map[string]interface{}{
			{
				"Effect": "Allow",
				"Principal": map[string]interface{}{
					"Service": servicePrincipals,
				},
				"Action": "sts:AssumeRole",
			},
		},
	}
}

// buildTrustPolicyWithAccount creates an assume role policy with account condition
// This prevents cross-account role assumption for enhanced security
func (c *Client) buildTrustPolicyWithAccount(services []string, accountID string) map[string]interface{} {
	// Build service principals
	servicePrincipals := make([]string, len(services))
	for i, service := range services {
		servicePrincipals[i] = fmt.Sprintf("%s.amazonaws.com", service)
	}

	return map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []map[string]interface{}{
			{
				"Effect": "Allow",
				"Principal": map[string]interface{}{
					"Service": servicePrincipals,
				},
				"Action": "sts:AssumeRole",
				"Condition": map[string]interface{}{
					"StringEquals": map[string]interface{}{
						"aws:SourceAccount": accountID,
					},
				},
			},
		},
	}
}

// buildInlinePolicy combines multiple policy templates into one
func (c *Client) buildInlinePolicy(policies []string) map[string]interface{} {
	statements := []interface{}{}

	// ALWAYS include spored-required EC2 permissions for self-management
	// This allows spored agent to:
	// - Read its own tags (TTL, idle timeout, etc.)
	// - Keep its lifecycle tags current (compute-seconds, fsx-id, …)
	// - Terminate/stop itself when conditions are met
	// The destructive and tag-write actions are scoped to spawn:managed=true.

	// Describe* are read-only and stay on "*" (Describe APIs don't support
	// resource-level IAM scoping).
	sporedReadPermissions := map[string]interface{}{
		"Effect": "Allow",
		"Action": []interface{}{
			"ec2:DescribeTags",
			"ec2:DescribeInstances",
		},
		"Resource": "*",
	}
	statements = append(statements, sporedReadPermissions)

	// Tag writes are scoped to already-managed instances (#174). Previously
	// ec2:CreateTags was granted on "*" with NO condition, so a compromised spore
	// could tag ANY instance spawn:managed=true and then terminate it — the
	// condition on the destructive actions below provided no containment.
	// Conditioning CreateTags/DeleteTags on ec2:ResourceTag/spawn:managed=true
	// means a spore can only (re)tag instances ALREADY in scope; it cannot pull a
	// foreign instance into scope. spored only ever tags its own instance (which
	// always carries spawn:managed=true), so legitimate use is unaffected. (This
	// also adds the previously-missing DeleteTags grant the FSx-mount path needs.)
	sporedTagPermissions := map[string]interface{}{
		"Effect": "Allow",
		"Action": []interface{}{
			"ec2:CreateTags",
			"ec2:DeleteTags",
		},
		"Resource": "*",
		"Condition": map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"ec2:ResourceTag/spawn:managed": "true",
			},
		},
	}
	statements = append(statements, sporedTagPermissions)

	sporedActionPermissions := map[string]interface{}{
		"Effect": "Allow",
		"Action": []interface{}{
			"ec2:TerminateInstances",
			"ec2:StopInstances",
		},
		"Resource": "*",
		"Condition": map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"ec2:ResourceTag/spawn:managed": "true",
			},
		},
	}
	statements = append(statements, sporedActionPermissions)

	// Allow the spored role to invoke the DNS-updater Function URL under AWS_IAM
	// auth (#173). This is the caller's half of the IAM-auth cutover: rather than
	// the DNS Lambda's resource policy enumerating every launch account (which
	// doesn't scale — accounts are unbounded and spored role names are dynamic),
	// each spored role grants ITSELF invoke on the fixed DNS function, and the
	// Lambda authorizes the SigV4-verified caller account. Scoped to the single
	// dns-updater function ARN (account-pinned; region wildcard mirrors the
	// existing setup-spawnd-iam-role.sh grant). Harmless before the AuthType flip
	// (the NONE URL doesn't check it) and required after it.
	sporedDNSInvoke := map[string]interface{}{
		"Effect": "Allow",
		"Action": []interface{}{
			"lambda:InvokeFunctionUrl",
		},
		"Resource": "arn:aws:lambda:*:966362334030:function:spawn-dns-updater",
	}
	statements = append(statements, sporedDNSInvoke)

	// Add user-specified policy templates
	for _, policyStr := range policies {
		// Get template
		template, ok := PolicyTemplates[policyStr]
		if !ok {
			continue
		}

		// Parse template
		var policy map[string]interface{}
		if err := json.Unmarshal([]byte(template), &policy); err != nil {
			continue
		}

		// Extract statements
		if stmts, ok := policy["Statement"].([]interface{}); ok {
			statements = append(statements, stmts...)
		}
	}

	return map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": statements,
	}
}

// buildIAMTags creates IAM tags with spawn metadata
func (c *Client) buildIAMTags(tags map[string]string) []types.Tag {
	if tags == nil {
		tags = make(map[string]string)
	}

	// Add spawn tags. spawn:created-at is the canonical creation-timestamp tag
	// across all resource types (#258); spawn:created is kept for back-compat
	// with anything already keying on it.
	tags["spawn:managed"] = "true"
	tags["spawn:created-by"] = "spawn"
	tags["spawn:created-at"] = time.Now().UTC().Format(time.RFC3339)
	tags["spawn:created"] = tags["spawn:created-at"]

	iamTags := make([]types.Tag, 0, len(tags))
	for key, value := range tags {
		iamTags = append(iamTags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	return iamTags
}

// roleExists checks if an IAM role exists
func (c *Client) roleExists(ctx context.Context, iamClient *iam.Client, roleName string) (bool, error) {
	_, err := iamClient.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		// Check if it's a not found error
		if strings.Contains(err.Error(), "NoSuchEntity") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check role existence: %w", err)
	}
	return true, nil
}

// instanceProfileExists checks if an instance profile exists
func (c *Client) instanceProfileExists(ctx context.Context, iamClient *iam.Client, profileName string) (bool, error) {
	_, err := iamClient.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	})
	if err != nil {
		// Check if it's a not found error
		if strings.Contains(err.Error(), "NoSuchEntity") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check instance profile existence: %w", err)
	}
	return true, nil
}
