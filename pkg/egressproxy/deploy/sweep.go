package deploy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/egressproxy/sweepproxy"
	"github.com/spore-host/spawn/pkg/launcher"
)

// The egress proxy of a detached parameter sweep (#1515 PR B).
//
// A detached sweep's members are launched by the sweep orchestrator Lambda,
// possibly days after `spawn launch --detach` returned. The CLI finds or
// creates each (region, VPC)'s proxy at submit time, as a foreground sweep
// does, and records it as a [SweepTarget] in the sweep's record. Before each
// launch the orchestrator checks the proxy again ([EnsureSweep]); if it has
// gone, or is no longer reusable (near its deadline, another version), the
// orchestrator launches a new one from the same recorded inputs, and waits for
// it across polls and re-invocations, one bounded look per poll, holding that
// VPC's members meanwhile.
//
// The pieces are the CLI's: [EnsureGroups], [Find] (that is, [Evaluate]),
// [Create] (that is, [RunInput]) and [CheckReady]. Only the waiting differs:
// [FindOrCreate] blocks in [WaitReady]; [EnsureSweep] returns, and is called
// again.
//
// A member's user data and security groups name its VPC's proxy: its address
// (the bootstrap's scoped http_proxy) and the clients group. The CLI renders
// them with the proxy of submit time and records that as the target's
// rendered [Binding]; a member launched when the target's current binding
// differs is rebound ([RebindUserData], [RebindGroups]) just before it is
// launched. So a member always names the proxy that is ready when it starts.

// The record's types (pkg/egressproxy/sweepproxy, which pkg/sweep's record
// carries without linking this package).
type (
	// SweepTarget is one (region, VPC) of a detached sweep whose members
	// count on the egress proxy, as the sweep's record carries it.
	SweepTarget = sweepproxy.Target
	// Binding is what a member names of its VPC's proxy.
	Binding = sweepproxy.Binding
)

// Sweep target phases.
const (
	PhaseReady    = sweepproxy.PhaseReady
	PhaseCreating = sweepproxy.PhaseCreating
	PhaseFailed   = sweepproxy.PhaseFailed
)

// PendingAddr is the proxy address a detached member's user data is
// rendered with before its VPC's proxy is known: the CLI renders every
// member before the confirmation prompt, and creates nothing before it, then
// rebinds each member to its proxy's real address ([RebindUserData]) before
// the params are uploaded. 192.0.2.1 is TEST-NET-1 (RFC 5737), which no host
// has, so no rendered text means it but the bootstrap's proxy URL. Uploaded
// params never carry it.
var PendingAddr = net.JoinHostPort("192.0.2.1", strconv.Itoa(egressproxy.Port))

// TargetGroups are t's two groups as last ensured.
func TargetGroups(t SweepTarget) Groups {
	return Groups{ClientsID: t.ClientsGroupID, ProxyID: t.ProxyGroupID}
}

// SetReady records p, ready, as t's proxy with groups g.
func SetReady(t *SweepTarget, p Proxy, g Groups) {
	t.Phase, t.ProxyID, t.ProxyAddr = PhaseReady, p.InstanceID, p.Addr()
	t.ClientsGroupID, t.ProxyGroupID = g.ClientsID, g.ProxyID
	t.CreatingSince, t.ErrorSince, t.LastError = time.Time{}, time.Time{}, ""
}

// SweepSubnets are publics as a target records them.
func SweepSubnets(publics []spawnaws.PublicSubnet) []sweepproxy.Subnet {
	out := make([]sweepproxy.Subnet, 0, len(publics))
	for _, p := range publics {
		out = append(out, sweepproxy.Subnet{SubnetID: p.SubnetID, AvailabilityZone: p.AvailabilityZone})
	}
	return out
}

func publicSubnets(subnets []sweepproxy.Subnet) []spawnaws.PublicSubnet {
	out := make([]spawnaws.PublicSubnet, 0, len(subnets))
	for _, s := range subnets {
		out = append(out, spawnaws.PublicSubnet{SubnetID: s.SubnetID, AvailabilityZone: s.AvailabilityZone})
	}
	return out
}

// BaseTagMap is a proxy's base tags as a [SweepTarget] records them: without
// spawn:launch-time, which is the launch's own, nor the lifetime and identity
// tags [ProxyTags] writes.
func BaseTagMap(tags []types.Tag) map[string]string {
	drop := map[string]bool{"spawn:launch-time": true, "spawn:ttl": true, "spawn:ttl-deadline": true, "Name": true, ProxyTag: true}
	out := map[string]string{}
	for _, t := range tags {
		if k := aws.ToString(t.Key); !drop[k] {
			out[k] = aws.ToString(t.Value)
		}
	}
	return out
}

func tagsOf(m map[string]string) []types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]types.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

// Sweep step outcomes.
const (
	// SweepLaunch: the proxy is ready; launch the members, bound to
	// [SweepTarget.Current].
	SweepLaunch = "launch"
	// SweepHold: a proxy is being made ready; launch none of the target's
	// members yet, and look again on a later poll.
	SweepHold = "hold"
	// SweepFail: no proxy could be made ready; fail the target's members
	// with [SweepStep.Reason].
	SweepFail = "fail"
)

// SweepStep is one [EnsureSweep]'s outcome.
type SweepStep struct {
	Outcome string
	// Reason says why the members are held or failed; for SweepLaunch, what
	// changed, if anything ("" when the proxy was simply reused).
	Reason string
	// Changed reports that the target was updated, so the record must be
	// saved.
	Changed bool
}

// EnsureSweep is one look at t's proxy before its members are launched: it
// never waits. Each call makes at most the reads of [EnsureGroups] and
// [Find], one [CheckReady] per proxy it has not checked before (each bounded
// by the probe's timeout), and one [Create]; the caller calls it again on its
// next poll while the outcome is SweepHold.
//
//   - PhaseFailed: SweepFail, always.
//   - PhaseCreating: one [CheckReady] of the proxy being created. Ready: the
//     target is ready, SweepLaunch. Never going to be ready, or still not
//     ready [ReadyTimeout] after it was launched: it is terminated and the
//     target fails. Else SweepHold.
//   - Otherwise: the groups are ensured (re-created if they were deleted,
//     their rules reconciled) and the VPC's proxies found and evaluated
//     against t.Version ([Find]). The recorded proxy, still reusable, is used
//     as it is. Another reusable proxy is used if one look finds it ready.
//     Failing both, a new proxy is launched from t's recorded inputs and the
//     target is creating: SweepHold.
//
// An AWS error before a proxy is launched holds the members and is retried on
// the next call, up to [ReadyTimeout] after the first; an authorization error
// fails the target at once, since no retry will be allowed.
//
// Like [FindOrCreate], it terminates only a proxy it launched itself and that
// never became ready: a proxy that is not reused is left to serve whoever uses
// it, until its idle period or deadline.
func EnsureSweep(ctx context.Context, d Deps, t *SweepTarget) SweepStep {
	switch t.Phase {
	case PhaseFailed:
		return SweepStep{Outcome: SweepFail, Reason: t.FailReason}
	case PhaseCreating:
		return pollCreating(ctx, d, t)
	}
	now := d.now()
	groups, err := EnsureGroups(ctx, d.EC2, t.VPCID, now)
	if err != nil {
		return awsError(t, now, fmt.Errorf("the egress proxy's security groups in %s: %w", t.VPCID, err))
	}
	reusable, rejected, err := Find(ctx, d.EC2, Want{
		VPCID: t.VPCID, Version: t.Version, ProxyGroupID: groups.ProxyID,
		InstanceProfile: t.InstanceProfile, Now: now,
	})
	if err != nil {
		return awsError(t, now, err)
	}
	for _, p := range reusable {
		if t.Phase == PhaseReady && p.InstanceID == t.ProxyID {
			changed := t.ProxyAddr != p.Addr() || TargetGroups(*t) != groups || !t.ErrorSince.IsZero()
			SetReady(t, p, groups)
			return SweepStep{Outcome: SweepLaunch, Changed: changed}
		}
	}
	var notes []string
	if t.ProxyID != "" {
		notes = append(notes, fmt.Sprintf("egress proxy %s is no longer reusable (%s)", t.ProxyID, rejectionOf(t.ProxyID, rejected)))
	}
	for _, p := range reusable {
		ready, ok, why, err := CheckReady(ctx, d, t.Region, p)
		if ok {
			SetReady(t, ready, groups)
			return SweepStep{Outcome: SweepLaunch, Changed: true,
				Reason: strings.Join(append(notes, "using egress proxy "+ready.InstanceID), "; ")}
		}
		if err != nil {
			why = err.Error()
		}
		// Not ours to end: another launch's, maybe still starting.
		notes = append(notes, fmt.Sprintf("egress proxy %s not used: %s", p.InstanceID, why))
	}

	spec := Spec{Region: t.Region, VPCID: t.VPCID, AvailabilityZone: t.AvailabilityZone,
		PublicSubnets: publicSubnets(t.PublicSubnets), Groups: groups, AMI: t.AMI, InstanceProfile: t.InstanceProfile,
		UserData: t.UserData, BaseTags: tagsOf(t.BaseTags)}
	created, err := Create(ctx, d, spec)
	if err != nil {
		return awsError(t, now, err)
	}
	t.Phase, t.ProxyID, t.ProxyAddr = PhaseCreating, created.InstanceID, ""
	t.ClientsGroupID, t.ProxyGroupID = groups.ClientsID, groups.ProxyID
	t.CreatingSince, t.ErrorSince, t.LastError = now, time.Time{}, ""
	t.Created = append(t.Created, created.InstanceID)
	notes = append(notes, fmt.Sprintf("launched egress proxy %s in %s; holding the members of %s until it is ready (up to %v)",
		created.InstanceID, created.SubnetID, t.VPCID, ReadyTimeout))
	return SweepStep{Outcome: SweepHold, Changed: true, Reason: strings.Join(notes, "; ")}
}

func rejectionOf(id string, rejected []Rejection) string {
	for _, r := range rejected {
		if r.InstanceID == id {
			return r.Reason
		}
	}
	return "it is not pending or running"
}

// pollCreating is EnsureSweep's PhaseCreating: one look.
func pollCreating(ctx context.Context, d Deps, t *SweepTarget) SweepStep {
	p, ok, why, err := CheckReady(ctx, d, t.Region, Proxy{InstanceID: t.ProxyID})
	if ok {
		SetReady(t, p, TargetGroups(*t))
		return SweepStep{Outcome: SweepLaunch, Changed: true, Reason: "egress proxy " + p.InstanceID + " is ready"}
	}
	elapsed := d.now().Sub(t.CreatingSince)
	if err == nil && elapsed < ReadyTimeout {
		return SweepStep{Outcome: SweepHold, Reason: fmt.Sprintf("egress proxy %s is not ready after %v: %s",
			t.ProxyID, elapsed.Round(time.Second), why)}
	}
	if err == nil {
		err = fmt.Errorf("%w within %v: %s: %s", ErrNotReady, ReadyTimeout, t.ProxyID, why)
	}
	reason := fmt.Sprintf("the egress proxy the sweep orchestrator launched for %s in %s did not become ready: %v", t.VPCID, t.Region, err)
	// The members of this VPC are failed whether or not the terminate takes:
	// the proxy's deadline and the TTL reaper end it otherwise.
	if terr := Terminate(context.WithoutCancel(ctx), d.EC2, t.ProxyID); terr != nil {
		reason += fmt.Sprintf("; and it could not be terminated (%v): its idle period, deadline and the TTL reaper end it", terr)
	} else {
		reason += "; terminated it"
	}
	fail(t, reason)
	return SweepStep{Outcome: SweepFail, Changed: true, Reason: reason}
}

func fail(t *SweepTarget, reason string) {
	t.Phase, t.FailReason, t.ProxyAddr = PhaseFailed, reason, ""
}

// awsError holds the members on an AWS error, until it has lasted
// [ReadyTimeout]; an authorization error fails the target at once.
func awsError(t *SweepTarget, now time.Time, err error) SweepStep {
	if t.ErrorSince.IsZero() {
		t.ErrorSince = now
	}
	t.LastError = err.Error()
	if spawnaws.IsAccessDenied(err) || now.Sub(t.ErrorSince) >= ReadyTimeout {
		reason := fmt.Sprintf("the sweep orchestrator could not make an egress proxy ready for %s in %s: %v", t.VPCID, t.Region, err)
		fail(t, reason)
		return SweepStep{Outcome: SweepFail, Changed: true, Reason: reason}
	}
	return SweepStep{Outcome: SweepHold, Changed: true, Reason: "retrying: " + err.Error()}
}

// RebindUserData rewrites a member's encoded Linux user data (gzip, then
// base64: [launcher.EncodeLinuxUserData]) from naming the proxy at from to
// naming the one at to. Every occurrence of from's proxy URL is replaced; the
// bootstrap names it only there (pkg/aws EgressProxyEnv). It refuses user
// data that never names from, which was not rendered for that proxy, and a
// result over EC2's limit. Equal addresses return encoded unchanged.
func RebindUserData(encoded, from, to string) (string, error) {
	if from == to {
		return encoded, nil
	}
	fromURL, err := spawnaws.EgressProxyURL(from)
	if err != nil {
		return "", err
	}
	toURL, err := spawnaws.EgressProxyURL(to)
	if err != nil {
		return "", err
	}
	script, err := decodeLinuxUserData(encoded)
	if err != nil {
		return "", err
	}
	out, n := replaceURL(script, fromURL, toURL)
	if n == 0 {
		return "", fmt.Errorf("the member's user data does not name the egress proxy %s it was recorded with", from)
	}
	ud := launcher.EncodeLinuxUserData(out)
	if err := spawnaws.CheckUserDataSize(ud); err != nil {
		return "", err
	}
	return ud, nil
}

// replaceURL replaces each occurrence of url not followed by a digit (so a
// longer port is not matched), and counts them.
func replaceURL(s, url, with string) (string, int) {
	var b strings.Builder
	n := 0
	for {
		i := strings.Index(s, url)
		if i < 0 {
			b.WriteString(s)
			return b.String(), n
		}
		end := i + len(url)
		b.WriteString(s[:i])
		if end < len(s) && s[end] >= '0' && s[end] <= '9' {
			b.WriteString(url)
		} else {
			b.WriteString(with)
			n++
		}
		s = s[end:]
	}
}

// UserDataNames reports whether encoded Linux user data names the proxy at
// addr.
func UserDataNames(encoded, addr string) bool {
	url, err := spawnaws.EgressProxyURL(addr)
	if err != nil {
		return false
	}
	script, err := decodeLinuxUserData(encoded)
	if err != nil {
		return false
	}
	_, n := replaceURL(script, url, url)
	return n > 0
}

func decodeLinuxUserData(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("the member's user data is not base64: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("the member's user data is not gzip (a Linux bootstrap): %w", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("the member's user data is not gzip (a Linux bootstrap): %w", err)
	}
	return string(body), nil
}

// RebindGroups is a member's security groups with the clients group from
// replaced by to. It refuses groups that do not carry from.
func RebindGroups(groups []string, from, to string) ([]string, error) {
	out := make([]string, 0, len(groups))
	found := false
	for _, g := range groups {
		if g == from {
			found = true
			g = to
		}
		out = append(out, g)
	}
	if !found {
		return nil, fmt.Errorf("the member's security groups %v do not carry the egress proxy's clients group %s it was recorded with", groups, from)
	}
	return out, nil
}
