package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
)

// --bootstrap-egress proxy on a detached parameter sweep (#1515 PR B).
//
// At --detach the CLI does what a foreground sweep does, in two halves around
// the confirmation prompt, since a declined launch creates nothing:
//
//  1. seedDetachedBootstrap, before the prompt, runs each --no-public-ip
//     member's subnet check. A member it counts on the proxy is rendered with
//     deploy.PendingAddr as its proxy address, marked with its VPC
//     (detachedEgressProxyVPCKey, which the orchestrator reads) and collected
//     here, by (region, VPC).
//  2. ensureDetachedEgressProxies, after the prompt, finds or creates each
//     (region, VPC)'s proxy, ready (findOrCreateEgressProxy, the foreground's
//     path, which also ensures spored-instance-profile: the orchestrator
//     cannot write IAM), binds its members to it (the address in their user
//     data, the clients group among their groups, after checking their own
//     groups reach it), and returns each VPC as a deploy.SweepTarget for the
//     sweep's record.
//
// The orchestrator ensures each target again before it launches the target's
// members (lambda/sweep-orchestrator/egress_proxy.go), and replaces a proxy
// that has gone. A target records what that takes: the CLI's version (the
// version a reusable proxy must carry, and the one whose spored the proxy's
// user data installs), the proxy's user data, AMI, base tags and profile, its
// VPC's public subnets, and the address and clients group the uploaded
// members name. A member launched after a replacement is rebound to the new
// proxy (deploy.RebindUserData, deploy.RebindGroups).

// detachedEgressProxyVPCKey is the param key naming the VPC whose egress
// proxy a member counts on. It must equal egressProxyVPCParam in
// lambda/sweep-orchestrator/egress_proxy.go; TestDetachedSweepSeedsEveryKeyTheLambdaReads
// holds the two together.
const detachedEgressProxyVPCKey = "spawn_egress_proxy_vpc"

// detachedEgressProxies are a detached sweep's proxy members, by (region,
// VPC), in the order their first member appears.
type detachedEgressProxies struct {
	order    []egressProxyVPC
	networks map[egressProxyVPC]aws.PrivateNetwork
	rows     map[egressProxyVPC][]int
}

// add records param set row, in region, as counting on the proxy of n's VPC.
func (e *detachedEgressProxies) add(region string, n aws.PrivateNetwork, row int) {
	if e.rows == nil {
		e.networks, e.rows = map[egressProxyVPC]aws.PrivateNetwork{}, map[egressProxyVPC][]int{}
	}
	k := egressProxyVPC{region, n.VPCID}
	if _, seen := e.rows[k]; !seen {
		e.order = append(e.order, k)
		e.networks[k] = n
	}
	e.rows[k] = append(e.rows[k], row)
}

// egressProxyUserData is deploy.UserData; a seam so tests need no build.
var egressProxyUserData = deploy.UserData

// ensureDetachedEgressProxies finds or creates the proxy of each (region,
// VPC) in e, ready, binds its members in pf to it, and returns the targets
// for the sweep's record. A no-op when e has none.
//
// If any VPC's proxy cannot be made ready, or a member cannot reach its VPC's,
// nothing is uploaded, and the proxies this call created are terminated, as
// a foreground sweep's are (ensureSweepEgressProxies). A proxy that was
// reused is left running: it is not this sweep's.
func ensureDetachedEgressProxies(ctx context.Context, awsClient *aws.Client, pf *ParamFileFormat, e *detachedEgressProxies, idle time.Duration, out io.Writer) ([]deploy.SweepTarget, error) {
	if e == nil || len(e.order) == 0 {
		return nil, nil
	}
	var created []egressProxyFor
	fail := func(k egressProxyVPC, err error) error {
		err = fmt.Errorf("egress proxy for %s in %s: %w", k.vpcID, k.region, err)
		for _, p := range created {
			id := p.result.Proxy.InstanceID
			if terr := egressProxyTerminate(context.WithoutCancel(ctx), p.client.Deps.EC2, id); terr != nil {
				err = errors.Join(err, fmt.Errorf("and egress proxy %s, which this sweep created, could not be "+
					"terminated, so terminate it yourself (`spawn terminate %s`); its idle period and "+
					"deadline end it otherwise: %w", id, id, terr))
				continue
			}
			fmt.Fprintf(out, "   terminated egress proxy %s, which this sweep created and no member will use\n", id)
		}
		return fmt.Errorf("%w; nothing was uploaded and no member will launch", err)
	}
	proxyUserData, err := egressProxyUserData(idle)
	if err != nil {
		return nil, fmt.Errorf("the egress proxy's user data: %w", err)
	}
	targets := make([]deploy.SweepTarget, 0, len(e.order))
	for _, k := range e.order {
		n := e.networks[k]
		p, err := findOrCreateEgressProxy(ctx, awsClient, k.region, n, idle, out)
		if err != nil {
			return nil, fail(k, err)
		}
		if p.result.Proxy.Created {
			created = append(created, p)
		}
		for _, row := range e.rows[k] {
			if err := bindDetachedMember(ctx, p, k.vpcID, pf, row); err != nil {
				return nil, fail(k, fmt.Errorf("parameter set %d: %w", row, err))
			}
		}
		t, err := sweepTargetFor(ctx, p, k, n, idle, proxyUserData)
		if err != nil {
			return nil, fail(k, err)
		}
		targets = append(targets, t)
	}
	if err := normalizeDetachedUserData(pf); err != nil {
		return nil, fail(e.order[0], err)
	}
	for i, k := range e.order {
		p := targets[i]
		how := "reusing"
		for _, c := range created {
			if c.result.Proxy.InstanceID == p.ProxyID {
				how = "launched"
			}
		}
		fmt.Fprintf(out, "✓ egress proxy: %s %s at %s in %s (%d member(s); the orchestrator replaces it if it has gone "+
			"before a member launches)\n", how, p.ProxyID, p.ProxyAddr, k.vpcID, len(e.rows[k]))
	}
	return targets, nil
}

// sweepTargetFor is the record of p, k's ready proxy: what the orchestrator
// needs to find it again, or to launch the one the CLI would.
func sweepTargetFor(ctx context.Context, p egressProxyFor, k egressProxyVPC, n aws.PrivateNetwork, idle time.Duration, proxyUserData string) (deploy.SweepTarget, error) {
	if p.client.AMI == nil || p.client.BaseTags == nil {
		return deploy.SweepTarget{}, errors.New("no AMI or tag source for the egress proxy")
	}
	ami, err := p.client.AMI(ctx)
	if err != nil {
		return deploy.SweepTarget{}, fmt.Errorf("the egress proxy's AMI (%s): %w", deploy.AMIParameter, err)
	}
	tags, err := p.client.BaseTags(ctx)
	if err != nil {
		return deploy.SweepTarget{}, fmt.Errorf("the egress proxy's tags: %w", err)
	}
	t := deploy.SweepTarget{
		Region: k.region, VPCID: k.vpcID, AvailabilityZone: n.AvailabilityZone, PublicSubnets: deploy.SweepSubnets(n.PublicSubnets),
		Idle: idle, Version: version(), InstanceProfile: p.profile, AMI: ami, UserData: proxyUserData,
		BaseTags:     deploy.BaseTagMap(tags),
		RenderedAddr: p.result.Proxy.Addr(), RenderedClientsGroupID: p.result.Groups.ClientsID,
	}
	deploy.SetReady(&t, p.result.Proxy, p.result.Groups)
	return t, nil
}

// bindDetachedMember binds param set row, which counts on p, to it: its own
// groups (or the VPC default) and the clients group, after checking they
// reach the proxy, and the proxy's address in its user data in place of
// deploy.PendingAddr. Written to the row; normalizeDetachedUserData folds
// identical user data back into the defaults.
func bindDetachedMember(ctx context.Context, p egressProxyFor, vpcID string, pf *ParamFileFormat, row int) error {
	r := pf.Params[row]
	settings, err := rowLaunchSettings(r)
	if err != nil {
		return err
	}
	groups, err := egressProxyMemberGroups(ctx, p.client.Deps.EC2, vpcID, settings.SecurityGroupIDs, p.result.Groups)
	if err != nil {
		return err
	}
	if err := egressProxyCheckMemberEgress(ctx, p.client.Deps.EC2, groups, p.result.Groups, p.result.Proxy.PrivateIP); err != nil {
		return err
	}
	settings.SecurityGroupIDs = groups
	ud := stringParam(r, detachedUserDataKey)
	if ud == "" {
		ud = stringParam(pf.Defaults, detachedUserDataKey)
	}
	bound, err := deploy.RebindUserData(ud, deploy.PendingAddr, p.result.Proxy.Addr())
	if err != nil {
		return err
	}
	r[detachedLaunchSettingsKey] = settings
	r[detachedUserDataKey] = bound
	return nil
}

// rowLaunchSettings is a row's seeded launch settings.
func rowLaunchSettings(r map[string]interface{}) (aws.LaunchSettings, error) {
	if s, ok := r[detachedLaunchSettingsKey].(aws.LaunchSettings); ok {
		return s, nil
	}
	return aws.DecodeLaunchSettings(r[detachedLaunchSettingsKey])
}

// normalizeDetachedUserData stores the members' user data once, under
// defaults, when every member's is the same, else per row, as
// seedDetachedBootstrap does; and refuses params any of whose user data
// still names deploy.PendingAddr, which no proxy has.
func normalizeDetachedUserData(pf *ParamFileFormat) error {
	if len(pf.Params) == 0 {
		return nil
	}
	uds := make([]string, len(pf.Params))
	same := true
	for i, r := range pf.Params {
		uds[i] = stringParam(r, detachedUserDataKey)
		if uds[i] == "" {
			uds[i] = stringParam(pf.Defaults, detachedUserDataKey)
		}
		if deploy.UserDataNames(uds[i], deploy.PendingAddr) {
			return fmt.Errorf("parameter set %d: its user data was never bound to an egress proxy", i)
		}
		same = same && uds[i] == uds[0]
	}
	if same {
		pf.Defaults[detachedUserDataKey] = uds[0]
		for _, r := range pf.Params {
			delete(r, detachedUserDataKey)
		}
		return nil
	}
	delete(pf.Defaults, detachedUserDataKey)
	for i, r := range pf.Params {
		r[detachedUserDataKey] = uds[i]
	}
	return nil
}
