//go:build e2e_tier0

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// Tier 0 regression coverage for #539: the CLI IAM flags (--iam-role,
// --iam-policy, --iam-policy-file, --iam-role-tags, --iam-allow-full-access)
// must actually reach the instances a parameter sweep launches, exactly as
// they do on the single-instance and batch-queue launch paths.
//
// Before the fix, launch_sweep.go called SetupSporedIAMRole() unconditionally
// and never looked at any of the CLI IAM flags: they parsed successfully and
// were silently discarded, so every sweep instance got the shared
// spored-instance-role regardless of what --iam-role/--iam-policy-file asked
// for — no warning, exit 0. This is the same bug class as #525 (CLI spend
// controls dropped on the sweep path), but worse: a wrong/missing IAM role
// means the workload dies on its first AWS API call with AccessDenied, which
// reads as a workload bug rather than a launch-flag bug, and the ENTIRE
// sweep's spend produces zero result.
//
// Assertions are on the instance's actual IAM instance profile (via
// DescribeInstances), not on any in-process value — a test that only checked
// "the flag was parsed" would have passed throughout the bug's life, since the
// flag WAS parsed, onto a config nobody read from.

// tagsAndProfileByInstanceType reads every instance out of EC2 and returns its
// tags AND IAM instance-profile ARN keyed by instance type. Fails if two
// instances share a type, since the mapping would be ambiguous.
func (e *spawnEnv) tagsAndProfileByInstanceType() (tags map[string]map[string]string, profileARN map[string]string) {
	e.t.Helper()
	out, err := e.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		e.t.Fatalf("DescribeInstances: %v", err)
	}
	tags = map[string]map[string]string{}
	profileARN = map[string]string{}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			itype := string(inst.InstanceType)
			if _, dup := tags[itype]; dup {
				e.t.Fatalf("two instances of type %s — cannot attribute state to a row", itype)
			}
			tg := map[string]string{}
			for _, t := range inst.Tags {
				tg[*t.Key] = *t.Value
			}
			tags[itype] = tg
			if inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil {
				profileARN[itype] = *inst.IamInstanceProfile.Arn
			}
		}
	}
	return tags, profileARN
}

// TestTier0_SweepAppliesCLIIAMRole is the core #539 case: --iam-role on the
// command line must become the launched instances' IAM instance profile,
// instead of the shared spored-instance-profile (SetupSporedIAMRole's default,
// backed by the spored-instance-role) that would otherwise attach.
func TestTier0_SweepAppliesCLIIAMRole(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
  - instance_type: c5.xlarge
`)
	env.launchForegroundSweep(file, "--iam-role", "sweep-custom-role")

	_, profileARN := env.tagsAndProfileByInstanceType()
	if len(profileARN) != 2 {
		t.Fatalf("expected 2 instances with an IAM instance profile, got %d: %v", len(profileARN), profileARN)
	}
	for itype, arn := range profileARN {
		if !strings.Contains(arn, "sweep-custom-role") {
			t.Errorf("%s: IAM instance profile ARN %q does not reference --iam-role sweep-custom-role — "+
				"got the shared spored-instance-profile instead", itype, arn)
		}
		if strings.Contains(arn, "spored-instance-profile") {
			t.Errorf("%s: instance still has the shared spored-instance-profile (%q) despite --iam-role", itype, arn)
		}
	}
}

// TestTier0_SweepAppliesCLIIAMPolicyFile covers the other half of #539's
// symptom: a --iam-policy-file was silently discarded on the sweep path too.
// Reusing --iam-role alongside it pins the resulting role name so the profile
// ARN assertion below is unambiguous.
func TestTier0_SweepAppliesCLIIAMPolicyFile(t *testing.T) {
	env := startSpawnSubstrate(t)
	policyPath := writeSweepFile(t, `{
  "Version": "2012-10-17",
  "Statement": [{"Effect": "Allow", "Action": "s3:GetObject", "Resource": "*"}]
}`)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
`)
	env.launchForegroundSweep(file, "--iam-role", "sweep-policy-file-role", "--iam-policy-file", policyPath)

	_, profileARN := env.tagsAndProfileByInstanceType()
	arn, ok := profileARN["c5.large"]
	if !ok {
		t.Fatalf("no instance profile recorded for c5.large; state: %v", profileARN)
	}
	if !strings.Contains(arn, "sweep-policy-file-role") {
		t.Errorf("IAM instance profile ARN %q does not reference --iam-role sweep-policy-file-role", arn)
	}

	// The role must actually carry the custom policy document, not just exist
	// under the right name — otherwise --iam-policy-file parsed and named a role
	// but never attached anything, which is the same silent-discard bug wearing
	// a disguise.
	pol, err := env.IAMClient().GetRolePolicy(context.Background(), &iam.GetRolePolicyInput{
		RoleName:   aws.String("sweep-policy-file-role"),
		PolicyName: aws.String("spawn-custom-policy"),
	})
	if err != nil {
		t.Fatalf("GetRolePolicy(sweep-policy-file-role, spawn-custom-policy): %v", err)
	}
	if pol.PolicyDocument == nil || !strings.Contains(*pol.PolicyDocument, "s3:GetObject") {
		t.Errorf("attached inline policy does not contain the --iam-policy-file document")
	}
}

// TestTier0_SweepRowIAMRoleBeatsCLI pins the precedence: a row's own iam_role:
// (already a supported param-file key, unaffected by #539) still outranks the
// CLI flag, mirroring the rule #525 established for spend controls.
func TestTier0_SweepRowIAMRoleBeatsCLI(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
  - instance_type: c5.xlarge
    iam_role: row-specific-profile
`)
	env.launchForegroundSweep(file, "--iam-role", "sweep-cli-role")

	_, profileARN := env.tagsAndProfileByInstanceType()
	cliARN, ok := profileARN["c5.large"]
	if !ok {
		t.Fatalf("no profile recorded for c5.large")
	}
	if !strings.Contains(cliARN, "sweep-cli-role") {
		t.Errorf("row without its own iam_role: got %q, want the CLI's sweep-cli-role", cliARN)
	}
	rowARN, ok := profileARN["c5.xlarge"]
	if !ok {
		t.Fatalf("no profile recorded for c5.xlarge")
	}
	if !strings.Contains(rowARN, "row-specific-profile") {
		t.Errorf("row with iam_role: row-specific-profile got %q — the row's own value must win over --iam-role", rowARN)
	}
}

// TestTier0_SweepDefaultsToSharedRoleWithoutIAMFlags is the regression guard in
// the other direction: with NO CLI IAM flags and no file-level iam_role:, a
// sweep must still fall back to the shared spored-instance-profile exactly as
// before #539 — this fix must not remove that fallback.
func TestTier0_SweepDefaultsToSharedRoleWithoutIAMFlags(t *testing.T) {
	env := startSpawnSubstrate(t)
	file := writeSweepFile(t, `defaults:
  on_complete: terminate
  ttl: 1h
params:
  - instance_type: c5.large
`)
	env.launchForegroundSweep(file)

	_, profileARN := env.tagsAndProfileByInstanceType()
	arn, ok := profileARN["c5.large"]
	if !ok {
		t.Fatalf("no profile recorded for c5.large")
	}
	if !strings.Contains(arn, "spored-instance-profile") {
		t.Errorf("expected the shared spored-instance-profile with no CLI IAM flags, got %q", arn)
	}
}
