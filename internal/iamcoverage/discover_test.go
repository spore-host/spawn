package iamcoverage

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/ssa"
)

var (
	fixtureOnce sync.Once
	fixtureProg *Program
	fixtureErr  error
)

// fixture loads testdata/fixture, whose every AWS call is known.
func fixture(t *testing.T) *Program {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureProg, fixtureErr = Load(LoadConfig{Dir: repoRoot, GOOS: "linux", GOARCH: "amd64",
			Patterns: []string{"./internal/iamcoverage/testdata/fixture"}, Root: repoRoot})
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return fixtureProg
}

const fixturePkg = "github.com/spore-host/spawn/internal/iamcoverage/testdata/fixture"

// TestDiscoversEveryShapeOfCall: a direct client call, a call through a
// spawn-style interface, a method value, a paginator, a waiter, an operation
// whose IAM action is named differently (HeadBucket), the transfer manager,
// an assume-role credentials provider and a hand-signed Function URL request
// are all found and mapped to their IAM actions, and nothing else is: not the
// unreachable function, and not GetCallerIdentity, which needs no permission.
func TestDiscoversEveryShapeOfCall(t *testing.T) {
	p := fixture(t)
	inv := p.Calls(p.Mains(), nil)
	want := []string{
		"ec2:CreateTags",           // implied: RunInstances with TagSpecifications
		"ec2:DescribeImages",       // waiter
		"ec2:DescribeInstances",    // through an interface
		"ec2:DescribeVolumes",      // paginator
		"ec2:RebootInstances",      // in feature, not cut here
		"ec2:RunInstances",         // implied (see TestImpliedActionsFollowTheCallSite)
		"ec2:StopInstances",        // method value
		"ec2:TerminateInstances",   // direct
		"events:PutTargets",        // implied
		"iam:PassRole",             // implied: RunInstances with IamInstanceProfile, a target's RoleArn
		"lambda:InvokeFunctionUrl", // SigV4-signed for "lambda"
		"s3:AbortMultipartUpload",  // transfer manager
		"s3:CreateBucket",          // implied
		"s3:ListBucket",            // HeadBucket
		"s3:PutObject",             // transfer manager
		"s3:PutObjectTagging",      // implied: UploadObject with Tagging
		"s3:TagResource",           // implied: CreateBucket with tags
		"sqs:CreateQueue",          // implied
		"sqs:TagQueue",             // implied: CreateQueue with Tags
		"sts:AssumeRole",           // stscreds.NewAssumeRoleProvider
	}
	if got := inv.Sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("discovered\n  %v\nwant\n  %v", got, want)
	}
	if _, ok := inv.Actions["ec2:DeleteVpc"]; ok {
		t.Error("ec2:DeleteVpc is only in an unreachable function and must not be inventoried")
	}
	// The one signing call whose service is not a constant is reported, not
	// guessed and not dropped.
	if len(inv.Unmapped) != 1 || !strings.Contains(inv.Unmapped[0], "not a constant") {
		t.Errorf("Unmapped = %v, want exactly the dynamic SignHTTP", inv.Unmapped)
	}
	// A site names where the call is, relative to the repository.
	sites := inv.Actions["ec2:TerminateInstances"]
	if len(sites) != 1 || !strings.HasPrefix(sites[0].Pos, "internal/iamcoverage/testdata/fixture/main.go:") {
		t.Fatalf("TerminateInstances sites = %+v", sites)
	}
	if chain := inv.PathTo(sites[0].Func); len(chain) < 2 || chain[0] != fixturePkg+".main" {
		t.Errorf("PathTo = %v, want a chain from main", chain)
	}
}

// TestCutRemovesASubtreeAndRootsSelectOne: a cut function's calls belong to
// another principal and leave this one's inventory; rooting at it yields only
// them. This is how spored's feature subcommands are held to their own roles.
func TestCutRemovesASubtreeAndRootsSelectOne(t *testing.T) {
	p := fixture(t)
	feature, err := p.Func(fixturePkg + ".feature")
	if err != nil {
		t.Fatal(err)
	}
	without := p.Calls(p.Mains(), []*ssa.Function{feature})
	if _, ok := without.Actions["ec2:RebootInstances"]; ok {
		t.Error("cutting feature left its ec2:RebootInstances in the inventory")
	}
	if _, ok := without.Actions["ec2:TerminateInstances"]; !ok {
		t.Error("cutting feature also removed calls outside it")
	}
	only := p.Calls([]*ssa.Function{feature}, nil)
	if got := only.Sorted(); !reflect.DeepEqual(got, []string{"ec2:RebootInstances"}) {
		t.Errorf("rooted at feature: %v, want [ec2:RebootInstances]", got)
	}
	if _, err := p.Func(fixturePkg + ".noSuchFunction"); err == nil {
		t.Error("a root or cut that does not exist must be an error, not an empty set")
	}
}

// TestUnmappedServicesAreReportedNotGuessed is CLAUDE.md rule 4 inside the
// mapping: a service package with no IAM prefix in the table, or an SDK
// library that is neither a service nor a mapped feature, is an error. The
// original reaper gate mapped an unknown interface to "" and skipped it, which
// is how route53 was invisible to it (#254).
func TestUnmappedServicesAreReportedNotGuessed(t *testing.T) {
	op, ok := mapOp(sdkServicePrefix+"neptunegraph", "CreateGraph", "neptunegraph.CreateGraph")
	if !ok || op.err == "" || len(op.actions) != 0 {
		t.Errorf("unknown service: %+v, want an error", op)
	}
	op, ok = mapOp(sdkRoot+"feature/s3/manager", "Upload", "manager.Upload")
	if !ok || op.err == "" {
		t.Errorf("unmapped feature package: %+v, want an error", op)
	}
	op, ok = mapOp(sdkRoot+"feature/s3/transfermanager", "CopyObject", "transfermanager.CopyObject")
	if !ok || op.err == "" {
		t.Errorf("unmapped transfer-manager method: %+v, want an error", op)
	}
	// And the names that differ map to what IAM authorizes.
	for call, want := range map[string][]string{
		"cloudwatchlogs.FilterLogEvents":           {"logs:FilterLogEvents"},
		"eventbridge.PutRule":                      {"events:PutRule"},
		"s3.DeleteBucketTagging":                   {"s3:PutBucketTagging"},
		"lambda.Invoke":                            {"lambda:InvokeFunction"},
		"apigatewaymanagementapi.PostToConnection": {"execute-api:ManageConnections"},
		"sts.GetCallerIdentity":                    {},
	} {
		pkg, opName, _ := strings.Cut(call, ".")
		got, ok := mapOp(sdkServicePrefix+pkg, opName, call)
		if !ok || got.err != "" || !reflect.DeepEqual(got.actions, want) {
			t.Errorf("%s -> %+v, want %v", call, got, want)
		}
	}
}

// TestAWildcardNeverSatisfiesTheGate: "*", "s3:*", "s3:Put*" and NotAction all
// match, but only as WildcardOnly, which the gate rejects. A Deny grants
// nothing, and a statement with a Principal (a trust or resource policy) is
// not an identity grant.
func TestAWildcardNeverSatisfiesTheGate(t *testing.T) {
	for _, doc := range []string{
		`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`,
		`{"Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":"*"}]}`,
		`{"Statement":[{"Effect":"Allow","Action":["s3:Put*"],"Resource":"*"}]}`,
		`{"Statement":[{"Effect":"Allow","NotAction":["iam:*"],"Resource":"*"}]}`,
	} {
		p, err := ParsePolicyJSON("t", doc)
		if err != nil {
			t.Fatal(err)
		}
		if g, _ := p.Grants("s3:PutObject"); g != WildcardOnly {
			t.Errorf("%s: Grants = %v, want WildcardOnly", doc, g)
		}
	}
	p, err := ParsePolicyJSON("t", `{"Statement":[
		{"Effect":"Deny","Action":"s3:GetObject","Resource":"*"},
		{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"},
		{"Effect":"Allow","Action":["S3:PUTOBJECT"],"Resource":"*"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := p.Grants("s3:GetObject"); g != NotGranted {
		t.Error("a Deny must grant nothing")
	}
	// An Allow cancelled by an unconditional Deny on every resource (#1397).
	denied, err := ParsePolicyJSON("t", `{"Statement":[
		{"Effect":"Allow","Action":"iam:PassRole","Resource":"*"},
		{"Effect":"Deny","Action":"iam:*","Resource":"*"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := denied.Grants("iam:PassRole"); g != Denied {
		t.Errorf("Allow and an unconditional Deny: Grants = %v, want Denied", g)
	}
	if g, _ := p.Grants("sts:AssumeRole"); g != NotGranted {
		t.Error("a trust-policy statement must grant nothing")
	}
	if g, _ := p.Grants("s3:PutObject"); g != Exact {
		t.Error("IAM action names are case-insensitive")
	}
}

const testTemplate = `Resources:
  Fn:
    Type: AWS::Serverless::Function
    Properties:
      Policies:
        - AWSLambdaBasicExecutionRole
        - DynamoDBCrudPolicy:
            TableName: t
        - !If
          - SomeCondition
          - Statement:
              - Effect: Allow
                Action:
                  - route53:ChangeResourceRecordSets
                Resource: '*'
          - !Ref AWS::NoValue
  Role:
    Type: AWS::IAM::Role
    Properties:
      ManagedPolicyArns: [arn:aws:iam::aws:policy/X]
      Policies:
        - PolicyName: p
          PolicyDocument:
            Statement:
              - Effect: Allow
                Action: sqs:SendMessage
                Resource: '*'
  Extra:
    Type: AWS::IAM::Policy
    Properties:
      Roles: [!Ref Role]
      PolicyDocument:
        Statement:
          - Effect: Allow
            Action: [sns:Publish]
            Resource: '*'
`

const testScript = `#!/bin/bash
cat > /tmp/trust.json <<EOF
{"Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}
EOF
POLICY=$(cat <<'EOF'
{
  "Statement": [
    {"Effect": "Allow", "Action": ["ec2:DescribeRegions"], "Resource": $ARNS},
    {"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::${BUCKET}/*"}
  ]
}
EOF
)
aws iam attach-role-policy --role-name r --policy-arn "arn:aws:iam::aws:policy/Y"
`

// TestPolicySourcesParseEveryForm: SAM inline statements behind !If (both
// branches count: a conditional grant is a grant in the configuration that
// enables it), SAM policy templates, managed policies, a role's inline
// policies and a separate AWS::IAM::Policy naming it, and shell
// here-documents with a JSON value spliced in unquoted.
func TestPolicySourcesParseEveryForm(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "t.yaml")
	if err := os.WriteFile(tmpl, []byte(testTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	fn, err := ParseCFNPolicies(tmpl, "Fn")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"route53:ChangeResourceRecordSets", "dynamodb:ConditionCheckItem"} {
		if g, _ := fn.Grants(a); g != Exact {
			t.Errorf("Fn: %s not granted", a)
		}
	}
	if !reflect.DeepEqual(fn.Opaque, []string{"AWSLambdaBasicExecutionRole"}) {
		t.Errorf("Fn opaque = %v", fn.Opaque)
	}
	role, err := ParseCFNPolicies(tmpl, "Role")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"sqs:SendMessage", "sns:Publish"} {
		if g, _ := role.Grants(a); g != Exact {
			t.Errorf("Role: %s not granted", a)
		}
	}
	if _, err := ParseCFNPolicies(tmpl, "Missing"); err == nil {
		t.Error("a missing resource must be an error")
	}

	sh := filepath.Join(dir, "s.sh")
	if err := os.WriteFile(sh, []byte(testScript), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := ParseShellPolicies(sh)
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.Actions(); !reflect.DeepEqual(got, []string{"ec2:describeregions", "s3:getobject"}) {
		t.Errorf("shell actions = %v (the trust policy must be skipped)", got)
	}
	if !reflect.DeepEqual(sp.Opaque, []string{"arn:aws:iam::aws:policy/Y"}) {
		t.Errorf("shell opaque = %v", sp.Opaque)
	}
}
