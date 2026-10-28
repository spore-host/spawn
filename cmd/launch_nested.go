package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// Nested spawn (#39). Two halves:
//
//   - the workstation side, `spawn launch --nested-spawn`, which creates or
//     reuses the opt-in role (aws.Client.SetupNestedSpawnProfile);
//   - the spore side: a spawn CLI running AS that role. It must not write IAM —
//     the role deliberately has no IAM write grant — so it picks a profile that
//     already exists instead of calling SetupSporedIAMRole or
//     CreateOrGetInstanceProfile (nestedChildProfile).
//
// The default launch path is untouched: with no --nested-spawn and a caller that
// is not a nested role, ensureNestedSpawnProfile reports "not handled".

// nestedCallerARN is the caller's STS identity ARN. A seam for tests.
var nestedCallerARN = func(ctx context.Context, c *aws.Client) (string, error) {
	_, arn, err := c.GetCallerIdentityInfo(ctx)
	return arn, err
}

// setupNestedSpawnProfile creates or reuses the --nested-spawn role. A seam
// for tests.
var setupNestedSpawnProfile = func(ctx context.Context, c *aws.Client) (string, error) {
	return c.SetupNestedSpawnProfile(ctx)
}

// validateNestedSpawnFlags refuses --nested-spawn combinations that cannot work,
// before any AWS call.
func validateNestedSpawnFlags() error {
	if !nestedSpawn {
		return nil
	}
	switch {
	case batchQueueFile != "" || queueTemplate != "":
		return fmt.Errorf("--nested-spawn is not supported with --batch-queue/--queue-template; launch the head node on its own")
	case paramFile != "" || params != "":
		return fmt.Errorf("--nested-spawn is not supported for parameter sweeps (--param-file/--params); launch the head node on its own")
	case instanceProfile != "":
		return fmt.Errorf("--nested-spawn and --instance-profile both choose the instance's IAM profile; pass one")
	case cliIAMFlagsRequireCustomProfile():
		return fmt.Errorf("--nested-spawn cannot be combined with --iam-role/--iam-policy/--iam-managed-policies/--iam-policy-file/--s3-read/--s3-write: the nested role's grants are fixed so they can be audited")
	case noTimeout:
		return fmt.Errorf("--nested-spawn cannot be combined with --no-timeout: an instance that can launch other instances must itself end")
	}
	return nil
}

// requireNestedSpawnTTL: an instance that can launch others must have a hard
// lifetime, not only an idle timeout (an idle timeout stops rather than
// terminates by default, and a head node busy dispatching may never be idle).
// Checked against the resolved config, so a TTL from config defaults counts.
//
// --instance-profile naming a nested-spawn role is held to the same rule
// (#1375); validateNestedSpawnRoleFlags checks it earlier unless the TTL may
// come from the wizard, which only this check sees.
func requireNestedSpawnTTL(config *aws.LaunchConfig) error {
	if nestedSpawn && strings.TrimSpace(config.TTL) == "" {
		return aws.NestedSpawnTTLError()
	}
	if err := aws.CheckNestedSpawnTTL("--instance-profile", instanceProfile, config.TTL); err != nil {
		return err
	}
	return config.ValidateNestedSpawnTTL()
}

// ensureNestedSpawnProfile sets config.IamInstanceProfile for the two nested
// cases and reports whether it handled the launch. false, nil means neither
// applies and the caller's default resolution runs unchanged.
func ensureNestedSpawnProfile(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig) (bool, error) {
	callerRole := ""
	if arn, err := nestedCallerARN(ctx, awsClient); err == nil {
		callerRole, _ = aws.NestedSpawnRoleFromCallerARN(arn)
	}
	if callerRole != "" {
		profile, err := nestedChildProfile(callerRole, nestedSpawn, cliIAMFlagsRequireCustomProfile(), config.DNSFunctionARN != "", noTimeout)
		if err != nil {
			return true, err
		}
		config.IamInstanceProfile = profile
		fmt.Fprintf(os.Stderr, "ℹ️  Running as nested-spawn role %s: using existing instance profile %s (a nested spore makes no IAM changes)\n", callerRole, profile)
		return true, nil
	}
	if !nestedSpawn {
		return false, nil
	}
	profile, err := setupNestedSpawnProfile(ctx, awsClient)
	if err != nil {
		return true, fmt.Errorf("set up the --nested-spawn role: %w", err)
	}
	if err := ensureDNSInvokeGrant(ctx, awsClient, config, profile); err != nil {
		return true, err
	}
	config.IamInstanceProfile = profile
	return true, nil
}

// nestedChildProfile is the profile a child launched FROM a nested spore gets.
// It never needs an IAM write: the parent launch made sure
// spored-instance-profile exists, and a nested child reuses the parent's own
// role (a profile is named after its role).
func nestedChildProfile(callerRole string, wantNested, customIAM, customDNS, noTimeoutSet bool) (string, error) {
	switch {
	case customIAM:
		return "", fmt.Errorf("this instance runs as the nested-spawn role %s, which cannot create IAM roles (a spore that could would be able to grant itself anything); "+
			"launch without --iam-*/--s3-* flags to use %s, or pass --instance-profile with an existing spawn-instance-* profile", callerRole, aws.SporedInstanceProfileName)
	case customDNS:
		return "", fmt.Errorf("this instance runs as the nested-spawn role %s, which cannot grant a child access to a custom DNS updater; launch the child without a custom DNS updater", callerRole)
	case noTimeoutSet:
		return "", fmt.Errorf("a nested spore cannot launch with --no-timeout: every child must end; set --ttl or --idle-timeout")
	case wantNested:
		return callerRole, nil
	}
	return aws.SporedInstanceProfileName, nil
}
