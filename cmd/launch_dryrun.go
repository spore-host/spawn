package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
)

// runLaunchDryRun resolves config exactly as launchWithProgress would through
// every step that's cheap/read-only to preview, then prints the result and
// returns WITHOUT calling any AWS mutation (spawn#569).
//
// This is not a second implementation of the launch pipeline: it calls the
// SAME functions launchWithProgress calls for AMI detection, OS/Windows
// guards, MPI/EFA/hibernation pre-flight, and MPI placement-group resolution
// — the identical code paths a real launch runs, all of which are read-only
// (DescribeImages/DescribeInstanceTypes/SSM GetParameter, never
// RunInstances/CreateRole/CreateSecurityGroup/CreateFileSystem/ImportKeyPair).
// It stops BEFORE the first step that would mutate anything: setupSSHKey
// (ImportKeyPair), ensureIAMProfile (CreateRole/CreateInstanceProfile),
// ensureSecurityGroup (CreateSecurityGroup), and FSx create/async-create — for
// those it prints what WOULD happen (the deterministic name/decision) without
// calling them.
//
// config must already have gone through buildLaunchConfig plus every step
// runLaunch applies before the estimateOnly/launchWithProgress branch (tag/
// team merge, name/instance-type validation, region resolution, compliance,
// the zombie-instance TTL default) — i.e. exactly what a real launch would
// have resolved by that same point. The caller (runLaunch) intercepts at
// that point, before launchWithProgress's first AWS call.
func runLaunchDryRun(ctx context.Context, out io.Writer, awsClient *aws.Client, config *aws.LaunchConfig) error {
	// AMI auto-detection, OS resolution, and Windows/nested-virt/MPI/EFA/
	// hibernation pre-flight — read-only (DescribeImages/DescribeInstanceTypes/
	// SSM GetParameter), so it's safe (and useful) to actually resolve here
	// rather than leave "auto" unresolved in the preview.
	if err := ensureAMIAndPreflight(ctx, awsClient, config, progress.NewQuietProgress()); err != nil {
		return err
	}

	// --user-data/--user-data-file: resolve the actual custom content against
	// the local filesystem — the same validation buildUserData does (a bad
	// --user-data-file path fails here exactly as it would on a real launch,
	// the "doubles as a linter" behavior, #569). This does NOT assemble the
	// full spored bootstrap script buildUserData would: that needs a resolved
	// SSH public key, and on a first-ever launch (no local key yet) a real
	// launch CREATES one first (setupSSHKey/EnsureKey) before reading it — a
	// dry-run calling buildUserData directly, without that creation step,
	// would fail here in a case where a real launch would have succeeded, the
	// opposite of "doubles as a linter". The plan instead reports the custom
	// portion's size and notes the rest is not computed here.
	customUserData, err := resolveCustomUserData()
	if err != nil {
		return fmt.Errorf("failed to build user data: %w", err)
	}

	// MPI/job-array flag validation, same checks launchWithProgress runs before
	// it does anything with them — the dry-run bypasses launchWithProgress
	// entirely, so it must run these itself rather than skip them.
	if err := validateMPIFlags(mpiEnabled, count, jobArrayName); err != nil {
		return err
	}
	if err := validateJobArrayFlags(count, jobArrayName); err != nil {
		return err
	}

	// MPI placement-group decision: also read-only (GetCapabilities/
	// DescribeInstanceTypes), and it mutates config (PlacementGroupPrefix,
	// EFAEnabled, spawn:mpi-* tags) the same way a real launch would.
	if mpiEnabled {
		if err := resolveMPIPlacement(ctx, awsClient, config); err != nil {
			return err
		}
	}

	plan := describeLaunchPlan(config)
	plan.CustomUserDataBytes = len(customUserData)

	// Priced by truffle, the suite's pricing authority (#533) — same call
	// --estimate-only and `spawn service --dry-run` use. Read-only (a Pricing
	// API / static-table lookup); best-effort — an unpriced type/region leaves
	// the rate fields empty rather than failing the whole preview.
	if dp, err := resolveLaunchDryRunPrice(ctx, awsClient, config.InstanceType, config.Region); err == nil {
		plan.OnDemandPricePerHour = dp.PricePerHour
		plan.PriceSource = dp.SourceLabel()
		if d, derr := parseDuration(plan.TTL); derr == nil {
			plan.MaxCostUSD = dp.PricePerHour * d.Hours()
		}
	}

	if getOutputFormat() == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}
	renderLaunchPlanTable(out, plan)
	return nil
}

// resolveLaunchDryRunPrice looks up the on-demand rate for the dry-run's
// "Rate:"/"Max cost:" lines. It is a package-level seam (like
// [resolveServiceDryRunPrice] in cmd/service.go and [checkAsync] in
// cmd/root.go) so tests can inject a fake truffle response without a real
// AWS/network call.
//
// The default delegates to [aws.ResolveDisplayPrice] — truffle, the suite's
// pricing authority (#533) — using the already-pinned awsClient's config
// rather than loading a fresh one, since the dry-run already has a
// region-pinned client (unlike cmd/service.go's renderServiceDryRun, which has
// none yet at that point).
var resolveLaunchDryRunPrice = func(ctx context.Context, awsClient *aws.Client, instanceType, region string) (aws.DisplayPrice, error) {
	return aws.ResolveDisplayPrice(ctx, awsClient.Config(), instanceType, region)
}

// launchPlan is the resolved, would-launch shape of a LaunchConfig, rendered
// for `spawn launch --dry-run` / `--print-config` (spawn#569). Field names are
// deliberately snake_case to match every other -o json struct in this repo
// (see cmd/status.go, cmd/task.go, cmd/service.go).
type launchPlan struct {
	Name                        string   `json:"name"`
	InstanceType                string   `json:"instance_type"`
	Region                      string   `json:"region"`
	AvailabilityZone            string   `json:"availability_zone,omitempty"`
	AMI                         string   `json:"ami"`
	TargetOS                    string   `json:"target_os,omitempty"`
	Spot                        bool     `json:"spot"`
	SpotMaxPrice                string   `json:"spot_max_price,omitempty"`
	ReservationID               string   `json:"reservation_id,omitempty"`
	CapacityBlock               bool     `json:"capacity_block,omitempty"`
	TTL                         string   `json:"ttl,omitempty"`
	IdleTimeout                 string   `json:"idle_timeout,omitempty"`
	OnComplete                  string   `json:"on_complete,omitempty"`
	CompletionFile              string   `json:"completion_file,omitempty"`
	PreStop                     string   `json:"pre_stop,omitempty"`
	DNSName                     string   `json:"dns_name,omitempty"`
	KeyName                     string   `json:"key_name,omitempty"`
	IamInstanceProfile          string   `json:"iam_instance_profile"`
	IamInstanceProfileIsPreview bool     `json:"iam_instance_profile_is_preview"`
	SecurityGroupIDs            []string `json:"security_group_ids,omitempty"`
	SecurityGroupPlan           string   `json:"security_group_plan,omitempty"`
	SubnetID                    string   `json:"subnet_id,omitempty"`
	RootVolumeSizeGiB           int32    `json:"root_volume_size_gib,omitempty"`
	PlacementGroup              string   `json:"placement_group,omitempty"`
	PlacementGroupPrefix        string   `json:"placement_group_prefix,omitempty"`
	EFAEnabled                  bool     `json:"efa_enabled,omitempty"`
	NestedVirtualization        bool     `json:"nested_virtualization,omitempty"`
	Hibernate                   bool     `json:"hibernate,omitempty"`
	JobArrayName                string   `json:"job_array_name,omitempty"`
	Count                       int      `json:"count,omitempty"`
	// CustomUserDataBytes is the size of --user-data/--user-data-file's
	// resolved content. It is NOT the full encoded bootstrap script a real
	// launch would send to RunInstances (spored install + storage mounts +
	// this content) — that additionally needs a resolved SSH public key,
	// which --dry-run does not create; see runLaunchDryRun's doc comment.
	CustomUserDataBytes  int               `json:"custom_user_data_bytes,omitempty"`
	Tags                 map[string]string `json:"tags,omitempty"`
	OnDemandPricePerHour float64           `json:"on_demand_price_per_hour,omitempty"`
	PriceSource          string            `json:"price_source,omitempty"`
	MaxCostUSD           float64           `json:"max_cost_usd,omitempty"`
}

// describeLaunchPlan projects a resolved LaunchConfig into the dry-run's
// display shape. Pure — no AWS, no I/O — so it's directly unit-testable.
func describeLaunchPlan(config *aws.LaunchConfig) launchPlan {
	plan := launchPlan{
		Name:                 config.Name,
		InstanceType:         config.InstanceType,
		Region:               config.Region,
		AvailabilityZone:     config.AvailabilityZone,
		AMI:                  orAutoUnresolved(config.AMI),
		TargetOS:             config.TargetOS,
		Spot:                 config.Spot,
		SpotMaxPrice:         config.SpotMaxPrice,
		ReservationID:        config.ReservationID,
		CapacityBlock:        config.CapacityBlock,
		TTL:                  config.TTL,
		IdleTimeout:          config.IdleTimeout,
		OnComplete:           config.OnComplete,
		CompletionFile:       config.CompletionFile,
		PreStop:              config.PreStop,
		DNSName:              config.DNSName,
		KeyName:              config.KeyName,
		SecurityGroupIDs:     config.SecurityGroupIDs,
		SubnetID:             config.SubnetID,
		RootVolumeSizeGiB:    config.RootVolumeSizeGiB,
		PlacementGroup:       config.PlacementGroup,
		PlacementGroupPrefix: config.PlacementGroupPrefix,
		EFAEnabled:           config.EFAEnabled,
		NestedVirtualization: config.NestedVirtualization,
		Hibernate:            config.Hibernate,
		JobArrayName:         jobArrayName,
		Count:                count,
		Tags:                 config.Tags,
	}

	plan.IamInstanceProfile, plan.IamInstanceProfileIsPreview = previewIAMInstanceProfile(config)
	plan.SecurityGroupPlan = previewSecurityGroupPlan(config)
	return plan
}

// orAutoUnresolved reports an unset AMI as an explicit sentinel — the dry-run
// only reaches this path if ensureAMIAndPreflight's AMI auto-detect failed
// non-fatally somewhere upstream (it doesn't: a detect failure returns an
// error), so in practice config.AMI is always resolved by here. Kept as a
// defensive label rather than printing "" if that ever changes.
func orAutoUnresolved(ami string) string {
	if ami == "" {
		return "auto (unresolved)"
	}
	return ami
}

// previewIAMInstanceProfile predicts the IAM instance profile a real launch's
// ensureIAMProfile would resolve to, WITHOUT calling IAM (spawn#569) — mirrors
// ensureIAMProfile's own precedence exactly:
//  1. --instance-profile: the name is already final, no prediction needed.
//  2. --iam-role/--iam-policy/--iam-managed-policies/--iam-policy-file: the
//     deterministic "spawn-instance-<hash>" name aws.PredictedInstanceRoleName
//     computes with no AWS call — UNLESS --iam-role names a specific role, in
//     which case that name IS the answer.
//  3. neither: the shared "spored-instance-profile" default.
//
// The second return value reports whether the shown name is a genuine
// PREDICTION (create-or-reuse not yet confirmed) as opposed to a name already
// known outright (instanceProfile/iamRole/the shared default) — real launch
// output already shows the field this predicts; see spawn#550's
// `config.IamInstanceProfile` print in launchWithProgress.
func previewIAMInstanceProfile(config *aws.LaunchConfig) (name string, isPrediction bool) {
	if config.IamInstanceProfile != "" {
		// Already resolved on the config (e.g. by an SDK caller or an earlier
		// pipeline step) — show it verbatim, no prediction.
		return config.IamInstanceProfile, false
	}
	if instanceProfile != "" {
		return instanceProfile, false
	}
	if iamRole != "" {
		return iamRole, false
	}
	if len(iamPolicy) > 0 || len(iamManagedPolicies) > 0 || iamPolicyFile != "" {
		cfg := aws.IAMRoleConfig{
			Policies:        iamPolicy,
			ManagedPolicies: iamManagedPolicies,
			PolicyFile:      iamPolicyFile,
		}
		return aws.PredictedInstanceRoleName(cfg), true
	}
	return "spored-instance-profile", false
}

// previewSecurityGroupPlan describes what ensureSecurityGroup would do,
// without calling EC2. Mirrors its own branching exactly.
func previewSecurityGroupPlan(config *aws.LaunchConfig) string {
	if mpiEnabled {
		return fmt.Sprintf("create-or-reuse spawn-mpi-%s (MPI inter-node ports)", jobArrayName)
	}
	if config.TargetOS == "windows" && len(config.SecurityGroupIDs) == 0 {
		return fmt.Sprintf("create-or-reuse spawn-windows-%s (RDP 3389 + SSH 22)", config.Name)
	}
	if len(config.SecurityGroupIDs) > 0 {
		return "use the explicitly given security group(s)"
	}
	return "use the account/VPC default security group"
}

// renderLaunchPlanTable prints the plan in the same plain key:value style
// `spawn service --dry-run` / `spawn task run --dry-run` already use in this
// repo (cmd/service.go's renderServiceDryRun, cmd/task.go's renderTaskDryRun)
// — no new templating engine, matching this codebase's existing dry-run
// convention.
func renderLaunchPlanTable(out io.Writer, p launchPlan) {
	fmt.Fprintln(out, "DRY RUN — nothing will be launched.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Name:         %s\n", orDash(p.Name))
	fmt.Fprintf(out, "Instance:     %s in %s\n", orDash(p.InstanceType), orDash(p.Region))
	if p.AvailabilityZone != "" {
		fmt.Fprintf(out, "AZ:           %s\n", p.AvailabilityZone)
	}
	fmt.Fprintf(out, "AMI:          %s", p.AMI)
	if p.TargetOS != "" {
		fmt.Fprintf(out, "  (%s)", p.TargetOS)
	}
	fmt.Fprintln(out)
	if p.Spot {
		label := "spot"
		if p.SpotMaxPrice != "" {
			label += fmt.Sprintf(" (max $%s/hr)", p.SpotMaxPrice)
		}
		fmt.Fprintf(out, "Purchase:     %s\n", label)
	} else if p.CapacityBlock {
		fmt.Fprintf(out, "Purchase:     capacity block %s\n", p.ReservationID)
	} else if p.ReservationID != "" {
		fmt.Fprintf(out, "Purchase:     on-demand into reservation %s\n", p.ReservationID)
	} else {
		fmt.Fprintln(out, "Purchase:     on-demand")
	}
	fmt.Fprintf(out, "Lifetime:     ttl=%s idle=%s on-complete=%s\n", orDash(p.TTL), orDash(p.IdleTimeout), orDash(p.OnComplete))
	if p.OnDemandPricePerHour > 0 {
		fmt.Fprintf(out, "Rate:         $%.4f/hr on-demand (%s)\n", p.OnDemandPricePerHour, p.PriceSource)
		if p.MaxCostUSD > 0 {
			fmt.Fprintf(out, "Max cost:     ~$%.2f (rate × ttl)\n", p.MaxCostUSD)
		}
	}
	if p.PreStop != "" {
		fmt.Fprintf(out, "Pre-stop:     %s\n", p.PreStop)
	}
	if p.DNSName != "" {
		fmt.Fprintf(out, "DNS:          %s\n", p.DNSName)
	}
	if p.KeyName != "" {
		fmt.Fprintf(out, "SSH key:      %s\n", p.KeyName)
	} else {
		fmt.Fprintln(out, "SSH key:      would resolve/create+import spawn's managed keypair (not done in --dry-run)")
	}
	if p.IamInstanceProfileIsPreview {
		fmt.Fprintf(out, "IAM profile:  %s  (predicted; create-or-reuse not yet confirmed)\n", p.IamInstanceProfile)
	} else {
		fmt.Fprintf(out, "IAM profile:  %s\n", p.IamInstanceProfile)
	}
	fmt.Fprintf(out, "Security grp: %s\n", p.SecurityGroupPlan)
	if p.SubnetID != "" {
		fmt.Fprintf(out, "Subnet:       %s\n", p.SubnetID)
	}
	if p.RootVolumeSizeGiB > 0 {
		fmt.Fprintf(out, "Root volume:  %d GiB\n", p.RootVolumeSizeGiB)
	} else {
		fmt.Fprintln(out, "Root volume:  AMI default")
	}
	if p.PlacementGroup != "" {
		fmt.Fprintf(out, "Placement:    %s\n", p.PlacementGroup)
	} else if p.PlacementGroupPrefix != "" {
		fmt.Fprintf(out, "Placement:    auto, per-AZ, prefix %s\n", p.PlacementGroupPrefix)
	}
	if p.EFAEnabled {
		fmt.Fprintln(out, "EFA:          enabled")
	}
	if p.NestedVirtualization {
		fmt.Fprintln(out, "Nested virt:  enabled")
	}
	if p.Hibernate {
		fmt.Fprintln(out, "Hibernate:    enabled")
	}
	if p.Count > 1 {
		fmt.Fprintf(out, "Job array:    %s × %d instances\n", orDash(p.JobArrayName), p.Count)
	}
	if p.CustomUserDataBytes > 0 {
		fmt.Fprintf(out, "User data:    %d bytes of --user-data/--user-data-file content (plus spored's own bootstrap script, not sized here — needs a resolved SSH key)\n", p.CustomUserDataBytes)
	} else {
		fmt.Fprintln(out, "User data:    spored's own bootstrap script only (no --user-data/--user-data-file given; not sized here — needs a resolved SSH key)")
	}
	if len(p.Tags) > 0 {
		fmt.Fprintln(out, "Tags:")
		keys := make([]string, 0, len(p.Tags))
		for k := range p.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(out, "  %s=%s\n", k, p.Tags[k])
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Re-run without --dry-run (or --print-config) to launch.")
}
