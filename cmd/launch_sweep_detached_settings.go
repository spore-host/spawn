package cmd

import (
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// A detached sweep member's RunInstances-side settings (#1353).
//
// The orchestrator Lambda used to launch a detached member with its instance
// type, AMI, key, profile, user data and tags and nothing else: volume_size,
// --security-group, --subnet, az, spot_max_price, hibernation, --efa,
// --placement-group, --reservation-id/--capacity-block and --nested-virtualization were
// all dropped on the way. The CLI now resolves them per member with
// aws.Client.ResolveLaunchSettings (the lookups Client.Launch makes for the same
// fields) and ships them under detachedLaunchSettingsKey; the Lambda applies
// them with aws.LaunchSettings.Apply, which composes them as Client.Launch does.
//
// What the Lambda cannot do is refused here, before anything is uploaded, by
// detachedSettingsGuard: settings that need the CLI to create or look up
// something at launch, or a grant the orchestrator's role has not got.
// TestDetachedSweepCarriesOrRefusesEveryLaunchSetting holds every LaunchConfig
// field to one or the other.

// detachedLaunchSettingsKey is the param key carrying a member's launch
// settings. It must equal launchSettingsParam in
// lambda/sweep-orchestrator/spored.go; TestDetachedSweepSeedsEveryKeyTheLambdaReads
// holds the two together. Always per row.
const detachedLaunchSettingsKey = "spawn_launch_settings"

// detachedSettingsGuard refuses a member config a detached sweep cannot launch
// as configured. cfg is the member's config after defaultSweepMember.
func detachedSettingsGuard(cfg aws.LaunchConfig) error {
	const noDetach = "Run the sweep with --no-detach, or drop the setting"
	refuse := func(what, why, instead string) error {
		return fmt.Errorf("%s cannot be used on a detached sweep: %s. %s", what, why, instead)
	}
	// A foreground sweep drops these too (#1366), so --no-detach is no remedy.
	const fsxInstead = "Create or recall the filesystem first and pass its --fsx-id " +
		"(a foreground sweep does not create or recall one either, #1366)"
	switch {
	case cfg.FSxLustreCreate:
		return refuse("--fsx-create", "the filesystem is created by the CLI at launch, and the orchestrator "+
			"launches the members later, without it", fsxInstead)
	case cfg.FSxLustreRecall != "":
		return refuse("--fsx-recall", "the filesystem is looked up by the CLI at launch, and the orchestrator "+
			"launches the members later, without it", fsxInstead)
	case cfg.EFAEnabled && cfg.EFAInterfaces > 1:
		return refuse(fmt.Sprintf("--efa-interfaces %d", cfg.EFAInterfaces), "an instance with more than one "+
			"network interface gets no public IP, and the Elastic IP a launch associates (and releases on "+
			"terminate) is not something the orchestrator manages", "Use --efa-interfaces 1, or run the sweep with --no-detach")
	case cfg.EBSKMSKeyID != "":
		return refuse("a customer-managed EBS KMS key", "the orchestrator's role has no grant on the key, so "+
			"EC2 could not create the encrypted volumes", noDetach)
	case cfg.VPCID != "" && cfg.SubnetID == "":
		return refuse("--vpc without --subnet", "RunInstances takes a subnet, not a VPC, and without a subnet "+
			"the member would land in the region's default VPC", "Pass --subnet with a subnet in that VPC "+
			"(a foreground sweep ignores --vpc without one too, #1366)")
	case cfg.HibernationRequested() && strings.ContainsAny(cfg.InstanceType, "|*"):
		return refuse(fmt.Sprintf("hibernation with the instance type pattern %q", cfg.InstanceType),
			"the root volume is sized for the instance type's memory, and the orchestrator picks the type "+
				"from the pattern only at launch", "Name one instance type, or run the sweep with --no-detach")
	}
	return nil
}
