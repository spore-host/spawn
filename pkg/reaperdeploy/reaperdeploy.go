// Package reaperdeploy stands up the ttl-reaper Lambda inside the caller's OWN
// account, with no cross-account trust anywhere (spawn#625).
//
// # Why this exists
//
// The reaper normally lives in spore.host's infra account and reaches into each
// spore-launching account by assuming a role there. That requires the launching
// account to trust an external principal, which any organization with a policy
// against external-account trust forbids outright — attempting it in one produced an
// automated security finding within 12 minutes and the org's tooling flipped the
// role's trust policy to Deny. Least privilege did not help: the objection was the
// external principal itself, not what it could do.
//
// So for a governed account the options were "grant a trust your org forbids",
// "hand-deploy a Lambda from a template in the spawn repo", or "have no backstop".
// In practice: no backstop. A spawn:managed instance sat stopped for 13 days past its
// TTL in exactly such an account, still billing EBS, because spored cannot act once
// an instance is stopped and nothing else was watching.
//
// This deploys the same reaper in REAPER_SCAN_SELF mode: it scans the account it runs
// in, assumes nothing, and trusts nobody.
package reaperdeploy

import (
	"fmt"
	"strings"
)

const (
	// FunctionName MUST keep the "spawn-ttl-reaper" prefix: that is what
	// aws.DetectReaperCoverage looks for (#624), so a deploy here is what makes
	// `spawn doctor` flip from "not covered" to "covered". The coupling is the
	// point — deploy and detect close the loop — and a test pins it.
	FunctionName = "spawn-ttl-reaper-selfscan"

	// RoleName is the function's execution role. Named, not generated, so a
	// re-deploy converges instead of accumulating roles (the #550 complaint about
	// spawn-instance-<hash> roles never being collected).
	RoleName = "spawn-ttl-reaper-selfscan-role"

	// RuleName is the EventBridge rule that invokes it on a schedule. An
	// EventBridge *rule* rather than EventBridge Scheduler deliberately: a rule
	// needs no separate IAM invoke role, which is one fewer principal for a
	// lagotto#170-style authorization bug to hide in.
	RuleName = "spawn-ttl-reaper-selfscan-schedule"

	// PolicyName is the inline policy carrying pkg/reaperiam's statements.
	PolicyName = "spawn-ttl-reaper-selfscan-policy"

	// DefaultSchedule matches the production reaper's cadence.
	DefaultSchedule = "rate(10 minutes)"

	// artifactAsset is the release asset published by the GoReleaser before-hook.
	artifactAsset = "ttl-reaper_lambda_linux_arm64.zip"
	releaseRepo   = "spore-host/spawn"
)

// ArtifactURL returns the GitHub Release download URL for the reaper Lambda zip at
// the given version. Pure, so it is unit-testable without network.
func ArtifactURL(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	return fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", releaseRepo, v, artifactAsset)
}

// DefaultBucketName derives the per-account artifact bucket when the caller supplies
// none. Account and region keep it unique and make it obvious where it came from.
func DefaultBucketName(accountID, region string) string {
	return fmt.Sprintf("spawn-reaper-artifacts-%s-%s", accountID, region)
}

// ObjectKey is the S3 key the zip is uploaded under for a version. Versioned, so a
// re-deploy at a different version does not overwrite the artifact a currently
// deployed function was created from.
func ObjectKey(version string) string {
	return fmt.Sprintf("ttl-reaper/%s.zip", "v"+strings.TrimPrefix(strings.TrimSpace(version), "v"))
}

// EnvVars are the reaper's environment for a scan-self deployment.
//
// dryRun is a parameter rather than a constant because arming is a separate,
// explicit step: a deploy lands with dry-run ON so the first scheduled runs log what
// they WOULD terminate and touch nothing. The production reaper was itself rolled out
// that way. A tool whose job is terminating instances should not arrive armed from a
// single command against an account whose tagging it has never been seen to evaluate.
//
// REAPER_ROLE_ARNS is deliberately absent, not empty-string: resolveAccounts treats
// its absence plus REAPER_SCAN_SELF=true as "scan only this account", which is the
// whole shape of this deployment.
func EnvVars(regions string, dryRun bool) map[string]string {
	env := map[string]string{
		"REAPER_SCAN_SELF": "true",
		"REAPER_DRY_RUN":   fmt.Sprintf("%t", dryRun),
	}
	if strings.TrimSpace(regions) != "" {
		env["REAPER_REGIONS"] = regions
	}
	return env
}

// Tags mark what spawn created, so a teardown (and a human) can tell these resources
// apart from anything else in the account.
func Tags(version string) map[string]string {
	return map[string]string{
		"spawn:managed":    "true",
		"spawn:created-by": "spawn reaper deploy",
		"spawn:component":  "ttl-reaper",
		"spawn:version":    strings.TrimPrefix(strings.TrimSpace(version), "v"),
	}
}

// AssumeRolePolicy is the trust policy for the execution role: Lambda only.
//
// Nothing else is trusted, and in particular no external account is — that is the
// entire reason this deployment shape exists.
const AssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "lambda.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}`

// ArmWarning is shown before arming. It names the consequence in the user's terms
// rather than describing the mechanism, because the consequence is irreversible.
func ArmWarning(accountID, region string) string {
	return fmt.Sprintf(
		"This arms the reaper in account %s (%s).\n"+
			"From the next scheduled run it will TERMINATE any instance tagged\n"+
			"spawn:managed=true whose spawn:ttl-deadline has passed, and DELETE any\n"+
			"orphaned spawn-managed FSx filesystem. Termination is not reversible.\n"+
			"\n"+
			"Read a dry-run cycle first if you haven't:\n"+
			"  aws logs tail /aws/lambda/%s --follow",
		accountID, region, FunctionName)
}
