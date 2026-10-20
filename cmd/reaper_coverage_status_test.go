package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/reaperdeploy"
)

// stubReaperLambda answers only GetFunction; any other call panics on the nil
// embedded interface, which is the point — status must not mutate anything.
type stubReaperLambda struct {
	reaperdeploy.LambdaAPI
	exists bool
	tags   map[string]string
}

func (s stubReaperLambda) GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	if !s.exists {
		return nil, &lambdatypes.ResourceNotFoundException{}
	}
	return &lambda.GetFunctionOutput{Tags: s.tags, Configuration: &lambdatypes.FunctionConfiguration{}}, nil
}

type stubReaperEvents struct{ reaperdeploy.EventsAPI }

func (stubReaperEvents) DescribeRule(context.Context, *eventbridge.DescribeRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	return nil, &ebtypes.ResourceNotFoundException{}
}

func stubReaperDeployer(t *testing.T, lam stubReaperLambda) {
	t.Helper()
	real := newReaperDeployerWithConfig
	newReaperDeployerWithConfig = func(context.Context) (*reaperdeploy.Deployer, string, string, sdkaws.Config, error) {
		return &reaperdeploy.Deployer{Lambda: lam, Events: stubReaperEvents{}}, "123456789012", "eu-west-1", sdkaws.Config{Region: "eu-west-1"}, nil
	}
	t.Cleanup(func() { newReaperDeployerWithConfig = real })
}

// #1157: `spawn reaper status` said NOT DEPLOYED in accounts the template
// reaper or a member role covers, contradicting `spawn doctor`. With no
// selfscan function it must ask the same coverage probe doctor uses.
func TestReaperStatusReportsCoverageFromTheDoctorProbe(t *testing.T) {
	stubReaperDeployer(t, stubReaperLambda{exists: false})
	calls := 0
	real := reaperCoverage
	reaperCoverage = func(context.Context, sdkaws.Config) spawnaws.ReaperCoverage {
		calls++
		return spawnaws.ReaperCoverage{Covered: true, Determined: true, How: "cross-account role spawn-ttl-reaper-ec2"}
	}
	t.Cleanup(func() { reaperCoverage = real })

	c := &cobra.Command{}
	var out bytes.Buffer
	c.SetOut(&out)
	if err := runReaperStatus(c, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	if calls != 1 {
		t.Fatalf("status consulted the coverage probe %d times, want 1", calls)
	}
	got := out.String()
	if strings.Contains(got, "NOT DEPLOYED") {
		t.Errorf("status says NOT DEPLOYED in a covered account:\n%s", got)
	}
	if !strings.Contains(got, "COVERED") || !strings.Contains(got, "spawn-ttl-reaper-ec2") {
		t.Errorf("status does not name the coverage it found:\n%s", got)
	}
}

// The three verdicts render distinctly; only a determined "not covered" says
// NOT DEPLOYED.
func TestReaperStatusRendersEachCoverageVerdict(t *testing.T) {
	info := reaperdeploy.Info{}
	notCovered := renderReaperStatusWithCLI(info, "1", "eu-west-1", "0.137.0", &spawnaws.ReaperCoverage{Determined: true})
	if !strings.Contains(notCovered, "NOT DEPLOYED") {
		t.Errorf("a determined, uncovered account must say NOT DEPLOYED:\n%s", notCovered)
	}
	unknown := renderReaperStatusWithCLI(info, "1", "eu-west-1", "0.137.0", &spawnaws.ReaperCoverage{Why: "AccessDenied"})
	if strings.Contains(unknown, "NOT DEPLOYED") || !strings.Contains(unknown, "could not be determined") {
		t.Errorf("an undetermined verdict must not claim NOT DEPLOYED:\n%s", unknown)
	}
}

// #1157: teardown removes the bucket deploy used — --bucket, else the one
// recorded on the function, else the default name.
func TestReaperTeardownBucketIsTheOneDeployUsed(t *testing.T) {
	newFS := func(args ...string) *pflag.FlagSet {
		fs := pflag.NewFlagSet("teardown", pflag.ContinueOnError)
		fs.StringVar(&reaperBucket, "bucket", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs
	}
	saved := reaperBucket
	t.Cleanup(func() { reaperBucket = saved })
	ctx := context.Background()
	tagged := &reaperdeploy.Deployer{Lambda: stubReaperLambda{exists: true, tags: map[string]string{reaperdeploy.ArtifactBucketTag: "recorded-bucket"}}, Events: stubReaperEvents{}}
	untagged := &reaperdeploy.Deployer{Lambda: stubReaperLambda{exists: false}, Events: stubReaperEvents{}}

	if got := reaperTeardownBucket(ctx, newFS("--bucket", "flag-bucket"), tagged, "123456789012", "eu-west-1"); got != "flag-bucket" {
		t.Errorf("--bucket ignored: got %q", got)
	}
	if got := reaperTeardownBucket(ctx, newFS(), tagged, "123456789012", "eu-west-1"); got != "recorded-bucket" {
		t.Errorf("the bucket recorded at deploy time was ignored: got %q", got)
	}
	if got, want := reaperTeardownBucket(ctx, newFS(), untagged, "123456789012", "eu-west-1"), reaperdeploy.DefaultBucketName("123456789012", "eu-west-1"); got != want {
		t.Errorf("fallback = %q, want the default %q", got, want)
	}
	if reaperTeardownCmd.Flags().Lookup("bucket") == nil {
		t.Error("spawn reaper teardown has no --bucket flag")
	}
}
