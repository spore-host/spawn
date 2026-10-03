package reaperdeploy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFunctionNameMatchesCoverageDetection is the assertion that closes the loop
// between #624 and #625.
//
// `spawn doctor` decides an account is covered by looking for a Lambda whose name
// starts with "spawn-ttl-reaper" (pkg/aws.DetectReaperCoverage). If this deploy named
// the function anything else, it would install a working reaper that `doctor` still
// reported as absent — the tool would be lying in the opposite direction from the bug
// #624 fixed, which is no better.
func TestFunctionNameMatchesCoverageDetection(t *testing.T) {
	const detectionPrefix = "spawn-ttl-reaper"
	if !strings.HasPrefix(FunctionName, detectionPrefix) {
		t.Errorf("FunctionName = %q must start with %q, or `spawn doctor` will not see this deployment",
			FunctionName, detectionPrefix)
	}
}

func TestArtifactURL(t *testing.T) {
	want := "https://github.com/spore-host/spawn/releases/download/v0.114.0/ttl-reaper_lambda_linux_arm64.zip"
	// A leading "v" must be tolerated: callers pass both forms.
	for _, in := range []string{"0.114.0", "v0.114.0", " v0.114.0 "} {
		if got := ArtifactURL(in); got != want {
			t.Errorf("ArtifactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestObjectKeyIsVersioned(t *testing.T) {
	// Versioned, so re-deploying at a new version cannot overwrite the artifact the
	// currently deployed function was created from.
	a, b := ObjectKey("0.114.0"), ObjectKey("0.115.0")
	if a == b {
		t.Fatalf("different versions must map to different keys, both gave %q", a)
	}
	if ObjectKey("v0.114.0") != a {
		t.Error("a leading 'v' must not change the key")
	}
	if !strings.HasSuffix(a, ".zip") {
		t.Errorf("key should end .zip, got %q", a)
	}
}

func TestDefaultBucketNameIsAccountAndRegionScoped(t *testing.T) {
	a := DefaultBucketName("111111111111", "us-east-1")
	b := DefaultBucketName("222222222222", "us-east-1")
	c := DefaultBucketName("111111111111", "us-west-2")
	if a == b || a == c {
		t.Error("bucket names must differ by account and by region (S3 names are global)")
	}
	for _, n := range []string{a, b, c} {
		if strings.ToLower(n) != n {
			t.Errorf("S3 bucket names must be lowercase: %q", n)
		}
		if len(n) > 63 {
			t.Errorf("bucket name exceeds S3's 63-char limit: %q", n)
		}
	}
}

// TestEnvVarsDeployUnarmed: a deploy must land in dry-run. The whole reason arming is
// a separate step is that this thing terminates instances, and a single command should
// not start doing that in an account whose tagging it has never been seen to evaluate.
func TestEnvVarsDeployUnarmed(t *testing.T) {
	env := EnvVars("us-east-1", true)
	if env["REAPER_DRY_RUN"] != "true" {
		t.Errorf("REAPER_DRY_RUN = %q, want \"true\"", env["REAPER_DRY_RUN"])
	}
	if env["REAPER_SCAN_SELF"] != "true" {
		t.Errorf("REAPER_SCAN_SELF = %q, want \"true\"", env["REAPER_SCAN_SELF"])
	}

	// REAPER_ROLE_ARNS must be ABSENT, not empty: resolveAccounts reads its absence
	// plus SCAN_SELF=true as "scan only this account", which is this whole shape.
	if _, present := env["REAPER_ROLE_ARNS"]; present {
		t.Error("REAPER_ROLE_ARNS must not be set — a self-hosted reaper assumes no roles, " +
			"and that is the point of the deployment shape")
	}

	armed := EnvVars("us-east-1", false)
	if armed["REAPER_DRY_RUN"] != "false" {
		t.Errorf("armed REAPER_DRY_RUN = %q, want \"false\"", armed["REAPER_DRY_RUN"])
	}
}

func TestEnvVarsOmitsEmptyRegions(t *testing.T) {
	// An empty REAPER_REGIONS is not the same as unset: parseRegions on an empty
	// string yields no regions, which would make the reaper scan nothing.
	if _, present := EnvVars("", true)["REAPER_REGIONS"]; present {
		t.Error("REAPER_REGIONS must be omitted when empty, not set to \"\"")
	}
	if got := EnvVars("us-east-1,us-west-2", true)["REAPER_REGIONS"]; got != "us-east-1,us-west-2" {
		t.Errorf("REAPER_REGIONS = %q", got)
	}
}

// TestAssumeRolePolicyTrustsOnlyLambda: the feature exists because an external
// principal in the trust policy is what a governed account forbids. Granting one here
// would defeat the entire purpose.
func TestAssumeRolePolicyTrustsOnlyLambda(t *testing.T) {
	var doc struct {
		Statement []struct {
			Effect    string                 `json:"Effect"`
			Principal map[string]interface{} `json:"Principal"`
			Action    string                 `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(AssumeRolePolicy), &doc); err != nil {
		t.Fatalf("trust policy is not valid JSON: %v", err)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("expected exactly one trust statement, got %d", len(doc.Statement))
	}
	st := doc.Statement[0]
	if svc, ok := st.Principal["Service"]; !ok || svc != "lambda.amazonaws.com" {
		t.Errorf("Principal must be only lambda.amazonaws.com, got %v", st.Principal)
	}
	if _, hasAWS := st.Principal["AWS"]; hasAWS {
		t.Error("the trust policy must not name an AWS account principal — an external " +
			"principal is exactly what this deployment shape exists to avoid")
	}
}

// TestArmWarningNamesTheIrreversibleConsequence: the prompt has to say what will
// happen in the user's terms, because it cannot be undone.
func TestArmWarningNamesTheIrreversibleConsequence(t *testing.T) {
	w := ArmWarning("123456789012", "us-west-2")
	for _, want := range []string{
		"123456789012",   // which account
		"us-west-2",      // which region
		"TERMINATE",      // what it does
		"spawn:managed",  // what it will touch
		"not reversible", // and that it cannot be undone
	} {
		if !strings.Contains(w, want) {
			t.Errorf("arm warning must mention %q:\n%s", want, w)
		}
	}
}

func TestTagsMarkOwnership(t *testing.T) {
	tags := Tags("v0.114.0")
	if tags["spawn:managed"] != "true" {
		t.Error("deployed resources must be marked spawn:managed so a teardown and a human can identify them")
	}
	if tags["spawn:version"] != "0.114.0" {
		t.Errorf("spawn:version = %q, want the v stripped", tags["spawn:version"])
	}
}
