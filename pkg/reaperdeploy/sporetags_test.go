package reaperdeploy

import (
	"context"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	libtags "github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/iamtags"
	"github.com/spore-host/libs/tags/s3tags"
	"github.com/spore-host/libs/tags/tagtest"

	"github.com/spore-host/spawn/internal/sdkanswer"
	"github.com/spore-host/spawn/pkg/sporetags"
)

// Every resource `spawn reaper deploy` creates carries
// spore-host:managed-by=spawn and spore-host:component=ttl-reaper, and none
// spore-host:role=service: the self-hosted reaper is tool infrastructure in
// the user's account that teardown removes (#1652). Checked by the tagtest
// Detector on real clients.
func TestSporeTags_ReaperDeploy(t *testing.T) {
	var role map[string]string
	var bucket map[string]string
	var fn map[string]string
	cfg := sdkanswer.Config("us-west-2", func(service, op string, in any) (any, error) {
		switch v := in.(type) {
		case *iam.GetRoleInput:
			if role == nil {
				return nil, &iamtypes.NoSuchEntityException{Message: awssdk.String("no role")}
			}
			return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: v.RoleName, Tags: iamtags.FromMap(role)}}, nil
		case *iam.CreateRoleInput:
			role = iamtags.ToMap(v.Tags)
		case *iam.TagRoleInput:
			for k, val := range iamtags.ToMap(v.Tags) {
				role[k] = val
			}
		case *s3.HeadBucketInput:
			return nil, sdkanswer.Err("NotFound")
		case *s3.PutBucketTaggingInput:
			bucket = s3tags.ToMap(v.Tagging.TagSet)
		case *lambda.GetFunctionInput:
			return nil, &lambdatypes.ResourceNotFoundException{Message: awssdk.String("no function")}
		case *lambda.CreateFunctionInput:
			fn = v.Tags
			return &lambda.CreateFunctionOutput{FunctionArn: awssdk.String("arn:aws:lambda:us-west-2:123456789012:function:" + FunctionName)}, nil
		}
		return nil, nil
	})
	d := tagtest.New(t, libtags.ToolSpawn,
		tagtest.Allow{Service: "S3", Operation: "CreateBucket", Reason: "the artifact bucket is tagged by the PutBucketTagging that follows, which the Detector checks"},
		tagtest.Allow{Service: "S3", Operation: "PutObject", Reason: "the artifact zip is an object in the tagged artifact bucket, removed with it"})
	d.Install(&cfg)
	dep := New(cfg)
	dep.PropagationDelay = -1
	dep.Fetch = func(context.Context, string) ([]byte, error) { return []byte("PK zip"), nil }
	opts := Options{AccountID: "123456789012", Region: "us-west-2", Version: "v0.0.0-test"}
	if _, err := dep.Deploy(context.Background(), opts); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	seen := map[string]int{}
	for _, c := range d.Calls() {
		seen[c.Service+" "+c.Operation]++
	}
	for _, want := range []string{"IAM CreateRole", "CloudWatch Logs CreateLogGroup", "S3 PutBucketTagging", "Lambda CreateFunction", "EventBridge PutRule"} {
		if seen[want] == 0 {
			t.Errorf("the Detector saw no %s; saw %v", want, seen)
		}
	}
	for what, m := range map[string]map[string]string{"role": role, "bucket": bucket, "function": fn} {
		if m[libtags.ManagedBy] != "spawn" || m[libtags.Component] != "ttl-reaper" || m[libtags.Role] != "" {
			t.Errorf("%s tags = %v, want managed-by=spawn, component=ttl-reaper and no spore-host:role", what, m)
		}
	}
	if role[libtags.Scope] != sporetags.ScopeGlobal || fn[libtags.Scope] != "us-west-2" {
		t.Errorf("scopes: role %q, function %q", role[libtags.Scope], fn[libtags.Scope])
	}

	// A re-deploy onto a role an older spawn created untagged converges it,
	// keeping the first spore-host:created.
	role = map[string]string{"spawn:managed": "true", libtags.Created: "2026-01-01T00:00:00Z"}
	if _, err := dep.Deploy(context.Background(), opts); err != nil {
		t.Fatalf("re-Deploy: %v", err)
	}
	if role[libtags.ManagedBy] != "spawn" || role[libtags.Created] != "2026-01-01T00:00:00Z" {
		t.Errorf("converged role tags = %v", role)
	}
}
