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
// Members do not use the proxy yet: setting http_proxy in their bootstrap is
// phase 4. Parameter sweeps (foreground and --detach), schedules and pools
// do not support it yet.

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
// not reach yet.
var errEgressProxyUnsupported = errors.New("--bootstrap-egress proxy is not supported here yet (#1515)")

// validateBootstrapEgressFlags refuses what --bootstrap-egress and
// --egress-proxy-idle cannot do, before any AWS call. Pure: flags only.
//   - An explicit --bootstrap-egress proxy without --no-public-ip: an instance
//     with a public IP needs no proxy. A default from the environment or the
//     config file is simply not used there.
//   - --egress-proxy-idle without the proxy, or not positive, or longer than
//     the proxy's life.
//   - The proxy on a parameter sweep, foreground or --detach: neither the
//     sweep's members nor the sweep-orchestrator Lambda find or create one.
func validateBootstrapEgressFlags(idleChanged, sweep bool) error {
	proxy := bootstrapEgress == aws.BootstrapEgressProxy
	switch {
	case proxy && !noPublicIP && bootstrapEgressSource == "--bootstrap-egress":
		return errors.New("--bootstrap-egress proxy applies only with --no-public-ip: an instance with a public IP " +
			"reaches the internet itself")
	case idleChanged && !(proxy && noPublicIP):
		return errors.New("--egress-proxy-idle applies only with --no-public-ip --bootstrap-egress proxy")
	case proxy && noPublicIP && sweep:
		return fmt.Errorf("%w: a parameter sweep's members, foreground or --detach, and the sweep orchestrator do not "+
			"find or create the egress proxy (set by %s). Launch with --bootstrap-egress none, or use a NAT route or "+
			"the VPC endpoints for the sweep", errEgressProxyUnsupported, bootstrapEgressSource)
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

// egressProxyClient is the AWS side of find-or-create; a seam so tests need
// no AWS.
var egressProxyClient = func(awsClient *aws.Client, region string) deploy.Client {
	return deploy.ClientFor(awsClient, region, version())
}

// ensureEgressProxy finds or creates the egress proxy a --bootstrap-egress
// proxy launch counts on, before any member is launched, and gives the
// members its client group and its address. A no-op unless the subnet check
// counts a need as reached through the proxy: with a NAT route, or no
// internet need at all, there is nothing for a proxy to do and none is made.
func ensureEgressProxy(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress, out io.Writer) error {
	if !config.NoPublicIP || config.BootstrapEgress != aws.BootstrapEgressProxy {
		return nil
	}
	// The same check the preflight ran, from the client's cache.
	check, err := preflightNoPublicIPFor(ctx, awsClient, config, noPublicIPInternetNeeds(), io.Discard)
	if err != nil {
		return err
	}
	if len(check.ViaEgressProxy) == 0 {
		prog.Skip("Egress proxy (nothing this launch needs goes through one)")
		return nil
	}
	const step = "Finding or creating the egress proxy"
	prog.Start(step)
	fail := func(err error) error {
		prog.Error(step, err)
		return fmt.Errorf("egress proxy for %s: %w", check.Network.VPCID, err)
	}
	n := check.Network
	client := egressProxyClient(awsClient, config.Region)
	// The proxy always runs as spored-instance-role, whatever the members
	// run as: it is shared by every launch in the VPC.
	profile, err := client.Profile(ctx)
	if err != nil {
		return fail(err)
	}
	result, err := egressProxyFindOrCreate(ctx, client.Deps, deploy.Request{
		Region: config.Region, VPCID: n.VPCID, AvailabilityZone: n.AvailabilityZone, PublicSubnets: n.PublicSubnets,
		Version: version(), Idle: egressProxyIdle, InstanceProfile: profile,
		AMI: client.AMI, UserData: deploy.UserData, BaseTags: client.BaseTags, Out: out,
	})
	if err != nil {
		return fail(err)
	}
	groups, err := egressProxyMemberGroups(ctx, client.Deps.EC2, n.VPCID, config.SecurityGroupIDs, result.Groups)
	if err != nil {
		return fail(err)
	}
	config.SecurityGroupIDs = groups
	config.EgressProxyAddr = result.Proxy.Addr()
	prog.Complete(step)
	how := "reusing"
	if result.Proxy.Created {
		how = "launched"
	}
	fmt.Fprintf(out, "✓ egress proxy: %s %s at %s in %s (until %s; members get %s)\n", how, result.Proxy.InstanceID,
		config.EgressProxyAddr, n.VPCID, result.Proxy.Deadline.UTC().Format(time.RFC3339), deploy.ClientsGroupName(n.VPCID))
	return nil
}
