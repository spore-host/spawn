package aws

import (
	"context"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// fakeRegionLambda answers ListFunctions per region from a fixed map of
// function name → environment (nil env = no Environment at all).
type fakeRegionLambda struct {
	mu     sync.Mutex
	byReg  map[string]map[string]map[string]string
	listed []string
}

func (f *fakeRegionLambda) listerFor(region string) reaperFunctionLister {
	return regionLister{f: f, region: region}
}

type regionLister struct {
	f      *fakeRegionLambda
	region string
}

func (l regionLister) ListFunctions(context.Context, *lambda.ListFunctionsInput, ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error) {
	l.f.mu.Lock()
	l.f.listed = append(l.f.listed, l.region)
	l.f.mu.Unlock()
	out := &lambda.ListFunctionsOutput{}
	for name, env := range l.f.byReg[l.region] {
		fc := lambdatypes.FunctionConfiguration{FunctionName: awssdk.String(name)}
		if env != nil {
			fc.Environment = &lambdatypes.EnvironmentResponse{Variables: env}
		}
		out.Functions = append(out.Functions, fc)
	}
	return out, nil
}

type fakeRoles struct{ exists bool }

func (r fakeRoles) GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if r.exists {
		return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: awssdk.String(ReaperCoverageRoleName)}}, nil
	}
	return nil, &iamtypes.NoSuchEntityException{}
}

// #1157: coverage follows the regions the reaper SCANS, not the region the
// function lives in.
func TestReaperCoverageFollowsTheScannedRegions(t *testing.T) {
	cases := []struct {
		name    string
		region  string
		byReg   map[string]map[string]map[string]string
		role    bool
		covered bool
	}{
		{"function here, scans here", "eu-west-1",
			map[string]map[string]map[string]string{"eu-west-1": {"spawn-ttl-reaper-selfscan": {"REAPER_REGIONS": "eu-west-1,us-east-1"}}}, false, true},
		{"function here, does NOT scan here", "eu-west-1",
			map[string]map[string]map[string]string{"eu-west-1": {"spawn-ttl-reaper-selfscan": {"REAPER_REGIONS": "us-east-1"}}}, false, false},
		{"function elsewhere, scans here", "us-west-2",
			map[string]map[string]map[string]string{"us-east-1": {"spawn-ttl-reaper-selfscan": {"REAPER_REGIONS": "us-east-1,us-west-2"}}}, false, true},
		{"function elsewhere, does not scan here", "us-west-2",
			map[string]map[string]map[string]string{"us-east-1": {"spawn-ttl-reaper-selfscan": {"REAPER_REGIONS": "us-east-1"}}}, false, false},
		{"unset REAPER_REGIONS = the reaper's defaults, which include us-east-2", "us-east-2",
			map[string]map[string]map[string]string{"us-east-1": {"spawn-ttl-reaper-production": {}}}, false, true},
		{"unset REAPER_REGIONS = defaults, which exclude sa-east-1", "sa-east-1",
			map[string]map[string]map[string]string{"sa-east-1": {"spawn-ttl-reaper-production": nil}}, false, false},
		{"member role, no local function", "eu-west-1", nil, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRegionLambda{byReg: c.byReg}
			got := detectReaperCoverage(context.Background(), c.region, f.listerFor, fakeRoles{exists: c.role})
			if got.Covered != c.covered {
				t.Errorf("Covered = %v, want %v (%+v)", got.Covered, c.covered, got)
			}
			if !got.Determined {
				t.Errorf("every probe succeeded, so the verdict must be determined: %+v", got)
			}
			if !c.covered && c.byReg != nil && !strings.Contains(got.Summary(), "scans only") {
				t.Errorf("a reaper that skips this region should be named in the summary: %q", got.Summary())
			}
		})
	}
}

// The role's verdict must not claim to know which regions the central reaper
// scans — that lives on a function in another account.
func TestReaperCoverageRoleVerdictSaysRegionsAreUnknown(t *testing.T) {
	f := &fakeRegionLambda{}
	got := detectReaperCoverage(context.Background(), "eu-west-1", f.listerFor, fakeRoles{exists: true})
	if !got.Covered || !strings.Contains(got.How, "cannot be read") {
		t.Errorf("role verdict = %+v", got)
	}
}
