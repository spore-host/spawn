package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
)

// windowsFlagRule records what a `spawn launch` flag does on a Windows instance.
//
// Every launch flag has one (TestEveryLaunchFlagHasAWindowsRule discovers the
// set from the cobra command, so a new flag fails CI until someone decides).
// A flag is either honoured on Windows, or refused when it is set for a
// Windows launch. There is no third state: a flag that is accepted and then
// dropped is how #1022 happened. --command and --user-data were silently
// ignored, so the workload never ran, --on-complete never fired, and the
// instance billed until its TTL.
type windowsFlagRule struct {
	// rejected: the launch is refused when this flag is set and the target is
	// Windows. why is then the user-facing reason.
	rejected bool
	// why says what the flag does on Windows (honoured) or why it can't
	// (rejected). Required either way, so every entry records a decision.
	why string
}

func honouredOnWindows(why string) windowsFlagRule { return windowsFlagRule{why: why} }
func rejectedOnWindows(why string) windowsFlagRule {
	return windowsFlagRule{rejected: true, why: why}
}

const (
	whyCLISide      = "read by the CLI on the launching machine; OS-independent"
	whyEC2Param     = "an EC2 API parameter or resource; OS-independent"
	whySporedTag    = "delivered as a tag that spored reads, and spored runs on Windows as a service"
	whyIAM          = "shapes the instance's IAM role; OS-independent"
	whyNoLinuxBoot  = "only the Linux bootstrap reads it; the Windows bootstrap has no equivalent"
	whyStorage      = "storage is mounted by the Linux bootstrap's mount script, which is not emitted for Windows"
	whyMPI          = "MPI setup is a bash script appended to the Linux bootstrap"
	whySweepOnly    = "has an effect only on a parameter sweep, and --params/--param-file are refused on Windows"
	whyQueueOnly    = "has an effect only with --queue-template, which is refused on Windows"
	whyDeprecatedNo = "deprecated and inert on every OS; cobra already warns"
)

// windowsLaunchFlags is the per-flag decision. Keyed by flag name, including
// hidden aliases and the persistent root flags a launch inherits.
var windowsLaunchFlags = map[string]windowsFlagRule{
	// Workload delivery: the reason this table exists (#1022).
	"command": rejectedOnWindows("the Windows bootstrap does not run a command, and nothing on Windows reads " +
		"the spawn:command tag, so the workload would never start and --on-complete would never fire. " +
		"Install the workload in a custom AMI, or run it over `spawn connect`"),
	"user-data": rejectedOnWindows("custom user data is spliced into the Linux bash bootstrap; " +
		"the Windows bootstrap is a fixed <powershell> block and would drop it"),
	"user-data-file": rejectedOnWindows("custom user data is spliced into the Linux bash bootstrap; " +
		"the Windows bootstrap is a fixed <powershell> block and would drop it"),
	"plugin":          rejectedOnWindows("plugin declarations are written to /etc/spawn/plugins.json by the Linux bootstrap only"),
	"config":          rejectedOnWindows("a launch config file carries plugin declarations, which only the Linux bootstrap installs"),
	"session-timeout": rejectedOnWindows("idle-shell logout is a /etc/profile.d script the Linux bootstrap writes"),
	"params":          rejectedOnWindows("a sweep delivers each row's command and PARAM_* values through tags that " + whyNoLinuxBoot),
	"param-file":      rejectedOnWindows("a sweep delivers each row's command and PARAM_* values through tags that " + whyNoLinuxBoot),
	"batch-queue":     rejectedOnWindows("batch-queue jobs run from a bash runner in the Linux bootstrap"),
	"queue-template":  rejectedOnWindows("batch-queue jobs run from a bash runner in the Linux bootstrap"),
	"strata-formation": rejectedOnWindows("strata environments are Linux (al2023) images mounted by strata-agent, " +
		"which the Windows bootstrap does not install"),
	"strata-profile": rejectedOnWindows("strata environments are Linux (al2023) images mounted by strata-agent, " +
		"which the Windows bootstrap does not install"),
	"strata-registry": rejectedOnWindows("strata environments are Linux (al2023) images mounted by strata-agent, " +
		"which the Windows bootstrap does not install"),

	// Storage: the mount script is skipped for Windows (launchWithProgress), so
	// each of these used to create or attach storage that was never mounted.
	"attach-volume":        rejectedOnWindows("the volume would be attached but never formatted or mounted: " + whyStorage),
	"efs-id":               rejectedOnWindows(whyStorage),
	"efs-mount-point":      rejectedOnWindows(whyStorage),
	"efs-profile":          rejectedOnWindows(whyStorage),
	"efs-mount-options":    rejectedOnWindows(whyStorage),
	"fsx-create":           rejectedOnWindows("FSx for Lustre has no Windows client; the filesystem would be created, billed and never mounted"),
	"fsx-id":               rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-lifecycle":        rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-ttl":              rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-skip-validate":    rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-recall":           rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-import-path":      rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-export-path":      rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-mount-point":      rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-s3-bucket":        rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-storage-capacity": rejectedOnWindows("FSx for Lustre has no Windows client"),
	"fsx-throughput":       rejectedOnWindows("FSx for Lustre has no Windows client"),

	// MPI.
	"mpi":                    rejectedOnWindows(whyMPI),
	"mpi-processes-per-node": rejectedOnWindows(whyMPI),
	"mpi-command":            rejectedOnWindows(whyMPI),
	"skip-mpi-install":       rejectedOnWindows(whyMPI),
	"placement-group":        honouredOnWindows(whyEC2Param),
	"auto-placement-group":   honouredOnWindows("acts only with --mpi, which is refused on Windows"),
	"efa":                    honouredOnWindows(whyEC2Param + " (an EFA network interface)"),
	"efa-interfaces":         honouredOnWindows(whyEC2Param + " (one EFA network interface per card)"),
	"efa-secondary-type":     honouredOnWindows(whyEC2Param + " (the interface type on cards after card 0); Windows configures the extra interfaces itself, though AWS documents that a multi-interface Windows instance needs static routing"),
	"efa-group":              honouredOnWindows(whyEC2Param + " (a security group shared by EFA instances)"),

	// --no-public-ip (#1433): the Windows bootstrap downloads the AWS CLI
	// installer from the internet, so its subnet check requires a NAT route.
	"no-public-ip":            honouredOnWindows(whyEC2Param + " (no public IP on the interface); the subnet check requires a NAT route on Windows, whose bootstrap downloads the AWS CLI installer. RDP: `spawn connect --rdp --via-ssm`"),
	"allow-missing-endpoints": honouredOnWindows("read by --no-public-ip's subnet check, the same on Windows"),

	// Instance shape, network and purchase: EC2 parameters.
	"instance-type":         honouredOnWindows(whyEC2Param),
	"region":                honouredOnWindows(whyEC2Param),
	"az":                    honouredOnWindows(whyEC2Param),
	"ami":                   honouredOnWindows(whyEC2Param),
	"os":                    honouredOnWindows("selects the Windows bootstrap itself"),
	"nested-virtualization": honouredOnWindows(whyEC2Param),
	"allow-cidr":            honouredOnWindows("sets the source of the managed Windows security group's RDP and SSH rules"),
	"vpc":                   honouredOnWindows(whyEC2Param),
	"subnet-id":             honouredOnWindows(whyEC2Param),
	"subnet":                honouredOnWindows(whyEC2Param),
	"security-group-ids":    honouredOnWindows(whyEC2Param),
	"security-group":        honouredOnWindows(whyEC2Param),
	"key-name":              honouredOnWindows("its public key is written to administrators_authorized_keys by the Windows bootstrap"),
	"key-pair":              honouredOnWindows("its public key is written to administrators_authorized_keys by the Windows bootstrap"),
	"spot":                  honouredOnWindows(whyEC2Param),
	"spot-max-price":        honouredOnWindows(whyEC2Param),
	"use-reservation":       honouredOnWindows(whyDeprecatedNo),
	"reservation-id":        honouredOnWindows(whyEC2Param),
	"capacity-block":        honouredOnWindows(whyEC2Param),
	"hibernate":             honouredOnWindows(whyEC2Param),
	"volume-size":           honouredOnWindows(whyEC2Param),
	"tag":                   honouredOnWindows(whyEC2Param),
	"team":                  honouredOnWindows(whyEC2Param + " (team tags)"),
	"name":                  honouredOnWindows(whyEC2Param + " (the Name tag)"),

	// Lifecycle: spored enforces these on Windows (#77).
	"ttl":                    honouredOnWindows(whySporedTag),
	"idle-timeout":           honouredOnWindows(whySporedTag),
	"no-timeout":             honouredOnWindows(whyCLISide),
	"on-idle":                honouredOnWindows(whySporedTag),
	"hibernate-on-idle":      honouredOnWindows(whySporedTag),
	"pre-stop":               honouredOnWindows(whySporedTag + "; the hook runs through PowerShell"),
	"pre-stop-timeout":       honouredOnWindows(whySporedTag),
	"spot-webhook-url":       honouredOnWindows(whySporedTag),
	"completion-webhook-url": honouredOnWindows(whySporedTag),
	"webhook-correlation":    honouredOnWindows(whySporedTag),
	"webhook-timeout":        honouredOnWindows(whySporedTag),
	"on-complete":            honouredOnWindows(whySporedTag + "; fires when the completion file appears"),
	"completion-file":        honouredOnWindows(whySporedTag),
	"completion-delay":       honouredOnWindows(whySporedTag),
	"cost-limit":             honouredOnWindows(whySporedTag),
	"active-ports":           honouredOnWindows(whySporedTag + "; Windows has its own port probe"),
	"active-processes":       honouredOnWindows(whySporedTag + "; Windows has its own process probe"),
	"idle-cpu-threshold":     honouredOnWindows(whySporedTag + "; Windows has its own CPU probe"),
	"metrics":                honouredOnWindows(whySporedTag + "; the Windows service runs the same metrics server"),
	"metrics-port":           honouredOnWindows(whySporedTag),
	"metrics-bind":           honouredOnWindows(whySporedTag),
	"metrics-path":           honouredOnWindows(whySporedTag),
	"tracing":                honouredOnWindows(whySporedTag),
	"tracing-exporter":       honouredOnWindows(whySporedTag),
	"tracing-sampling":       honouredOnWindows(whySporedTag),
	"slack-workspace":        honouredOnWindows(whySporedTag),
	"notify-platform":        honouredOnWindows(whySporedTag),
	"dns":                    honouredOnWindows(whySporedTag),
	"no-dns":                 honouredOnWindows(whySporedTag),
	"dns-domain":             honouredOnWindows(whySporedTag),
	"dns-api-endpoint":       honouredOnWindows(whySporedTag),
	"dns-function-arn":       honouredOnWindows(whyIAM),

	// Job arrays: plain arrays launch N Windows instances. The array-specific
	// parts that need the bootstrap (MPI, storage) are refused above.
	"count":          honouredOnWindows(whyEC2Param),
	"job-array-name": honouredOnWindows(whySporedTag),
	"instance-names": honouredOnWindows(whyEC2Param + " (Name tags)"),
	"min-viable":     honouredOnWindows(whyCLISide),

	// Sweep-only and queue-only modifiers.
	"cartesian":                 honouredOnWindows(whyDeprecatedNo),
	"max-concurrent":            honouredOnWindows(whySweepOnly),
	"max-concurrent-auto":       honouredOnWindows(whySweepOnly),
	"max-concurrent-per-region": honouredOnWindows(whySweepOnly),
	"launch-delay":              honouredOnWindows(whySweepOnly),
	"detach":                    honouredOnWindows(whySweepOnly),
	"no-detach":                 honouredOnWindows(whySweepOnly),
	"sweep-name":                honouredOnWindows(whySweepOnly),
	"budget":                    honouredOnWindows(whySweepOnly),
	"mode":                      honouredOnWindows(whyCLISide),
	"regions-include":           honouredOnWindows(whyCLISide),
	"regions-exclude":           honouredOnWindows(whyCLISide),
	"regions-geographic":        honouredOnWindows(whyCLISide),
	"template-var":              honouredOnWindows(whyQueueOnly),

	// IAM.
	"iam-role":              honouredOnWindows(whyIAM),
	"iam-policy":            honouredOnWindows(whyIAM),
	"iam-allow-full-access": honouredOnWindows(whyIAM),
	"iam-managed-policies":  honouredOnWindows(whyIAM),
	"iam-policy-file":       honouredOnWindows(whyIAM),
	"iam-trust-services":    honouredOnWindows(whyIAM),
	"iam-role-tags":         honouredOnWindows(whyIAM),
	"instance-profile":      honouredOnWindows(whyIAM),
	"nested-spawn":          honouredOnWindows(whyIAM),
	"s3-read":               honouredOnWindows(whyIAM),
	"s3-write":              honouredOnWindows(whyIAM),

	// CLI behaviour.
	"allow-cost-limit-overrun": honouredOnWindows(whyCLISide),
	"estimate-only":            honouredOnWindows(whyCLISide),
	"yes":                      honouredOnWindows(whyCLISide),
	"interactive":              honouredOnWindows(whyCLISide),
	"quiet":                    honouredOnWindows(whyCLISide),
	"wait-for-running":         honouredOnWindows(whyCLISide),
	"wait-for-ssh":             honouredOnWindows("waits for the Windows Administrator password instead of SSH"),
	"skip-region-check":        honouredOnWindows(whyCLISide),
	"terminate-on-error":       honouredOnWindows(whyCLISide),
	"keep-on-error":            honouredOnWindows(whyCLISide),
	"dry-run":                  honouredOnWindows(whyCLISide),
	"print-config":             honouredOnWindows(whyCLISide),
	"output-id":                honouredOnWindows(whyCLISide),
	"wait":                     honouredOnWindows(whyCLISide),
	"wait-timeout":             honouredOnWindows(whyCLISide),
	"nist-800-171":             honouredOnWindows(whyCLISide + " (compliance validation of the launch config)"),
	"nist-800-53":              honouredOnWindows(whyCLISide + " (compliance validation of the launch config)"),
	"compliance-strict":        honouredOnWindows(whyCLISide + " (compliance validation of the launch config)"),

	// Persistent root flags.
	"accessibility": honouredOnWindows(whyCLISide),
	"account":       honouredOnWindows(whyCLISide),
	"lang":          honouredOnWindows(whyCLISide),
	"no-color":      honouredOnWindows(whyCLISide),
	"no-emoji":      honouredOnWindows(whyCLISide),
	"output":        honouredOnWindows(whyCLISide),
	"profile":       honouredOnWindows(whyCLISide),
	"verbose":       honouredOnWindows(whyCLISide),
}

// launchFlags is launch's flag set, read by windowsLifecycleGuard. Assigned in
// init rather than read from launchCmd directly: launchCmd's RunE reaches the
// guard, so naming launchCmd there is an initialization cycle.
var launchFlags *pflag.FlagSet

func init() { launchFlags = launchCmd.Flags() }

// windowsUnsupportedFlags refuses a Windows launch that sets a flag Windows
// can't honour (#1022), naming every such flag at once. A Linux target, or an
// empty one, passes: the caller decides the OS.
func windowsUnsupportedFlags(targetOS string, fs *pflag.FlagSet) error {
	if !isWindowsTarget(targetOS) || fs == nil {
		return nil
	}
	var refused []string
	fs.VisitAll(func(f *pflag.Flag) { // VisitAll is sorted, so the message is stable
		if !f.Changed {
			return
		}
		if rule, ok := windowsLaunchFlags[f.Name]; ok && rule.rejected {
			refused = append(refused, fmt.Sprintf("  --%s: %s", f.Name, rule.why))
		}
	})
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("refusing a Windows launch: %d flag(s) would be silently ignored on Windows, "+
		"so the instance would start without them (#1022):\n%s",
		len(refused), strings.Join(refused, "\n"))
}
