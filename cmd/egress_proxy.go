package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
)

// `spawn egress-proxy status | delete | upgrade` (#1515 phase 5): the
// commands for the egress proxies --bootstrap-egress proxy launches create,
// one per VPC. The work is pkg/egressproxy/deploy's (Inventory, PlanDelete and
// Delete, PlanUpgrade and Upgrade); this file is flags, prompts and output.
//
// None of them writes IAM: the proxy's instance profile is named, not set up,
// except by upgrade's find-or-create, which sets it up exactly as a launch
// does (and names it without setup on a nested spore).

var (
	egressProxyStatusAllRegions bool
	egressProxyStatusVPC        string

	egressProxyDeleteVPC    string
	egressProxyDeleteAll    bool
	egressProxyDeleteYes    bool
	egressProxyDeleteDryRun bool

	egressProxyUpgradeVPC     string
	egressProxyUpgradeYes     bool
	egressProxyUpgradeKeepOld bool
)

var egressProxyCmd = &cobra.Command{
	Use:   "egress-proxy",
	Short: "Show, delete or upgrade the spawn egress proxies (--bootstrap-egress proxy)",
	Long: `A --no-public-ip launch with --bootstrap-egress proxy, in a VPC with no NAT,
fetches spawn's own bootstrap downloads through a spawn egress proxy: one
t4g.micro per VPC running 'spored egress-proxy', with two security groups,
spawn-egress-proxy-<vpc> and spawn-egress-clients-<vpc>. The launch finds or
creates it. A proxy lives at most 6 days and ends sooner when unused for its
idle period (72h by default); the TTL reaper is the backstop for both, and its
managed-network sweep deletes the groups once nothing uses them.

These commands show, delete and upgrade them. The region is the global
--region, else your configured region.`,
}

var egressProxyStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the egress proxies and their security groups",
	Long: `Show every instance tagged spawn:egress-proxy in the region (or in --vpc):
its state, address, the spawn release that launched it, its deadline, the
idle activity it published, and
whether the next --bootstrap-egress proxy launch would reuse it (and if not,
why). For each VPC, also the two security groups and how many network
interfaces use the clients group.

-o json prints the same as JSON, with stable snake_case keys.

Read-only: ec2:DescribeInstances and ec2:DescribeSecurityGroups.`,
	Args: cobra.NoArgs,
	RunE: runEgressProxyStatus,
}

var egressProxyDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Terminate a VPC's egress proxy and delete its security groups",
	Long: `Terminate the egress proxy instances of a VPC (--vpc) or of every VPC in
the region (--all), wait until they are terminated, then delete the two
security groups: the proxy's group first, then the clients group.

Only instances spawn can tell it launched as the VPC's proxy are terminated:
tagged spawn:egress-proxy=<vpc>, carrying exactly the proxy group, running with
spored-instance-profile, and with a deadline no later than launch plus the
proxy's life. A tagged instance that fails these checks is refused, left
alone, and named with the reason; the command then exits non-zero.

A clients group that member instances still carry is kept, and its users are
named; the TTL reaper's managed-network sweep deletes it once nothing uses it.
Members that have finished their bootstrap do not use the proxy, so deleting it
does not affect them.`,
	Args: cobra.NoArgs,
	RunE: runEgressProxyDelete,
}

var egressProxyUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Replace a VPC's egress proxy with one at this CLI's version",
	Long: `Launch a fresh egress proxy at this CLI's version in the VPC named by
--vpc, through the same find-or-create a launch uses, wait until it is ready,
then terminate the old one (unless --keep-old, which leaves it to expire on
its own). The new proxy
keeps the idle period the old one published, else 72h.

If the VPC's proxy is already at this CLI's version with more than 12 hours left, it
is reported as current and nothing changes. A VPC with no proxy is left
without one: the next --bootstrap-egress proxy launch creates it.

A member that is mid-bootstrap through the old proxy when it is terminated can
fail its download; members that have finished their bootstrap do not use it.`,
	Args: cobra.NoArgs,
	RunE: runEgressProxyUpgrade,
}

func init() {
	rootCmd.AddCommand(egressProxyCmd)
	egressProxyCmd.AddCommand(egressProxyStatusCmd, egressProxyDeleteCmd, egressProxyUpgradeCmd)

	f := egressProxyStatusCmd.Flags()
	f.BoolVar(&egressProxyStatusAllRegions, "all-regions", false, "Show every region enabled in this account")
	f.StringVar(&egressProxyStatusVPC, "vpc", "", "Show only this VPC (a vpc- ID)")

	f = egressProxyDeleteCmd.Flags()
	f.StringVar(&egressProxyDeleteVPC, "vpc", "", "The VPC whose proxy and groups to delete (a vpc- ID)")
	f.BoolVar(&egressProxyDeleteAll, "all", false, "Delete the proxies and groups of every VPC in the region")
	f.BoolVarP(&egressProxyDeleteYes, "yes", "y", false, "Do not ask for confirmation")
	f.BoolVar(&egressProxyDeleteDryRun, "dry-run", false, "Print what would be terminated and deleted, and change nothing")

	f = egressProxyUpgradeCmd.Flags()
	f.StringVar(&egressProxyUpgradeVPC, "vpc", "", "The VPC whose proxy to upgrade (a vpc- ID; required)")
	f.BoolVarP(&egressProxyUpgradeYes, "yes", "y", false, "Do not ask for confirmation")
	f.BoolVar(&egressProxyUpgradeKeepOld, "keep-old", false, "Leave the old proxy running until it expires on its own")
}

// egressProxyEnv is what the commands reach AWS through; a seam so tests need
// no AWS.
type egressProxyEnv struct {
	// regions resolves the global --region (or every enabled region).
	regions func(ctx context.Context, all bool) ([]string, error)
	// client is the regional find-or-create client (EC2, SSM, profile, AMI,
	// tags).
	client func(region string) deploy.Client
	// network reads the VPC's public subnets, from a subnet in it.
	network func(ctx context.Context, region, subnetID string) (aws.PrivateNetwork, error)
	// audit records each termination and group deletion.
	audit *audit.AuditLogger
	now   func() time.Time
}

// newEgressProxyEnv builds the real environment.
var newEgressProxyEnv = func(ctx context.Context) (*egressProxyEnv, error) {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	userID, err := client.GetAccountID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get caller identity: %w", err)
	}
	return &egressProxyEnv{
		regions: func(ctx context.Context, all bool) ([]string, error) {
			return resolveCleanupRegions(ctx, client, "", all)
		},
		client: func(region string) deploy.Client { return egressProxyClient(client, region) },
		network: func(ctx context.Context, region, subnetID string) (aws.PrivateNetwork, error) {
			return client.DescribePrivateNetwork(ctx, region, subnetID)
		},
		audit: newAuditLogger(userID, uuid.New().String()),
		now:   time.Now,
	}, nil
}

// The deploy calls; seams so tests need no AWS.
var (
	egressProxyInventory = deploy.Inventory
	egressProxyDelete    = deploy.Delete
	egressProxyUpgrade   = deploy.Upgrade
)

// egressProxyConfirm asks before delete and upgrade change anything; a seam
// for tests.
var egressProxyConfirm = confirmYes

// egressProxyWant is what a proxy is judged against: this CLI's version and
// the spored profile, named rather than set up.
func egressProxyWant(now time.Time) deploy.Want {
	return deploy.Want{Version: version(), InstanceProfile: aws.SporedInstanceProfileName, Now: now}
}

// errEgressProxyRefused is wrapped when a command refused a tagged instance.
var errEgressProxyRefused = errors.New("refused to touch an instance tagged as an egress proxy")

func validateVPCFlag(name, v string) error {
	if v != "" && !strings.HasPrefix(v, "vpc-") {
		return fmt.Errorf("--%s %q is not a VPC ID (vpc-...)", name, v)
	}
	return nil
}

// ---- status ---------------------------------------------------------------

// egressProxyStatusReport is status's output; -o json prints it as is, so its
// keys are a contract: snake_case, and only ever added to.
type egressProxyStatusReport struct {
	CLIVersion  string                   `json:"cli_version"`
	GeneratedAt time.Time                `json:"generated_at"`
	Regions     []string                 `json:"regions"`
	VPCs        []egressProxyVPCStatus   `json:"vpcs"`
	Errors      []egressProxyRegionError `json:"errors"`
}

type egressProxyRegionError struct {
	Region string `json:"region"`
	Error  string `json:"error"`
}

type egressProxyVPCStatus struct {
	Region       string                     `json:"region"`
	VPCID        string                     `json:"vpc_id"`
	ProxyGroup   egressProxyGroupStatus     `json:"proxy_group"`
	ClientsGroup egressProxyGroupStatus     `json:"clients_group"`
	Proxies      []egressProxyInstanceState `json:"proxies"`
}

type egressProxyGroupStatus struct {
	Name   string `json:"name"`
	ID     string `json:"id,omitempty"`
	Exists bool   `json:"exists"`
	// NotSpawns is why a group of spawn's name is not spawn's.
	NotSpawns string   `json:"not_spawns,omitempty"`
	ENICount  int      `json:"eni_count"`
	Instances []string `json:"instances"`
}

type egressProxyInstanceState struct {
	InstanceID     string     `json:"instance_id"`
	State          string     `json:"state"`
	PrivateIP      string     `json:"private_ip,omitempty"`
	Port           int        `json:"port"`
	PublicIP       string     `json:"public_ip,omitempty"`
	Version        string     `json:"version,omitempty"`
	VersionMatches bool       `json:"version_matches"`
	Launched       *time.Time `json:"launch_time"`
	Deadline       *time.Time `json:"deadline"`
	// TimeLeftSeconds is until the deadline; negative once it has passed.
	TimeLeftSeconds *int64 `json:"time_left_seconds"`
	// Reusable is whether the next --bootstrap-egress proxy launch would
	// reuse it; NotReusedReason says why not.
	Reusable        bool   `json:"reusable"`
	NotReusedReason string `json:"not_reused_reason,omitempty"`
	// SpawnsProxy is whether spawn can tell it launched it as the VPC's
	// proxy (delete and upgrade terminate only these); NotSpawnsReason says
	// why not.
	SpawnsProxy     bool                `json:"spawns_proxy"`
	NotSpawnsReason string              `json:"not_spawns_reason,omitempty"`
	Activity        egressProxyActivity `json:"activity"`
}

type egressProxyActivity struct {
	// Known is whether the proxy published its activity tags.
	Known             bool       `json:"known"`
	LastActivity      *time.Time `json:"last_activity"`
	IdleSeconds       *int64     `json:"idle_seconds"`
	IdlePeriodSeconds *int64     `json:"idle_period_seconds"`
	IdleExpiresAt     *time.Time `json:"idle_expires_at"`
	// ExpiresAt is the earlier of the idle expiry and the deadline.
	ExpiresAt *time.Time `json:"expires_at"`
	Note      string     `json:"note,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func secondsPtr(d time.Duration) *int64 {
	s := int64(d / time.Second)
	return &s
}

// activityUnknownNote is what status says of a proxy without activity tags.
func activityUnknownNote() string {
	return "unknown (proxy predates spawn v" + egressproxy.ActivityTagsSince + ")"
}

// egressProxyVPCReport turns one VPC's inventory into its report. Pure.
func egressProxyVPCReport(region string, inv deploy.VPCInventory, cliVersion string, now time.Time) egressProxyVPCStatus {
	group := func(g deploy.GroupState) egressProxyGroupStatus {
		users := g.Users
		if users == nil {
			users = []string{}
		}
		return egressProxyGroupStatus{Name: g.Name, ID: g.ID, Exists: g.Exists(), NotSpawns: g.Foreign,
			ENICount: g.ENIs, Instances: users}
	}
	out := egressProxyVPCStatus{Region: region, VPCID: inv.VPCID, ProxyGroup: group(inv.Groups.Proxy),
		ClientsGroup: group(inv.Groups.Clients), Proxies: []egressProxyInstanceState{}}
	for _, l := range inv.Proxies {
		p := l.Proxy
		launched := l.Launched()
		st := egressProxyInstanceState{
			InstanceID: p.InstanceID, State: p.State, PrivateIP: p.PrivateIP, Port: egressproxy.Port,
			PublicIP: l.PublicIP, Version: p.Version, VersionMatches: p.Version != "" && p.Version == cliVersion,
			Launched: timePtr(launched), Deadline: timePtr(p.Deadline),
			Reusable: l.NotReused == "", NotReusedReason: l.NotReused,
			SpawnsProxy: l.NotSpawns == "", NotSpawnsReason: l.NotSpawns,
		}
		if !p.Deadline.IsZero() {
			st.TimeLeftSeconds = secondsPtr(p.Deadline.Sub(now))
		}
		if l.LastActivity.IsZero() {
			st.Activity = egressProxyActivity{Note: activityUnknownNote(), ExpiresAt: timePtr(p.Deadline)}
		} else {
			st.Activity = egressProxyActivity{Known: true, LastActivity: timePtr(l.LastActivity),
				IdleSeconds: secondsPtr(now.Sub(l.LastActivity)), ExpiresAt: timePtr(l.Expiry())}
			if l.Idle > 0 {
				st.Activity.IdlePeriodSeconds = secondsPtr(l.Idle)
				st.Activity.IdleExpiresAt = timePtr(l.IdleExpiry())
			}
		}
		out.Proxies = append(out.Proxies, st)
	}
	return out
}

func runEgressProxyStatus(cmd *cobra.Command, _ []string) error {
	if err := validateVPCFlag("vpc", egressProxyStatusVPC); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	env, err := newEgressProxyEnv(ctx)
	if err != nil {
		return err
	}
	regions, err := env.regions(ctx, egressProxyStatusAllRegions)
	if err != nil {
		return err
	}
	now := env.now()
	report := egressProxyStatusReport{CLIVersion: version(), GeneratedAt: now.UTC(), Regions: regions,
		VPCs: []egressProxyVPCStatus{}, Errors: []egressProxyRegionError{}}
	for _, region := range regions {
		inv, err := egressProxyInventory(ctx, env.client(region).Deps.EC2, egressProxyStatusVPC, egressProxyWant(now))
		if err != nil {
			report.Errors = append(report.Errors, egressProxyRegionError{Region: region, Error: err.Error()})
			continue
		}
		for _, v := range inv {
			report.VPCs = append(report.VPCs, egressProxyVPCReport(region, v, version(), now))
		}
	}
	out := cmd.OutOrStdout()
	if getOutputFormat() == outputFormatJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderEgressProxyStatus(out, report, now)
	}
	if len(report.Errors) > 0 {
		return fmt.Errorf("could not read the egress proxies of %d region(s): %s", len(report.Errors), report.Errors[0].Error)
	}
	return nil
}

// renderEgressProxyStatus is status's text output. Pure.
func renderEgressProxyStatus(w io.Writer, r egressProxyStatusReport, now time.Time) {
	where := displayCleanupRegions(r.Regions)
	if len(r.VPCs) == 0 {
		fmt.Fprintf(w, "No spawn egress proxy or proxy security group in %s.\n", where)
		for _, e := range r.Errors {
			fmt.Fprintf(w, "  %s: %s\n", e.Region, e.Error)
		}
		return
	}
	fmt.Fprintf(w, "Egress proxies in %s (this spawn: %s)\n", where, r.CLIVersion)
	for _, v := range r.VPCs {
		fmt.Fprintf(w, "\n%s (%s)\n", v.VPCID, v.Region)
		if len(v.Proxies) == 0 {
			fmt.Fprintf(w, "  no proxy instance\n")
		}
		for _, p := range v.Proxies {
			addr := "no private IP"
			if p.PrivateIP != "" {
				addr = fmt.Sprintf("%s:%d", p.PrivateIP, p.Port)
			}
			pub := "no public IP"
			if p.PublicIP != "" {
				pub = "public " + p.PublicIP
			}
			fmt.Fprintf(w, "  %s  %s  %s  %s\n", p.InstanceID, p.State, addr, pub)
			match := "matches this spawn"
			if !p.VersionMatches {
				match = "not this spawn's " + r.CLIVersion
			}
			fmt.Fprintf(w, "    version   %s (%s)\n", orNone(p.Version), match)
			fmt.Fprintf(w, "    launched  %s\n", fmtStatusTime(p.Launched))
			dl := fmtStatusTime(p.Deadline)
			if p.TimeLeftSeconds != nil {
				if left := time.Duration(*p.TimeLeftSeconds) * time.Second; left > 0 {
					dl += " (" + formatDuration(left) + " left)"
				} else {
					dl += " (passed " + formatDuration(-left) + " ago)"
				}
			}
			fmt.Fprintf(w, "    deadline  %s\n", dl)
			a := p.Activity
			if !a.Known {
				fmt.Fprintf(w, "    activity  %s\n", a.Note)
			} else {
				line := fmt.Sprintf("idle for %s (last %s)", formatDuration(time.Duration(*a.IdleSeconds)*time.Second),
					fmtStatusTime(a.LastActivity))
				if a.ExpiresAt != nil {
					why := "deadline"
					if a.IdleExpiresAt != nil && a.IdleExpiresAt.Equal(*a.ExpiresAt) {
						why = "idle period " + formatDuration(time.Duration(*a.IdlePeriodSeconds)*time.Second)
					}
					if left := a.ExpiresAt.Sub(now); left > 0 {
						line += fmt.Sprintf(", expires in %s (%s)", formatDuration(left), why)
					} else {
						line += fmt.Sprintf(", expiry due (%s)", why)
					}
				}
				fmt.Fprintf(w, "    activity  %s\n", line)
			}
			switch {
			case p.Reusable:
				fmt.Fprintf(w, "    reuse     yes: the next --bootstrap-egress proxy launch uses it\n")
			default:
				fmt.Fprintf(w, "    reuse     no: %s\n", p.NotReusedReason)
			}
			if !p.SpawnsProxy {
				fmt.Fprintf(w, "    NOT SPAWN'S: %s; delete and upgrade will not touch it\n", p.NotSpawnsReason)
			}
		}
		for _, g := range []egressProxyGroupStatus{v.ProxyGroup, v.ClientsGroup} {
			switch {
			case !g.Exists:
				fmt.Fprintf(w, "  group %s: none\n", g.Name)
			case g.NotSpawns != "":
				fmt.Fprintf(w, "  group %s (%s): not spawn's: %s\n", g.Name, g.ID, g.NotSpawns)
			case g.ENICount == 0:
				fmt.Fprintf(w, "  group %s (%s): unused\n", g.Name, g.ID)
			default:
				fmt.Fprintf(w, "  group %s (%s): on %d network interface(s) of %s\n", g.Name, g.ID, g.ENICount,
					strings.Join(g.Instances, ", "))
			}
		}
	}
	for _, e := range r.Errors {
		fmt.Fprintf(w, "\n%s: %s\n", e.Region, e.Error)
	}
}

func fmtStatusTime(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// ---- delete ---------------------------------------------------------------

func runEgressProxyDelete(cmd *cobra.Command, _ []string) error {
	switch {
	case egressProxyDeleteVPC == "" && !egressProxyDeleteAll:
		return errors.New("name the VPC with --vpc, or pass --all for every VPC in the region")
	case egressProxyDeleteVPC != "" && egressProxyDeleteAll:
		return errors.New("--vpc and --all both choose the VPCs; pass one")
	}
	if err := validateVPCFlag("vpc", egressProxyDeleteVPC); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	env, err := newEgressProxyEnv(ctx)
	if err != nil {
		return err
	}
	regions, err := env.regions(ctx, false)
	if err != nil {
		return err
	}
	region := regions[0]
	client := env.client(region)
	inv, err := egressProxyInventory(ctx, client.Deps.EC2, egressProxyDeleteVPC, egressProxyWant(env.now()))
	if err != nil {
		return err
	}
	var plans []deploy.DeletePlan
	refused := 0
	for _, v := range inv {
		p := deploy.PlanDelete(v)
		refused += len(p.Refused)
		if !p.Empty() || len(p.Refused) > 0 {
			plans = append(plans, p)
		}
	}
	scope := egressProxyDeleteVPC
	if scope == "" {
		scope = "any VPC"
	}
	if len(plans) == 0 {
		fmt.Fprintf(out, "No spawn egress proxy or proxy security group in %s in %s: nothing to delete.\n", scope, region)
		return nil
	}
	fmt.Fprintf(out, "In %s:\n", region)
	for _, p := range plans {
		printEgressProxyDeletePlan(out, p)
	}
	refusedErr := func() error {
		if refused == 0 {
			return nil
		}
		return fmt.Errorf("%w: %d instance(s) tagged %s are not ones spawn can tell it launched as the proxy (reasons above); "+
			"they were not touched. If one is yours to end, end it with 'spawn terminate' or the EC2 console",
			errEgressProxyRefused, refused, deploy.ProxyTag)
	}
	if egressProxyDeleteDryRun {
		fmt.Fprintln(out, "\nDry run: nothing was terminated or deleted.")
		return refusedErr()
	}
	act := false
	for _, p := range plans {
		act = act || len(p.Terminate) > 0 || len(p.Await) > 0 || p.Groups.Proxy.Owned() || p.Groups.Clients.Owned()
	}
	if !act {
		return refusedErr()
	}
	if !egressProxyConfirm(egressProxyDeleteYes, "Terminate and delete the above?") {
		return newAborted("Aborted: no egress proxy was terminated and no group was deleted.")
	}
	var failed []string
	for _, p := range plans {
		res, err := egressProxyDelete(ctx, client.Deps, p, 0)
		for _, id := range res.Terminated {
			env.audit.LogOperationWithRegion("egress_proxy_terminate", id, region, "success", nil)
		}
		for _, id := range res.DeletedGroups {
			env.audit.LogOperationWithRegion("egress_proxy_delete_group", id, region, "success", nil)
		}
		printEgressProxyDeleteResult(out, p.VPCID, res)
		if err != nil {
			env.audit.LogOperationWithRegion("egress_proxy_delete", p.VPCID, region, "failed", err)
			failed = append(failed, fmt.Sprintf("%s: %v", p.VPCID, err))
		}
	}
	if len(failed) > 0 {
		return errors.Join(fmt.Errorf("egress proxy delete failed in %d VPC(s): %s", len(failed), strings.Join(failed, "; ")), refusedErr())
	}
	return refusedErr()
}

func printEgressProxyDeletePlan(w io.Writer, p deploy.DeletePlan) {
	fmt.Fprintf(w, "  %s\n", p.VPCID)
	for _, x := range p.Terminate {
		fmt.Fprintf(w, "    terminate  %s (%s, spawn %s)\n", x.InstanceID, x.State, orNone(x.Version))
	}
	for _, x := range p.Await {
		fmt.Fprintf(w, "    wait for   %s (already shutting down)\n", x.InstanceID)
	}
	for _, r := range p.Refused {
		fmt.Fprintf(w, "    REFUSE     %s: %s\n", r.InstanceID, r.Reason)
	}
	for _, g := range []deploy.GroupState{p.Groups.Proxy, p.Groups.Clients} {
		switch {
		case !g.Exists():
		case !g.Owned():
			fmt.Fprintf(w, "    leave      %s (%s): not spawn's: %s\n", g.Name, g.ID, g.Foreign)
		default:
			note := ""
			if others := otherUsers(g.Users, p); len(others) > 0 {
				note = fmt.Sprintf(" (kept while used by %s)", strings.Join(others, ", "))
			}
			fmt.Fprintf(w, "    delete     %s (%s)%s\n", g.Name, g.ID, note)
		}
	}
}

// otherUsers are the users of a group that the plan does not terminate.
func otherUsers(users []string, p deploy.DeletePlan) []string {
	ending := map[string]bool{}
	for _, x := range append(append([]deploy.Proxy(nil), p.Terminate...), p.Await...) {
		ending[x.InstanceID] = true
	}
	var out []string
	for _, u := range users {
		if !ending[u] {
			out = append(out, u)
		}
	}
	return out
}

func printEgressProxyDeleteResult(w io.Writer, vpcID string, res deploy.DeleteResult) {
	for _, id := range res.Terminated {
		fmt.Fprintf(w, "✓ %s: terminated %s\n", vpcID, id)
	}
	for _, id := range res.DeletedGroups {
		fmt.Fprintf(w, "✓ %s: deleted security group %s\n", vpcID, id)
	}
	if k := res.Kept; k != nil {
		fmt.Fprintf(w, "• %s: kept %s (%s): still used by %s. The TTL reaper's managed-network sweep deletes it once no "+
			"instance uses it (and it is older than the sweep's grace), or run this command again then.\n",
			vpcID, k.Name, k.ID, strings.Join(k.Users, ", "))
	}
}

// ---- upgrade --------------------------------------------------------------

func runEgressProxyUpgrade(cmd *cobra.Command, _ []string) error {
	if egressProxyUpgradeVPC == "" {
		return errors.New("name the VPC with --vpc")
	}
	if err := validateVPCFlag("vpc", egressProxyUpgradeVPC); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	env, err := newEgressProxyEnv(ctx)
	if err != nil {
		return err
	}
	regions, err := env.regions(ctx, false)
	if err != nil {
		return err
	}
	region, vpc := regions[0], egressProxyUpgradeVPC
	client := env.client(region)
	inv, err := egressProxyInventory(ctx, client.Deps.EC2, vpc, egressProxyWant(env.now()))
	if err != nil {
		return err
	}
	if len(inv) == 0 {
		inv = []deploy.VPCInventory{{VPCID: vpc}}
	}
	plan := deploy.PlanUpgrade(inv[0])
	for _, r := range plan.Refused {
		fmt.Fprintf(out, "• %s is tagged as %s's proxy but is not one spawn can tell it launched, so upgrade leaves it alone: %s\n",
			r.InstanceID, vpc, r.Reason)
	}
	if c := plan.Current; c != nil {
		fmt.Fprintf(out, "✓ %s's egress proxy %s is already current: spawn %s, until %s. Nothing to do.\n",
			vpc, c.Proxy.InstanceID, c.Proxy.Version, c.Proxy.Deadline.UTC().Format(time.RFC3339))
		return nil
	}
	if len(plan.Replace) == 0 {
		if len(plan.Refused) > 0 {
			return fmt.Errorf("%w in %s: no proxy spawn launched there to upgrade", errEgressProxyRefused, vpc)
		}
		fmt.Fprintf(out, "No egress proxy in %s (%s): nothing to upgrade. The next --bootstrap-egress proxy launch there creates one.\n",
			vpc, region)
		return nil
	}
	idle := plan.Idle()
	fmt.Fprintf(out, "In %s, %s:\n", region, vpc)
	fmt.Fprintf(out, "  launch    a new egress proxy at spawn %s (idle period %s), and wait until it is ready\n", version(), idle)
	for _, l := range plan.Replace {
		what := "terminate"
		if egressProxyUpgradeKeepOld {
			what = "keep     "
		}
		why := l.NotReused
		fmt.Fprintf(out, "  %s %s (spawn %s): %s\n", what, l.Proxy.InstanceID, orNone(l.Proxy.Version), why)
	}
	if !egressProxyUpgradeKeepOld {
		fmt.Fprintln(out, "\n  A member that is mid-bootstrap through the old proxy when it is terminated can fail its download;\n"+
			"  members that have finished their bootstrap do not use it.")
	}
	if !egressProxyConfirm(egressProxyUpgradeYes, "Upgrade the egress proxy?") {
		return newAborted("Aborted: no egress proxy was launched or terminated.")
	}

	subnet := plan.Replace[0].Proxy.SubnetID
	network, err := env.network(ctx, region, subnet)
	if err != nil {
		return fmt.Errorf("read %s's public subnets (from the old proxy's subnet %s): %w", vpc, subnet, err)
	}
	profile, err := client.Profile(ctx)
	if err != nil {
		return err
	}
	res, err := egressProxyUpgrade(ctx, client.Deps, deploy.Request{
		Region: region, VPCID: vpc, AvailabilityZone: plan.Replace[0].Proxy.AvailabilityZone,
		PublicSubnets: network.PublicSubnets, Version: version(), Idle: idle, InstanceProfile: profile,
		AMI: client.AMI, UserData: deploy.UserData, BaseTags: client.BaseTags, Out: out,
	}, plan, egressProxyUpgradeKeepOld)
	if res.New.InstanceID != "" {
		how := "reusing"
		if res.New.Created {
			how = "launched"
		}
		env.audit.LogOperationWithRegion("egress_proxy_upgrade", res.New.InstanceID, region, "success", nil)
		fmt.Fprintf(out, "✓ %s: %s egress proxy %s at %s (spawn %s, until %s)\n", vpc, how, res.New.InstanceID,
			res.New.Addr(), orNone(res.New.Version), res.New.Deadline.UTC().Format(time.RFC3339))
	}
	for _, id := range res.Terminated {
		env.audit.LogOperationWithRegion("egress_proxy_terminate", id, region, "success", nil)
		fmt.Fprintf(out, "✓ %s: terminated the old proxy %s\n", vpc, id)
	}
	for _, id := range res.Kept {
		fmt.Fprintf(out, "• %s: kept the old proxy %s; it ends at its idle period or its deadline\n", vpc, id)
	}
	if err != nil {
		env.audit.LogOperationWithRegion("egress_proxy_upgrade", vpc, region, "failed", err)
		return fmt.Errorf("egress proxy upgrade in %s: %w", vpc, err)
	}
	return nil
}
