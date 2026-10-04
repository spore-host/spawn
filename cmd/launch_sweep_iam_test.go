package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// iamFlagState saves and restores every CLI IAM flag global.
func withIAMFlags(t *testing.T, mutate func()) {
	t.Helper()
	pRole, pPol, pMan, pFile := iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile
	pRead, pWrite := s3ReadBuckets, s3WriteBuckets
	t.Cleanup(func() {
		iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = pRole, pPol, pMan, pFile
		s3ReadBuckets, s3WriteBuckets = pRead, pWrite
	})
	iamRole, iamPolicy, iamManagedPolicies, iamPolicyFile = "", nil, nil, ""
	s3ReadBuckets, s3WriteBuckets = nil, nil
	mutate()
}

// TestCLIIAMFlagsRequireCustomProfile is the remaining half of spawn#539.
//
// #539 was "the sweep path accepts the IAM flags, never reads them, and never
// warns" — every sweep instance silently got the shared spored-instance-role,
// so a workload whose only problem was "this role cannot read my bucket" had no
// CLI-level fix and found out by paying for a boot that died on its first
// `aws s3` call.
//
// That was fixed for the flags which existed at the time. #614 then added
// --s3-read/--s3-write to the SINGLE-instance condition and not to the sweep
// one, so those two were silently dropped on sweeps — the same bug, re-created
// in the same place, because the condition was duplicated rather than shared.
//
// Both paths now call this one predicate. A flag listed here is honoured
// everywhere or nowhere.
func TestCLIIAMFlagsRequireCustomProfile(t *testing.T) {
	tests := []struct {
		name   string
		set    func()
		expect bool
	}{
		{"nothing set", func() {}, false},
		{"--iam-role", func() { iamRole = "my-role" }, true},
		{"--iam-policy", func() { iamPolicy = []string{"s3:ReadOnly"} }, true},
		{"--iam-managed-policy", func() { iamManagedPolicies = []string{"arn:aws:iam::aws:policy/X"} }, true},
		{"--iam-policy-file", func() { iamPolicyFile = "/tmp/p.json" }, true},
		// The two #614 flags. These are the ones the sweep path dropped.
		{"--s3-read", func() { s3ReadBuckets = []string{"my-inputs"} }, true},
		{"--s3-write", func() { s3WriteBuckets = []string{"my-outputs"} }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withIAMFlags(t, tt.set)
			if got := cliIAMFlagsRequireCustomProfile(); got != tt.expect {
				t.Errorf("cliIAMFlagsRequireCustomProfile() = %v, want %v.\n\n"+
					"A flag that does not reach this predicate falls back to "+
					"spored-instance-role, whose fixed policy covers only spawn's own "+
					"infrastructure buckets — so the workload boots, bills, and dies on "+
					"403 (#539/#614).", got, tt.expect)
			}
		})
	}
}

// TestBothLaunchPathsShareTheIAMPredicate is the class gate.
//
// The specific defect was not a missing flag — it was a DUPLICATED condition
// that drifted. So assert the duplication is gone: neither path may re-spell the
// condition inline.
func TestBothLaunchPathsShareTheIAMPredicate(t *testing.T) {
	for _, f := range []string{"launch_single.go", "launch_sweep.go"} {
		src := readSourceFile(t, f)
		// Match a CALL SITE, not the declaration. Searching for the bare name
		// passed vacuously: the function is DEFINED in launch_sweep.go, so that
		// file matched its own definition and the gate stayed green with the
		// drifted inline condition restored.
		if !callsIAMPredicate.MatchString(src) {
			t.Errorf("%s has no `if [!]cliIAMFlagsRequireCustomProfile()` call site; an "+
				"inline condition is how --s3-read/--s3-write came to be honoured on one "+
				"path and ignored on the other (#539/#614)", f)
		}
		// The old inline spelling must not come back alongside it.
		if strings.Contains(src, `iamRole != "" || len(iamPolicy) > 0`) {
			t.Errorf("%s still spells the IAM-flag condition inline", f)
		}
	}
}

// TestSweepAppliesScopedS3Policy: the sweep path must build the same scoped
// bucket policy the single-instance and task paths build, not merely decide that
// a custom profile is needed.
func TestSweepAppliesScopedS3Policy(t *testing.T) {
	src := readSourceFile(t, "launch_sweep.go")
	if !strings.Contains(src, "taskStagingPolicy(s3ReadBuckets, s3WriteBuckets") {
		t.Error("the sweep path does not build the scoped S3 policy from --s3-read/" +
			"--s3-write. Deciding a custom profile is needed is not enough — without the " +
			"inline policy the role has no grant on the caller's buckets and every row " +
			"still 403s (#539/#614).")
	}
	if !strings.Contains(src, "validateS3BucketFlags(s3ReadBuckets, s3WriteBuckets)") {
		t.Error("the sweep path does not validate the bucket names before creating IAM, " +
			"so a malformed bucket surfaces as an AWS error after the role exists")
	}
}

// callsIAMPredicate matches a USE of the shared predicate in a condition,
// which is what the gate is actually about — the bare name also matches the
// declaration.
var callsIAMPredicate = regexp.MustCompile(`if !?cliIAMFlagsRequireCustomProfile\(\)`)

// readSourceFile reads a file from this package's directory. The two gates above
// assert on source text because the property is structural — "these two call
// sites share one predicate" — and there is no runtime value that expresses it.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
