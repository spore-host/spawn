package iamcoverage

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ebs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/imagebuilder"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/tools/go/ssa"
)

// TestImpliedActionsFollowTheCallSite is #1377's discovery gate: an operation
// whose input sets the field an impliedRules entry names needs the rule's
// action too, and one that provably does not set it does not. Where the input
// is built out of the analysis' sight (a method value, a helper's result), the
// field counts as set: the gate's failure mode is a silent AccessDenied, so it
// over-approximates. A parameter is followed to its callers' arguments.
func TestImpliedActionsFollowTheCallSite(t *testing.T) {
	p := fixture(t)
	queue := []string{"sqs:CreateQueue"}
	queueTagged := []string{"sqs:CreateQueue", "sqs:TagQueue"}
	for fn, want := range map[string][]string{
		"queueTagged":             queueTagged,
		"queueUntagged":           queue, // no Tags, and Tags: nil
		"queueTaggedByAssignment": queueTagged,
		"queueFromParam":          queueTagged, // one caller tags
		"queueFromUntaggedParam":  queue,       // its one caller does not
		"queueUntaggedEitherWay":  queue,       // both branches of a phi untagged
		"queueTaggedViaInterface": queueTagged,
		"queueMethodValue":        queueTagged,
		"bucketTagged":            {"s3:CreateBucket", "s3:TagResource"},
		"bucketUntagged":          {"s3:CreateBucket"},
		"uploadTagged":            {"s3:AbortMultipartUpload", "s3:PutObject", "s3:PutObjectTagging"},
		"runTagged":               {"ec2:CreateTags", "ec2:RunInstances"},
	} {
		f, err := p.Func(fixturePkg + "." + fn)
		if err != nil {
			t.Fatal(err)
		}
		inv := p.Calls([]*ssa.Function{f}, nil)
		if len(inv.Unmapped) > 0 {
			t.Errorf("%s: unmapped %v", fn, inv.Unmapped)
		}
		if got := inv.Sorted(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", fn, got, want)
		}
	}
}

// TestAnUngrantedImpliedActionFailsTheCoverageGate: a principal whose code
// creates a queue with Tags, holding sqs:CreateQueue and not sqs:TagQueue, is a
// gap — and is not once TagQueue is granted, or when its call sets no Tags.
func TestAnUngrantedImpliedActionFailsTheCoverageGate(t *testing.T) {
	p := fixture(t)
	invFor := func(fn string) map[string]*principalInventory {
		f, err := p.Func(fixturePkg + "." + fn)
		if err != nil {
			t.Fatal(err)
		}
		pi := &principalInventory{actions: map[string][]Site{}, paths: map[string][]string{}}
		pi.add(p.Calls([]*ssa.Function{f}, nil))
		return map[string]*principalInventory{"fixture": pi}
	}
	pr := func(policy string) principal {
		return principal{name: "fixture", contexts: []credContext{{name: "function role", variants: []variant{
			{name: policy, policy: goPolicy("test", `{"Statement":[{"Effect":"Allow","Action":`+policy+`,"Resource":"*"}]}`)},
		}}}}
	}
	run := func(fn, policy string) []string {
		pp := pr(policy)
		return coverageErrors([]principal{pp}, invFor(fn), func(v variant) *Policy { return v.policy(t) }, nil)
	}

	errs := run("queueTagged", `["sqs:CreateQueue"]`)
	if len(errs) != 1 || !strings.Contains(errs[0], "calls sqs:TagQueue") {
		t.Errorf("CreateQueue with Tags and no TagQueue grant: errors %q, want exactly one naming sqs:TagQueue", errs)
	}
	if errs := run("queueTagged", `["sqs:CreateQueue","sqs:TagQueue"]`); len(errs) != 0 {
		t.Errorf("TagQueue granted: %q, want none", errs)
	}
	if errs := run("queueUntagged", `["sqs:CreateQueue"]`); len(errs) != 0 {
		t.Errorf("CreateQueue without Tags: %q, want none", errs)
	}
}

// impliedInputTypes is the SDK input type of every impliedRules operation, so
// each rule's field is checked against the real type even when no code here
// calls the operation (the site-level check only fires where it is called).
var impliedInputTypes = map[string]any{
	"sqs:CreateQueue":                                   sqs.CreateQueueInput{},
	"iam:CreateRole":                                    iam.CreateRoleInput{},
	"iam:CreateInstanceProfile":                         iam.CreateInstanceProfileInput{},
	"iam:CreatePolicy":                                  iam.CreatePolicyInput{},
	"ec2:RunInstances":                                  ec2.RunInstancesInput{},
	"ec2:CreateSecurityGroup":                           ec2.CreateSecurityGroupInput{},
	"ec2:CreateVolume":                                  ec2.CreateVolumeInput{},
	"ec2:CreateImage":                                   ec2.CreateImageInput{},
	"ec2:CopyImage":                                     ec2.CopyImageInput{},
	"ec2:RegisterImage":                                 ec2.RegisterImageInput{},
	"ec2:CreateKeyPair":                                 ec2.CreateKeyPairInput{},
	"ec2:ImportKeyPair":                                 ec2.ImportKeyPairInput{},
	"ec2:AllocateAddress":                               ec2.AllocateAddressInput{},
	"ec2:CreatePlacementGroup":                          ec2.CreatePlacementGroupInput{},
	"ec2:AuthorizeSecurityGroupIngress":                 ec2.AuthorizeSecurityGroupIngressInput{},
	"ec2:AuthorizeSecurityGroupEgress":                  ec2.AuthorizeSecurityGroupEgressInput{},
	"ec2:PurchaseCapacityBlock":                         ec2.PurchaseCapacityBlockInput{},
	"ebs:StartSnapshot":                                 ebs.StartSnapshotInput{},
	"dynamodb:CreateTable":                              dynamodb.CreateTableInput{},
	"lambda:CreateFunction":                             lambda.CreateFunctionInput{},
	"events:PutRule":                                    eventbridge.PutRuleInput{},
	"sns:CreateTopic":                                   sns.CreateTopicInput{},
	"fsx:CreateFileSystem":                              fsx.CreateFileSystemInput{},
	"fsx:CreateDataRepositoryAssociation":               fsx.CreateDataRepositoryAssociationInput{},
	"elasticfilesystem:CreateFileSystem":                efs.CreateFileSystemInput{},
	"elasticfilesystem:CreateAccessPoint":               efs.CreateAccessPointInput{},
	"imagebuilder:CreateInfrastructureConfiguration":    imagebuilder.CreateInfrastructureConfigurationInput{},
	"imagebuilder:ImportDiskImage":                      imagebuilder.ImportDiskImageInput{},
	"sts:AssumeRole":                                    sts.AssumeRoleInput{},
	"s3:PutObject":                                      s3.PutObjectInput{},
	"s3:CopyObject":                                     s3.CopyObjectInput{},
	"s3:CreateMultipartUpload":                          s3.CreateMultipartUploadInput{},
	"s3:CreateBucket":                                   s3.CreateBucketInput{},
	sdkRoot + "feature/s3/transfermanager.UploadObject": tm.UploadObjectInput{},
}

// TestEveryImpliedRuleCitesItsSource: a rule is only as good as the AWS page
// it came from, so every rule cites at least one docs.aws.amazon.com page, has
// an action, and names a field its operation's input really has.
func TestEveryImpliedRuleCitesItsSource(t *testing.T) {
	if len(impliedRules) < 10 {
		t.Fatalf("only %d implied rules; the table has been lost", len(impliedRules))
	}
	for _, r := range impliedRules {
		if len(r.Docs) == 0 {
			t.Errorf("%s +%s: no documentation cited", r.Op, r.Field)
		}
		for _, d := range r.Docs {
			u, err := url.Parse(d)
			if err != nil || u.Scheme != "https" || u.Host != "docs.aws.amazon.com" || u.Path == "" {
				t.Errorf("%s +%s: %q is not a docs.aws.amazon.com page", r.Op, r.Field, d)
			}
		}
		if len(r.Actions) == 0 || r.Field == "" {
			t.Errorf("%s: a rule needs a field and an action: %+v", r.Op, r)
		}
		for _, a := range r.Actions {
			if !strings.Contains(a, ":") {
				t.Errorf("%s: %q is not an IAM action", r.Op, a)
			}
		}
		in, ok := impliedInputTypes[r.Op]
		if !ok {
			t.Errorf("%s: add its SDK input type to impliedInputTypes", r.Op)
			continue
		}
		typ := reflect.TypeOf(in)
		for _, name := range strings.Split(r.Field, ".") {
			if typ.Kind() == reflect.Pointer {
				typ = typ.Elem()
			}
			// The names come from impliedRules, a constant table in this package.
			// nosemgrep
			f, ok := typ.FieldByName(name)
			if !ok {
				t.Errorf("%s: %s has no field %s (rule field %s)", r.Op, typ, name, r.Field)
				break
			}
			typ = f.Type
		}
	}
}

// TestARuleForAMissingFieldIsReportedNotSkipped: a rule whose field the SDK
// input does not have (renamed in an SDK update, say) makes the call unmapped,
// which every gate fails on, instead of silently never firing.
func TestARuleForAMissingFieldIsReportedNotSkipped(t *testing.T) {
	p := fixture(t)
	f, err := p.Func(fixturePkg + ".queueFromParam")
	if err != nil {
		t.Fatal(err)
	}
	input := f.Params[2].Type() // *sqs.CreateQueueInput
	saved := impliedByOp["sqs:CreateQueue"]
	defer func() { impliedByOp["sqs:CreateQueue"] = saved }()
	op := foundOp{display: "sqs.CreateQueue", actions: []string{"sqs:CreateQueue"}, key: "sqs:CreateQueue", input: input}

	impliedByOp["sqs:CreateQueue"] = []impliedRule{{Op: "sqs:CreateQueue", Field: "QueueTags", Actions: []string{"sqs:TagQueue"}}}
	if got := applyImplied(op, nil, nil, nil); got.err == "" || len(got.actions) != 0 {
		t.Errorf("a rule naming a field CreateQueueInput lacks: %+v, want an error and no actions", got)
	}
	impliedByOp["sqs:CreateQueue"] = []impliedRule{{Op: "sqs:CreateQueue", Field: "Tags", Actions: []string{"sqs:TagQueue"}}}
	if got := applyImplied(op, nil, nil, nil); got.err != "" || !reflect.DeepEqual(got.actions, []string{"sqs:CreateQueue", "sqs:TagQueue"}) {
		t.Errorf("an unknown input with a real field: %+v, want both actions", got)
	}
}
