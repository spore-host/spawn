package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"

	"github.com/spore-host/spawn/pkg/aws"
)

// What each `spawn launch` flag does on a parameter sweep (#1366, #697).
//
// A sweep member starts from the config a single launch would build
// (sweepBaseConfig) and is launched by aws.Client.Launch (foreground) or the
// orchestrator (detached, #1353). Anything a single launch does around that
// call, in runLaunch or launchWithProgress, a sweep does not do unless it is
// wired in: --fsx-create, --fsx-recall and --vpc without --subnet were accepted
// on a sweep and dropped, with nothing said (#1366), and so were the storage
// mounts, the Strata environment, the custom DNS target, compliance mode and
// the job-array and MPI flags.
//
// Every launch flag has a rule here, and TestEveryLaunchFlagHasASweepRule
// discovers the set from the cobra command, so a new flag fails CI until
// someone decides. A flag either reaches the members, steers the sweep or the
// CLI itself, or is refused by sweepUnsupportedFlags before any AWS call.
type sweepFlagRule struct {
	kind sweepFlagKind
	// why says how the flag reaches the members, or why it can't (the
	// user-facing reason, with the remedy, for a refused flag).
	why string
	// unlessSet: a refused flag is accepted when any of these is also set
	// (--vpc with --subnet: the subnet names the VPC).
	unlessSet []string
}

type sweepFlagKind int

const (
	// sweepViaConfig: set on the config every member starts from
	// (sweepBaseConfig), and every field it sets is carried to the member
	// (foregroundDispositions, detachedDispositions).
	sweepViaConfig sweepFlagKind = iota
	// sweepViaUserData: read by the user-data builder each member's bootstrap
	// is rendered with (buildUserDataForKey, launchStorageScript).
	sweepViaUserData
	// sweepSteers: read by the sweep path itself (orchestration, IAM, regions).
	sweepSteers
	// sweepCLIOnly: acts on the CLI or the dispatch, not on a member.
	sweepCLIOnly
	// sweepRefused: a sweep cannot honour it; sweepUnsupportedFlags refuses it.
	sweepRefused
)

func viaConfig(why string) sweepFlagRule   { return sweepFlagRule{kind: sweepViaConfig, why: why} }
func viaUserData(why string) sweepFlagRule { return sweepFlagRule{kind: sweepViaUserData, why: why} }
func steers(why string) sweepFlagRule      { return sweepFlagRule{kind: sweepSteers, why: why} }
func cliOnly(why string) sweepFlagRule     { return sweepFlagRule{kind: sweepCLIOnly, why: why} }
func refusedOnSweep(why string) sweepFlagRule {
	return sweepFlagRule{kind: sweepRefused, why: why}
}

const (
	whySweepConfig  = "set on the config every member starts from"
	whySweepTag     = whySweepConfig + "; delivered as a tag spored reads"
	whySweepEC2     = whySweepConfig + "; an EC2 launch parameter"
	whySweepBoot    = "read by the user-data builder every member's bootstrap is rendered with"
	whySweepStorage = "mounted by the storage script in every member's user data, as on a single launch (#1366)"
	whySweepIAM     = "resolved once for the sweep into its instance profile (applyCLIIAMToSweep, #539)"
	whySweepOrch    = "steers the sweep's orchestration"
	whyShape        = "belongs to another launch shape: a sweep launches one instance per row. " +
		"Drop it, or launch the job array or MPI cluster on its own"

	// sweepFSxInstead is the remedy for the FSx settings a sweep cannot honour.
	// Each member mounts an existing filesystem passed with --fsx-id, on either
	// orchestration path (#1366).
	sweepFSxInstead = "Create or recall the filesystem first (a single `spawn launch` with --fsx-create " +
		"--fsx-lifecycle durable --fsx-ttl, or with --fsx-recall, leaves one; `spawn fsx list` shows its ID) " +
		"and pass its --fsx-id: every member mounts it"
	sweepFSxCreateWhy = "a sweep does not create a filesystem; the create is done only by a single launch. " + sweepFSxInstead
	sweepFSxRecallWhy = "a sweep does not recall a filesystem; the recall is done only by a single launch. " + sweepFSxInstead
	sweepFSxOnlyWhy   = "read only by --fsx-create, which a sweep refuses. " + sweepFSxInstead
	sweepVPCWhy       = "RunInstances takes a subnet, not a VPC, and without a subnet every member would land " +
		"in the region's default VPC. Pass --subnet with a subnet in that VPC"
)

// sweepLaunchFlags is the per-flag decision. Keyed by flag name, including
// hidden aliases and the persistent root flags a launch inherits.
var sweepLaunchFlags = map[string]sweepFlagRule{
	// Instance shape, network and purchase.
	"instance-type":         viaConfig(whySweepEC2 + "; a row's instance_type: wins"),
	"region":                viaConfig(whySweepEC2 + "; a row's region: wins"),
	"az":                    viaConfig(whySweepEC2 + "; a row's az: wins"),
	"ami":                   viaConfig(whySweepEC2 + "; a row's ami: wins"),
	"volume-size":           viaConfig(whySweepEC2 + "; a row's volume_size: wins (#544)"),
	"key-name":              viaConfig(whySweepEC2 + "; a row's key_name: wins"),
	"key-pair":              viaConfig(whySweepEC2 + "; a row's key_name: wins"),
	"security-group":        viaConfig(whySweepEC2),
	"security-group-ids":    viaConfig(whySweepEC2),
	"subnet":                viaConfig(whySweepEC2),
	"subnet-id":             viaConfig(whySweepEC2),
	"efa":                   viaConfig(whySweepEC2),
	"efa-interfaces":        viaConfig(whySweepEC2 + " (a detached sweep refuses more than one, #1353)"),
	"placement-group":       viaConfig(whySweepEC2),
	"spot":                  viaConfig(whySweepEC2 + "; a row's spot: wins"),
	"spot-max-price":        viaConfig(whySweepEC2 + "; a row's spot_max_price: wins (#1366)"),
	"reservation-id":        viaConfig(whySweepEC2),
	"capacity-block":        viaConfig(whySweepEC2),
	"hibernate":             viaConfig(whySweepEC2 + "; a row's hibernate: wins"),
	"nested-virtualization": viaConfig(whySweepEC2),
	"attach-volume":         viaConfig(whySweepEC2 + " (the volumes); " + whySweepStorage),
	"tag":                   viaConfig(whySweepEC2 + " (instance and volume tags, #1356)"),
	"team":                  viaConfig(whySweepEC2 + " (team tags, #1356)"),
	"name":                  viaConfig(whySweepConfig + " (the Name tag; a row's name: wins)"),
	"vpc":                   {kind: sweepRefused, why: sweepVPCWhy, unlessSet: []string{"subnet", "subnet-id"}},

	// Lifecycle and notifications: tags spored reads.
	"ttl":                    viaConfig(whySweepTag + "; a row's ttl: wins (#525)"),
	"idle-timeout":           viaConfig(whySweepTag + "; a row's idle_timeout: wins (#525)"),
	"on-idle":                viaConfig(whySweepTag),
	"hibernate-on-idle":      viaConfig(whySweepTag),
	"pre-stop":               viaConfig(whySweepTag),
	"pre-stop-timeout":       viaConfig(whySweepTag),
	"spot-webhook-url":       viaConfig(whySweepTag),
	"completion-webhook-url": viaConfig(whySweepTag),
	"webhook-correlation":    viaConfig(whySweepTag),
	"webhook-timeout":        viaConfig(whySweepTag),
	"on-complete":            viaConfig(whySweepTag + "; a row's on_complete: wins"),
	"completion-file":        viaConfig(whySweepTag),
	"completion-delay":       viaConfig(whySweepTag),
	"session-timeout":        viaConfig(whySweepTag),
	"cost-limit":             viaConfig(whySweepTag + "; a row's cost_limit: wins (#525)"),
	"command":                viaConfig(whySweepTag + "; a row's command: wins"),
	"active-ports":           viaConfig(whySweepTag),
	"active-processes":       viaConfig(whySweepTag),
	"idle-cpu-threshold":     viaConfig(whySweepTag + " (#1308)"),
	"metrics":                viaConfig(whySweepTag + " (#1308)"),
	"metrics-port":           viaConfig(whySweepTag + " (#1308)"),
	"metrics-bind":           viaConfig(whySweepTag + " (#1308)"),
	"metrics-path":           viaConfig(whySweepTag + " (#1308)"),
	"tracing":                viaConfig(whySweepTag + " (#1308)"),
	"tracing-exporter":       viaConfig(whySweepTag + " (#1308)"),
	"tracing-sampling":       viaConfig(whySweepTag + " (#1308)"),
	"slack-workspace":        viaConfig(whySweepTag),
	"notify-platform":        viaConfig(whySweepTag),
	"dns":                    viaConfig(whySweepTag + "; a row's dns: wins"),
	"no-dns":                 viaConfig(whySweepConfig + ": withholds the spawn:dns-name tag (#872)"),

	// The workload and the bootstrap.
	"user-data":      viaUserData(whySweepBoot + "; a row's user_data: runs after it (#1327)"),
	"user-data-file": viaUserData(whySweepBoot),
	"plugin":         viaUserData(whySweepBoot),
	"config":         viaUserData(whySweepBoot + " (its plugin declarations)"),

	// Storage: an existing filesystem is mounted on every member.
	"efs-id":            viaConfig(whySweepStorage),
	"efs-mount-point":   viaConfig(whySweepStorage),
	"efs-mount-options": viaUserData(whySweepStorage),
	"efs-profile":       viaUserData(whySweepStorage),
	"fsx-id":            viaConfig(whySweepStorage),
	"fsx-mount-point":   viaConfig(whySweepStorage),
	"fsx-skip-validate": viaUserData("skips the --fsx-id lookup, and with it the mount, as on a single launch"),
	// A filesystem made by the launch is not: the create and the recall live in
	// launchWithProgress, which a sweep never reaches (#1366).
	"fsx-create":           refusedOnSweep(sweepFSxCreateWhy),
	"fsx-recall":           refusedOnSweep(sweepFSxRecallWhy),
	"fsx-lifecycle":        refusedOnSweep(sweepFSxOnlyWhy),
	"fsx-ttl":              refusedOnSweep(sweepFSxOnlyWhy),
	"fsx-storage-capacity": refusedOnSweep(sweepFSxOnlyWhy),
	"fsx-throughput":       refusedOnSweep(sweepFSxOnlyWhy),
	"fsx-s3-bucket":        refusedOnSweep(sweepFSxOnlyWhy),
	"fsx-import-path":      refusedOnSweep(sweepFSxOnlyWhy),
	"fsx-export-path":      refusedOnSweep(sweepFSxOnlyWhy),

	// Done by a single launch around RunInstances, and not by a sweep.
	"allow-cidr": refusedOnSweep("it sets the source of the managed Windows security group a single launch " +
		"creates, and a sweep creates none. Pass --security-group with a group that allows it"),
	"efa-group": refusedOnSweep("the shared EFA security group is created and attached only by a single launch. " +
		"Pass --security-group with the group's ID"),
	"strata-formation": refusedOnSweep("the Strata lockfile is resolved and uploaded only by a single launch, " +
		"so no member would get its environment. Launch the instances on their own"),
	"strata-profile": refusedOnSweep("the Strata lockfile is resolved and uploaded only by a single launch, " +
		"so no member would get its environment. Launch the instances on their own"),
	"strata-registry": refusedOnSweep("the Strata lockfile is resolved and uploaded only by a single launch, " +
		"so no member would get its environment. Launch the instances on their own"),
	"dns-domain": refusedOnSweep("a custom DNS target is resolved, and the instance role granted on its " +
		"updater, only by a single launch, so a member's dns: name would register with spore.host's"),
	"dns-api-endpoint": refusedOnSweep("a custom DNS target is resolved, and the instance role granted on its " +
		"updater, only by a single launch, so a member's dns: name would register with spore.host's"),
	"dns-function-arn": refusedOnSweep("a custom DNS target is resolved, and the instance role granted on its " +
		"updater, only by a single launch, so a member's dns: name would register with spore.host's"),
	"nist-800-171": refusedOnSweep("compliance mode is validated and enforced (encrypted EBS, IMDSv2) only " +
		"on a single launch, so the members would launch without it"),
	"nist-800-53": refusedOnSweep("compliance mode is validated and enforced (encrypted EBS, IMDSv2) only " +
		"on a single launch, so the members would launch without it"),
	"compliance-strict": refusedOnSweep("compliance mode is validated and enforced (encrypted EBS, IMDSv2) only " +
		"on a single launch, so the members would launch without it"),
	"interactive": refusedOnSweep("the wizard configures one instance, and a sweep never runs it. " +
		"Put the settings in flags or the param file"),

	// Other launch shapes.
	"count":                  refusedOnSweep("--count launches a job array, and a sweep never reads it. " + whyShape),
	"job-array-name":         refusedOnSweep(whyShape),
	"instance-names":         refusedOnSweep(whyShape),
	"min-viable":             refusedOnSweep(whyShape),
	"mpi":                    refusedOnSweep(whyShape),
	"mpi-command":            refusedOnSweep(whyShape),
	"mpi-processes-per-node": refusedOnSweep(whyShape),
	"skip-mpi-install":       refusedOnSweep(whyShape),
	"auto-placement-group":   refusedOnSweep("it acts only with --mpi. " + whyShape),
	"template-var":           refusedOnSweep("it substitutes into --queue-template, which a sweep is not"),

	// The sweep's own controls.
	"param-file":                steers(whySweepOrch),
	"params":                    steers(whySweepOrch),
	"sweep-name":                steers(whySweepOrch),
	"detach":                    steers(whySweepOrch),
	"no-detach":                 steers(whySweepOrch),
	"max-concurrent":            steers(whySweepOrch),
	"max-concurrent-auto":       steers(whySweepOrch),
	"max-concurrent-per-region": steers(whySweepOrch),
	"launch-delay":              steers(whySweepOrch),
	"budget":                    steers(whySweepOrch),
	"mode":                      steers(whySweepOrch + " (multi-region distribution)"),
	"regions-include":           steers(whySweepOrch + " (multi-region distribution)"),
	"regions-exclude":           steers(whySweepOrch + " (multi-region distribution)"),
	"regions-geographic":        steers(whySweepOrch + " (multi-region distribution)"),
	"estimate-only":             steers("prices the sweep and launches nothing (#524)"),
	"no-timeout":                steers("lifts the sweep's 1h idle-timeout default"),
	"os":                        steers("the members' target OS"),
	"output-id":                 steers("writes the sweep ID"),
	"wait":                      steers("waits for a detached sweep"),
	"wait-timeout":              steers("bounds --wait"),
	"wait-for-ssh":              steers("read by the foreground sweep's fleet check (#1305)"),
	"iam-role":                  steers(whySweepIAM),
	"iam-policy":                steers(whySweepIAM),
	"iam-policy-file":           steers(whySweepIAM),
	"iam-managed-policies":      steers(whySweepIAM),
	"iam-trust-services":        steers(whySweepIAM),
	"iam-role-tags":             steers(whySweepIAM),
	"iam-allow-full-access":     steers(whySweepIAM),
	"instance-profile":          steers(whySweepIAM),
	"s3-read":                   steers(whySweepIAM),
	"s3-write":                  steers(whySweepIAM),

	// The CLI and the dispatch.
	"keep-on-error":            cliOnly("the foreground sweep's post-launch error policy (#1305)"),
	"terminate-on-error":       cliOnly("the foreground sweep's post-launch error policy (#1305)"),
	"yes":                      cliOnly("skips confirmation prompts"),
	"quiet":                    cliOnly("output verbosity"),
	"wait-for-running":         cliOnly("a foreground sweep waits for each member's spored regardless (#1305), and a detached one returns at once"),
	"skip-region-check":        cliOnly("skips a single launch's data-locality prompt; a sweep prompts for none"),
	"allow-cost-limit-overrun": cliOnly("relaxes only the --fsx-create storage check (#616), and a sweep refuses --fsx-create"),
	"dry-run":                  cliOnly("refused for a sweep by the dispatch (#569); --estimate-only previews one"),
	"print-config":             cliOnly("refused for a sweep by the dispatch (#569); --estimate-only previews one"),
	"nested-spawn":             cliOnly("refused for a sweep by validateNestedSpawnFlags"),
	"batch-queue":              cliOnly("a batch-queue launch is dispatched before a sweep, so it is never one"),
	"queue-template":           cliOnly("a batch-queue launch is dispatched before a sweep, so it is never one"),
	"use-reservation":          cliOnly("deprecated and inert on every launch; cobra already warns"),
	"cartesian":                cliOnly("deprecated and inert (#674); cobra warns"),

	// Persistent root flags.
	"accessibility": cliOnly("CLI output"),
	"account":       cliOnly("the CLI's AWS account"),
	"lang":          cliOnly("CLI output"),
	"no-color":      cliOnly("CLI output"),
	"no-emoji":      cliOnly("CLI output"),
	"output":        cliOnly("CLI output"),
	"profile":       cliOnly("the CLI's AWS profile"),
	"verbose":       cliOnly("CLI output"),
}

// isSweepLaunch reports whether runLaunch dispatches to the parameter sweep:
// --param-file/--params, and no batch queue, which is dispatched first.
func isSweepLaunch() bool {
	return (paramFile != "" || params != "") && batchQueueFile == "" && queueTemplate == ""
}

// sweepUnsupportedFlags refuses a parameter sweep that sets a flag a sweep
// cannot honour (#1366), naming every such flag at once. Called by runLaunch
// before any AWS call, so nothing is created or looked up for a sweep that
// would launch its members without the setting.
func sweepUnsupportedFlags(fs *pflag.FlagSet) error {
	if fs == nil {
		return nil
	}
	var refused []string
	fs.VisitAll(func(f *pflag.Flag) { // VisitAll is sorted, so the message is stable
		if !f.Changed {
			return
		}
		rule, ok := sweepLaunchFlags[f.Name]
		if !ok || rule.kind != sweepRefused {
			return
		}
		for _, other := range rule.unlessSet {
			if o := fs.Lookup(other); o != nil && o.Changed {
				return
			}
		}
		refused = append(refused, fmt.Sprintf("  --%s: %s", f.Name, rule.why))
	})
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("refusing a parameter sweep: %d flag(s) cannot be honoured by a sweep, "+
		"which would launch its members without them (#1366):\n%s",
		len(refused), strings.Join(refused, "\n"))
}

// sweepMemberGuard refuses a member config neither orchestration path can
// launch as configured (#1366): the backstop under sweepUnsupportedFlags, at
// the point where each path builds its members, for a caller that did not come
// through runLaunch. cfg is the member's config after defaultSweepMember.
func sweepMemberGuard(cfg aws.LaunchConfig) error {
	refuse := func(what, why string) error {
		return fmt.Errorf("%s cannot be used on a parameter sweep: %s", what, why)
	}
	switch {
	case cfg.FSxLustreCreate:
		return refuse("--fsx-create", sweepFSxCreateWhy)
	case cfg.FSxLustreRecall != "":
		return refuse("--fsx-recall", sweepFSxRecallWhy)
	case cfg.VPCID != "" && cfg.SubnetID == "":
		return refuse("--vpc without --subnet", sweepVPCWhy)
	}
	return nil
}
