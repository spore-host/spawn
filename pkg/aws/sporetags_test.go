package aws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	fsxtypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/aws/aws-sdk-go-v2/service/imagebuilder"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	libtags "github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"

	"github.com/spore-host/spawn/internal/sdkanswer"
	"github.com/spore-host/spawn/pkg/sporetags"
)

// The spore.host tags on what spawn creates (#1652). Each create path runs
// through real SDK clients with the libs/tags/tagtest Detector installed, so
// a create call that goes out without spore-host:managed-by=spawn fails the
// test whichever code path made it.

// detect returns an install func adding a Detector for tool to a config.
func detect(t *testing.T, tool libtags.Tool, allow ...tagtest.Allow) (*tagtest.Detector, func(*aws.Config)) {
	d := tagtest.New(t, tool, allow...)
	return d, d.Install
}

// mustHaveCreate fails t unless the Detector saw op, so a test cannot pass by
// never reaching the create it is about.
func mustHaveCreate(t *testing.T, d *tagtest.Detector, service, op string) {
	t.Helper()
	for _, c := range d.Calls() {
		if c.Service == service && c.Operation == op {
			return
		}
	}
	t.Errorf("the Detector saw no %s %s; calls: %+v", service, op, d.Calls())
}

// Launch: the instance, its volumes and its persistent Spot request.
func TestSporeTags_LaunchTagsInstanceVolumeAndSpotRequest(t *testing.T) {
	f := &fakeEC2{}
	c := newFakeEC2Client(t, f)
	d := tagtest.New(t, libtags.ToolSpawn)
	d.Install(&c.cfg)
	_, err := c.Launch(context.Background(), LaunchConfig{InstanceType: "m8a.4xlarge", Region: "us-west-2",
		AMI: "ami-0123456789abcdef0", PricePerHour: 1, Spot: true, TTL: "1h", IdleTimeout: "15m"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	mustHaveCreate(t, d, "EC2", "RunInstances")
	run := f.request("RunInstances")
	for _, rt := range []string{"instance", "volume", "spot-instances-request"} {
		if !hasTag(run, rt, libtags.ManagedBy, "spawn") || !hasTag(run, rt, libtags.Schema, libtags.SchemaVersion) {
			t.Errorf("the %s is not tagged %s=spawn and %s", rt, libtags.ManagedBy, libtags.Schema)
		}
	}
}

// Precedence: a caller (calque, lagotto) that supplies its own
// spore-host:managed-by keeps it on the instance and its volumes. The
// Detector is calque's, so a single spec carrying managed-by=spawn fails.
func TestSporeTags_LaunchKeepsTheCallersManagedBy(t *testing.T) {
	f := &fakeEC2{}
	c := newFakeEC2Client(t, f)
	d := tagtest.New(t, libtags.ToolCalque)
	d.Install(&c.cfg)
	_, err := c.Launch(context.Background(), LaunchConfig{InstanceType: "m8a.4xlarge", Region: "us-west-2",
		AMI: "ami-0123456789abcdef0", PricePerHour: 1, TTL: "1h",
		Tags: map[string]string{libtags.ManagedBy: "calque", libtags.Scope: "run:r-1", libtags.Created: "2026-11-01T00:00:00Z"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	mustHaveCreate(t, d, "EC2", "RunInstances")
	run := f.request("RunInstances")
	for _, rt := range []string{"instance", "volume"} {
		if !hasTag(run, rt, libtags.ManagedBy, "calque") || hasTag(run, rt, libtags.ManagedBy, "spawn") {
			t.Errorf("the %s lost the caller's %s=calque", rt, libtags.ManagedBy)
		}
		if !hasTag(run, rt, libtags.Scope, "run:r-1") || !hasTag(run, rt, libtags.Created, "2026-11-01T00:00:00Z") {
			t.Errorf("the %s lost the caller's scope or created", rt)
		}
		// spawn still fills what the caller left out, and keeps its own
		// lifecycle tags whoever manages the instance.
		if !hasTag(run, rt, libtags.Schema, libtags.SchemaVersion) || !hasTag(run, rt, "spawn:managed", "true") {
			t.Errorf("the %s is missing spawn's %s or spawn:managed", rt, libtags.Schema)
		}
	}
}

// buildTags writes spawn's spore-host keys after the caller's, never in
// place of them, and never repeats one (EC2 rejects a repeated key).
func TestSporeTags_BuildTagsCallerWinsAndNoDuplicates(t *testing.T) {
	tags := mustBuildTags(t, LaunchConfig{Name: "n", TTL: "1h", Region: "us-east-1", PricePerHour: 0.1,
		Tags: map[string]string{libtags.ManagedBy: "lagotto"}}, "123456789012", "arn:aws:iam::1:user/u", "")
	seen := map[string]int{}
	got := map[string]string{}
	for _, tg := range tags {
		seen[*tg.Key]++
		got[*tg.Key] = *tg.Value
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times", k, n)
		}
	}
	if got[libtags.ManagedBy] != "lagotto" {
		t.Errorf("%s = %q, want the caller's lagotto", libtags.ManagedBy, got[libtags.ManagedBy])
	}
	if _, ok := got[libtags.Created]; !ok {
		t.Errorf("no %s", libtags.Created)
	}
}

// A detached sweep member's tags are rendered early and re-anchored at the
// real launch: spore-host:created moves with spawn:launch-time.
func TestSporeTags_AnchorMovesCreated(t *testing.T) {
	tags := mustBuildTags(t, LaunchConfig{Name: "n", TTL: "1h", Region: "us-east-1", PricePerHour: 0.1},
		"123456789012", "arn:aws:iam::1:user/u", "")
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	out, err := AnchorLaunchTags(tags, at)
	if err != nil {
		t.Fatal(err)
	}
	if v := findTagValue(out, libtags.Created); v != "2030-01-02T03:04:05Z" {
		t.Errorf("%s = %q after anchoring, want the launch instant", libtags.Created, v)
	}
}

// Every network resource and key pair.
func TestSporeTags_NetResourcesAndKeyPair(t *testing.T) {
	cfg := sdkanswer.Config("us-west-2", func(service, op string, in any) (any, error) {
		switch in.(type) {
		case *ec2.CreateSecurityGroupInput:
			return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-new")}, nil
		case *ec2.DescribeSecurityGroupsInput:
			return &ec2.DescribeSecurityGroupsOutput{}, nil
		case *ec2.DescribePlacementGroupsInput:
			return &ec2.DescribePlacementGroupsOutput{PlacementGroups: []ec2types.PlacementGroup{{State: ec2types.PlacementGroupStateAvailable}}}, nil
		case *sts.GetCallerIdentityInput:
			return &sts.GetCallerIdentityOutput{Account: aws.String("123456789012"), Arn: aws.String("arn:aws:iam::123456789012:user/u")}, nil
		}
		return nil, nil
	})
	d := tagtest.New(t, libtags.ToolSpawn)
	d.Install(&cfg)
	c := NewClientFromConfig(cfg)
	ctx := context.Background()
	_, _ = c.CreateOrGetMPISecurityGroup(ctx, "us-west-2", "vpc-1", "spawn-mpi-x")
	_, _ = c.CreateOrGetWindowsSecurityGroup(ctx, "us-west-2", "vpc-1", "spawn-win-x", "203.0.113.0/24")
	_, _ = c.CreateOrGetDCVSecurityGroup(ctx, "us-west-2", "vpc-1")
	_, _ = c.CreateOrGetWebSecurityGroup(ctx, "us-west-2", "vpc-1")
	_ = c.CreatePlacementGroup(ctx, "pg-x", "us-west-2")
	_ = c.ImportKeyPair(ctx, "us-west-2", "k", []byte("ssh-ed25519 AAAA test"))
	for _, op := range []string{"CreateSecurityGroup", "CreatePlacementGroup", "ImportKeyPair"} {
		mustHaveCreate(t, d, "EC2", op)
	}
}

// The instance-role path: the spawn-instance-<hash> role and profile and the
// spored managed policy, all tagged at create. The boundary exists already,
// and a launch does not write to it (only `spawn setup-account` converges it).
func TestSporeTags_InstanceRoleSetup(t *testing.T) {
	m := newIAMModel()
	m.seedBoundary()
	d, install := detect(t, libtags.ToolSpawn)
	c := m.client(t, install)
	profile, err := c.CreateOrGetInstanceProfile(context.Background(), IAMRoleConfig{Policies: []string{"s3:ReadOnly"}})
	if err != nil {
		t.Fatalf("CreateOrGetInstanceProfile: %v", err)
	}
	for _, op := range []string{"CreateRole", "CreateInstanceProfile", "CreatePolicy"} {
		mustHaveCreate(t, d, "IAM", op)
	}
	if got := m.roles[profile].tags; got[libtags.ManagedBy] != "spawn" || got[libtags.Scope] != sporetags.ScopeGlobal ||
		got[libtags.Owner] != "AIDAEXAMPLEUSER" || got["spawn:managed"] != "true" {
		t.Errorf("role tags = %v", got)
	}
	if got := m.profiles[profile].tags; got[libtags.ManagedBy] != "spawn" {
		t.Errorf("profile tags = %v", got)
	}
	for arn, p := range m.policies {
		if strings.Contains(arn, "/spored-") && p.tags[libtags.ManagedBy] != "spawn" {
			t.Errorf("spored policy %s tags = %v", arn, p.tags)
		}
	}
	if b := m.policies[spawnInstanceBoundaryARN(m.account)]; len(b.tags) != 0 {
		t.Errorf("a launch tagged the boundary: %v", b.tags)
	}
}

// Precedence on IAM: a caller's spore-host:managed-by on IAMRoleConfig.Tags
// survives on the role and the profile. The spored managed policy is
// spawn's, shared by every role it is attached to, whoever's the role is.
func TestSporeTags_InstanceRoleKeepsTheCallersManagedBy(t *testing.T) {
	m := newIAMModel()
	m.seedBoundary()
	d, install := detect(t, libtags.ToolCalque, tagtest.Allow{Service: "IAM", Operation: "CreatePolicy",
		Reason: "the spored managed policy is spawn's, shared by every role it is attached to, so it is managed-by=spawn on a calque role too"})
	c := m.client(t, install)
	callerTags := map[string]string{libtags.ManagedBy: "calque", libtags.Scope: "pool-workers"}
	profile, err := c.CreateOrGetInstanceProfile(context.Background(), IAMRoleConfig{Policies: []string{"s3:ReadOnly"}, Tags: callerTags})
	if err != nil {
		t.Fatalf("CreateOrGetInstanceProfile: %v", err)
	}
	mustHaveCreate(t, d, "IAM", "CreateRole")
	mustHaveCreate(t, d, "IAM", "CreateInstanceProfile")
	for what, got := range map[string]map[string]string{"role": m.roles[profile].tags, "profile": m.profiles[profile].tags} {
		if got[libtags.ManagedBy] != "calque" || got[libtags.Scope] != "pool-workers" {
			t.Errorf("%s tags = %v, want the caller's managed-by and scope", what, got)
		}
		if got["spawn:managed"] != "true" || got[libtags.Schema] != libtags.SchemaVersion {
			t.Errorf("%s tags = %v, want spawn's spawn:managed and the schema filled in", what, got)
		}
	}
	if len(callerTags) != 2 {
		t.Errorf("the caller's tag map was modified: %v", callerTags)
	}
	for arn, p := range m.policies {
		if strings.Contains(arn, "/spored-") && p.tags[libtags.ManagedBy] != "spawn" {
			t.Errorf("spored policy %s tags = %v", arn, p.tags)
		}
	}
	// A second call, on the existing role and profile, keeps calque's.
	if _, err := c.CreateOrGetInstanceProfile(context.Background(), IAMRoleConfig{Policies: []string{"s3:ReadOnly"}, Tags: callerTags}); err != nil {
		t.Fatal(err)
	}
	if got := m.roles[profile].tags[libtags.ManagedBy]; got != "calque" {
		t.Errorf("after reuse the role is managed-by %q, want calque", got)
	}
}

// The shared spored role and profile, and converging the tags onto them and
// onto the spored managed policy once an older spawn left them untagged,
// keeping a spore-host:created that is there.
func TestSporeTags_SporedRoleAndConverge(t *testing.T) {
	m := newIAMModel()
	m.seedBoundary()
	d, install := detect(t, libtags.ToolSpawn)
	c := m.client(t, install)
	if _, err := c.SetupSporedIAMRole(context.Background()); err != nil {
		t.Fatalf("SetupSporedIAMRole: %v", err)
	}
	mustHaveCreate(t, d, "IAM", "CreateRole")
	mustHaveCreate(t, d, "IAM", "CreateInstanceProfile")
	mustHaveCreate(t, d, "IAM", "CreatePolicy")
	if got := m.roles[SporedRoleName].tags; got[libtags.ManagedBy] != "spawn" || got["spawn:managed"] != "true" {
		t.Errorf("spored role tags = %v", got)
	}

	// As an older spawn left them: no spore-host:* tags, except a created
	// on the role, which must survive.
	m.stripSporeTags()
	m.roles[SporedRoleName].tags[libtags.Created] = "2026-01-01T00:00:00Z"
	if _, err := c.SetupSporedIAMRole(context.Background()); err != nil {
		t.Fatalf("SetupSporedIAMRole again: %v", err)
	}
	for _, op := range []string{"TagRole", "TagInstanceProfile", "TagPolicy"} {
		if m.called(op) == 0 {
			t.Errorf("the converge made no %s", op)
		}
	}
	if got := m.roles[SporedRoleName].tags; got[libtags.ManagedBy] != "spawn" || got[libtags.Created] != "2026-01-01T00:00:00Z" {
		t.Errorf("converged role tags = %v, want managed-by=spawn and the first created kept", got)
	}
	if got := m.profiles[SporedInstanceProfileName].tags; got[libtags.ManagedBy] != "spawn" {
		t.Errorf("converged profile tags = %v", got)
	}
	for arn, p := range m.policies {
		if strings.Contains(arn, "/spored-") && p.tags[libtags.ManagedBy] != "spawn" {
			t.Errorf("converged spored policy %s tags = %v", arn, p.tags)
		}
	}
}

// A principal granted iam:CreatePolicy but not iam:TagPolicy (the documented
// user baseline) still gets the spored policy, untagged, rather than losing
// the managed path.
func TestSporeTags_CreatePolicyFallsBackUntagged(t *testing.T) {
	m := newIAMModel()
	m.seedBoundary()
	m.denyTaggedCreatePolicy = true
	_, install := detect(t, libtags.ToolSpawn, tagtest.Allow{Service: "IAM", Operation: "CreatePolicy",
		Reason: "the untagged retry after IAM refused the tagged create (no iam:TagPolicy); the tagged first attempt is what this test is not about"})
	c := m.client(t, install)
	if _, err := c.SetupSporedIAMRole(context.Background()); err != nil {
		t.Fatalf("SetupSporedIAMRole: %v", err)
	}
	found := false
	for arn := range m.policies {
		if strings.Contains(arn, "/spored-") {
			found = true
		}
	}
	if !found || m.called("CreatePolicy") < 2 {
		t.Errorf("no spored policy after a refused tagged create (CreatePolicy calls: %d)", m.called("CreatePolicy"))
	}
}

// The boundary: created with spore-host:managed-by=spawn and
// spore-host:permanent=true, and an existing untagged one is given them by
// `spawn setup-account` (EnsureSpawnInstanceBoundary).
func TestSporeTags_BoundaryTaggedPermanent(t *testing.T) {
	m := newIAMModel()
	d, install := detect(t, libtags.ToolSpawn)
	c := m.client(t, install)
	api := NewBoundaryAdminIAM(c.cfg)
	out, err := EnsureSpawnInstanceBoundary(context.Background(), api, m.account, func(s string) { t.Log(s) })
	if err != nil || !out.Created {
		t.Fatalf("EnsureSpawnInstanceBoundary = %+v, %v", out, err)
	}
	mustHaveCreate(t, d, "IAM", "CreatePolicy")
	b := m.policies[spawnInstanceBoundaryARN(m.account)]
	if b.tags[libtags.ManagedBy] != "spawn" || b.tags[sporetags.PermanentKey] != "true" || b.tags[libtags.Role] != "" {
		t.Errorf("boundary tags = %v, want managed-by=spawn, %s=true and no spore-host:role", b.tags, sporetags.PermanentKey)
	}

	// Converge an untagged one.
	m2 := newIAMModel()
	m2.seedBoundary()
	c2 := m2.client(t)
	out, err = EnsureSpawnInstanceBoundary(context.Background(), NewBoundaryAdminIAM(c2.cfg), m2.account, func(s string) { t.Log(s) })
	if err != nil || !out.Tagged || out.Created {
		t.Fatalf("EnsureSpawnInstanceBoundary on an untagged boundary = %+v, %v; want Tagged", out, err)
	}
	if b := m2.policies[spawnInstanceBoundaryARN(m2.account)]; b.tags[sporetags.PermanentKey] != "true" || b.tags[libtags.ManagedBy] != "spawn" {
		t.Errorf("converged boundary tags = %v", b.tags)
	}
	// And a third run has nothing to add.
	if out, _ = EnsureSpawnInstanceBoundary(context.Background(), NewBoundaryAdminIAM(c2.cfg), m2.account, func(string) {}); out.Tagged {
		t.Errorf("a re-run re-tagged the boundary")
	}
}

// The ISO-import role, profile, infrastructure configuration and image.
func TestSporeTags_ISOImport(t *testing.T) {
	m := newIAMModel()
	var infra *imagebuilder.CreateInfrastructureConfigurationInput
	answer := func(service, op string, in any) (any, error) {
		switch v := in.(type) {
		case *imagebuilder.ListInfrastructureConfigurationsInput:
			return &imagebuilder.ListInfrastructureConfigurationsOutput{}, nil
		case *imagebuilder.CreateInfrastructureConfigurationInput:
			infra = v
			return &imagebuilder.CreateInfrastructureConfigurationOutput{InfrastructureConfigurationArn: aws.String("arn:infra")}, nil
		case *imagebuilder.ImportDiskImageInput:
			return &imagebuilder.ImportDiskImageOutput{ImageBuildVersionArn: aws.String("arn:image")}, nil
		}
		return m.answer(service, op, in)
	}
	cfg := sdkanswer.Config("us-east-1", answer)
	d := tagtest.New(t, libtags.ToolSpawn)
	d.Install(&cfg)
	c := NewClientFromConfig(cfg)
	if _, err := c.EnsureImportInfrastructure(context.Background(), EnsureImportInfrastructureInput{Region: "us-east-1"}); err != nil {
		t.Fatalf("EnsureImportInfrastructure: %v", err)
	}
	if _, err := c.ImportWindowsISO(context.Background(), ImportWindowsISOInput{Region: "us-east-1", Name: "w", SemanticVersion: "1.0.0",
		URI: "s3://b/k.ISO", InfrastructureConfigurationArn: "arn:infra"}); err != nil {
		t.Fatalf("ImportWindowsISO: %v", err)
	}
	mustHaveCreate(t, d, "IAM", "CreateRole")
	mustHaveCreate(t, d, "IAM", "CreateInstanceProfile")
	mustHaveCreate(t, d, "imagebuilder", "CreateInfrastructureConfiguration")
	mustHaveCreate(t, d, "imagebuilder", "ImportDiskImage")
	if infra == nil || infra.ResourceTags[libtags.ManagedBy] != "spawn" {
		t.Errorf("the build instances Image Builder launches are not tagged managed-by=spawn: %+v", infra)
	}

	// A second run converges an existing role and profile left untagged.
	m.stripSporeTags()
	if _, err := c.EnsureImportInfrastructure(context.Background(), EnsureImportInfrastructureInput{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if m.roles[importInstanceProfileRole].tags[libtags.ManagedBy] != "spawn" || m.profiles[importInstanceProfileRole].tags[libtags.ManagedBy] != "spawn" {
		t.Errorf("an existing ISO-import role or profile was not converged")
	}
}

// createdImageID is the id the fake CreateImage answers with.
const createdImageID = "ami-0123456789abcdef0"

// create-ami: the image and its snapshots, with the caller's managed-by kept.
func TestSporeTags_CreateAMI(t *testing.T) {
	for _, tc := range []struct {
		tool libtags.Tool
		tags map[string]string
	}{{libtags.ToolSpawn, nil}, {libtags.ToolCalque, map[string]string{libtags.ManagedBy: "calque"}}} {
		var got *ec2.CreateImageInput
		cfg := sdkanswer.Config("us-east-1", func(service, op string, in any) (any, error) {
			if v, ok := in.(*ec2.CreateImageInput); ok {
				got = v
				// CreateImage's answer, not an id handed to EC2.
				return &ec2.CreateImageOutput{ImageId: aws.String(createdImageID)}, nil
			}
			return nil, sdkanswer.Err("NotInFake")
		})
		d := tagtest.New(t, tc.tool)
		d.Install(&cfg)
		if _, err := NewClientFromConfig(cfg).CreateAMI(context.Background(), "us-east-1",
			CreateAMIInput{InstanceID: "i-1", Name: "n", Tags: tc.tags}); err != nil {
			t.Fatal(err)
		}
		mustHaveCreate(t, d, "EC2", "CreateImage")
		if len(got.TagSpecifications) != 2 || got.TagSpecifications[1].ResourceType != ec2types.ResourceTypeSnapshot {
			t.Errorf("specs = %+v, want the image and its snapshots", got.TagSpecifications)
		}
	}
}

// The results and FSx buckets (tagged by the PutBucketTagging after
// CreateBucket) and the staging bucket.
func TestSporeTags_Buckets(t *testing.T) {
	var puts []*s3.PutBucketTaggingInput
	cfg := sdkanswer.Config("us-west-2", func(service, op string, in any) (any, error) {
		switch v := in.(type) {
		case *s3.HeadBucketInput:
			return nil, sdkanswer.Err("NotFound")
		case *s3.PutBucketTaggingInput:
			puts = append(puts, v)
		case *s3.GetBucketTaggingInput:
			return nil, sdkanswer.Err("NoSuchTagSet")
		case *s3.GetBucketLifecycleConfigurationInput:
			return nil, sdkanswer.Err("NoSuchLifecycleConfiguration")
		case *sts.GetCallerIdentityInput:
			return &sts.GetCallerIdentityOutput{Account: aws.String("123456789012"), Arn: aws.String("arn:aws:iam::123456789012:user/u")}, nil
		}
		return nil, nil
	})
	d := tagtest.New(t, libtags.ToolSpawn, tagtest.Allow{Service: "S3", Operation: "CreateBucket",
		Reason: "spawn's buckets are tagged by the PutBucketTagging that follows CreateBucket, which the Detector checks"})
	d.Install(&cfg)
	c := NewClientFromConfig(cfg)
	ctx := context.Background()
	if err := c.CreateS3BucketIfNotExists(ctx, "spawn-results-123456789012-us-west-2", "us-west-2"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateS3BucketWithTags(ctx, S3BucketConfig{BucketName: "spawn-fsx-x", Region: "us-west-2", StackName: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureStagingBucket(ctx, "spawn-staging-123456789012-us-west-2", "us-west-2", "123456789012"); err != nil {
		t.Fatal(err)
	}
	mustHaveCreate(t, d, "S3", "PutBucketTagging")
	if len(puts) != 3 {
		t.Fatalf("%d PutBucketTagging calls, want one per bucket", len(puts))
	}
	for _, p := range puts {
		got := map[string]string{}
		for _, tg := range p.Tagging.TagSet {
			got[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
		if got[libtags.ManagedBy] != "spawn" || got[libtags.Scope] != "us-west-2" || got["spawn:managed"] != "true" {
			t.Errorf("bucket %s tags = %v", aws.ToString(p.Bucket), got)
		}
	}
}

// An ephemeral FSx filesystem.
func TestSporeTags_FSx(t *testing.T) {
	cfg := sdkanswer.Config("us-west-2", func(service, op string, in any) (any, error) {
		if _, ok := in.(*fsx.CreateFileSystemInput); ok {
			return &fsx.CreateFileSystemOutput{FileSystem: &fsxtypes.FileSystem{FileSystemId: aws.String("fs-1")}}, nil
		}
		return nil, nil
	})
	d := tagtest.New(t, libtags.ToolSpawn)
	d.Install(&cfg)
	c := NewClientFromConfig(cfg)
	_, _ = c.CreateFSxLustreFilesystemAsync(context.Background(), FSxConfig{Region: "us-west-2", SubnetID: "subnet-1",
		StorageCapacity: 1200, S3Bucket: "b"})
	mustHaveCreate(t, d, "FSx", "CreateFileSystem")
}
