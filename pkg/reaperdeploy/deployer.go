package reaperdeploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/spore-host/spawn/pkg/reaperiam"
)

// APIs is the slice of AWS each step needs, as interfaces, so the Deployer is
// testable without AWS and so each call site is visible in one place.
type IAMAPI interface {
	CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

type LambdaAPI interface {
	CreateFunction(context.Context, *lambda.CreateFunctionInput, ...func(*lambda.Options)) (*lambda.CreateFunctionOutput, error)
	GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error)
	UpdateFunctionCode(context.Context, *lambda.UpdateFunctionCodeInput, ...func(*lambda.Options)) (*lambda.UpdateFunctionCodeOutput, error)
	UpdateFunctionConfiguration(context.Context, *lambda.UpdateFunctionConfigurationInput, ...func(*lambda.Options)) (*lambda.UpdateFunctionConfigurationOutput, error)
	AddPermission(context.Context, *lambda.AddPermissionInput, ...func(*lambda.Options)) (*lambda.AddPermissionOutput, error)
	RemovePermission(context.Context, *lambda.RemovePermissionInput, ...func(*lambda.Options)) (*lambda.RemovePermissionOutput, error)
	DeleteFunction(context.Context, *lambda.DeleteFunctionInput, ...func(*lambda.Options)) (*lambda.DeleteFunctionOutput, error)
}

type EventsAPI interface {
	PutRule(context.Context, *eventbridge.PutRuleInput, ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error)
	PutTargets(context.Context, *eventbridge.PutTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error)
	DescribeRule(context.Context, *eventbridge.DescribeRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error)
	RemoveTargets(context.Context, *eventbridge.RemoveTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error)
	DeleteRule(context.Context, *eventbridge.DeleteRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error)
}

type S3API interface {
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	// Tagging at creation, so a later teardown has POSITIVE evidence the bucket is
	// ours instead of inferring it from the name (#755's lesson: the stamp has to
	// be written at the creation site or nothing can act on it later).
	PutBucketTagging(context.Context, *s3.PutBucketTaggingInput, ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
	GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
	// Teardown needs all three: S3 refuses DeleteBucket on a non-empty bucket, so
	// the contents must be listed and removed first.
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteBucket(context.Context, *s3.DeleteBucketInput, ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

// Deployer converges the reaper's resources in one account.
type Deployer struct {
	// Logs is optional: only the idle check uses it, and a nil Logs reports
	// "undetermined" rather than failing, so existing callers keep working.
	Logs   LogsAPI
	IAM    IAMAPI
	Lambda LambdaAPI
	Events EventsAPI
	S3     S3API

	// Fetch retrieves the artifact. Injectable so tests never reach the network and
	// so --artifact can supply a local file.
	Fetch func(ctx context.Context, ref string) ([]byte, error)

	// PropagationDelay is how long to wait after CREATING the execution role before
	// handing it to Lambda. IAM is eventually consistent and CreateFunction fails
	// with "The role defined for the function cannot be assumed by Lambda" if the
	// role is too fresh. Zero uses DefaultPropagationDelay; tests set it to a
	// negative value to skip the wait entirely, which keeps the unit suite fast
	// (it was 40s before this was injectable).
	PropagationDelay time.Duration
}

// DefaultPropagationDelay is the post-CreateRole wait.
const DefaultPropagationDelay = 10 * time.Second

// Options are the inputs to a deploy.
type Options struct {
	AccountID string
	Region    string
	Version   string
	// Artifact overrides the release download: a local path or an explicit URL.
	// Exists so this is testable before any release carries the asset, and so
	// mirrored or air-gapped environments work.
	Artifact string
	Bucket   string
	Schedule string
	Regions  string
}

// Result records what a deploy did, so the command can print it and a test can
// assert it without parsing output.
type Result struct {
	Actions     []string
	FunctionARN string
	RoleARN     string
	Bucket      string
	Key         string
	DryRun      bool
}

// Deploy converges role → artifact → function → schedule.
//
// Ordering matters and is not arbitrary: the role must exist before the function can
// reference it, the artifact must be in S3 before CreateFunction can read it, and the
// rule must target a function that exists. Each step is idempotent so a re-run after
// a partial failure converges rather than erroring.
func (d *Deployer) Deploy(ctx context.Context, opts Options) (*Result, error) {
	if opts.AccountID == "" || opts.Region == "" {
		return nil, errors.New("reaperdeploy: account ID and region are required")
	}
	if opts.Schedule == "" {
		opts.Schedule = DefaultSchedule
	}
	if opts.Bucket == "" {
		opts.Bucket = DefaultBucketName(opts.AccountID, opts.Region)
	}
	res := &Result{Bucket: opts.Bucket, Key: ObjectKey(opts.Version), DryRun: true}

	roleARN, created, err := d.ensureRole(ctx, opts)
	if err != nil {
		return nil, err
	}
	res.RoleARN = roleARN
	res.Actions = append(res.Actions, actionWord(created, "role", RoleName))

	// IAM is eventually consistent: a role created a moment ago is not yet usable by
	// Lambda, which fails CreateFunction with "The role defined for the function
	// cannot be assumed by Lambda". Only wait when we just created it.
	if created {
		delay := d.PropagationDelay
		if delay == 0 {
			delay = DefaultPropagationDelay
		}
		if delay > 0 {
			res.Actions = append(res.Actions, "waited for IAM propagation")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	if err := d.ensureBucket(ctx, opts.Bucket, opts.Region); err != nil {
		return nil, err
	}
	res.Actions = append(res.Actions, actionWord(true, "artifact bucket", opts.Bucket))

	zip, err := d.fetchArtifact(ctx, opts)
	if err != nil {
		return nil, err
	}
	if _, err := d.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: awssdk.String(opts.Bucket),
		Key:    awssdk.String(res.Key),
		Body:   strings.NewReader(string(zip)),
	}); err != nil {
		return nil, fmt.Errorf("reaperdeploy: upload artifact: %w", err)
	}
	res.Actions = append(res.Actions, fmt.Sprintf("uploaded artifact s3://%s/%s (%d bytes)", opts.Bucket, res.Key, len(zip)))

	fnARN, fnCreated, err := d.ensureFunction(ctx, opts, roleARN, res.Key)
	if err != nil {
		return nil, err
	}
	res.FunctionARN = fnARN
	res.Actions = append(res.Actions, actionWord(fnCreated, "function", FunctionName))

	if err := d.ensureSchedule(ctx, opts, fnARN); err != nil {
		return nil, err
	}
	res.Actions = append(res.Actions, fmt.Sprintf("scheduled %s (%s)", RuleName, opts.Schedule))

	return res, nil
}

func (d *Deployer) ensureRole(ctx context.Context, opts Options) (string, bool, error) {
	arn := fmt.Sprintf("arn:aws:iam::%s:role/%s", opts.AccountID, RoleName)

	created := false
	if _, err := d.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(RoleName)}); err != nil {
		var nse *iamtypes.NoSuchEntityException
		if !errors.As(err, &nse) {
			return "", false, fmt.Errorf("reaperdeploy: get role %s: %w", RoleName, err)
		}
		if _, err := d.IAM.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 awssdk.String(RoleName),
			AssumeRolePolicyDocument: awssdk.String(AssumeRolePolicy),
			Description:              awssdk.String("Execution role for the spawn ttl-reaper scanning this account (spawn#625)"),
			Tags:                     iamTags(opts.Version),
		}); err != nil {
			return "", false, fmt.Errorf("reaperdeploy: create role %s: %w", RoleName, err)
		}
		created = true
	}

	// Always re-apply: this is how a re-deploy picks up a widened permission set,
	// and it is the mechanism that heals a role created before the fsx:/ssm: grants
	// were added (#625).
	policy, err := reaperiam.ScanSelfPolicyDocument()
	if err != nil {
		return "", false, err
	}
	if _, err := d.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       awssdk.String(RoleName),
		PolicyName:     awssdk.String(PolicyName),
		PolicyDocument: awssdk.String(policy),
	}); err != nil {
		return "", false, fmt.Errorf("reaperdeploy: put role policy: %w", err)
	}

	// Logs. Without this the reaper runs and you cannot read what it did — which
	// matters most in dry-run, where the log IS the output.
	if _, err := d.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName:  awssdk.String(RoleName),
		PolicyArn: awssdk.String("arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"),
	}); err != nil {
		return "", false, fmt.Errorf("reaperdeploy: attach basic execution policy: %w", err)
	}

	return arn, created, nil
}

func (d *Deployer) ensureBucket(ctx context.Context, bucket, region string) error {
	if _, err := d.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awssdk.String(bucket)}); err == nil {
		return nil
	}
	in := &s3.CreateBucketInput{Bucket: awssdk.String(bucket)}
	// us-east-1 must NOT carry a LocationConstraint; every other region must.
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	if _, err := d.S3.CreateBucket(ctx, in); err != nil {
		// A bucket we already own is success, not failure — the HeadBucket above can
		// fail for permission reasons even when the bucket exists.
		var owned *s3types.BucketAlreadyOwnedByYou
		var exists *s3types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("reaperdeploy: create bucket %s: %w", bucket, err)
	}

	// Stamp it at the creation site, so a later teardown can prove the bucket is
	// ours rather than inferring it from the name. This is #755's lesson applied
	// here: spawn:created was read in three places and written in none, and the
	// two safety properties composed into a leak with no collector.
	//
	// Non-fatal: a deploy that cannot tag has still produced a working reaper, and
	// failing here would turn a cosmetic gap into an outage. The teardown handles
	// an untagged bucket explicitly rather than assuming the tag is present.
	if _, err := d.S3.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: awssdk.String(bucket),
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{
			{Key: awssdk.String("spawn:managed"), Value: awssdk.String("true")},
			{Key: awssdk.String("spawn:component"), Value: awssdk.String("ttl-reaper")},
			{Key: awssdk.String("spawn:created-by"), Value: awssdk.String("spawn reaper deploy")},
		}},
	}); err != nil {
		log.Printf("reaperdeploy: could not tag bucket %s (%v) — teardown will need --force-artifacts to remove it", bucket, err)
	}
	return nil
}

// artifactKeyPrefix is the only prefix the reaper writes under. Teardown refuses
// to delete a bucket holding anything else, because a bucket someone repurposed
// is not ours to empty.
const artifactKeyPrefix = "ttl-reaper/"

// removeArtifactBucket empties and deletes the artifact bucket.
//
// This closes the gap #653 recorded: teardown removed the rule, permission,
// function and role, and DELIBERATELY left the bucket, reporting what it kept.
// That was defensible as an explicit choice and is not defensible under "leave no
// trace" — and it blocks idle self-removal (#772), because a reaper that removes
// itself while leaving a bucket has converted a visible trace into a claimed-clean
// one, which is worse than leaving it.
//
// Two guards, because deleting a bucket destroys its contents irreversibly:
//
//   - the bucket must be TAGGED spawn:managed, or force must be set. The name
//     embeds the account id so it cannot belong to another account, but a name is
//     not evidence that WE made it — someone could have created it by hand.
//   - every object must be under ttl-reaper/. A bucket holding anything else has
//     been repurposed, and emptying it would destroy data this code never wrote.
//
// Returns the actions taken, so the caller reports work that actually happened
// rather than work it intended — the DeleteRule lesson a few lines up.
func (d *Deployer) removeArtifactBucket(ctx context.Context, bucket string, force bool) ([]string, error) {
	if _, err := d.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awssdk.String(bucket)}); err != nil {
		return nil, nil // already gone: a teardown re-run converges
	}

	if !force {
		out, err := d.S3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: awssdk.String(bucket)})
		managed := false
		if err == nil {
			for _, t := range out.TagSet {
				if awssdk.ToString(t.Key) == "spawn:managed" && awssdk.ToString(t.Value) == "true" {
					managed = true
				}
			}
		}
		if !managed {
			// An untagged bucket predates the tagging above. Say what to do rather
			// than silently keeping it, which is the behaviour being fixed.
			return nil, fmt.Errorf("bucket %s is not tagged spawn:managed=true, so it cannot be "+
				"confirmed as spawn's; re-run with --force-artifacts to remove it anyway", bucket)
		}
	}

	var keys []string
	var token *string
	for {
		page, err := d.S3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            awssdk.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", bucket, err)
		}
		for _, o := range page.Contents {
			k := awssdk.ToString(o.Key)
			if !strings.HasPrefix(k, artifactKeyPrefix) {
				return nil, fmt.Errorf("bucket %s holds %q, which spawn never wrote — refusing to "+
					"empty a bucket that has been repurposed", bucket, k)
			}
			keys = append(keys, k)
		}
		if page.NextContinuationToken == nil {
			break
		}
		token = page.NextContinuationToken
	}

	for _, k := range keys {
		if _, err := d.S3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: awssdk.String(bucket), Key: awssdk.String(k),
		}); err != nil {
			return nil, fmt.Errorf("delete %s/%s: %w", bucket, k, err)
		}
	}
	if _, err := d.S3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: awssdk.String(bucket)}); err != nil {
		return nil, fmt.Errorf("delete bucket %s: %w", bucket, err)
	}
	return []string{fmt.Sprintf("deleted artifact bucket %s (%d object(s))", bucket, len(keys))}, nil
}

func (d *Deployer) fetchArtifact(ctx context.Context, opts Options) ([]byte, error) {
	ref := opts.Artifact
	if ref == "" {
		if opts.Version == "" {
			return nil, errors.New("reaperdeploy: need --version or --artifact to locate the reaper Lambda zip")
		}
		ref = ArtifactURL(opts.Version)
	}
	fetch := d.Fetch
	if fetch == nil {
		fetch = DefaultFetch
	}
	b, err := fetch(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("reaperdeploy: fetch artifact %s: %w", ref, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("reaperdeploy: artifact %s is empty", ref)
	}
	// A zip starts "PK". Catching an HTML error page here beats deploying it and
	// getting Runtime.InvalidEntrypoint at invoke time.
	if len(b) < 2 || b[0] != 'P' || b[1] != 'K' {
		return nil, fmt.Errorf("reaperdeploy: artifact %s is not a zip (got %q…) — "+
			"a 404 page or an LFS pointer will look like this", ref, firstBytes(b, 16))
	}
	return b, nil
}

// DefaultFetch reads a local path or downloads an http(s) URL.
func DefaultFetch(ctx context.Context, ref string) ([]byte, error) {
	if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		return os.ReadFile(ref)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func (d *Deployer) ensureFunction(ctx context.Context, opts Options, roleARN, key string) (string, bool, error) {
	env := EnvVars(opts.Regions, true) // always deploy unarmed; `spawn reaper arm` flips it

	out, err := d.Lambda.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: awssdk.String(FunctionName)})
	if err == nil {
		// Update code first, then configuration. The reverse order can leave a
		// function configured for an artifact it is not yet running.
		if _, err := d.Lambda.UpdateFunctionCode(ctx, &lambda.UpdateFunctionCodeInput{
			FunctionName: awssdk.String(FunctionName),
			S3Bucket:     awssdk.String(opts.Bucket),
			S3Key:        awssdk.String(key),
		}); err != nil {
			return "", false, fmt.Errorf("reaperdeploy: update function code: %w", err)
		}
		// Preserve REAPER_DRY_RUN on an existing function: re-deploying must not
		// silently DISARM a reaper an operator deliberately armed.
		if cur := out.Configuration; cur != nil && cur.Environment != nil {
			if v, ok := cur.Environment.Variables["REAPER_DRY_RUN"]; ok {
				env["REAPER_DRY_RUN"] = v
			}
		}
		if _, err := d.Lambda.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
			FunctionName: awssdk.String(FunctionName),
			Role:         awssdk.String(roleARN),
			Environment:  &lambdatypes.Environment{Variables: env},
			Timeout:      awssdk.Int32(900),
			MemorySize:   awssdk.Int32(256),
		}); err != nil {
			return "", false, fmt.Errorf("reaperdeploy: update function configuration: %w", err)
		}
		return awssdk.ToString(out.Configuration.FunctionArn), false, nil
	}

	var nf *lambdatypes.ResourceNotFoundException
	if !errors.As(err, &nf) {
		return "", false, fmt.Errorf("reaperdeploy: get function: %w", err)
	}

	created, err := d.Lambda.CreateFunction(ctx, &lambda.CreateFunctionInput{
		FunctionName:  awssdk.String(FunctionName),
		Role:          awssdk.String(roleARN),
		Runtime:       lambdatypes.RuntimeProvidedal2023,
		Handler:       awssdk.String("bootstrap"),
		Architectures: []lambdatypes.Architecture{lambdatypes.ArchitectureArm64},
		Code:          &lambdatypes.FunctionCode{S3Bucket: awssdk.String(opts.Bucket), S3Key: awssdk.String(key)},
		Environment:   &lambdatypes.Environment{Variables: env},
		Timeout:       awssdk.Int32(900),
		MemorySize:    awssdk.Int32(256),
		Description:   awssdk.String("spawn ttl-reaper, scanning this account (spawn#625)"),
		Tags:          Tags(opts.Version),
	})
	if err != nil {
		return "", false, fmt.Errorf("reaperdeploy: create function: %w", err)
	}
	return awssdk.ToString(created.FunctionArn), true, nil
}

func (d *Deployer) ensureSchedule(ctx context.Context, opts Options, fnARN string) error {
	if _, err := d.Events.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:               awssdk.String(RuleName),
		ScheduleExpression: awssdk.String(opts.Schedule),
		State:              ebtypes.RuleStateEnabled,
		Description:        awssdk.String("Invokes the spawn ttl-reaper (spawn#625)"),
	}); err != nil {
		return fmt.Errorf("reaperdeploy: put rule: %w", err)
	}

	// The permission must exist before the target, or EventBridge accepts the target
	// and then silently fails every invocation with AccessDeniedException — visible
	// only in the rule's failed-invocation metric, which nobody is watching.
	if _, err := d.Lambda.AddPermission(ctx, &lambda.AddPermissionInput{
		FunctionName: awssdk.String(FunctionName),
		StatementId:  awssdk.String(RuleName),
		Action:       awssdk.String("lambda:InvokeFunction"),
		Principal:    awssdk.String("events.amazonaws.com"),
		SourceArn:    awssdk.String(fmt.Sprintf("arn:aws:events:%s:%s:rule/%s", opts.Region, opts.AccountID, RuleName)),
	}); err != nil {
		// Already present is success: this step is idempotent by re-running.
		var exists *lambdatypes.ResourceConflictException
		if !errors.As(err, &exists) {
			return fmt.Errorf("reaperdeploy: add invoke permission: %w", err)
		}
	}

	if _, err := d.Events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:    awssdk.String(RuleName),
		Targets: []ebtypes.Target{{Id: awssdk.String("reaper"), Arn: awssdk.String(fnARN)}},
	}); err != nil {
		return fmt.Errorf("reaperdeploy: put targets: %w", err)
	}
	return nil
}

// Arm flips REAPER_DRY_RUN to false on the deployed function.
func (d *Deployer) Arm(ctx context.Context) error {
	out, err := d.Lambda.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: awssdk.String(FunctionName)})
	if err != nil {
		return fmt.Errorf("reaperdeploy: the reaper is not deployed in this account/region (%w) — run 'spawn reaper deploy' first", err)
	}
	env := map[string]string{}
	if out.Configuration != nil && out.Configuration.Environment != nil {
		for k, v := range out.Configuration.Environment.Variables {
			env[k] = v
		}
	}
	env["REAPER_DRY_RUN"] = "false"
	env["REAPER_SCAN_SELF"] = "true"
	if _, err := d.Lambda.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
		FunctionName: awssdk.String(FunctionName),
		Environment:  &lambdatypes.Environment{Variables: env},
	}); err != nil {
		return fmt.Errorf("reaperdeploy: arm: %w", err)
	}
	return nil
}

// Info is what Inspect found.
type Info struct {
	Deployed    bool
	Armed       bool
	Version     string
	Regions     string
	Schedule    string
	RuleEnabled bool
	FunctionARN string
}

// Inspect reports the deployed state without changing anything.
func (d *Deployer) Inspect(ctx context.Context) (Info, error) {
	var info Info
	out, err := d.Lambda.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: awssdk.String(FunctionName)})
	if err != nil {
		var nf *lambdatypes.ResourceNotFoundException
		if errors.As(err, &nf) {
			return info, nil // not deployed is a finding, not an error
		}
		return info, fmt.Errorf("reaperdeploy: get function: %w", err)
	}
	info.Deployed = true
	if c := out.Configuration; c != nil {
		info.FunctionARN = awssdk.ToString(c.FunctionArn)
		if c.Environment != nil {
			info.Armed = !strings.EqualFold(c.Environment.Variables["REAPER_DRY_RUN"], "true")
			info.Regions = c.Environment.Variables["REAPER_REGIONS"]
		}
	}
	if out.Tags != nil {
		info.Version = out.Tags["spawn:version"]
	}

	if rule, err := d.Events.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: awssdk.String(RuleName)}); err == nil {
		info.Schedule = awssdk.ToString(rule.ScheduleExpression)
		info.RuleEnabled = rule.State == ebtypes.RuleStateEnabled
	}
	return info, nil
}

// TeardownOptions controls how far a teardown goes.
type TeardownOptions struct {
	// Bucket is the artifact bucket to remove. Empty leaves it alone, which is
	// the pre-#653 behaviour and is kept only for callers that genuinely want it.
	Bucket string
	// KeepArtifacts leaves the bucket in place. The opt-OUT, deliberately: the
	// default is now to remove it, because silent retention is the thing being
	// fixed (#653).
	KeepArtifacts bool
	// ForceArtifacts removes the bucket even when it is not tagged
	// spawn:managed=true. Needed for buckets created before the tag existed.
	ForceArtifacts bool
}

// Teardown removes what Deploy created, most-dependent first, and now the
// artifact bucket too.
//
// It USED to leave the bucket deliberately, on the reasoning that emptying an S3
// bucket a user may have put other things in is a liberty — and it said so. That
// was a defensible explicit choice. It is not defensible under "leave no trace"
// (#653), and it blocks idle self-removal (#772): a reaper that removes itself
// while leaving a bucket has turned a visible trace into a claimed-clean one,
// which is worse than leaving it.
//
// The original concern is answered rather than overruled — removeArtifactBucket
// refuses a bucket that is not tagged as ours, and refuses one holding any object
// spawn did not write. So the liberty is only taken over a bucket that is
// provably spawn's and contains only spawn's artifacts.
func (d *Deployer) Teardown(ctx context.Context, opts TeardownOptions) ([]string, error) {
	var removed []string
	var firstErr error
	note := func(what string, err error) {
		if err == nil {
			removed = append(removed, what)
			return
		}
		if isNotFound(err) {
			return // already gone: a teardown re-run converges
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	_, err := d.Events.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{
		Rule: awssdk.String(RuleName), Ids: []string{"reaper"},
	})
	note("rule target", err)

	// EventBridge's DeleteRule is idempotent: deleting a rule that does not exist
	// returns SUCCESS, not ResourceNotFoundException. So err==nil says nothing about
	// whether anything was there, and reporting "removed rule X" on a second
	// teardown was a claim about work that never happened. Check first, then act.
	if _, derr := d.Events.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: awssdk.String(RuleName)}); derr == nil {
		_, err = d.Events.DeleteRule(ctx, &eventbridge.DeleteRuleInput{Name: awssdk.String(RuleName)})
		note("rule "+RuleName, err)
	}

	_, err = d.Lambda.RemovePermission(ctx, &lambda.RemovePermissionInput{
		FunctionName: awssdk.String(FunctionName), StatementId: awssdk.String(RuleName),
	})
	note("invoke permission", err)

	_, err = d.Lambda.DeleteFunction(ctx, &lambda.DeleteFunctionInput{FunctionName: awssdk.String(FunctionName)})
	note("function "+FunctionName, err)

	_, err = d.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
		RoleName: awssdk.String(RoleName), PolicyName: awssdk.String(PolicyName),
	})
	note("role policy", err)

	_, err = d.IAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
		RoleName:  awssdk.String(RoleName),
		PolicyArn: awssdk.String("arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"),
	})
	note("basic execution policy", err)

	_, err = d.IAM.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: awssdk.String(RoleName)})
	note("role "+RoleName, err)

	// The bucket LAST: it is the only step whose failure should not stop the
	// rest, and the rest is what actually disarms the reaper. A teardown that
	// refused to remove the function because a bucket tag was missing would be
	// the wrong trade.
	switch {
	case opts.Bucket == "":
		// Nothing asked for.
	case opts.KeepArtifacts:
		removed = append(removed, fmt.Sprintf("kept artifact bucket %s (--keep-artifacts)", opts.Bucket))
	default:
		acts, berr := d.removeArtifactBucket(ctx, opts.Bucket, opts.ForceArtifacts)
		removed = append(removed, acts...)
		if berr != nil && firstErr == nil {
			firstErr = berr
		}
	}

	return removed, firstErr
}

func isNotFound(err error) bool {
	var iamNF *iamtypes.NoSuchEntityException
	var lamNF *lambdatypes.ResourceNotFoundException
	var ebNF *ebtypes.ResourceNotFoundException
	return errors.As(err, &iamNF) || errors.As(err, &lamNF) || errors.As(err, &ebNF)
}

func iamTags(version string) []iamtypes.Tag {
	var out []iamtypes.Tag
	for k, v := range Tags(version) {
		out = append(out, iamtypes.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return out
}

func actionWord(created bool, kind, name string) string {
	if created {
		return "created " + kind + " " + name
	}
	return "converged " + kind + " " + name
}

func firstBytes(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return string(b[:n])
}

// New builds a Deployer with real AWS clients.
//
// It exists so cmd/ never constructs an SDK client itself: cmd is meant to be a thin
// CLI layer that delegates AWS work to pkg/ (#326/#327), and TestNoNewAWSSDKImportsInCmd
// enforces it. Writing this constructor was the right answer to that gate; adding four
// services to its allowlist would have been the wrong one.
func New(cfg awssdk.Config) *Deployer {
	return &Deployer{
		IAM:    iam.NewFromConfig(cfg),
		Lambda: lambda.NewFromConfig(cfg),
		Events: eventbridge.NewFromConfig(cfg),
		S3:     s3.NewFromConfig(cfg),
		Logs:   cwlogs.NewFromConfig(cfg),
	}
}
