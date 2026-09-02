//go:build e2e_tier0

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// Tier 0 regression coverage for #550: two `spawn launch` invocations with
// IDENTICAL launch configuration must resolve to the SAME IAM instance
// profile. The bug this guards against is not that profile resolution is
// wrong in any single call — generateRoleName/hashPolicies are deterministic
// given a config, and SetupSporedIAMRole always names the same shared
// profile — it's that TWO DIFFERENT resolution paths exist
// (CreateOrGetInstanceProfile vs. SetupSporedIAMRole, see the comments on both
// in pkg/aws/iam.go), and which one a given launch takes depends entirely on
// whether IAM flags were passed to THAT invocation. A caller who believes two
// invocations are "the same kind of launch" can silently land on different
// profiles with different (and possibly bucket-inaccessible) grants.
//
// This test also covers the highest-value, most tractable fix from #550:
// --instance-profile, which bypasses resolution entirely and is verified here
// to (a) attach verbatim and (b) win over --iam-role/--iam-policy when both
// are given.

// instanceProfileARN returns the sole running instance's IAM instance-profile
// ARN. Fails if there isn't exactly one instance — these tests each launch
// exactly one, so ambiguity means the test itself is wrong.
func (e *spawnEnv) instanceProfileARN() string {
	e.t.Helper()
	out, err := e.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		e.t.Fatalf("DescribeInstances: %v", err)
	}
	var arns []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil {
				arns = append(arns, *inst.IamInstanceProfile.Arn)
			}
		}
	}
	if len(arns) != 1 {
		e.t.Fatalf("expected exactly 1 instance with an IAM instance profile, got %d: %v", len(arns), arns)
	}
	return arns[0]
}

// TestTier0_IAMProfileResolution_ConsistentAcrossIdenticalLaunches is the core
// #550 regression: launching twice with the SAME flags (no IAM flags at all,
// the exact shape of the original bug report) must resolve to the SAME
// profile both times — never a fresh spawn-instance-<hash> once and the
// shared spored-instance-profile the next, or vice versa.
func TestTier0_IAMProfileResolution_ConsistentAcrossIdenticalLaunches(t *testing.T) {
	env := startSpawnSubstrate(t)

	env.launchOK("consistent-a", "--instance-type", "t3.small")
	firstARN := env.instanceProfileARN()

	env.launchOK("consistent-b", "--instance-type", "t3.small")

	// Now two instances exist; re-derive by filtering DescribeInstances for the
	// second instance specifically (instanceProfileARN's "exactly 1" invariant
	// no longer holds with two instances up).
	out, err := env.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	var secondARN string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			for _, tg := range inst.Tags {
				if tg.Key != nil && *tg.Key == "Name" && tg.Value != nil && *tg.Value == "consistent-b" {
					if inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil {
						secondARN = *inst.IamInstanceProfile.Arn
					}
				}
			}
		}
	}
	if secondARN == "" {
		t.Fatalf("could not find consistent-b's IAM instance profile ARN")
	}
	if firstARN != secondARN {
		t.Errorf("two identical launches (no IAM flags) resolved to DIFFERENT instance profiles: %q vs %q — "+
			"this is the #550 symptom: profile resolution must be consistent given identical inputs", firstARN, secondARN)
	}
	// Both should be the shared default — this pins WHICH profile, not just
	// that they match each other (a test that only compared the two could pass
	// vacuously if both were wrong in the same way).
	if !strings.Contains(firstARN, "spored-instance-profile") {
		t.Errorf("expected the shared spored-instance-profile with no IAM flags, got %q", firstARN)
	}
}

// TestTier0_IAMProfileResolution_SameIAMFlagsConsistent covers the other half:
// two launches with the SAME --iam-policy-file (the path that goes through
// CreateOrGetInstanceProfile, hashed by policy content) must also converge to
// the same profile, not just default-vs-default above.
func TestTier0_IAMProfileResolution_SameIAMFlagsConsistent(t *testing.T) {
	env := startSpawnSubstrate(t)
	policyPath := writeSweepFile(t, `{
  "Version": "2012-10-17",
  "Statement": [{"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::campaign-bucket/*"}]
}`)

	env.launchOK("hashed-a", "--instance-type", "t3.small", "--iam-policy-file", policyPath)
	firstARN := env.instanceProfileARN()

	env2 := startSpawnSubstrate(t) // fresh account/emulator: isolates from the first launch's cached role
	env2.launchOK("hashed-b", "--instance-type", "t3.small", "--iam-policy-file", policyPath)
	secondARN := env2.instanceProfileARN()

	// Compare only the profile NAME (the part after the last "/"), since the two
	// envs are different emulator instances and could differ in the account-id
	// portion of the ARN.
	firstName := firstARN[strings.LastIndex(firstARN, "/")+1:]
	secondName := secondARN[strings.LastIndex(secondARN, "/")+1:]
	if firstName != secondName {
		t.Errorf("two launches with the SAME --iam-policy-file resolved to different profile names: %q vs %q — "+
			"generateRoleName/hashPolicies must be deterministic for identical policy content", firstName, secondName)
	}
	if strings.Contains(firstName, "spored-instance-profile") {
		t.Errorf("a launch WITH --iam-policy-file must not fall back to the shared spored-instance-profile, got %q", firstName)
	}
}

// TestTier0_InstanceProfileFlag_AttachesVerbatim verifies --instance-profile
// attaches the named profile with NO resolution at all — not
// spored-instance-profile, not a spawn-instance-<hash> name.
func TestTier0_InstanceProfileFlag_AttachesVerbatim(t *testing.T) {
	env := startSpawnSubstrate(t)

	// Pre-create the target instance profile via IAM directly, matching what an
	// operator who already has a bucket-scoped profile would do.
	env.runOK("launch", "seed-role-holder", "--instance-type", "t3.small",
		"--iam-role", "my-preexisting-profile", "--region", "us-east-1", "-y", "-o", "json",
		"--wait-for-running=false", "--wait-for-ssh=false")

	env.launchOK("bypass-test", "--instance-type", "t3.small", "--instance-profile", "my-preexisting-profile")

	out, err := env.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		t.Fatalf("DescribeInstances: %v", err)
	}
	var arn string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			for _, tg := range inst.Tags {
				if tg.Key != nil && *tg.Key == "Name" && tg.Value != nil && *tg.Value == "bypass-test" {
					if inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil {
						arn = *inst.IamInstanceProfile.Arn
					}
				}
			}
		}
	}
	if arn == "" {
		t.Fatalf("bypass-test instance has no IAM instance profile at all")
	}
	if !strings.Contains(arn, "my-preexisting-profile") {
		t.Errorf("--instance-profile my-preexisting-profile did not attach verbatim, got %q", arn)
	}
}

// TestTier0_InstanceProfileFlag_BeatsIAMRoleFlags verifies --instance-profile
// takes precedence over --iam-role/--iam-policy when both are given on the
// same invocation — the explicit, deterministic choice must never be
// second-guessed by the resolution heuristic those other flags would trigger.
func TestTier0_InstanceProfileFlag_BeatsIAMRoleFlags(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.launchOK("precedence-test", "--instance-type", "t3.small",
		"--instance-profile", "explicit-wins",
		"--iam-role", "should-be-ignored")

	arn := env.instanceProfileARN()
	if !strings.Contains(arn, "explicit-wins") {
		t.Errorf("--instance-profile should win over --iam-role, got %q", arn)
	}
	if strings.Contains(arn, "should-be-ignored") {
		t.Errorf("--iam-role must be ignored when --instance-profile is set, got %q", arn)
	}
}
