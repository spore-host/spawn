package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
	"github.com/spore-host/spawn/pkg/progress"
)

// --bootstrap-egress (#1515 phase 3): the opt-in to the spawn egress proxy. A
// --no-public-ip launch whose subnet has no NAT route cannot reach the hosts
// outside AWS its bootstrap fetches from (the EFA installer, say), and is
// refused. With --bootstrap-egress proxy those of them on the proxy's
// allowlist count as reached through a proxy in the VPC, which the launch
// finds or creates before any member starts, and every member gets the
// proxy's client security group. The proxy is never created without the
// opt-in (owner decision, 2026-11-02).
//
// Members fetch through it only around spawn's own fetches from outside AWS
// (phase 4: pkg/userdata's EFA and apt steps, pkg/launcher's AWS CLI
// install), never for the user's command. A foreground parameter sweep
// (--no-detach) finds or creates the proxy of each VPC its members count on
// (ensureSweepEgressProxies). A detached sweep does too, at submit time, and
// records each VPC's proxy for the sweep orchestrator, which ensures it again
// before each launch and replaces it if it has gone (#1515 PR B:
// launch_sweep_detached_egress_proxy.go). Schedules (which run detached
// sweeps and carry no --no-public-ip) and pools do not use it.

// bootstrapEgressEnv sets --bootstrap-egress's default for one shell.
const bootstrapEgressEnv = "SPAWN_BOOTSTRAP_EGRESS"

// bootstrapEgressSource says where the effective --bootstrap-egress came
// from, for messages: the flag, the environment, the config file, or the
// built-in default. Set by resolveBootstrapEgressFlag.
var bootstrapEgressSource = "default"

// resolveBootstrapEgress is --bootstrap-egress's value by precedence: the flag
// when given, else SPAWN_BOOTSTRAP_EGRESS, else the `spawn defaults` file's
// bootstrap_egress, else none. The same order libs/sporeconfig resolves the
// shared settings in; the key itself is spawn's, so it lives with spawn's
// other launch defaults. Pure.
func resolveBootstrapEgress(flagChanged bool, flagValue, envValue, fileValue string) (mode, source string, err error) {
	mode, source = aws.BootstrapEgressNone, "default"
	switch {
	case flagChanged:
		mode, source = flagValue, "--bootstrap-egress"
	case strings.TrimSpace(envValue) != "":
		mode, source = strings.TrimSpace(envValue), bootstrapEgressEnv
	case fileValue != "":
		mode, source = fileValue, "bootstrap_egress in ~/.spawn/config.yaml"
	}
	if err := spawnconfig.ValidateBootstrapEgress(mode); err != nil || mode == "" {
		return mode, source, fmt.Errorf("%s: invalid bootstrap egress %q: use %s or %s",
			source, mode, aws.BootstrapEgressNone, aws.BootstrapEgressProxy)
	}
	return mode, source, nil
}

// resolveBootstrapEgressFlag sets bootstrapEgress to its effective value for
// this invocation, before any AWS call.
func resolveBootstrapEgressFlag(changed bool) error {
	fileValue := ""
	if d, err := spawnconfig.LoadLaunchDefaults(); err == nil && d != nil {
		fileValue = d.BootstrapEgress
	}
	mode, source, err := resolveBootstrapEgress(changed, bootstrapEgress, os.Getenv(bootstrapEgressEnv), fileValue)
	if err != nil {
		return err
	}
	bootstrapEgress, bootstrapEgressSource = mode, source
	return nil
}

// errEgressProxyUnsupported is wrapped by a refusal on a path the proxy does
// not reach.
var errEgressProxyUnsupported = errors.New("--bootstrap-egress proxy is not supported here (#1515)")

// validateBootstrapEgressFlags refuses what --bootstrap-egress and
// --egress-proxy-idle cannot do, before any AWS call. Pure: flags only.
//   - An explicit --bootstrap-egress proxy without --no-public-ip: an instance
//     with a public IP needs no proxy. A default from the environment or the
//     config file is simply not used there.
//   - --egress-proxy-idle without the proxy, or not positive, or longer than
//     the proxy's life.
//
// Every launch path that takes --no-public-ip supports the proxy, a detached
// parameter sweep included (#1515 PR B).
func validateBootstrapEgressFlags(idleChanged bool) error {
	proxy := bootstrapEgress == aws.BootstrapEgressProxy
	switch {
	case proxy && !noPublicIP && bootstrapEgressSource == "--bootstrap-egress":
		return errors.New("--bootstrap-egress proxy applies only with --no-public-ip: an instance with a public IP " +
			"reaches the internet itself")
	case idleChanged && !(proxy && noPublicIP):
		return errors.New("--egress-proxy-idle applies only with --no-public-ip --bootstrap-egress proxy")
	}
	if proxy && noPublicIP {
		if err := egressproxy.ValidateIdle(egressProxyIdle, egressproxy.MaxLife); err != nil {
			return fmt.Errorf("--egress-proxy-idle: %w", err)
		}
	}
	return nil
}

// launchBootstrapEgress is the LaunchConfig.BootstrapEgress of a launch: the
// effective mode with --no-public-ip, else "" (the proxy is a private launch's
// only).
func launchBootstrapEgress() string {
	if !noPublicIP || bootstrapEgress == aws.BootstrapEgressNone {
		return ""
	}
	return bootstrapEgress
}

// egressProxyFindOrCreate is deploy.FindOrCreate; a seam so tests need no AWS.
var egressProxyFindOrCreate = deploy.FindOrCreate

// egressProxyMemberGroups is deploy.MemberGroups; a seam so tests need no AWS.
var egressProxyMemberGroups = deploy.MemberGroups

// egressProxyCheckMemberEgress is deploy.CheckMemberEgress; a seam so tests
// need no AWS.
var egressProxyCheckMemberEgress = deploy.CheckMemberEgress

// egressProxyClient is the AWS side of find-or-create; a seam so tests need
// no AWS.
var egressProxyClient = func(awsClient *aws.Client, region string) deploy.Client {
	return deploy.ClientFor(awsClient, region, version())
}

// egressProxyTerminate is deploy.Terminate; a seam so tests need no AWS.
var egressProxyTerminate = deploy.Terminate

// egressProxyNeed is whether cfg counts on the egress proxy, and the network
// it would serve: the same check the preflight ran, from the client's cache.
// A no-op unless cfg is a --no-public-ip --bootstrap-egress proxy launch;
// with a NAT route, or no internet need at all, there is nothing for a proxy
// to do. internet is noPublicIPInternetNeeds for a single launch, nil for a
// sweep member (a sweep refuses --mpi).
func egressProxyNeed(ctx context.Context, awsClient *aws.Client, cfg *aws.LaunchConfig, internet []string) (aws.PrivateNetwork, bool, error) {
	if !cfg.NoPublicIP || cfg.BootstrapEgress != aws.BootstrapEgressProxy {
		return aws.PrivateNetwork{}, false, nil
	}
	check, err := preflightNoPublicIPFor(ctx, awsClient, cfg, internet, io.Discard)
	if err != nil {
		return aws.PrivateNetwork{}, false, err
	}
	return check.Network, len(check.ViaEgressProxy) > 0, nil
}

// egressProxyFor is one VPC's proxy, found or created and ready, the client
// it was reached through, and the instance profile it runs with.
type egressProxyFor struct {
	client  deploy.Client
	result  deploy.Result
	profile string
}

// findOrCreateEgressProxy finds or creates the egress proxy of n's VPC in
// region, with idle as its idle period, and waits for it to be ready.
func findOrCreateEgressProxy(ctx context.Context, awsClient *aws.Client, region string, n aws.PrivateNetwork, idle time.Duration, out io.Writer) (egressProxyFor, error) {
	client := egressProxyClient(awsClient, region)
	// The proxy always runs as spored-instance-role, whatever the members
	// run as: it is shared by every launch in the VPC.
	profile, err := client.Profile(ctx)
	if err != nil {
		return egressProxyFor{client: client}, err
	}
	result, err := egressProxyFindOrCreate(ctx, client.Deps, deploy.Request{
		Region: region, VPCID: n.VPCID, AvailabilityZone: n.AvailabilityZone, PublicSubnets: n.PublicSubnets,
		Version: version(), Idle: idle, InstanceProfile: profile,
		AMI: client.AMI, UserData: deploy.UserData, BaseTags: client.BaseTags, Out: out,
	})
	return egressProxyFor{client: client, result: result, profile: profile}, err
}

// attach gives config the proxy's client group, beside its own groups (or the
// VPC default), and the proxy's address. A member whose own groups allow no
// egress to the proxy would stall in its bootstrap: it is refused now, before
// any member exists (#1515).
func (p egressProxyFor) attach(ctx context.Context, vpcID string, config *aws.LaunchConfig) error {
	groups, err := egressProxyMemberGroups(ctx, p.client.Deps.EC2, vpcID, config.SecurityGroupIDs, p.result.Groups)
	if err != nil {
		return err
	}
	if err := egressProxyCheckMemberEgress(ctx, p.client.Deps.EC2, groups, p.result.Groups, p.result.Proxy.PrivateIP); err != nil {
		return err
	}
	config.SecurityGroupIDs = groups
	config.EgressProxyAddr = p.result.Proxy.Addr()
	return nil
}

// report says which proxy members of vpcID use.
func (p egressProxyFor) report(out io.Writer, vpcID string) {
	how := "reusing"
	if p.result.Proxy.Created {
		how = "launched"
	}
	fmt.Fprintf(out, "✓ egress proxy: %s %s at %s in %s (until %s; members get %s)\n", how, p.result.Proxy.InstanceID,
		p.result.Proxy.Addr(), vpcID, p.result.Proxy.Deadline.UTC().Format(time.RFC3339), deploy.ClientsGroupName(vpcID))
}

// ensureEgressProxy finds or creates the egress proxy a --bootstrap-egress
// proxy launch counts on, before any member is launched, and gives the
// members its client group and its address. A no-op unless the subnet check
// counts a need as reached through the proxy (egressProxyNeed).
func ensureEgressProxy(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress, out io.Writer) error {
	n, need, err := egressProxyNeed(ctx, awsClient, config, noPublicIPInternetNeeds())
	if err != nil || !need {
		return err
	}
	const step = "Finding or creating the egress proxy"
	prog.Start(step)
	fail := func(err error) error {
		prog.Error(step, err)
		return fmt.Errorf("egress proxy for %s: %w", n.VPCID, err)
	}
	p, err := findOrCreateEgressProxy(ctx, awsClient, config.Region, n, egressProxyIdle, out)
	if err != nil {
		return fail(err)
	}
	if err := p.attach(ctx, n.VPCID, config); err != nil {
		return fail(err)
	}
	prog.Complete(step)
	p.report(out, n.VPCID)
	return nil
}

// egressProxyVPC is a (region, VPC) whose sweep members count on the proxy.
type egressProxyVPC struct{ region, vpcID string }

// ensureSweepEgressProxies is ensureEgressProxy for a foreground parameter
// sweep (#1515): each member is checked in its own region and subnet, and the
// proxy of every distinct (region, VPC) some member counts on is found or
// created once, ready, before any member is launched. Each such member then
// gets its VPC's client group and address; a member that counts on no proxy
// (a NAT route, an image with an aws CLI, not opted in) gets neither.
//
// The VPCs are taken one at a time, in the order their first member appears.
// If any VPC's proxy cannot be made ready, or a member cannot reach its VPC's,
// no member is launched, and the proxies this call created in the other VPCs
// are terminated, since nothing will use them. A proxy that was reused is
// left running: it is not this sweep's. (FindOrCreate already terminated the
// failed VPC's own new proxy.)
//
// indexes are the members' parameter-set numbers, for the error (nil: their
// positions). idle is the proxy's idle period.
func ensureSweepEgressProxies(ctx context.Context, awsClient *aws.Client, cfgs []*aws.LaunchConfig, indexes []int, idle time.Duration, prog *progress.Progress, out io.Writer) error {
	paramSet := func(i int) int {
		if indexes != nil {
			return indexes[i]
		}
		return i
	}
	var order []egressProxyVPC
	members := map[egressProxyVPC][]int{}
	networks := map[egressProxyVPC]aws.PrivateNetwork{}
	for i, cfg := range cfgs {
		n, need, err := egressProxyNeed(ctx, awsClient, cfg, nil)
		if err != nil {
			return fmt.Errorf("parameter set %d: %w", paramSet(i), err)
		}
		if !need {
			continue
		}
		k := egressProxyVPC{cfg.Region, n.VPCID}
		if _, seen := members[k]; !seen {
			order = append(order, k)
			networks[k] = n
		}
		members[k] = append(members[k], i)
	}
	if len(order) == 0 {
		return nil
	}
	step := "Finding or creating the egress proxy"
	if len(order) > 1 {
		step = fmt.Sprintf("Finding or creating the egress proxies of %d VPCs", len(order))
	}
	prog.Start(step)
	ready := map[egressProxyVPC]egressProxyFor{}
	var created []egressProxyFor
	fail := func(k egressProxyVPC, err error) error {
		err = fmt.Errorf("egress proxy for %s in %s: %w", k.vpcID, k.region, err)
		for _, p := range created {
			id := p.result.Proxy.InstanceID
			// A ^C must not stop the teardown of what this sweep made.
			if terr := egressProxyTerminate(context.WithoutCancel(ctx), p.client.Deps.EC2, id); terr != nil {
				err = errors.Join(err, fmt.Errorf("and egress proxy %s, which this sweep created, could not be "+
					"terminated, so terminate it yourself (`spawn terminate %s`); its idle period and "+
					"deadline end it otherwise: %w", id, id, terr))
				continue
			}
			fmt.Fprintf(out, "   terminated egress proxy %s, which this sweep created and no member will use\n", id)
		}
		prog.Error(step, err)
		return fmt.Errorf("%w; no member was launched", err)
	}
	for _, k := range order {
		p, err := findOrCreateEgressProxy(ctx, awsClient, k.region, networks[k], idle, out)
		if err != nil {
			return fail(k, err)
		}
		ready[k] = p
		if p.result.Proxy.Created {
			created = append(created, p)
		}
	}
	for _, k := range order {
		for _, i := range members[k] {
			if err := ready[k].attach(ctx, k.vpcID, cfgs[i]); err != nil {
				return fail(k, fmt.Errorf("parameter set %d: %w", paramSet(i), err))
			}
		}
	}
	prog.Complete(step)
	for _, k := range order {
		ready[k].report(out, k.vpcID)
	}
	return nil
}
