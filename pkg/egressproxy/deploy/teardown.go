package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/egressproxy"
)

// Teardown and rotation for `spawn egress-proxy delete` and `upgrade` (#1515
// phase 5). Both terminate only instances [Verify] accepts as the VPC's proxy:
// its tag names it, but an instance can write that tag on itself, so a tagged
// instance that fails Verify is refused and left alone, with the reason.
const (
	// TerminateTimeout bounds the wait for proxies to reach terminated.
	TerminateTimeout = 5 * time.Minute
	// GroupDeleteTimeout bounds the retries of a group delete EC2 refuses
	// with DependencyViolation: a terminated instance's interface is
	// released a little after the instance reports terminated.
	GroupDeleteTimeout = 2 * time.Minute
	// teardownPoll is the gap between polls of both waits.
	teardownPoll = 10 * time.Second
)

// DeletePlan is what delete does in one VPC. Building it reads; it writes
// nothing.
type DeletePlan struct {
	VPCID string
	// Terminate are spawn's proxies ([Verify]) that are not already shutting
	// down; Await are those that are, which delete only waits for.
	Terminate, Await []Proxy
	// Refused are instances tagged as the VPC's proxy that fail [Verify].
	// Nothing touches them.
	Refused []Rejection
	Groups  GroupsState
}

// PlanDelete is the delete plan of inv. Version and remaining life do not
// matter here: any proxy spawn can tell it launched is ended.
func PlanDelete(inv VPCInventory) DeletePlan {
	plan := DeletePlan{VPCID: inv.VPCID, Groups: inv.Groups}
	for _, l := range inv.Proxies {
		switch {
		case l.NotSpawns != "":
			plan.Refused = append(plan.Refused, Rejection{InstanceID: l.Proxy.InstanceID, Reason: l.NotSpawns})
		case l.Proxy.State == string(types.InstanceStateNameShuttingDown):
			plan.Await = append(plan.Await, l.Proxy)
		default:
			plan.Terminate = append(plan.Terminate, l.Proxy)
		}
	}
	return plan
}

// Empty reports whether the plan has nothing to end or delete.
func (p DeletePlan) Empty() bool {
	return len(p.Terminate) == 0 && len(p.Await) == 0 && !p.Groups.Proxy.Owned() && !p.Groups.Clients.Owned()
}

// DeleteResult is what [Delete] did.
type DeleteResult struct {
	// Terminated are the proxies it terminated.
	Terminated []string
	// DeletedGroups are the group IDs it deleted.
	DeletedGroups []string
	// Kept is the group it left because an instance still carries it, and
	// every group after it in [DeleteGroups]'s order; nil when none.
	Kept *GroupInUseError
}

// Delete carries out plan: it terminates plan.Terminate, waits up to timeout
// ([TerminateTimeout] when zero) for them and plan.Await to be terminated,
// then deletes the VPC's groups with [DeleteGroups], the proxy group first. A
// group an instance still carries is kept and reported in Kept, not an error:
// the clients group stays while members carry it, and the TTL reaper's
// managed-network sweep collects it once nothing does. A refused instance is
// never touched; if it carries the proxy group, that group is kept too.
func Delete(ctx context.Context, d Deps, plan DeletePlan, timeout time.Duration) (DeleteResult, error) {
	var res DeleteResult
	var wait []string
	for _, p := range plan.Terminate {
		if err := Terminate(ctx, d.EC2, p.InstanceID); err != nil {
			return res, err
		}
		res.Terminated = append(res.Terminated, p.InstanceID)
		wait = append(wait, p.InstanceID)
	}
	for _, p := range plan.Await {
		wait = append(wait, p.InstanceID)
	}
	if err := WaitTerminated(ctx, d, wait, timeout); err != nil {
		return res, err
	}
	deleted, err := deleteGroupsRetrying(ctx, d, plan.VPCID, GroupDeleteTimeout)
	res.DeletedGroups = deleted
	var inUse *GroupInUseError
	if errors.As(err, &inUse) {
		res.Kept = inUse
		return res, nil
	}
	return res, err
}

// ErrTerminateTimeout is wrapped when proxies did not reach terminated in time.
var ErrTerminateTimeout = errors.New("the egress proxy did not finish terminating")

// WaitTerminated waits, up to timeout ([TerminateTimeout] when zero), until
// every instance in ids is terminated or no longer listed.
func WaitTerminated(ctx context.Context, d Deps, ids []string, timeout time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = TerminateTimeout
	}
	deadline := d.now().Add(timeout)
	for {
		left, err := notTerminated(ctx, d.EC2, ids)
		if err == nil && len(left) == 0 {
			return nil
		}
		if !d.now().Before(deadline) {
			if err != nil {
				return fmt.Errorf("%w within %v: %v", ErrTerminateTimeout, timeout, err)
			}
			return fmt.Errorf("%w within %v: %s", ErrTerminateTimeout, timeout, join(left))
		}
		if err := d.sleep(ctx, minDuration(teardownPoll, deadline.Sub(d.now()))); err != nil {
			return fmt.Errorf("%w: %v", ErrTerminateTimeout, err)
		}
	}
}

// notTerminated are the instances of ids that are listed in a state other
// than terminated, as "id (state)".
func notTerminated(ctx context.Context, api EC2, ids []string) ([]string, error) {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		// Long gone: EC2 no longer knows them.
		if strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
			return nil, nil
		}
		return nil, fmt.Errorf("describe %s: %w", join(ids), err)
	}
	var left []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if s := stateOf(inst); s != string(types.InstanceStateNameTerminated) {
				left = append(left, fmt.Sprintf("%s (%s)", aws.ToString(inst.InstanceId), s))
			}
		}
	}
	return left, nil
}

// deleteGroupsRetrying is [DeleteGroups], retried while EC2 refuses a delete
// with DependencyViolation (an interface still being released), up to timeout.
func deleteGroupsRetrying(ctx context.Context, d Deps, vpcID string, timeout time.Duration) ([]string, error) {
	deadline := d.now().Add(timeout)
	var all []string
	for {
		deleted, err := DeleteGroups(ctx, d.EC2, vpcID)
		all = append(all, deleted...)
		if err == nil || !strings.Contains(err.Error(), "DependencyViolation") || !d.now().Before(deadline) {
			return all, err
		}
		if serr := d.sleep(ctx, minDuration(teardownPoll, deadline.Sub(d.now()))); serr != nil {
			return all, err
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}

// UpgradePlan is what upgrade does in one VPC. Building it reads; it writes
// nothing.
type UpgradePlan struct {
	VPCID string
	// Current is a proxy find-or-create would reuse as it stands: at this
	// spawn's version with life left. Upgrade then does nothing.
	Current *Listed
	// Replace are spawn's proxies ([Verify]) that are not current and not
	// already shutting down: terminated once the new proxy is ready, unless
	// they are kept.
	Replace []Listed
	// Refused are instances tagged as the VPC's proxy that fail [Verify].
	Refused []Rejection
}

// PlanUpgrade is the upgrade plan of inv.
func PlanUpgrade(inv VPCInventory) UpgradePlan {
	plan := UpgradePlan{VPCID: inv.VPCID}
	for i, l := range inv.Proxies {
		switch {
		case l.NotSpawns != "":
			plan.Refused = append(plan.Refused, Rejection{InstanceID: l.Proxy.InstanceID, Reason: l.NotSpawns})
		case l.NotReused == "":
			if plan.Current == nil {
				plan.Current = &inv.Proxies[i]
			}
		case l.Proxy.State != string(types.InstanceStateNameShuttingDown):
			plan.Replace = append(plan.Replace, l)
		}
	}
	return plan
}

// Idle is the idle period a replacement proxy gets: the one the newest proxy
// being replaced published ([egressproxy.IdleTag]) when it is valid, so an
// upgrade keeps what the launch chose, else [egressproxy.DefaultIdle].
func (p UpgradePlan) Idle() time.Duration {
	for _, l := range p.Replace {
		if l.Idle > 0 && egressproxy.ValidateIdle(l.Idle, egressproxy.MaxLife) == nil {
			return l.Idle
		}
	}
	return egressproxy.DefaultIdle
}

// UpgradeResult is what [Upgrade] did.
type UpgradeResult struct {
	// New is the proxy find-or-create returned: launched by this call
	// (Created), or one that became reusable meanwhile.
	New Proxy
	// Terminated are the replaced proxies it terminated; Kept those it left
	// running (--keep-old).
	Terminated, Kept []string
}

// Upgrade replaces plan's proxies with a proxy at r.Version: [FindOrCreate],
// which launches one (none in plan is reusable) and waits until it is ready,
// then [Terminate] for each of plan.Replace unless keepOld. If find-or-create
// fails, nothing is terminated. A member mid-bootstrap through a replaced
// proxy can fail its fetch; one that has finished its bootstrap no longer uses
// the proxy.
func Upgrade(ctx context.Context, d Deps, r Request, plan UpgradePlan, keepOld bool) (UpgradeResult, error) {
	var res UpgradeResult
	fc, err := FindOrCreate(ctx, d, r)
	if err != nil {
		return res, err
	}
	res.New = fc.Proxy
	for _, l := range plan.Replace {
		id := l.Proxy.InstanceID
		if id == res.New.InstanceID {
			continue
		}
		if keepOld {
			res.Kept = append(res.Kept, id)
			continue
		}
		if err := Terminate(ctx, d.EC2, id); err != nil {
			return res, err
		}
		res.Terminated = append(res.Terminated, id)
	}
	return res, nil
}
