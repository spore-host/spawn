package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
)

// The spawn egress proxy on detached sweeps (#1515 PR B).
//
// `spawn launch --detach --no-public-ip --bootstrap-egress proxy` finds or
// creates the egress proxy of each (region, VPC) its members count on, binds
// those members to it (their user data's proxy address, their clients
// group), marks each with the VPC (egressProxyVPCParam), and records each VPC
// as a deploy.SweepTarget in the sweep's record. The proxy may be gone by the
// time a member is launched, hours or days later: it idles out, rotates under
// the reaper's ceiling, or is deleted. So before a member of such a VPC is
// launched, its target is ensured (deploy.EnsureSweep):
//
//   - reusable proxy: the member is launched, rebound to the proxy that is
//     ready now if it is not the one it was rendered for;
//   - none: one is launched from the CLI's recorded inputs, the target is
//     "creating", and the member is held (with every member queued after it:
//     the launch queue is ordered), to be looked at again on the next poll,
//     in this invocation or the next;
//   - not ready within deploy.ReadyTimeout: it is terminated, and the VPC's
//     members are failed, each with the recorded reason, as the queue
//     reaches them.
//
// Each look is bounded: no invocation waits for a proxy to boot.

// egressProxyVPCParam is the param key naming the VPC whose egress proxy a
// member counts on. It must equal detachedEgressProxyVPCKey in
// cmd/launch_sweep_detached_egress_proxy.go; TestDetachedSweepSeedsEveryKeyTheLambdaReads
// holds the two together.
const egressProxyVPCParam = "spawn_egress_proxy_vpc"

// Seams, so the gate can be driven without AWS.
var (
	ensureEgressProxy      = deploy.EnsureSweep
	egressProxyDeps        = memberProxyDeps
	checkMemberProxyEgress = deploy.CheckMemberEgress
)

// memberConfigs are the assumed-role configs, by account|region.
var memberConfigs sync.Map

var errNoMemberCredentials = errors.New("no member-account credentials")

// rememberMemberConfig keeps the assumed-role config the polling loops'
// clients were built from, for the egress proxy's EC2 and SSM calls in the
// same account and region. Each invocation builds its clients, and so
// replaces the config, before it launches anything.
func rememberMemberConfig(accountID, region string, cfg aws.Config) {
	memberConfigs.Store(accountID+"|"+region, cfg)
}

// memberProxyDeps is the egress proxy's AWS in the sweep's account and
// region, on SpawnSweepCrossAccountRole: its EC2, and its SSM for the
// readiness probe.
func memberProxyDeps(_ context.Context, state *SweepRecord, region string) (deploy.Deps, error) {
	v, ok := memberConfigs.Load(state.AWSAccountID + "|" + region)
	if !ok {
		return deploy.Deps{}, fmt.Errorf("%w for %s in %s", errNoMemberCredentials, state.AWSAccountID, region)
	}
	cfg := v.(aws.Config)
	return deploy.Deps{EC2: ec2.NewFromConfig(cfg), Prober: spawnaws.NewClientFromConfig(cfg)}, nil
}

// admission is the gate's verdict on one member.
type admission int

const (
	admitLaunch admission = iota
	// admitHold: launch neither it nor any member queued after it this pass.
	admitHold
	// admitFail: record it failed with the reason, and go on.
	admitFail
)

// egressGate ensures a pass's egress proxies: each target at most once per
// pass (one launch batch), however many of its members the pass reaches.
// Safe for the multi-region path's per-region goroutines; each target is
// one region's.
type egressGate struct {
	state *SweepRecord
	mu    sync.Mutex
	done  map[int]*gateTarget
	// changed: a target was updated, so the record must be saved.
	changed bool
}

type gateTarget struct {
	mu   sync.Mutex
	once bool
	step deploy.SweepStep
	deps deploy.Deps
	err  error
	// checked are the member group sets whose egress to the current proxy
	// was checked this pass, and the answer.
	checked map[string]error
}

func newEgressGate(state *SweepRecord) *egressGate {
	return &egressGate{state: state, done: map[int]*gateTarget{}}
}

// targetIndex is the record's target for (region, vpc), or -1.
func (g *egressGate) targetIndex(region, vpc string) int {
	for i, t := range g.state.EgressProxies {
		if t.Region == region && t.VPCID == vpc {
			return i
		}
	}
	return -1
}

func (g *egressGate) target(i int) *gateTarget {
	g.mu.Lock()
	defer g.mu.Unlock()
	gt, ok := g.done[i]
	if !ok {
		gt = &gateTarget{checked: map[string]error{}}
		g.done[i] = gt
	}
	return gt
}

// Changed reports whether a target was updated this pass.
func (g *egressGate) Changed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.changed
}

// admit decides member paramIndex's launch in region. config is its merged
// params; on admitLaunch it is rebound, in place, to the proxy that is ready
// now. A member that counts on no proxy is admitted unchanged.
func (g *egressGate) admit(ctx context.Context, region string, config map[string]interface{}, paramIndex int) (admission, string) {
	vpc := getStringParam(config, egressProxyVPCParam, "")
	if vpc == "" {
		return admitLaunch, ""
	}
	i := g.targetIndex(region, vpc)
	if i < 0 {
		return admitFail, fmt.Sprintf("param set %d counts on the egress proxy of %s in %s, but the sweep's record "+
			"carries no egress proxy for it, so its bootstrap could not reach the hosts it needs; it was not launched (#1515)",
			paramIndex, vpc, region)
	}
	gt := g.target(i)
	gt.mu.Lock()
	defer gt.mu.Unlock()
	t := &g.state.EgressProxies[i]
	if !gt.once {
		gt.once = true
		gt.deps, gt.err = egressProxyDeps(ctx, g.state, region)
		if gt.err == nil {
			gt.step = ensureEgressProxy(ctx, gt.deps, t)
			if gt.step.Changed {
				g.mu.Lock()
				g.changed = true
				g.mu.Unlock()
			}
			if gt.step.Reason != "" {
				log.Printf("Sweep %s: egress proxy of %s in %s: %s: %s", g.state.SweepID, vpc, region, gt.step.Outcome, gt.step.Reason)
			}
		}
	}
	if gt.err != nil {
		return admitFail, fmt.Sprintf("param set %d: the egress proxy of %s in %s cannot be checked: %v; it was not launched", paramIndex, vpc, region, gt.err)
	}
	switch gt.step.Outcome {
	case deploy.SweepHold:
		return admitHold, gt.step.Reason
	case deploy.SweepFail:
		return admitFail, fmt.Sprintf("param set %d was not launched: %s (#1515)", paramIndex, gt.step.Reason)
	}
	if err := gt.rebind(ctx, *t, config); err != nil {
		return admitFail, fmt.Sprintf("param set %d: %v; it was not launched (#1515)", paramIndex, err)
	}
	return admitLaunch, ""
}

// rebind points the member's user data and groups at t's current proxy, and,
// when that is not the proxy it was rendered for, checks the member's own
// groups let it reach the new one (the CLI checked the old).
func (gt *gateTarget) rebind(ctx context.Context, t deploy.SweepTarget, config map[string]interface{}) error {
	from, to := t.Rendered(), t.Current()
	if from == to {
		return nil
	}
	ud, err := deploy.RebindUserData(getStringParam(config, userDataParam, ""), from.Addr, to.Addr)
	if err != nil {
		return err
	}
	raw, ok := getRawParam(config, launchSettingsParam)
	if !ok {
		return fmt.Errorf("it counts on the egress proxy but carries no %s to give the proxy's clients group", launchSettingsParam)
	}
	settings, err := spawnaws.DecodeLaunchSettings(raw)
	if err != nil {
		return err
	}
	if settings.SecurityGroupIDs, err = deploy.RebindGroups(settings.SecurityGroupIDs, from.ClientsGroupID, to.ClientsGroupID); err != nil {
		return err
	}
	key := strings.Join(settings.SecurityGroupIDs, ",")
	checked, done := gt.checked[key]
	if !done {
		ip, _, _ := net.SplitHostPort(to.Addr)
		checked = checkMemberProxyEgress(ctx, gt.deps.EC2, settings.SecurityGroupIDs, deploy.TargetGroups(t), ip)
		gt.checked[key] = checked
	}
	if checked != nil {
		return checked
	}
	config[userDataParam] = ud
	config[launchSettingsParam] = settings
	return nil
}

// creatingEgressProxies are the proxies a cancel may terminate, by region:
// the ones this orchestrator launched for the sweep that are not yet ready.
// A ready proxy, or one the CLI made, may be serving other sweeps and
// launches in the VPC; it ends at its idle period or deadline.
func creatingEgressProxies(state *SweepRecord) map[string][]string {
	out := map[string][]string{}
	for _, t := range state.EgressProxies {
		if ids := t.CreatingProxies(); len(ids) > 0 {
			out[t.Region] = append(out[t.Region], ids...)
		}
	}
	return out
}
