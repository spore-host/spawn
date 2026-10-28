package cmd

import (
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
)

// A nested-spawn role (aws.NestedSpawnRolePrefix) named directly carries the
// same capability --nested-spawn grants: the instance may call RunInstances and
// iam:PassRole. --nested-spawn requires --ttl; naming the role used to skip that
// rule (#1375). These checks hold every path that accepts a profile to it:
//
//   - a single launch (and a job array or MPI cluster, which share its config):
//     --instance-profile naming the role requires --ttl, before any AWS call,
//     and cannot be combined with --no-timeout, exactly as --nested-spawn;
//   - --iam-role naming it is refused outright on every launch path: spawn
//     creates or rewrites the --iam-role role (CreateOrGetInstanceProfile),
//     which would replace the nested role's fixed, audited grants, or create a
//     role under the nested prefix with arbitrary ones;
//   - a batch queue refuses the role outright, as it refuses --nested-spawn;
//   - a foreground sweep (and `spawn resume`) refuses any row whose effective
//     profile is the role and whose effective TTL is empty
//     (refuseNestedSweepRowsWithoutTTL); a detached sweep or schedule refuses
//     the role whatever the TTL (#1372, orchestratorPassableProfile);
//   - `spawn burst --instance-profile` applies the TTL rule
//     (validateBurstParams);
//   - aws.Client.Launch and launcher.Provision apply it to whatever config they
//     are given, the backstop for SDK callers and every command whose profile is
//     one spawn creates (task run, pool, app, image, pipeline).

// validateNestedSpawnRoleFlags refuses the nested-spawn role named by
// --iam-role or --instance-profile where it cannot be used or has no TTL. Called
// by runLaunch before any AWS call. wizard says the TTL may still come from the
// wizard; requireNestedSpawnTTL then checks the resolved config instead.
func validateNestedSpawnRoleFlags(wizard bool) error {
	if aws.IsNestedSpawnRole(iamRole) {
		return aws.NestedSpawnRoleReservedError("--iam-role", iamRole,
			"spawn creates or rewrites the --iam-role role's grants, and a nested-spawn role's are fixed so they can be audited")
	}
	if !aws.IsNestedSpawnRole(instanceProfile) {
		return nil
	}
	switch {
	case batchQueueFile != "" || queueTemplate != "":
		return aws.NestedSpawnRoleReservedError("--instance-profile", instanceProfile,
			"nested spawn is not supported with --batch-queue/--queue-template; launch the head node on its own")
	case noTimeout:
		return fmt.Errorf("--instance-profile %q is a nested-spawn role (%s*) and, like --nested-spawn, cannot be combined with --no-timeout: %w",
			instanceProfile, aws.NestedSpawnRolePrefix, aws.ErrNestedSpawnNeedsTTL)
	case paramFile != "" || params != "":
		// Each row's TTL may come from the param file: refuseNestedSweepRowsWithoutTTL.
		return nil
	case wizard:
		return nil
	}
	return aws.CheckNestedSpawnTTL("--instance-profile", instanceProfile, ttl)
}

// refuseNestedSweepRowsWithoutTTL refuses a sweep any of whose rows would launch
// with a nested-spawn profile and no TTL. cliProfile is --instance-profile.
//
// Resolved per row, with the precedence the launch itself uses:
//
//   - profile: the row's own iam_role:, else --instance-profile (which
//     applyCLIIAMToSweep writes into defaults:), else defaults: iam_role:.
//     --iam-role cannot name a nested role (validateNestedSpawnRoleFlags) and
//     --iam-policy & co. name spawn-instance-<hash>, never one.
//   - TTL: the row's own ttl:, else defaults: ttl:, into which --ttl has been
//     folded (applyCLISpendControlsToSweep) — sweepRowBound, the resolution the
//     --no-detach guard uses. An idle timeout does not count.
//
// launchParameterSweep calls it for a sweep that will run in the foreground,
// before the sweep creates anything (a detached sweep refuses the role outright
// instead); runResume for every resumed row.
func refuseNestedSweepRowsWithoutTTL(pf *ParamFileFormat, cliProfile string) error {
	for i, row := range pf.Params {
		source, profile := fmt.Sprintf("param set %d's iam_role:", i), stringParam(row, "iam_role")
		if profile == "" {
			if cliProfile != "" {
				source, profile = fmt.Sprintf("--instance-profile (param set %d)", i), cliProfile
			} else {
				source, profile = fmt.Sprintf("defaults: iam_role: (param set %d)", i), stringParam(pf.Defaults, "iam_role")
			}
		}
		rowTTL, _ := sweepRowBound(pf.Defaults, row)
		if err := aws.CheckNestedSpawnTTL(source, profile, rowTTL); err != nil {
			return fmt.Errorf("%w; set --ttl, or ttl: under defaults: or on the row", err)
		}
	}
	return nil
}
