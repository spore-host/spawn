package aws

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/elasticip"
)

// SporedRoleName and SporedInstanceProfileName are the SHARED, long-lived IAM
// identities spawn creates once and reuses for every launch. Named constants
// because cleanup has to recognise them (spawn#713): they are not per-run
// resources and must never be swept just because nothing is running right now.
const (
	SporedRoleName            = "spored-instance-role"
	SporedInstanceProfileName = "spored-instance-profile"
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

// isConcurrentModification reports whether err is IAM's "wait and try again"
// response to two racing operations on the same resource (spawn#648).
//
// This is the error IAM actually returns when tagged resource creation races:
//
//	ConcurrentModification: The previous tagging operation is still ongoing.
//	Please wait for a while and perform the next tagging operation until it
//	finishes.
//
// It is distinct from throttling — the request was accepted and rejected on
// conflict, not rate-limited — and from EntityAlreadyExists, because the
// resource may not exist yet. Before this it matched neither predicate, so
// retryIAM returned on the FIRST attempt without ever sleeping, which is the
// opposite of what IAM is asking for.
//
// Every CreateRole/CreateInstanceProfile call on the launch path passes Tags,
// and it is that implicit tagging operation which serialises — so this is the
// COMMON concurrent-launch failure, not the rare one. Reported from two
// `spawn task run` invocations ~60ms apart (a Nextflow executor fanning out two
// processes): one succeeded, the other died at CreateInstanceProfile and took
// the whole DAG with it.
func isConcurrentModification(err error) bool {
	if err == nil {
		return false
	}
	if strings.HasPrefix(iamErrorCode(err), "ConcurrentModification") {
		return true
	}
	// Fallback for non-modeled errors that only carry the code in the message
	// (emulators, wrapped errors), mirroring isThrottle/isAlreadyExists.
	return strings.Contains(err.Error(), "ConcurrentModification")
}

// retryIAM runs fn, retrying on throttling with a short backoff. "Already
// exists" is treated as success (the resource is present, which is the goal),
// so concurrent launches racing to ensure the same shared role/profile converge
// instead of failing (#64).
//
// ConcurrentModification is retried for the same reason (spawn#648): IAM
// documents it as a wait-and-retry condition, and the existing
// 500ms × attempt backoff over 5 attempts (7.5s total) is the right shape for a
// tagging operation that has to drain.
func retryIAM(fn func() error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = fn()
		if err == nil || isAlreadyExists(err) {
			return nil
		}
		if !isThrottle(err) && !isConcurrentModification(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return err
}

// IAMRoleConfig contains configuration for creating IAM roles
type IAMRoleConfig struct {
	RoleName         string            // User-provided or auto-generated
	Policies         []string          // Service-level policies (s3:ReadOnly, etc.)
	ManagedPolicies  []string          // AWS managed policy ARNs
	PolicyFile       string            // Path to custom policy JSON
	InlinePolicyJSON string            // Custom policy JSON supplied as a string (like PolicyFile, no file); e.g. a scoped S3 policy
	TrustServices    []string          // Services that can assume role
	Tags             map[string]string // Tags for role
}

// GenerateScopedS3Policy creates an S3 policy scoped to spawn resources.
//
// The results bucket is spawn-results-<account>-<region>, the one spawn creates
// and writes to (taskproto.DefaultResultsBucket). It used to be
// spawn-results-<region>, a bucket spawn never uses, with accountID accepted and
// ignored, so a role built from this could not write results (#1049).
//
// Deprecated: nothing calls this, in spawn or in any repository importing
// pkg/aws (#1049). Build a policy for the buckets a launch actually uses
// instead, as `spawn task run` and `spawn pool` do. It will be removed in a
// future minor release.
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
					"arn:aws:s3:::spawn-results-%s-%s/*",
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
					"arn:aws:s3:::spawn-results-%s-%s",
					"arn:aws:s3:::spawn-schedules-*"
				]
			}
		]
	}`, region, accountID, region, region, accountID, region)
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

// FullAccessPolicyTemplates are the built-in templates that grant a service
// wildcard action on Resource "*" (e.g. s3:* on *). They are powerful enough to
// be dangerous on an instance role, so requesting one requires an explicit
// opt-in (the --iam-allow-full-access flag / AllowFullAccessPolicies); otherwise
// the scoped ReadOnly/WriteOnly variants should be used. See ValidatePolicyNames.
var FullAccessPolicyTemplates = map[string]bool{
	"s3:FullAccess":       true,
	"dynamodb:FullAccess": true,
	"sqs:FullAccess":      true,
}

// ValidatePolicyNames checks a list of requested --iam-policy template names.
// Unknown names are rejected. Wildcard *:FullAccess templates are rejected unless
// allowFullAccess is true, steering callers toward the scoped variants by default
// (2026-06 audit, M-sec: wildcard IAM attachable to the instance role).
func ValidatePolicyNames(policies []string, allowFullAccess bool) error {
	for _, p := range policies {
		if _, ok := PolicyTemplates[p]; !ok {
			return fmt.Errorf("unknown IAM policy template %q (see --help for the built-in set, e.g. s3:ReadOnly)", p)
		}
		if FullAccessPolicyTemplates[p] && !allowFullAccess {
			return fmt.Errorf("%q grants wildcard access (%s on all resources) to the instance role; "+
				"use a scoped template like %s, or pass --iam-allow-full-access to opt in deliberately",
				p, strings.SplitN(p, ":", 2)[0]+":*", scopedAlternative(p))
		}
	}
	return nil
}

// scopedAlternative suggests the ReadOnly variant of a FullAccess template name.
func scopedAlternative(fullAccess string) string {
	svc := strings.SplitN(fullAccess, ":", 2)[0]
	return svc + ":ReadOnly (or " + svc + ":WriteOnly)"
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
// scoped to config: RoleName (or, if empty, a deterministic
// spawn-instance-<hash of Policies|ManagedPolicies|PolicyFile|InlinePolicyJSON>
// name — see generateRoleName/hashPolicies) with exactly the policies config
// describes attached.
//
// #550 root cause: spawn has TWO independent instance-profile resolution paths,
// and which one a given launch takes is decided entirely by whether that
// specific invocation passed IAM configuration, NOT by any property of the
// workload or the account:
//
//   - Callers that pass IAM config (any CLI --iam-role/--iam-policy/
//     --iam-policy-file flag, `spawn task run`'s always-on scoped S3 staging
//     policy, `spawn spawn-pool-worker`'s SQS+S3 policy, …) call THIS function.
//     It creates (or reuses, if the hash matches) a profile named after that
//     config's hash/RoleName, carrying exactly the policies that config
//     specifies plus the spored self-management baseline
//     (ensureSporedBaselinePolicy) and SSM.
//   - Callers that pass NONE (plain `spawn launch` with no --iam-* flags,
//     `spawn resume`, a batch queue with no --iam-role, a sweep row with no
//     iam_role:) call SetupSporedIAMRole instead, which ALWAYS returns the
//     single fixed name "spored-instance-profile"/"spored-instance-role" — a
//     shared, account-wide role whose ONLY guaranteed grants are the spored
//     self-management baseline + SSM. It carries no S3/data-bucket access
//     unless something else has separately attached an inline/managed policy
//     to that exact role name.
//
// So two launches that a user perceives as "the same kind of launch, days
// apart" land on completely different profiles — one scoped to the workload's
// actual bucket, one not — purely because one of them happened to go through
// `spawn task run` (or carried an explicit --iam-policy-file) and the other was
// a bare `spawn launch`/`spawn resume`/queue/sweep-row invocation with no IAM
// flags at all. This is NOT hash nondeterminism within this function — a given
// config always hashes to the same name — it's two different call sites
// disagreeing about which grants a workload needs, with no signal to the user
// about which one fired. --instance-profile (#550) sidesteps both paths
// entirely by naming the profile to attach outright; the launch/list/status
// commands now also print whichever profile actually got resolved, so the
// divergence is visible without reading ec2:DescribeInstances by hand.
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
	} else if len(config.Policies) > 0 || config.PolicyFile != "" || config.InlinePolicyJSON != "" {
		// Role exists but caller specified policies — update the inline policy so that
		// re-using a cached role (same hash) still picks up any new policy additions.
		if err := c.updateInlinePolicy(ctx, iamClient, roleName, config); err != nil {
			return "", fmt.Errorf("failed to update IAM role policy: %w", err)
		}
	}

	// Guarantee SSM on user-supplied/custom roles too (spore.host baseline —
	// applies to new and pre-existing roles alike; idempotent).
	if err := c.ensureSSMManagedPolicy(ctx, iamClient, roleName); err != nil {
		return "", err
	}

	// Guarantee the spored self-management baseline on every role this
	// function returns, unconditionally — new or pre-existing, and regardless
	// of which policy source (Policies/PolicyFile/InlinePolicyJSON/managed-only)
	// the caller used. PutRolePolicy overwrites, so this is idempotent and safe
	// to call every time; it also heals a role cached from before this fix
	// existed. Without it spored can't read its own tags at all, so TTL,
	// --on-complete and --pre-stop silently never fire (spawn#502 — the
	// PolicyFile path had never received the spawn#406 fix that the other two
	// policy sources did; this makes the guarantee independent of the branch
	// taken instead of per-branch, so a future policy source can't reopen it).
	if err := c.ensureSporedBaselinePolicy(ctx, iamClient, roleName); err != nil {
		return "", err
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

		// Wait for the instance profile to become readable (IAM eventual
		// consistency), by POLLING rather than sleeping blind (#752).
		//
		// This was `time.Sleep(10 * time.Second)` — and waitForInstanceProfile,
		// 830 lines below in this same file, exists precisely to replace it. Its
		// own doc comment says so: it "returns as soon as the profile is readable
		// — instantly against a strongly consistent endpoint — instead of a blind
		// fixed sleep". The blind sleep it describes was still here.
		//
		// Beyond the wasted 10 seconds on every role setup, the sleep was not
		// ctx-aware, so a Ctrl-C during it was ignored; and 10s is simultaneously
		// too long for the common case and no guarantee for the slow one. The
		// statement immediately below is already GetInstanceProfile, i.e. exactly
		// the readiness probe this now runs.
		waitForInstanceProfile(ctx, iamClient, profileName)
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

	// An existing profile is only usable if it holds this role (#1318). One
	// can exist empty: a cleanup or reaper delete (pkg/instancerole) that
	// removed the role and had not yet deleted the profile, or an earlier
	// launch that died between CreateInstanceProfile and AddRoleToInstanceProfile.
	// Handed to RunInstances as it was, it booted an instance with no
	// credentials, so spored could not read its tags or end itself. Re-add.
	if profileExists {
		if err := ensureRoleInProfile(ctx, iamClient, profileName, roleName, profile.InstanceProfile.Roles); err != nil {
			return "", err
		}
	}

	// Return the profile name, not the ARN — EC2 RunInstances iamInstanceProfile.name
	// expects the bare name, not "arn:aws:iam::ACCOUNT:instance-profile/NAME".
	return profileName, nil
}

// ensureRoleInProfile makes sure the existing instance profile name holds
// roleName, adding it when the profile is empty (#1318). A profile holding a
// different role is an error rather than something to repair: a profile has
// one role, and replacing someone else's would take their instance's
// credentials away.
func ensureRoleInProfile(ctx context.Context, iamClient *iam.Client, profileName, roleName string, roles []types.Role) error {
	for _, r := range roles {
		if aws.ToString(r.RoleName) == roleName {
			return nil
		}
	}
	if len(roles) > 0 {
		return fmt.Errorf("instance profile %s holds role %s, not %s", profileName, aws.ToString(roles[0].RoleName), roleName)
	}
	err := retryIAM(func() error {
		_, e := iamClient.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
			RoleName:            aws.String(roleName),
		})
		return e
	})
	// LimitExceeded: a concurrent launch re-added it first, which is the
	// outcome wanted. Anything else is a real failure.
	if err != nil && !strings.Contains(err.Error(), "LimitExceeded") {
		return fmt.Errorf("re-attach role %s to instance profile %s: %w", roleName, profileName, err)
	}
	waitForInstanceProfile(ctx, iamClient, profileName)
	return nil
}

// generateRoleName creates a deterministic role name based on policies.
//
// Note (#550, #1300): this function never deletes a spawn-instance-<hash>
// role/profile — it cannot know whether another running instance uses one.
// pkg/instancerole is the deleter: the TTL reaper (REAPER_INSTANCE_ROLES) and
// `spawn cleanup --instance-roles` remove a pair once it and its profile are
// tagged spawn:managed=true (buildIAMTags), no instance has the profile
// attached, and the role is 24h old and unused for 24h.
func (c *Client) generateRoleName(config IAMRoleConfig) string {
	return PredictedInstanceRoleName(config)
}

// hashPolicies generates a hash of all policy sources for role naming
func (c *Client) hashPolicies(config IAMRoleConfig) string {
	return hashPolicies(config)
}

// PredictedInstanceRoleName computes the same deterministic
// "spawn-instance-<hash>" role/profile name [Client.CreateOrGetInstanceProfile]
// would resolve to for this policy config, with NO AWS calls — it is a pure
// function of config.Policies/ManagedPolicies/PolicyFile/InlinePolicyJSON.
//
// Exported so a preview path (e.g. `spawn launch --dry-run`, spawn#569) can show
// the instance profile a real launch would create-or-reuse, without touching IAM.
// This intentionally does NOT cover config.RoleName being explicitly set (the
// caller-named-role branch) or the SetupSporedIAMRole default-role branch (no
// IAM flags at all) — callers preview those directly since both are already
// known without any hashing.
func PredictedInstanceRoleName(config IAMRoleConfig) string {
	policyHash := hashPolicies(config)
	return fmt.Sprintf("spawn-instance-%s", policyHash[:8])
}

// hashPolicies generates a hash of all policy sources for role naming.
func hashPolicies(config IAMRoleConfig) string {
	// Combine all policy sources for hashing
	data := fmt.Sprintf("%v|%v|%s|%s",
		config.Policies,
		config.ManagedPolicies,
		config.PolicyFile,
		config.InlinePolicyJSON)

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

	// Build trust policy with account condition. An instance profile is always
	// assumed by the EC2 service, so default to "ec2" when the caller gave no
	// trust services — otherwise the policy has an empty principal and IAM rejects
	// it with MalformedPolicyDocument ("statement with no principals").
	trustServices := config.TrustServices
	if len(trustServices) == 0 {
		trustServices = []string{"ec2"}
	}
	trustPolicy := c.buildTrustPolicyWithAccount(trustServices, accountID)
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
		doc, err := c.sporedPolicyDoc(ctx, string(policyJSON))
		if err != nil {
			return err
		}

		_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spawn-inline-policy"),
			PolicyDocument: aws.String(doc),
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

	// Attach a caller-supplied scoped policy passed as a string (no file).
	// This path bypasses buildInlinePolicy; ensureSporedBaselinePolicy (called
	// unconditionally by CreateOrGetInstanceProfile on every role) is what
	// covers what spawn#406 originally attached here directly.
	if config.InlinePolicyJSON != "" {
		_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spawn-scoped-policy"),
			PolicyDocument: aws.String(config.InlinePolicyJSON),
		})
		if err != nil {
			return fmt.Errorf("failed to attach scoped policy: %w", err)
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

// updateInlinePolicy replaces the inline policy on an existing role. It refreshes
// the shorthand-derived inline policy (when Policies is set), the custom policy
// file (when PolicyFile is set), and the caller-supplied scoped policy string
// (when InlinePolicyJSON is set), so a cached role picks up any of them.
func (c *Client) updateInlinePolicy(ctx context.Context, iamClient *iam.Client, roleName string, config IAMRoleConfig) error {
	if len(config.Policies) > 0 {
		policy := c.buildInlinePolicy(config.Policies)
		policyJSON, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("failed to marshal inline policy: %w", err)
		}
		doc, err := c.sporedPolicyDoc(ctx, string(policyJSON))
		if err != nil {
			return err
		}
		if _, err := iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spawn-inline-policy"),
			PolicyDocument: aws.String(doc),
		}); err != nil {
			return err
		}
	}
	if config.PolicyFile != "" {
		policyDoc, err := os.ReadFile(config.PolicyFile)
		if err != nil {
			return fmt.Errorf("failed to read policy file: %w", err)
		}
		if _, err := iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spawn-custom-policy"),
			PolicyDocument: aws.String(string(policyDoc)),
		}); err != nil {
			return fmt.Errorf("failed to attach custom policy: %w", err)
		}
	}
	if config.InlinePolicyJSON != "" {
		if _, err := iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spawn-scoped-policy"),
			PolicyDocument: aws.String(config.InlinePolicyJSON),
		}); err != nil {
			return err
		}
	}
	return nil
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
	statements := c.sporedSelfManagementStatements()

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

// sporedBaselinePolicyDoc returns the spored self-management policy as a
// standalone JSON document, attached via ensureSporedBaselinePolicy to every
// instance role regardless of policy source — without these grants spored
// can't read its own tags, so TTL and on-complete silently never fire
// (spawn#406, spawn#502). Returns "" only on a marshal error (never expected
// for this static content).
func (c *Client) sporedBaselinePolicyDoc() string {
	doc := map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": c.sporedSelfManagementStatements(),
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	return string(b)
}

// sporedSelfManagementStatements returns the policy statements every
// spored-running instance role MUST carry, regardless of any additional
// user/scoped policy: read its own tags (TTL/idle/on-complete/completion-file),
// keep lifecycle tags current, terminate/stop itself, invoke the DNS updater,
// and mount an async FSx. Destructive + tag-write actions are scoped to
// spawn:managed=true. Shared by buildInlinePolicy and sporedBaselinePolicyDoc so
// the two paths can never drift.
func (c *Client) sporedSelfManagementStatements() []interface{} {
	statements := []interface{}{}

	// ALWAYS include spored-required EC2 permissions for self-management
	// This allows spored agent to:
	// - Read its own tags (TTL, idle timeout, etc.)
	// - Keep its lifecycle tags current (compute-seconds, fsx-id, …)
	// - Terminate/stop itself when conditions are met
	// The destructive and tag-write actions are scoped to spawn:managed=true.

	// Describe* are read-only and stay on "*" (Describe APIs don't support
	// resource-level IAM scoping). DescribeVolumes is required alongside
	// DescribeInstances for LookupAndTagEBSCost (pkg/provider/ec2.go): it reads
	// the block-device mappings from DescribeInstances, then needs
	// DescribeVolumes for the actual sizes/types to price them. Without it, the
	// EBS cost lookup 403s on every instance whose role carries this policy —
	// which #502 made every spawn-launched instance — and silently falls back
	// to an unmeasured constant (spawn#517).
	sporedReadPermissions := map[string]interface{}{
		"Effect": "Allow",
		"Action": []interface{}{
			"ec2:DescribeTags",
			"ec2:DescribeInstances",
			"ec2:DescribeVolumes",
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
	//
	// That reasoning is correct and was not sufficient: re-tagging an instance
	// already in scope is itself an attack when the reaper EXECUTES a tag's value.
	// See reaperControlTagKeys below, and the Deny appended right after this
	// statement — GHSA-8q3p-qcv8-2pr4.
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
	statements = append(statements, reaperControlTagDeny())

	// ec2:CancelSpotInstanceRequests (#922): spored cancels its own persistent
	// Spot request before it self-terminates, or EC2 launches a replacement. The
	// request carries the instance's tags, so the same spawn:managed condition
	// scopes it.
	sporedActionPermissions := map[string]interface{}{
		"Effect": "Allow",
		"Action": []interface{}{
			"ec2:TerminateInstances",
			"ec2:StopInstances",
			"ec2:CancelSpotInstanceRequests",
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
	//
	// This is spore.host's updater only. A custom updater's grant (#1158) is a
	// separate inline policy, added by EnsureDNSInvokeGrant: these roles are
	// shared between launches, so a launch-specific Resource here would be
	// overwritten by the next launch to use a different updater.
	statements = append(statements, sporedDNSInvokeStatement(spawnconfig.DefaultDNSFunctionARN))

	// Allow spored to mount an async-created ephemeral FSx (#194/#221). The mount
	// path polls fsx:DescribeFileSystems until AVAILABLE and sets up the S3 export
	// via fsx:CreateDataRepositoryAssociation. Without these, the mount silently
	// failed with AccessDenied (the spored role had NO fsx:* perms) — one of the
	// two causes of #221 (the other was a startup race). These FSx APIs don't
	// support resource-level scoping for Describe, so Resource is "*"; spored only
	// ever touches the filesystem its own instance was tagged with.
	sporedFSxMount := map[string]interface{}{
		"Effect": "Allow",
		"Action": []interface{}{
			"fsx:DescribeFileSystems",
			"fsx:CreateDataRepositoryAssociation",
			"fsx:DescribeDataRepositoryAssociations",
		},
		"Resource": "*",
	}
	statements = append(statements, sporedFSxMount)

	// FSx creates a per-filesystem service-linked role
	// (AWSServiceRoleForFSxS3Access_<fsid>) on the FIRST
	// CreateDataRepositoryAssociation in an account, and that requires the CALLING
	// principal to hold iam:CreateServiceLinkedRole. #221 added the fsx:* actions
	// above — necessary but not sufficient — so every DRA on a fresh filesystem
	// failed with "Amazon FSx is unable to create Service-Linked-Role to access the
	// S3 bucket" (#622).
	//
	// The cost of missing this was silent and large: spored mounts anyway by design,
	// so --fsx-import-path produced a mounted but EMPTY 1200 GiB filesystem (the FSx
	// Lustre minimum, ~$174/month) while the CLI printed "Instance Ready!" and the
	// workload read an empty directory.
	//
	// It looked account-specific because the role persists once created: anyone who
	// had ever made a DRA by hand never saw it.
	//
	// Scoped by condition to the one service principal that needs it, so this is not
	// a general service-linked-role creation grant.
	statements = append(statements, map[string]interface{}{
		"Effect":   "Allow",
		"Action":   "iam:CreateServiceLinkedRole",
		"Resource": "*",
		"Condition": map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"iam:AWSServiceName": "s3.data-source.lustre.fsx.amazonaws.com",
			},
		},
	})

	statements = append(statements, sporedHybridRegistryStatement())
	statements = append(statements, sporedSelfTerminateStatement())
	statements = append(statements, sporedOwnElasticIPStatements()...)

	return statements
}

// sporedHybridRegistryActions are exactly the DynamoDB calls the in-instance
// registry client (pkg/registry.PeerRegistry) makes: Register is PutItem, the
// heartbeat is UpdateItem, Deregister on shutdown is DeleteItem, DiscoverPeers is
// Query. No Scan, no batch calls, no table management — EnsureTable runs on the
// workstation under the user's own credentials (#905). A test derives this list
// from the client's source so the two cannot drift.
var sporedHybridRegistryActions = []string{
	"dynamodb:PutItem",
	"dynamodb:UpdateItem",
	"dynamodb:DeleteItem",
	"dynamodb:Query",
}

// sporedHybridRegistryResource is the registry table in the instance's OWN
// account, any region. The account is sporedAccountPlaceholder, bound to the
// literal account ID when the policy is written (bindPolicyAccount).
const sporedHybridRegistryResource = "arn:aws:dynamodb:*:" + sporedAccountPlaceholder + ":table/spawn-hybrid-registry"

// sporedAccountPlaceholder stands for the role's own account ID in the spored
// policy templates, and bindPolicyAccount replaces it before PutRolePolicy.
//
// It used to be ${aws:PrincipalAccount}, so the static sporedDCVRolePolicy could
// carry the grant without knowing the account. IAM does not accept that variable
// in a Resource: PutRolePolicy rejects the whole document with
// "MalformedPolicyDocument: The policy failed legacy parsing", so every launch
// that set up the spored role failed (#1069). IAM Access Analyzer and
// simulate-custom-policy both accept the same document, which is why nothing
// short of a real PutRolePolicy caught it. Deliberately not a 12-digit number and
// not "*": an unbound document is refused by bindPolicyAccount rather than
// written with an account that matches nothing, or every account.
const sporedAccountPlaceholder = "SPAWN-ACCOUNT-ID"

// accountIDPattern is a 12-digit AWS account ID.
var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// bindPolicyAccount returns doc with sporedAccountPlaceholder replaced by
// accountID. It refuses an account that is not 12 digits, and a document that
// still carries ${aws:PrincipalAccount}, which PutRolePolicy rejects (#1069).
func bindPolicyAccount(doc, accountID string) (string, error) {
	if strings.Contains(doc, "${aws:PrincipalAccount}") {
		return "", fmt.Errorf("policy uses ${aws:PrincipalAccount}, which IAM rejects as MalformedPolicyDocument (#1069)")
	}
	if !strings.Contains(doc, sporedAccountPlaceholder) {
		return doc, nil
	}
	if !accountIDPattern.MatchString(accountID) {
		return "", fmt.Errorf("cannot bind the spored policy to account %q: not a 12-digit account ID", accountID)
	}
	return strings.ReplaceAll(doc, sporedAccountPlaceholder, accountID), nil
}

// sporedPolicyDoc binds a spored policy template to the caller's account — the
// account the role is created in — ready for PutRolePolicy.
func (c *Client) sporedPolicyDoc(ctx context.Context, doc string) (string, error) {
	if !strings.Contains(doc, sporedAccountPlaceholder) {
		return bindPolicyAccount(doc, "")
	}
	acct, _, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve the account for the spored policy: %w", err)
	}
	return bindPolicyAccount(doc, acct)
}

// sporedHybridRegistryStatement lets spored register a job-array member in the
// hybrid registry (#905). Every spored whose instance carries spawn:job-array-id
// calls registry.Register at startup; neither spored policy granted any action
// on the table, so registration could not succeed on any member (it is a logged
// warning, so nobody noticed), and `spawn burst`, whose whole purpose is cloud
// members joining a registry-coordinated array, could not work. (It also failed
// earlier, for want of a region; that half is #899.)
//
// Scoped to the one table and the four item-level actions the client uses. It
// cannot be narrowed further to the member's own item: the partition key is the
// job-array id, which the policy cannot know. That is strictly less than the
// role already holds — it can terminate any spawn-managed instance in the
// account.
func sporedHybridRegistryStatement() map[string]interface{} {
	actions := make([]interface{}, 0, len(sporedHybridRegistryActions))
	for _, a := range sporedHybridRegistryActions {
		actions = append(actions, a)
	}
	return map[string]interface{}{
		"Effect":   "Allow",
		"Action":   actions,
		"Resource": sporedHybridRegistryResource,
	}
}

// sporedDNSInvokeStatement lets the instance role call a DNS updater's Function
// URL under AWS_IAM auth (#173). functionARN is spore.host's in both baseline
// policies, and a custom updater's in the policy EnsureDNSInvokeGrant adds.
func sporedDNSInvokeStatement(functionARN string) map[string]interface{} {
	return map[string]interface{}{
		"Effect":   "Allow",
		"Action":   []interface{}{"lambda:InvokeFunctionUrl"},
		"Resource": functionARN,
	}
}

// DNSInvokePolicyName is the inline policy EnsureDNSInvokeGrant writes for
// functionARN: one per function, so grants for different updaters accumulate on
// a shared role instead of replacing each other.
func DNSInvokePolicyName(functionARN string) string {
	sum := sha256.Sum256([]byte(functionARN))
	return fmt.Sprintf("spored-dns-invoke-%x", sum[:4])
}

// EnsureDNSInvokeGrant lets roleName's instances register DNS through a custom
// updater (#1158): it writes an inline policy granting lambda:InvokeFunctionUrl
// on functionARN. A Function URL call across accounts needs an allow in both the
// function's resource policy and the caller's identity policy, and the role's
// baselines grant only spore.host's function, so without this a self-hosted
// updater answers 403 and spored records spawn:dns-status=failed.
//
// A separate policy, not an edit to the baseline: spored-instance-role and the
// spawn-instance-<hash> roles are shared by every launch with the same IAM
// configuration, and the baselines are rewritten on each one, so a
// launch-specific Resource there would be replaced by the next launch through a
// different updater, cutting off the instances already running. Idempotent.
func (c *Client) EnsureDNSInvokeGrant(ctx context.Context, roleName, functionARN string) error {
	if functionARN == "" {
		return fmt.Errorf("EnsureDNSInvokeGrant: no function ARN")
	}
	b, err := json.Marshal(map[string]interface{}{
		"Version":   "2012-10-17",
		"Statement": []interface{}{sporedDNSInvokeStatement(functionARN)},
	})
	if err != nil {
		return err
	}
	doc, err := c.sporedPolicyDoc(ctx, string(b))
	if err != nil {
		return err
	}
	iamClient := iam.NewFromConfig(c.cfg)
	name := DNSInvokePolicyName(functionARN)
	err = retryIAM(func() error {
		_, e := iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String(name),
			PolicyDocument: aws.String(doc),
		})
		return e
	})
	if err != nil {
		return fmt.Errorf("grant %s lambda:InvokeFunctionUrl on the DNS updater %s: %w", roleName, functionARN, err)
	}
	return nil
}

// sporedSelfTerminateStatement lets an instance terminate ITSELF, and nothing
// else, whatever its tags (#1080).
//
// The grant above is scoped to spawn:managed=true, which every instance spawn
// launches carries. A replacement EC2 launches from a leaked persistent Spot
// request does not: it has no tags at all. So when spored on a replacement
// retired it (#1008) it could cancel the request but not terminate the
// instance, and fell back to an OS shutdown. With EC2's default shutdown
// behaviour that STOPS it: untagged, request cancelled, never started again,
// beyond the reaper's tag-scoped terminate, and billing its EBS volume forever.
//
// aws:ARN is the resource's ARN and ec2:SourceInstanceARN is the instance the
// calling credentials were issued to, so the condition holds only for a call
// about the caller itself. Proven on real AWS before this was written: IAM's
// PutRolePolicy accepts and stores the policy variable (unlike
// ${aws:PrincipalAccount} in a Resource, #1069), and from an untagged instance a
// dry-run terminate of another instance was refused, of itself allowed, of
// itself plus another refused, and a real self-terminate succeeded.
func sporedSelfTerminateStatement() map[string]interface{} {
	return map[string]interface{}{
		"Sid":      "TerminateOnlySelf",
		"Effect":   "Allow",
		"Action":   "ec2:TerminateInstances",
		"Resource": "*",
		"Condition": map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"aws:ARN": "${ec2:SourceInstanceARN}",
			},
		},
	}
}

// sporedOwnElasticIPStatements let spored release the Elastic IP a
// multi-interface EFA launch allocated for it (#661), and no other address.
//
// Why spored needs it: only `spawn terminate` released the address, so every
// other way an instance ends (TTL, idle, cost limit, on-complete, a Spot
// reclaim) left it allocated and billing. spored makes one ReleaseAddress after
// its own TerminateInstances and on a Spot terminate notice
// (pkg/elasticip.ReleaseForInstance).
//
// The scope. DescribeAddresses has no resource-level permissions, so it is on
// "*" like the other Describes here; it is read-only. ReleaseAddress is the
// widening, and it is conditioned on BOTH tags, so it is not a grant on spawn's
// addresses in general:
//
//   - ec2:ResourceTag/spawn:instance-arn must equal ${ec2:SourceInstanceARN},
//     the ARN of the instance whose role credentials signed the request. Launch
//     writes that tag at allocation, from RunInstances' own answer. Another
//     instance's address carries another ARN; a user's address carries none.
//     A request not made with instance credentials has no SourceInstanceARN,
//     the variable resolves to nothing, and StringEquals does not match.
//   - ec2:ResourceTag/spawn:managed must be true.
//
// The instance cannot forge the first tag on someone else's address: CreateTags
// is allowed on any spawn:managed=true resource, which includes these addresses,
// so spawn:instance-arn and spawn:instance-id are in reaperControlTagKeys and
// the Deny below covers them. The variable is in a Condition, never a Resource,
// which is what PutRolePolicy rejected in #1069; it is the same variable the
// TerminateOnlySelf statement uses, which was proven on real AWS (#1080).
func sporedOwnElasticIPStatements() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"Sid":      "DescribeElasticIPs",
			"Effect":   "Allow",
			"Action":   "ec2:DescribeAddresses",
			"Resource": "*",
		},
		map[string]interface{}{
			"Sid":      "ReleaseOnlyOwnElasticIP",
			"Effect":   "Allow",
			"Action":   "ec2:ReleaseAddress",
			"Resource": "*",
			"Condition": map[string]interface{}{
				"StringEquals": map[string]interface{}{
					"ec2:ResourceTag/" + elasticip.ManagedTagKey:     "true",
					"ec2:ResourceTag/" + elasticip.InstanceARNTagKey: "${ec2:SourceInstanceARN}",
				},
			},
		},
	}
}

// sporedOwnElasticIPStatementsJSON renders them for the static DCV policy, so
// the two policies cannot drift on them.
func sporedOwnElasticIPStatementsJSON() string {
	parts := make([]string, 0, 2)
	for _, st := range sporedOwnElasticIPStatements() {
		b, err := json.Marshal(st)
		if err != nil {
			panic(err) // static content; cannot fail
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ",\n    ")
}

// sporedSelfTerminateStatementJSON renders it for the static DCV policy, so the
// two policies cannot drift on it.
func sporedSelfTerminateStatementJSON() string {
	b, err := json.Marshal(sporedSelfTerminateStatement())
	if err != nil {
		panic(err) // static content; cannot fail
	}
	return string(b)
}

// sporedHybridRegistryStatementJSON renders the same statement for the static
// DCV policy, so the two policies cannot drift on it.
func sporedHybridRegistryStatementJSON() string {
	b, err := json.Marshal(sporedHybridRegistryStatement())
	if err != nil {
		panic(err) // static content; cannot fail
	}
	return string(b)
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

// SetupSporedIAMRole creates or retrieves the IAM role and instance profile for spored
// Returns the instance profile name
// ssmManagedInstanceCoreARN is the AWS managed policy that lets an instance
// register with SSM (Session Manager + RunCommand). spore.host guarantees this
// on every spawn'd instance: `spawn connect` uses it (required on Windows, a
// no-public-IP fallback on Linux), and the MPI cohort's control-plane peer
// assembly + readiness probes push/run over SSM (#cohort-mpi). Because there is
// no on-instance fallback for that path, SSM must be present, so attaching it
// is a hard requirement on every instance-profile path, not best-effort.
const ssmManagedInstanceCoreARN = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"

// ensureSSMManagedPolicy attaches AmazonSSMManagedInstanceCore to roleName.
// Idempotent: AttachRolePolicy is a no-op if the policy is already attached, so
// this is safe to call on both freshly-created and pre-existing roles. Returns
// an error on failure (unlike the previous best-effort attach) so no spawn'd
// instance silently lacks SSM.
func (c *Client) ensureSSMManagedPolicy(ctx context.Context, iamClient *iam.Client, roleName string) error {
	err := retryIAM(func() error {
		_, e := iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(ssmManagedInstanceCoreARN),
		})
		return e
	})
	if err != nil {
		return fmt.Errorf("attach AmazonSSMManagedInstanceCore to %s: %w", roleName, err)
	}
	return nil
}

// ensureSporedBaselinePolicy attaches the spored self-management inline policy
// to roleName. Idempotent: PutRolePolicy overwrites an existing inline policy
// of the same name, so this is safe to call on every role on every call —
// new, pre-existing, and any policy source (Policies/PolicyFile/InlinePolicyJSON/
// managed-policies-only). Without these grants spored cannot read its own
// instance tags, so TTL, --on-complete and --pre-stop silently never fire
// (spawn#502 — the PolicyFile path never received the spawn#406 fix that the
// other two policy sources did, because it was attached per-branch instead of
// unconditionally here).
func (c *Client) ensureSporedBaselinePolicy(ctx context.Context, iamClient *iam.Client, roleName string) error {
	doc, err := c.sporedPolicyDoc(ctx, c.sporedBaselinePolicyDoc())
	if err != nil {
		return err
	}
	err = retryIAM(func() error {
		_, e := iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyName:     aws.String("spored-baseline-policy"),
			PolicyDocument: aws.String(doc),
		})
		return e
	})
	if err != nil {
		return fmt.Errorf("attach spored-baseline-policy to %s: %w", roleName, err)
	}
	return nil
}

// SetupSporedIAMRole is the OTHER instance-profile resolution path (see the
// #550 note on CreateOrGetInstanceProfile above for the full contrast): it
// always resolves to the SAME fixed name, regardless of what the workload
// needs, with no data-bucket access beyond whatever has separately been
// attached to that exact shared role. Callers that pass no IAM configuration
// at all land here; callers that pass any --iam-role/--iam-policy/
// --iam-policy-file (or a task/pool-worker's built-in scoped policy) land on
// CreateOrGetInstanceProfile instead. Two otherwise-identical `spawn launch`
// invocations landing on different SIDES of that split is exactly #550's
// symptom.
func (c *Client) SetupSporedIAMRole(ctx context.Context) (string, error) {
	iamClient := iam.NewFromConfig(c.cfg)

	roleName := SporedRoleName
	instanceProfileName := SporedInstanceProfileName
	policyName := "spored-policy"

	roleCreated := false
	profileCreated := false

	// 1. Check if role exists, create if not
	_, err := iamClient.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})

	if err != nil {
		roleCreated = true
		// Role doesn't exist, create it
		// retryIAM, not a bare call (spawn#648): this passes Tags, so two concurrent
		// launches racing to create the shared spored role hit IAM's
		// ConcurrentModification on the implicit tagging operation. retryIAM also
		// treats EntityAlreadyExists as success, which is what the string match
		// below was doing by hand — so the behaviour is preserved and the retry is
		// inherited rather than reimplemented.
		err = retryIAM(func() error {
			_, e := iamClient.CreateRole(ctx, &iam.CreateRoleInput{
				RoleName:                 aws.String(roleName),
				AssumeRolePolicyDocument: aws.String(sporedTrustPolicy),
				Description:              aws.String("IAM role for spored daemon on EC2 instances"),
				Tags: []types.Tag{
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
				},
			})
			return e
		})
		if err != nil && !contains(err.Error(), "EntityAlreadyExists") {
			return "", fmt.Errorf("failed to create IAM role: %w", err)
		}
	}

	// 2. Attach inline policy to role (sporedDCVRolePolicy, defined below),
	// bound to this account (#1069).
	dcvDoc, err := c.sporedPolicyDoc(ctx, sporedDCVRolePolicy)
	if err != nil {
		return "", err
	}
	_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(dcvDoc),
	})
	if err != nil {
		return "", fmt.Errorf("failed to attach policy to role: %w", err)
	}

	// 2b. Attach AmazonSSMManagedInstanceCore so the instance registers with SSM.
	// spore.host guarantees SSM on every instance: `spawn connect` uses it, and
	// the MPI cohort assembles peers / probes readiness over SSM with no
	// on-instance fallback — so a missing attach is now a hard error, not a warn.
	if err := c.ensureSSMManagedPolicy(ctx, iamClient, roleName); err != nil {
		return "", err
	}

	// 3. Check if instance profile exists, create if not
	_, err = iamClient.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(instanceProfileName),
	})

	if err != nil {
		profileCreated = true
		// Instance profile doesn't exist, create it
		// retryIAM for the same reason as CreateRole above (spawn#648) — Tags make
		// this serialise, and EntityAlreadyExists is already success to retryIAM.
		err = retryIAM(func() error {
			_, e := iamClient.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
				InstanceProfileName: aws.String(instanceProfileName),
				Tags: []types.Tag{
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
				},
			})
			return e
		})
		if err != nil && !contains(err.Error(), "EntityAlreadyExists") {
			return "", fmt.Errorf("failed to create instance profile: %w", err)
		}

		// Add role to instance profile
		// LimitExceeded here means another launch already attached the role (a
		// profile holds exactly one), which retryIAM treats as success via
		// isAlreadyExists — same as the string match below (spawn#648).
		err = retryIAM(func() error {
			_, e := iamClient.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
				InstanceProfileName: aws.String(instanceProfileName),
				RoleName:            aws.String(roleName),
			})
			return e
		})
		if err != nil && !contains(err.Error(), "LimitExceeded") {
			return "", fmt.Errorf("failed to add role to instance profile: %w", err)
		}
	}

	// If we created new resources, wait for IAM to propagate (eventual
	// consistency). Poll GetInstanceProfile until it's retrievable rather than
	// sleeping a fixed 10s — returns immediately once consistent (instant
	// against emulators), bounded so it can't hang.
	if roleCreated || profileCreated {
		waitForInstanceProfile(ctx, iamClient, instanceProfileName)
	}

	return instanceProfileName, nil
}

// sporedDCVRolePolicy is the inline policy for the DCV/app-launch spored role
// (spawn app launch).
//
// #622 note on keeping this in sync: this policy already carried the #221
// fsx:CreateDataRepositoryAssociation grant but NOT the
// iam:CreateServiceLinkedRole that FSx needs to create the per-filesystem
// service-linked role on the first association in an account. So this path had the
// identical silent failure — --fsx-import-path mounting an EMPTY 1200 GiB
// filesystem — and it is the path taken by a launch with NO --iam-* flags, i.e. the
// simplest invocation. The grant below is conditioned to the single FSx-S3 service
// principal. A test asserts both this policy and the dynamic baseline carry it, so
// the two cannot diverge on it again.
//
// Kept in sync with buildInlinePolicy (the standard spored
// role in iam.go): the same self-management grants PLUS the DCV-only S3 reads
// (dcv-license, spawn-certs). spawn#282 reconciled the two — this role previously
// granted ec2:CreateTags on "*" UNCONDITIONED (the #174 tag-then-terminate class)
// and lacked the FSx-mount (#221) and lambda:InvokeFunctionUrl (#173 DNS-sign)
// grants, so a DCV instance couldn't mount ephemeral FSx and, after the #173
// AuthType:AWS_IAM cutover, couldn't register DNS. Both are now included;
// CreateTags is scoped to spawn:managed=true (DCV instances always carry that tag
// at launch, via buildTags).

// reaperControlTagKeys are the instance tags the TTL reaper does not merely read
// but ACTS on, and in the pre-stop case EXECUTES. The instance must never be able
// to write them (GHSA-8q3p-qcv8-2pr4).
//
// The attack this closes: ec2:CreateTags is granted on Resource "*" conditioned on
// ec2:ResourceTag/spawn:managed=true, so a spore can retag any OTHER spawn-managed
// instance in the account — the policy comment above reasons correctly that it
// "cannot pull a foreign instance into scope", which turns out to be the wrong
// worry. The reaper feeds spawn:pre-stop to SSM AWS-RunShellScript, which runs as
// root, so writing a victim's spawn:pre-stop (or clearing its
// spawn:local-username, so no privilege drop happens) is root command execution on
// the victim. Any unprivileged local user on any spore can reach the role's
// credentials through IMDS, so this is a one-spore-to-whole-account pivot.
//
// Why only these. The obvious longer list is wrong: spored legitimately
// writes spawn:last-heartbeat (pkg/agent/agent.go) and both writes and deletes
// spawn:ttl-deadline (the `spored ttl set` path, via ttl.SetTags), so denying
// those keys would break idle detection and `spored ttl` rather than harden
// anything. These are read-only to the instance by construction: they are
// set once at launch by the CLI and nothing in pkg/agent or cmd/spored ever writes
// them. Tampering with the deadline remains possible and remains contained by the
// unconditional max-age ceiling instead (#76) — that is the right tool for it,
// because the deadline has a legitimate in-instance writer and the hook does not.
//
// Deny, not a narrower Allow. An explicit Deny cannot be overridden by a later
// Allow, and these two policies have already diverged once in exactly that way:
// the DCV copy granted ec2:CreateTags on "*" UNCONDITIONED while the dynamic
// baseline was being scoped for #174.
//
// Two more keys, because "contained by the max-age ceiling" was only true while
// the ceiling's own inputs were out of the instance's reach, and they weren't
// (GHSA-c93q-4jh7-6pvj). The reaper measures age from spawn:launch-time, and it
// scans only spawn:managed=true. An instance that rewrote the first stayed
// forever young; one that deleted the second vanished from every reaper check.
// Both are set once by the CLI at launch and nothing on the instance writes them
// (TestNoDeniedTagKeyHasAnInInstanceWriter). Denying DeleteTags on
// spawn:managed is safe for the same reason: the Allow above is conditioned on
// that tag, so once it is gone the instance can no longer tag itself at all.
//
// And the two that say which instance a launch Elastic IP belongs to (#661).
// The instance role may release an address whose spawn:instance-arn is its own
// ARN (sporedOwnElasticIPStatements), and these addresses carry
// spawn:managed=true, so the CreateTags Allow reaches them: without the Deny, a
// spore could write its own ARN onto another instance's address and release it.
// spawn:instance-id is what the reaper's sweep and `spawn terminate` select by.
// Launch writes both, from the workstation, and nothing on the instance does.
var reaperControlTagKeys = []string{
	"spawn:pre-stop",
	"spawn:pre-stop-timeout",
	"spawn:local-username",
	"spawn:launch-time",
	"spawn:managed",
	elasticip.InstanceIDTagKey,
	elasticip.InstanceARNTagKey,
}

// reaperControlTagDeny is the statement form for buildInlinePolicy.
//
// ForAnyValue:StringEquals is the correct operator: aws:TagKeys is multi-valued,
// and the deny must fire when the request carries *any* control key, even
// alongside legitimate ones. ForAllValues here would only deny a request made up
// entirely of control keys, so appending one legitimate tag would defeat it.
func reaperControlTagDeny() map[string]interface{} {
	keys := make([]interface{}, 0, len(reaperControlTagKeys))
	for _, k := range reaperControlTagKeys {
		keys = append(keys, k)
	}
	return map[string]interface{}{
		"Effect": "Deny",
		"Action": []interface{}{
			"ec2:CreateTags",
			"ec2:DeleteTags",
		},
		"Resource": "*",
		"Condition": map[string]interface{}{
			"ForAnyValue:StringEquals": map[string]interface{}{
				"aws:TagKeys": keys,
			},
		},
	}
}

// reaperControlTagKeysJSON renders the same list for the static DCV policy below,
// so the two policies cannot drift on it — the failure mode this whole comment
// block is about.
func reaperControlTagKeysJSON() string {
	quoted := make([]string, 0, len(reaperControlTagKeys))
	for _, k := range reaperControlTagKeys {
		quoted = append(quoted, `"`+k+`"`)
	}
	return strings.Join(quoted, ", ")
}

// sporedTrustPolicy is the assume-role trust policy for the spored EC2 role:
// it lets the EC2 service assume the role. Used by SetupSporedIAMRole when
// creating the role.
const sporedTrustPolicy = `{
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
}`

// A var rather than a const so the reaper control-tag deny below is rendered from
// reaperControlTagKeys, the same slice buildInlinePolicy uses. Hardcoding the list
// twice is the drift this file has already shipped once.
var sporedDCVRolePolicy = fmt.Sprintf(`{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeTags",
        "ec2:DescribeInstances",
        "ec2:DescribeVolumes"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": ["ec2:CreateTags", "ec2:DeleteTags"],
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    },
    {
      "Effect": "Deny",
      "Action": ["ec2:CreateTags", "ec2:DeleteTags"],
      "Resource": "*",
      "Condition": {
        "ForAnyValue:StringEquals": {
          "aws:TagKeys": [%s]
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:TerminateInstances",
        "ec2:StopInstances",
        "ec2:CancelSpotInstanceRequests"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": [
        "fsx:DescribeFileSystems",
        "fsx:CreateDataRepositoryAssociation",
        "fsx:DescribeDataRepositoryAssociations"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "iam:CreateServiceLinkedRole",
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "iam:AWSServiceName": "s3.data-source.lustre.fsx.amazonaws.com"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": ["lambda:InvokeFunctionUrl"],
      "Resource": %q
    },
    {
      "Effect": "Allow",
      "Action": [
        "ecr:GetAuthorizationToken",
        "ecr:BatchCheckLayerAvailability",
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchGetImage"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::spawn-certs-*/*"
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::dcv-license.*/*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:ListBucket",
        "s3:GetBucketLocation"
      ],
      "Resource": [
        "arn:aws:s3:::spawn-schedules-*",
        "arn:aws:s3:::spawn-schedules-*/*"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:ListBucket",
        "s3:GetBucketLocation"
      ],
      "Resource": [
        "arn:aws:s3:::spawn-binaries-*",
        "arn:aws:s3:::spawn-binaries-*/*"
      ]
    },
    %s,
    %s,
    %s
  ]
}`, reaperControlTagKeysJSON(), spawnconfig.DefaultDNSFunctionARN, sporedHybridRegistryStatementJSON(), sporedSelfTerminateStatementJSON(), sporedOwnElasticIPStatementsJSON())

// waitForInstanceProfile polls until the instance profile is retrievable (IAM
// is eventually consistent after creation), or a bounded deadline passes. It
// returns as soon as the profile is readable — instantly against a strongly
// consistent endpoint — instead of a blind fixed sleep. Best-effort: a timeout
// is not fatal (the subsequent RunInstances will surface any real problem).
func waitForInstanceProfile(ctx context.Context, iamClient *iam.Client, name string) {
	const (
		deadline = 30 * time.Second
		interval = 500 * time.Millisecond
	)
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := iamClient.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(name),
		}); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
