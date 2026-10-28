package cmd

import (
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/instancerole"
)

// Which instance profiles a detached sweep's members may use (#1331).
//
// The foreground path launches with the user's own credentials, so any profile
// the user may pass works. A detached sweep's members are launched by the
// sweep-orchestrator Lambda through SpawnSweepCrossAccountRole in the member
// account, and RunInstances with a profile needs iam:PassRole on the profile's
// role. That role may pass exactly the roles spawn itself creates
// (scripts/setup-sweep-cross-account-role.sh), and only to EC2:
//
//   - spored-instance-role, in spored-instance-profile, the default
//     (SetupSporedIAMRole);
//   - spawn-instance-<hash>, the per-policy roles --iam-policy, --iam-policy-file,
//     --iam-managed-policies and --s3-read/--s3-write create
//     (CreateOrGetInstanceProfile, which names the profile after its role);
//   - spawnd-role, the profile setup-spawnd-iam-role.sh makes.
//
// Any other profile (--instance-profile NAME, --iam-role NAME, a row's own
// iam_role:) cannot be covered by a scoped grant: a PassRole broad enough to
// pass an arbitrary role lets the orchestrator hand any role in the account,
// admin included, to an instance. Such a sweep is refused before anything is
// created, rather than uploaded to fail every member at RunInstances with an
// authorization error. --no-detach runs it with the user's own credentials.
//
// The nested-spawn roles (aws.NestedSpawnRolePrefix, spawn-instance-nested-*)
// are inside the spawn-instance-* family but are never passable (#1372). Such a
// role may itself call RunInstances and iam:PassRole, and `spawn launch
// --nested-spawn` gives it only to an instance with a --ttl. A member naming one
// would get that capability from the orchestrator, on the say-so of whoever
// wrote the params, with no such requirement. The cross-account role denies the
// PassRole explicitly, and the sweep orchestrator refuses the member too.
//
// TestDetachedPassableProfilesMatchTheGrant holds this list to the script.

// orchestratorPassableProfiles are the profiles named in full; the prefix below
// covers the rest.
var orchestratorPassableProfiles = []string{aws.SporedInstanceProfileName, "spawnd-role"}

// orchestratorPassableProfile reports whether the orchestrator may launch a
// member with profile. "" is the default, spored-instance-profile.
func orchestratorPassableProfile(profile string) bool {
	if aws.IsNestedSpawnRole(profile) {
		return false
	}
	if profile == "" || strings.HasPrefix(profile, instancerole.NamePrefix) {
		return true
	}
	for _, p := range orchestratorPassableProfiles {
		if profile == p {
			return true
		}
	}
	return false
}

// unpassableProfileError is the refusal for a profile the orchestrator cannot
// pass. source says where it came from.
func unpassableProfileError(source, profile string) error {
	if aws.IsNestedSpawnRole(profile) {
		return fmt.Errorf("%s names instance profile %q, a nested-spawn role (%s*), which a detached sweep cannot use: "+
			"nested spawn isn't available for detached sweeps (#1372). Use --nested-spawn on a single launch, which "+
			"requires --ttl", source, profile, aws.NestedSpawnRolePrefix)
	}
	return fmt.Errorf("%s names instance profile %q, which a detached sweep cannot use: its members are launched "+
		"by spawn's sweep orchestrator, which may pass only the roles spawn creates (%s and %s*) to EC2, never an "+
		"arbitrary role (#1331). Use --iam-policy/--iam-policy-file/--iam-managed-policies/--s3-read/--s3-write "+
		"without --iam-role, so spawn creates a %s* role with those grants, or run the sweep with --no-detach, "+
		"which launches with your own credentials", source, profile,
		strings.Join(orchestratorPassableProfiles, ", "), instancerole.NamePrefix, instancerole.NamePrefix)
}

// refuseUnpassableDetachedProfiles refuses a detached sweep, before any IAM or
// EC2 resource is created, when the CLI IAM flags or any row would give a member
// a profile the orchestrator cannot pass.
func refuseUnpassableDetachedProfiles(pf *ParamFileFormat) error {
	if instanceProfile != "" && !orchestratorPassableProfile(instanceProfile) {
		return unpassableProfileError("--instance-profile", instanceProfile)
	}
	// --iam-role NAME is the name of the role (and profile) spawn creates; with
	// no name spawn picks spawn-instance-<hash>.
	if instanceProfile == "" && cliIAMFlagsRequireCustomProfile() && iamRole != "" && !orchestratorPassableProfile(iamRole) {
		return unpassableProfileError("--iam-role", iamRole)
	}
	cliSetsProfile := instanceProfile != "" || cliIAMFlagsRequireCustomProfile()
	if p := stringParam(pf.Defaults, "iam_role"); !cliSetsProfile && !orchestratorPassableProfile(p) {
		return unpassableProfileError("defaults: iam_role", p)
	}
	for i, row := range pf.Params {
		if p := stringParam(row, "iam_role"); !orchestratorPassableProfile(p) {
			return unpassableProfileError(fmt.Sprintf("param set %d's iam_role:", i), p)
		}
	}
	return nil
}
