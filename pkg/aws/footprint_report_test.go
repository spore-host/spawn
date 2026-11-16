package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	rgt "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/libs/footprint"
	libtags "github.com/spore-host/libs/tags"

	"github.com/spore-host/spawn/internal/sdkanswer"
	fs "github.com/spore-host/spawn/pkg/footprintspawn"
)

// spawn#1653: `spawn footprint -o json` in the libs/footprint schema. The
// account is answered in-process (internal/sdkanswer), so the real SDK
// clients, paginators and all, run against a scripted account.

const fpAcct = "123456789012"

func spawnT(kv ...string) map[string]string {
	m := map[string]string{libtags.ManagedBy: "spawn", "spawn:managed": "true"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func ec2T(m map[string]string) []ec2types.Tag {
	var out []ec2types.Tag
	for k, v := range m {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func iamT(m map[string]string) []iamtypes.Tag {
	var out []iamtypes.Tag
	for k, v := range m {
		out = append(out, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

// fpAccount is a scripted account: one region, and enough of each source for
// every class and ownership.
func fpAccount(t *testing.T) sdkanswer.Func {
	t.Helper()
	return func(service, op string, in any) (any, error) {
		switch service + "." + op {
		case "Resource Groups Tagging API.GetResources":
			// The spore-host:managed-by scan: a lagotto role-like queue
			// (foreign) and a spawn-tagged queue only the new tag marks.
			return &rgt.GetResourcesOutput{ResourceTagMappingList: []rgttypes.ResourceTagMapping{
				{ResourceARN: aws.String("arn:aws:sqs:us-east-1:" + fpAcct + ":lagotto-watch"), Tags: []rgttypes.Tag{{Key: aws.String(libtags.ManagedBy), Value: aws.String("lagotto")}}},
				{ResourceARN: aws.String("arn:aws:sqs:us-east-1:" + fpAcct + ":spawn-pool-run1"), Tags: []rgttypes.Tag{
					{Key: aws.String(libtags.ManagedBy), Value: aws.String("spawn")}, {Key: aws.String("spawn:managed"), Value: aws.String("true")}}},
			}}, nil
		case "IAM.ListPolicies":
			return &iam.ListPoliciesOutput{Policies: []iamtypes.Policy{
				{Arn: aws.String("arn:aws:iam::" + fpAcct + ":policy/spore-host/spawn-instance-boundary"), PolicyName: aws.String("spawn-instance-boundary"),
					Path: aws.String("/spore-host/"), AttachmentCount: aws.Int32(0), PermissionsBoundaryUsageCount: aws.Int32(0)},
				{Arn: aws.String("arn:aws:iam::" + fpAcct + ":policy/spore-host/spored-policy-v3"), PolicyName: aws.String("spored-policy-v3"),
					Path: aws.String("/spore-host/"), AttachmentCount: aws.Int32(0)},
				{Arn: aws.String("arn:aws:iam::" + fpAcct + ":policy/spore-host/spored-baseline-policy-v2"), PolicyName: aws.String("spored-baseline-policy-v2"),
					Path: aws.String("/spore-host/"), AttachmentCount: aws.Int32(2)},
			}}, nil
		case "IAM.ListPolicyTags":
			arn := aws.ToString(in.(*iam.ListPolicyTagsInput).PolicyArn)
			if strings.HasSuffix(arn, "boundary") {
				return &iam.ListPolicyTagsOutput{Tags: iamT(spawnT(libtags.Permanent, "true"))}, nil
			}
			return &iam.ListPolicyTagsOutput{Tags: iamT(spawnT())}, nil
		case "IAM.ListRoleTags":
			name := aws.ToString(in.(*iam.ListRoleTagsInput).RoleName)
			if strings.HasPrefix(name, "calque-") {
				return &iam.ListRoleTagsOutput{Tags: iamT(map[string]string{libtags.ManagedBy: "calque"})}, nil
			}
			return &iam.ListRoleTagsOutput{Tags: iamT(spawnT())}, nil
		case "IAM.ListInstanceProfileTags":
			return &iam.ListInstanceProfileTagsOutput{Tags: iamT(spawnT())}, nil
		case "IAM.ListAttachedRolePolicies":
			if aws.ToString(in.(*iam.ListAttachedRolePoliciesInput).RoleName) == "spawn-instance-busy" {
				return &iam.ListAttachedRolePoliciesOutput{AttachedPolicies: []iamtypes.AttachedPolicy{
					{PolicyArn: aws.String("arn:aws:iam::" + fpAcct + ":policy/spore-host/spored-baseline-policy-v2")}}}, nil
			}
			return nil, nil
		case "CloudFormation.DescribeStacks":
			return &cloudformation.DescribeStacksOutput{Stacks: []cfntypes.Stack{
				{StackName: aws.String("spawn-bot"), StackId: aws.String("arn:aws:cloudformation:us-east-1:" + fpAcct + ":stack/spawn-bot/1"),
					StackStatus: cfntypes.StackStatusCreateComplete, Tags: []cfntypes.Tag{{Key: aws.String(libtags.Role), Value: aws.String("service")}}},
				{StackName: aws.String("spawn-reaper-crossaccount"), StackId: aws.String("arn:aws:cloudformation:us-east-1:" + fpAcct + ":stack/spawn-reaper-crossaccount/2"),
					StackStatus: cfntypes.StackStatusUpdateComplete},
				{StackName: aws.String("spore-member-events"), StackId: aws.String("arn:aws:cloudformation:us-east-1:" + fpAcct + ":stack/spore-member-events/3"),
					StackStatus: cfntypes.StackStatusUpdateInProgress},
				{StackName: aws.String("someone-elses"), StackId: aws.String("arn:aws:cloudformation:us-east-1:" + fpAcct + ":stack/someone-elses/4"),
					StackStatus: cfntypes.StackStatusCreateComplete},
			}}, nil
		case "SNS.ListTopics":
			return &sns.ListTopicsOutput{Topics: []snstypes.Topic{
				{TopicArn: aws.String("arn:aws:sns:us-east-1:" + fpAcct + ":spawn-pipeline-abc")},
				{TopicArn: aws.String("arn:aws:sns:us-east-1:" + fpAcct + ":unrelated")},
			}}, nil
		case "SNS.ListTagsForResource":
			return &sns.ListTagsForResourceOutput{Tags: []snstypes.Tag{{Key: aws.String(libtags.ManagedBy), Value: aws.String("spawn")}}}, nil
		case "SQS.ListQueues":
			return &sqs.ListQueuesOutput{QueueUrls: []string{"https://sqs.us-east-1.amazonaws.com/" + fpAcct + "/calque-jobs"}}, nil
		case "SQS.ListQueueTags":
			return &sqs.ListQueueTagsOutput{Tags: map[string]string{libtags.ManagedBy: "calque"}}, nil
		case "EC2.DescribeSecurityGroups":
			f := in.(*ec2.DescribeSecurityGroupsInput).Filters[0]
			if aws.ToString(f.Name) == "group-name" {
				return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{
					{GroupId: aws.String("sg-calque"), GroupName: aws.String("calque-nfs-sg"), Tags: ec2T(map[string]string{libtags.ManagedBy: "calque"})}}}, nil
			}
			return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{
				{GroupId: aws.String("sg-busy"), GroupName: aws.String("spawn-dcv")},
				{GroupId: aws.String("sg-idle"), GroupName: aws.String("spawn-mpi-x")},
			}}, nil
		case "EC2.DescribeNetworkInterfaces":
			return &ec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: []ec2types.NetworkInterface{
				{Groups: []ec2types.GroupIdentifier{{GroupId: aws.String("sg-busy")}}}}}, nil
		case "EC2.DescribeImages":
			return &ec2.DescribeImagesOutput{Images: []ec2types.Image{{ImageId: aws.String("ami-0123456789abcdef1"), Name: aws.String("calque-worker-2026")}}}, nil
		case "EC2.DescribeRegions":
			return &ec2.DescribeRegionsOutput{Regions: []ec2types.Region{{RegionName: aws.String("us-east-1")}}}, nil
		case "EC2.DescribeInstances":
			fl := in.(*ec2.DescribeInstancesInput).Filters
			if len(fl) > 0 && aws.ToString(fl[0].Name) == "instance-id" {
				return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
					InstanceId:         aws.String("i-0123456789abcdef0"),
					SecurityGroups:     []ec2types.GroupIdentifier{{GroupId: aws.String("sg-busy")}},
					IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String("arn:aws:iam::" + fpAcct + ":instance-profile/spawn-instance-busy")},
				}}}}}, nil
			}
			// WorkersInUse: one live instance with the busy profile, in pool run1.
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
				InstanceId:         aws.String("i-0123456789abcdef0"),
				IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String("arn:aws:iam::" + fpAcct + ":instance-profile/spawn-instance-busy")},
				Tags:               ec2T(map[string]string{"spawn:pool-run-id": "run1"}),
			}}}}}, nil
		case "S3.GetBucketTagging":
			return &s3.GetBucketTaggingOutput{TagSet: []s3types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")},
				{Key: aws.String(libtags.ManagedBy), Value: aws.String("spawn")}}}, nil
		case "S3.ListObjectVersions":
			return &s3.ListObjectVersionsOutput{Versions: []s3types.ObjectVersion{{Key: aws.String("task/out.json")}}}, nil
		}
		return nil, nil
	}
}

// fpInput is what the existing scans would have handed FootprintReport.
func fpInput() FootprintInput {
	return FootprintInput{
		Account: fpAcct, Regions: []string{"us-east-1"}, Now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
		Tagged: []ManagedResource{
			{ARN: "arn:aws:ec2:us-east-1:" + fpAcct + ":instance/i-0123456789abcdef0", Service: "ec2", ResourceType: "instance",
				ID: "i-0123456789abcdef0", Region: "us-east-1", State: "running", Tags: spawnT()},
			{ARN: "arn:aws:ec2:us-east-1:" + fpAcct + ":volume/vol-0123456789abcdef0", Service: "ec2", ResourceType: "volume",
				ID: "vol-0123456789abcdef0", Region: "us-east-1", State: "available", Tags: spawnT()},
			{ARN: "arn:aws:ec2:us-east-1:" + fpAcct + ":security-group/sg-busy", Service: "ec2", ResourceType: "security-group",
				ID: "sg-busy", Region: "us-east-1", Tags: spawnT()},
			{ARN: "arn:aws:ec2:us-east-1:" + fpAcct + ":security-group/sg-idle", Service: "ec2", ResourceType: "security-group",
				ID: "sg-idle", Region: "us-east-1", Tags: spawnT()},
			{ARN: "arn:aws:ec2:us-east-1::image/ami-mine", Service: "ec2", ResourceType: "image", ID: "ami-mine", Region: "us-east-1", Tags: spawnT()},
		},
		Global: []ControlPlaneResource{
			{Service: "iam", Kind: "role", Name: "spawn-instance-busy", ARN: "arn:aws:iam::" + fpAcct + ":role/spawn-instance-busy"},
			{Service: "iam", Kind: "instance-profile", Name: "spawn-instance-busy", ARN: "arn:aws:iam::" + fpAcct + ":instance-profile/spawn-instance-busy",
				profileRoleARNs: []string{"arn:aws:iam::" + fpAcct + ":role/spawn-instance-busy"}},
			{Service: "iam", Kind: "role", Name: "spawn-instance-idle", ARN: "arn:aws:iam::" + fpAcct + ":role/spawn-instance-idle"},
			{Service: "iam", Kind: "role", Name: "calque-worker", ARN: "arn:aws:iam::" + fpAcct + ":role/calque-worker"},
			{Service: "s3", Kind: "bucket", Name: "spawn-results-" + fpAcct + "-us-east-1", ARN: "arn:aws:s3:::spawn-results-" + fpAcct + "-us-east-1", bucketRegion: "us-east-1"},
			{Service: "s3", Kind: "bucket", Name: "spawn-staging-" + fpAcct + "-us-east-1", ARN: "arn:aws:s3:::spawn-staging-" + fpAcct + "-us-east-1", bucketRegion: "us-east-1"},
		},
	}
}

func fpReport(t *testing.T) (*footprint.Report, map[string]footprint.Resource) {
	t.Helper()
	c := NewClientFromConfig(sdkanswer.Config("us-east-1", fpAccount(t)))
	rep := c.FootprintReport(context.Background(), fpInput())
	byName := map[string]footprint.Resource{}
	for _, r := range rep.Resources {
		byName[r.Type+" "+r.Name] = r
	}
	return rep, byName
}

func wantRes(t *testing.T, by map[string]footprint.Resource, key string, own footprint.Ownership, class footprint.Class, deletable bool) footprint.Resource {
	t.Helper()
	r, ok := by[key]
	if !ok {
		t.Fatalf("%s not in the report; have %v", key, fpKeys(by))
	}
	if r.Ownership != own || r.Class != class || r.Deletable != deletable {
		t.Errorf("%s: ownership %s class %s deletable %v (%s), want %s %s %v", key, r.Ownership, r.Class, r.Deletable, r.Reason, own, class, deletable)
	}
	return r
}

func fpKeys(m map[string]footprint.Resource) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The new inventory sources: /spore-host/ policies, stacks, spawn-pipeline-*
// topics, and calque's resources as foreign.
func TestFootprintReportNewSources(t *testing.T) {
	_, by := fpReport(t)

	wantRes(t, by, fs.TypeManagedPolicy+" spawn-instance-boundary", footprint.OwnershipTagged, footprint.ClassPermanent, false)
	wantRes(t, by, fs.TypeManagedPolicy+" spored-policy-v3", footprint.OwnershipTagged, footprint.ClassLitter, true)
	wantRes(t, by, fs.TypeManagedPolicy+" spored-baseline-policy-v2", footprint.OwnershipTagged, footprint.ClassActive, true)

	wantRes(t, by, fs.TypeStack+" spawn-bot", footprint.OwnershipLegacyName, footprint.ClassPermanent, false)
	wantRes(t, by, fs.TypeStack+" spawn-reaper-crossaccount", footprint.OwnershipLegacyName, footprint.ClassResidue, false)
	wantRes(t, by, fs.TypeStack+" spore-member-events", footprint.OwnershipLegacyName, footprint.ClassActive, false)
	if _, ok := by[fs.TypeStack+" someone-elses"]; ok {
		t.Error("a stack not named like spore.host's was listed")
	}

	wantRes(t, by, fs.TypeTopic+" spawn-pipeline-abc", footprint.OwnershipTagged, footprint.ClassLitter, false)
	if _, ok := by[fs.TypeTopic+" unrelated"]; ok {
		t.Error("a topic not named spawn-pipeline-* was listed")
	}

	for _, k := range []string{
		fs.TypeQueue + " calque-jobs", fs.TypeSecurityGroup + " calque-nfs-sg", fs.TypeRole + " calque-worker", fs.TypeQueue + " lagotto-watch",
	} {
		wantRes(t, by, k, footprint.OwnershipForeign, footprint.ClassLitter, false)
	}
	wantRes(t, by, fs.TypeImage+" calque-worker-2026", footprint.OwnershipForeign, footprint.ClassUserData, false)
}

// Classification of what the existing scans found, and the dependencies.
func TestFootprintReportClassifiesAndLinks(t *testing.T) {
	_, by := fpReport(t)

	inst := wantRes(t, by, fs.TypeInstance+" i-0123456789abcdef0", footprint.OwnershipTagged, footprint.ClassActive, true)
	wantRes(t, by, fs.TypeVolume+" vol-0123456789abcdef0", footprint.OwnershipTagged, footprint.ClassLitter, true)
	wantRes(t, by, fs.TypeSecurityGroup+" spawn-dcv", footprint.OwnershipTagged, footprint.ClassActive, true)
	wantRes(t, by, fs.TypeSecurityGroup+" spawn-mpi-x", footprint.OwnershipTagged, footprint.ClassLitter, true)
	wantRes(t, by, fs.TypeImage+" ami-mine", footprint.OwnershipTagged, footprint.ClassUserData, false)
	prof := wantRes(t, by, fs.TypeInstanceProfile+" spawn-instance-busy", footprint.OwnershipTagged, footprint.ClassActive, true)
	role := wantRes(t, by, fs.TypeRole+" spawn-instance-busy", footprint.OwnershipTagged, footprint.ClassActive, true)
	wantRes(t, by, fs.TypeRole+" spawn-instance-idle", footprint.OwnershipTagged, footprint.ClassLitter, true)
	wantRes(t, by, fs.TypeQueue+" spawn-pool-run1", footprint.OwnershipTagged, footprint.ClassActive, true)
	wantRes(t, by, fs.TypeBucket+" spawn-results-"+fpAcct+"-us-east-1", footprint.OwnershipTagged, footprint.ClassUserData, false)
	wantRes(t, by, fs.TypeBucket+" spawn-staging-"+fpAcct+"-us-east-1", footprint.OwnershipTagged, footprint.ClassLitter, true)

	has := func(r footprint.Resource, arn string) bool {
		for _, d := range r.DependsOn {
			if d == arn {
				return true
			}
		}
		return false
	}
	if !has(inst, "arn:aws:ec2:us-east-1:"+fpAcct+":security-group/sg-busy") || !has(inst, prof.ARN) {
		t.Errorf("instance depends_on %v, want its security group and profile", inst.DependsOn)
	}
	if !has(prof, role.ARN) {
		t.Errorf("profile depends_on %v, want its role", prof.DependsOn)
	}
	if !has(role, "arn:aws:iam::"+fpAcct+":policy/spore-host/spored-baseline-policy-v2") {
		t.Errorf("role depends_on %v, want its attached policy", role.DependsOn)
	}
}

// The report round-trips through footprint.Decode, which is what
// `spawn teardown --plan` reads it with.
func TestFootprintReportRoundTrips(t *testing.T) {
	rep, _ := fpReport(t)
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(rep); err != nil {
		t.Fatal(err)
	}
	got, err := footprint.Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v\n%s", err, buf.String())
	}
	if got.Tool != fs.Tool || got.Account != fpAcct || len(got.Resources) != len(rep.Resources) {
		t.Fatalf("decoded tool %q account %q %d resources, want spawn %s %d", got.Tool, got.Account, len(got.Resources), fpAcct, len(rep.Resources))
	}
	for i := range rep.Resources {
		a, b := rep.Resources[i], got.Resources[i]
		if a.ARN != b.ARN || a.Class != b.Class || a.Ownership != b.Ownership || a.Deletable != b.Deletable || len(a.DependsOn) != len(b.DependsOn) {
			t.Errorf("resource %d changed in the round trip: %+v -> %+v", i, a, b)
		}
	}
	if _, err := footprint.NewPlan(fs.Tool, fs.Patterns, got.Resources); err != nil {
		t.Errorf("the decoded report does not make a plan: %v", err)
	}
}

// Every class and ownership the report gives is one Decode accepts, and a
// failed listing is a warning, never a dropped report.
func TestFootprintReportWarnsOnAFailedSource(t *testing.T) {
	f := fpAccount(t)
	c := NewClientFromConfig(sdkanswer.Config("us-east-1", func(service, op string, in any) (any, error) {
		if service == "CloudFormation" {
			return nil, sdkanswer.Err("AccessDenied")
		}
		return f(service, op, in)
	}))
	rep := c.FootprintReport(context.Background(), fpInput())
	found := false
	for _, w := range rep.Warnings {
		found = found || strings.Contains(w, "cloudformation:DescribeStacks")
	}
	if !found {
		t.Errorf("warnings %v do not name the denied cloudformation:DescribeStacks", rep.Warnings)
	}
	if len(rep.Resources) == 0 {
		t.Error("a denied source emptied the report")
	}
}

// An instance check that fails makes every role, profile and pool queue
// active: "unused" could not be shown.
func TestFootprintReportFailsClosedOnInstances(t *testing.T) {
	f := fpAccount(t)
	c := NewClientFromConfig(sdkanswer.Config("us-east-1", func(service, op string, in any) (any, error) {
		if service == "EC2" && op == "DescribeRegions" {
			return nil, sdkanswer.Err("UnauthorizedOperation")
		}
		return f(service, op, in)
	}))
	rep := c.FootprintReport(context.Background(), fpInput())
	for _, r := range rep.Resources {
		if (r.Type == fs.TypeRole || r.Type == fs.TypeInstanceProfile) && r.Ownership == footprint.OwnershipTagged && r.Class != footprint.ClassActive {
			t.Errorf("%s %s is %s although instances could not be read", r.Type, r.Name, r.Class)
		}
	}
}
