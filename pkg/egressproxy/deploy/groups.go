package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
)

// Groups are a VPC's two egress-proxy security groups.
//
//   - Clients (spawn-egress-clients-<vpc>) has no rules at all, not even the
//     allow-all egress EC2 gives a new group, so attaching it to a member
//     widens nothing: it is only a name the proxy's ingress rule can point at.
//     A member's egress to the proxy comes from its own groups.
//   - Proxy (spawn-egress-proxy-<vpc>) admits TCP [egressproxy.Port] from
//     Clients and nothing else, and sends only TCP 443 and 80 anywhere. DNS
//     needs no rule: the VPC resolver (and IMDS and the time service) are
//     link-local, and security groups do not filter them.
//
// Both carry spawn:managed=true, a spawn:created stamp and their purpose
// (pkg/aws LifecycleTags), so the TTL reaper's managed-network sweep deletes
// either once no instance has used it for its grace period, and [DeleteGroups]
// is their explicit deleter. Neither is signed: nothing spawn creates carries a
// signature yet (#76), so a group is spawn's when it has these tags and the
// name spawn gives it in that VPC.
type Groups struct {
	ClientsID, ProxyID string
}

// rule is one security group permission reduced to a single source or
// destination, so two rule sets compare by value.
type rule struct {
	proto      string
	from, to   int32
	cidr       string
	cidr6      string
	group      string
	prefixList string
}

func (r rule) String() string {
	src := r.cidr + r.cidr6 + r.group + r.prefixList
	if r.proto == "-1" {
		return "all traffic " + src
	}
	if r.from == r.to {
		return fmt.Sprintf("%s %d %s", r.proto, r.from, src)
	}
	return fmt.Sprintf("%s %d-%d %s", r.proto, r.from, r.to, src)
}

// permission is r as the EC2 API takes it.
func (r rule) permission(description string) types.IpPermission {
	p := types.IpPermission{IpProtocol: aws.String(r.proto)}
	if r.proto != "-1" {
		p.FromPort, p.ToPort = aws.Int32(r.from), aws.Int32(r.to)
	}
	var desc *string
	if description != "" {
		desc = aws.String(description)
	}
	switch {
	case r.cidr != "":
		p.IpRanges = []types.IpRange{{CidrIp: aws.String(r.cidr), Description: desc}}
	case r.cidr6 != "":
		p.Ipv6Ranges = []types.Ipv6Range{{CidrIpv6: aws.String(r.cidr6), Description: desc}}
	case r.group != "":
		p.UserIdGroupPairs = []types.UserIdGroupPair{{GroupId: aws.String(r.group), Description: desc}}
	case r.prefixList != "":
		p.PrefixListIds = []types.PrefixListId{{PrefixListId: aws.String(r.prefixList), Description: desc}}
	}
	return p
}

// rulesOf flattens EC2 permissions into rules.
func rulesOf(perms []types.IpPermission) []rule {
	var out []rule
	for _, p := range perms {
		base := rule{proto: aws.ToString(p.IpProtocol)}
		if base.proto != "-1" {
			base.from, base.to = aws.ToInt32(p.FromPort), aws.ToInt32(p.ToPort)
		}
		for _, x := range p.IpRanges {
			r := base
			r.cidr = aws.ToString(x.CidrIp)
			out = append(out, r)
		}
		for _, x := range p.Ipv6Ranges {
			r := base
			r.cidr6 = aws.ToString(x.CidrIpv6)
			out = append(out, r)
		}
		for _, x := range p.UserIdGroupPairs {
			r := base
			r.group = aws.ToString(x.GroupId)
			out = append(out, r)
		}
		for _, x := range p.PrefixListIds {
			r := base
			r.prefixList = aws.ToString(x.PrefixListId)
			out = append(out, r)
		}
	}
	return out
}

// ProxyIngress is every inbound rule the proxy's group has: the proxy port,
// from the clients group only.
func ProxyIngress(g Groups) []types.IpPermission {
	return permissions(proxyIngress(g), "spawn egress proxy clients")
}

// ProxyEgress is every outbound rule the proxy's group has: HTTPS and HTTP,
// which are all the proxy forwards (pkg/egressproxy: CONNECT to 443, GET and
// HEAD to 80), and what spored and the SSM agent reach AWS over.
func ProxyEgress() []types.IpPermission {
	return permissions(proxyEgress(), "spawn egress proxy upstream")
}

func proxyIngress(g Groups) []rule {
	return []rule{{proto: "tcp", from: egressproxy.Port, to: egressproxy.Port, group: g.ClientsID}}
}

func proxyEgress() []rule {
	return []rule{
		{proto: "tcp", from: 443, to: 443, cidr: "0.0.0.0/0"},
		{proto: "tcp", from: 80, to: 80, cidr: "0.0.0.0/0"},
	}
}

func permissions(rules []rule, description string) []types.IpPermission {
	out := make([]types.IpPermission, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.permission(description))
	}
	return out
}

// diff is what to add and what to remove to turn have into want.
func diff(have, want []rule) (add, remove []rule) {
	in := func(xs []rule, r rule) bool {
		for _, x := range xs {
			if x == r {
				return true
			}
		}
		return false
	}
	for _, w := range want {
		if !in(have, w) {
			add = append(add, w)
		}
	}
	for _, h := range have {
		if !in(want, h) {
			remove = append(remove, h)
		}
	}
	return add, remove
}

// EnsureGroups finds the VPC's two groups, creating either that is missing,
// and makes their rules exactly what [Groups] says: missing rules added, and
// any other rule (EC2's default allow-all egress, or one added by hand)
// removed. A group of spawn's name in the VPC that does not carry spawn's
// tags is refused rather than adopted.
func EnsureGroups(ctx context.Context, api EC2, vpcID string, now time.Time) (Groups, error) {
	clients, err := ensureGroup(ctx, api, vpcID, ClientsGroupName(vpcID), PurposeClients,
		"spawn egress proxy clients: no rules; the proxy admits this group", now)
	if err != nil {
		return Groups{}, err
	}
	proxy, err := ensureGroup(ctx, api, vpcID, ProxyGroupName(vpcID), PurposeProxy,
		"spawn egress proxy: tcp 3128 from its clients group; tcp 443 and 80 out", now)
	if err != nil {
		return Groups{}, err
	}
	g := Groups{ClientsID: aws.ToString(clients.GroupId), ProxyID: aws.ToString(proxy.GroupId)}
	if err := reconcile(ctx, api, clients, nil, nil); err != nil {
		return g, err
	}
	if err := reconcile(ctx, api, proxy, proxyIngress(g), proxyEgress()); err != nil {
		return g, err
	}
	return g, nil
}

// describeGroup finds name in vpcID; nil when there is none.
func describeGroup(ctx context.Context, api EC2, vpcID, name string) (*types.SecurityGroup, error) {
	out, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{Name: aws.String("group-name"), Values: []string{name}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe security group %s in %s: %w", name, vpcID, err)
	}
	if len(out.SecurityGroups) == 0 {
		return nil, nil
	}
	return &out.SecurityGroups[0], nil
}

// ErrForeignGroup is wrapped when a group of spawn's name lacks spawn's tags.
var ErrForeignGroup = errors.New("a security group of spawn's name is not spawn's")

// ownGroup reports why sg, found by name, is not spawn's group of purpose.
func ownGroup(sg types.SecurityGroup, vpcID, purpose string) error {
	tags := tagMap(sg.Tags)
	if tags["spawn:managed"] != "true" || tags["spawn:purpose"] != purpose || tags[ProxyTag] != vpcID {
		return fmt.Errorf("%w: %s (%s) in %s has spawn:managed=%q spawn:purpose=%q %s=%q, not true, %s and %s; "+
			"spawn will not adopt it", ErrForeignGroup, aws.ToString(sg.GroupName), aws.ToString(sg.GroupId), vpcID,
			tags["spawn:managed"], tags["spawn:purpose"], ProxyTag, tags[ProxyTag], purpose, vpcID)
	}
	return nil
}

func ensureGroup(ctx context.Context, api EC2, vpcID, name, purpose, description string, now time.Time) (types.SecurityGroup, error) {
	sg, err := describeGroup(ctx, api, vpcID, name)
	if err != nil {
		return types.SecurityGroup{}, err
	}
	if sg != nil {
		return *sg, ownGroup(*sg, vpcID, purpose)
	}
	created, err := api.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String(description),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeSecurityGroup,
			// LifecycleTags: the reaper's managed-network sweep finds and ages
			// it by these (TestEveryNetResourceCreationIsTagged).
			Tags: append(spawnaws.LifecycleTags(name, purpose, now),
				types.Tag{Key: aws.String(ProxyTag), Value: aws.String(vpcID)}),
		}},
	})
	if err != nil {
		// A concurrent launch made it first: use theirs.
		if strings.Contains(err.Error(), "InvalidGroup.Duplicate") {
			if sg, derr := describeGroup(ctx, api, vpcID, name); derr == nil && sg != nil {
				return *sg, ownGroup(*sg, vpcID, purpose)
			}
		}
		return types.SecurityGroup{}, fmt.Errorf("create security group %s in %s: %w", name, vpcID, err)
	}
	id := aws.ToString(created.GroupId)
	// Read it back rather than assume what EC2 put in it (its default egress).
	out, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{id}})
	if err != nil || len(out.SecurityGroups) == 0 {
		return types.SecurityGroup{}, fmt.Errorf("read back security group %s (%s): %v", name, id, err)
	}
	return out.SecurityGroups[0], nil
}

func isDuplicate(err error) bool {
	return err != nil && strings.Contains(err.Error(), "InvalidPermission.Duplicate")
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "InvalidPermission.NotFound")
}

// reconcile makes sg's rules exactly ingress and egress. Adds before removes,
// so a group being tightened is never briefly closed to what it should allow.
func reconcile(ctx context.Context, api EC2, sg types.SecurityGroup, ingress, egress []rule) error {
	id := aws.ToString(sg.GroupId)
	addIn, rmIn := diff(rulesOf(sg.IpPermissions), ingress)
	addOut, rmOut := diff(rulesOf(sg.IpPermissionsEgress), egress)
	desc := "spawn egress proxy"
	if len(addIn) > 0 {
		if _, err := api.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(id), IpPermissions: permissions(addIn, desc)}); err != nil && !isDuplicate(err) {
			return fmt.Errorf("authorize ingress on %s: %w", id, err)
		}
	}
	if len(addOut) > 0 {
		if _, err := api.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
			GroupId: aws.String(id), IpPermissions: permissions(addOut, desc)}); err != nil && !isDuplicate(err) {
			return fmt.Errorf("authorize egress on %s: %w", id, err)
		}
	}
	if len(rmIn) > 0 {
		if _, err := api.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
			GroupId: aws.String(id), IpPermissions: permissions(rmIn, "")}); err != nil && !isNotFound(err) {
			return fmt.Errorf("revoke ingress %s on %s: %w", rulesString(rmIn), id, err)
		}
	}
	if len(rmOut) > 0 {
		if _, err := api.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
			GroupId: aws.String(id), IpPermissions: permissions(rmOut, "")}); err != nil && !isNotFound(err) {
			return fmt.Errorf("revoke egress %s on %s: %w", rulesString(rmOut), id, err)
		}
	}
	return nil
}

func rulesString(rs []rule) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = r.String()
	}
	sort.Strings(s)
	return strings.Join(s, "; ")
}

// DefaultGroupID is the VPC's default security group, which a launch that
// names no group gets from EC2 implicitly. An opted-in member names the
// clients group, which would otherwise replace it, so it is named too.
func DefaultGroupID(ctx context.Context, api EC2, vpcID string) (string, error) {
	sg, err := describeGroup(ctx, api, vpcID, "default")
	if err != nil {
		return "", err
	}
	if sg == nil {
		return "", fmt.Errorf("VPC %s has no default security group", vpcID)
	}
	return aws.ToString(sg.GroupId), nil
}

// MemberGroups is the member's security groups with the clients group added:
// its own when it names any, else the VPC's default group (what EC2 would
// have given it) and the clients group.
func MemberGroups(ctx context.Context, api EC2, vpcID string, own []string, g Groups) ([]string, error) {
	if len(own) == 0 {
		def, err := DefaultGroupID(ctx, api, vpcID)
		if err != nil {
			return nil, err
		}
		own = []string{def}
	}
	for _, id := range own {
		if id == g.ClientsID {
			return own, nil
		}
	}
	return append(append([]string(nil), own...), g.ClientsID), nil
}

// ErrGroupsInUse is wrapped by [GroupInUseError]: something still uses or
// references a group, so it was kept.
var ErrGroupsInUse = errors.New("the egress proxy security groups are still in use")

// DeleteGroups is the deleter of a VPC's two groups (owner rule: every
// resource spawn creates has a named one), in one pass that waits for
// nothing. [Delete] runs the same pass, but waits for network interfaces to
// release the groups and retries a delete EC2 refuses with
// DependencyViolation.
//
// Each group is handled on its own, so one that cannot go never stops the
// other (#1651). A group is kept, as a [GroupInUseError], while an instance
// in any state but terminated carries it, while a network interface carries
// it, or while a group that is kept has a rule naming it. Before deleting,
// every rule in a group being deleted that names the other is revoked: the
// proxy group's ingress names the clients group, and EC2 will not delete a
// group another group's rule names, so without this a failed proxy-group
// delete strands the clients group too. A group already gone is not an error;
// a group of spawn's name that is not spawn's is left alone, and is one. The
// TTL reaper's managed-network sweep is the other deleter, for groups nobody
// runs this on.
//
// It returns the IDs it deleted, the groups it kept, and every error joined.
// A kept group is among the errors: a group left behind is not a clean
// delete, whatever the reason.
func DeleteGroups(ctx context.Context, api EC2, vpcID string) ([]string, []*GroupInUseError, error) {
	return groupDeleter{d: Deps{EC2: api}, vpcID: vpcID}.run(ctx)
}

// GroupInUseError is a group [DeleteGroups] kept because something still
// uses or references it. It wraps [ErrGroupsInUse].
type GroupInUseError struct {
	Name, ID string
	// Users are the instances, in any state but terminated, carrying it,
	// sorted.
	Users []string
	// NetworkInterfaces are the interfaces still carrying it once no
	// instance does: one a terminated instance has not released yet, or one
	// something other than an instance made. Sorted.
	NetworkInterfaces []string
	// ReferencedBy are the kept groups whose rules name it.
	ReferencedBy []string
}

// Reason is why the group was kept, as "used by i-1, i-2".
func (e *GroupInUseError) Reason() string {
	var why []string
	if len(e.Users) > 0 {
		why = append(why, "used by "+join(e.Users))
	}
	if len(e.NetworkInterfaces) > 0 {
		why = append(why, "attached to network interface(s) "+join(e.NetworkInterfaces))
	}
	if len(e.ReferencedBy) > 0 {
		why = append(why, "named in the rules of "+join(e.ReferencedBy)+", which is kept")
	}
	return strings.Join(why, "; ")
}

func (e *GroupInUseError) Error() string {
	return fmt.Sprintf("%v: %s (%s) is %s", ErrGroupsInUse, e.Name, e.ID, e.Reason())
}

func (e *GroupInUseError) Unwrap() error { return ErrGroupsInUse }

// Backoff of a group delete EC2 refuses with DependencyViolation.
const (
	deleteBackoffMin = 5 * time.Second
	deleteBackoffMax = time.Minute
)

// groupDeleter is [DeleteGroups]'s pass over one VPC.
type groupDeleter struct {
	d     Deps
	vpcID string
	// eniWait bounds the wait for network interfaces to release the groups,
	// and deleteWait the retries of a delete EC2 refuses with
	// DependencyViolation. Zero waits for nothing.
	eniWait, deleteWait time.Duration
}

// deleteTarget is one of the VPC's groups that is spawn's, as first read.
type deleteTarget struct {
	name string
	// order is its place in the delete order, the proxy group first.
	order int
	sg    types.SecurityGroup
}

func (t deleteTarget) id() string { return aws.ToString(t.sg.GroupId) }

// names reports whether any of t's rules names group id.
func (t deleteTarget) names(id string) bool {
	for _, r := range append(rulesOf(t.sg.IpPermissions), rulesOf(t.sg.IpPermissionsEgress)...) {
		if r.group == id {
			return true
		}
	}
	return false
}

func (g groupDeleter) run(ctx context.Context) ([]string, []*GroupInUseError, error) {
	var errs []error
	var targets []deleteTarget
	for i, n := range []struct{ name, purpose string }{
		{ProxyGroupName(g.vpcID), PurposeProxy}, {ClientsGroupName(g.vpcID), PurposeClients},
	} {
		sg, err := describeGroup(ctx, g.d.EC2, g.vpcID, n.name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if sg == nil {
			continue
		}
		if err := ownGroup(*sg, g.vpcID, n.purpose); err != nil {
			errs = append(errs, err)
			continue
		}
		targets = append(targets, deleteTarget{name: n.name, order: i, sg: *sg})
	}

	ready, stay, kept, werrs := g.awaitUnused(ctx, targets)
	errs = append(errs, werrs...)
	ready, kept = keepReferenced(ready, stay, kept)
	errs = append(errs, g.revokeCrossReferences(ctx, ready)...)

	var deleted []string
	deadline := g.d.now().Add(g.deleteWait)
	for _, t := range ready {
		gone, err := g.deleteGroup(ctx, t, deadline)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if gone {
			deleted = append(deleted, t.id())
		}
	}
	for _, k := range kept {
		errs = append(errs, k)
	}
	return deleted, kept, errors.Join(errs...)
}

// awaitUnused sorts targets into those nothing uses (ready, in delete
// order) and the rest (stay): kept, because an instance or a network
// interface still carries them, or unread, with the read's error. While
// only interfaces carry a group it looks again, up to g.eniWait: a
// terminated instance's interface is released a little after the instance
// reports terminated. An instance's use is not waited for.
func (g groupDeleter) awaitUnused(ctx context.Context, targets []deleteTarget) (ready, stay []deleteTarget, kept []*GroupInUseError, errs []error) {
	deadline := g.d.now().Add(g.eniWait)
	pending := targets
	for len(pending) > 0 {
		var waiting []deleteTarget
		var waitingENIs [][]string
		for _, t := range pending {
			users, _, err := groupUsers(ctx, g.d.EC2, t.id())
			if err != nil {
				stay, errs = append(stay, t), append(errs, err)
				continue
			}
			if len(users) > 0 {
				stay, kept = append(stay, t), append(kept, &GroupInUseError{Name: t.name, ID: t.id(), Users: users})
				continue
			}
			enis, err := groupInterfaces(ctx, g.d.EC2, t.id())
			if spawnaws.IsAccessDenied(err) {
				// A caller that may not read interfaces (the nested-spawn
				// role has no ec2:DescribeNetworkInterfaces: its inline
				// policy is at IAM's size cap) still deletes: EC2 refuses
				// while an interface carries the group, and that refusal
				// is retried.
				ready = append(ready, t)
				continue
			}
			if err != nil {
				stay, errs = append(stay, t), append(errs, err)
				continue
			}
			if len(enis) == 0 {
				ready = append(ready, t)
				continue
			}
			waiting, waitingENIs = append(waiting, t), append(waitingENIs, enis)
		}
		if len(waiting) == 0 {
			break
		}
		left := deadline.Sub(g.d.now())
		var serr error
		if left > 0 {
			serr = g.d.sleep(ctx, minDuration(teardownPoll, left))
		}
		if left <= 0 || serr != nil {
			for i, t := range waiting {
				stay = append(stay, t)
				kept = append(kept, &GroupInUseError{Name: t.name, ID: t.id(), NetworkInterfaces: waitingENIs[i]})
			}
			if serr != nil {
				errs = append(errs, fmt.Errorf("waiting for network interfaces to release %s: %w", g.vpcID, serr))
			}
			break
		}
		pending = waiting
	}
	sort.SliceStable(ready, func(i, j int) bool { return ready[i].order < ready[j].order })
	return ready, stay, kept, errs
}

// keepReferenced keeps each ready group that a group staying has a rule
// naming: EC2 would refuse to delete it. Rules in a group that stays are not
// revoked, since something still uses that group.
func keepReferenced(ready, stay []deleteTarget, kept []*GroupInUseError) ([]deleteTarget, []*GroupInUseError) {
	for changed := true; changed; {
		changed = false
		var still []deleteTarget
		for _, r := range ready {
			var by []string
			for _, s := range stay {
				if s.id() != r.id() && s.names(r.id()) {
					by = append(by, s.id())
				}
			}
			if len(by) == 0 {
				still = append(still, r)
				continue
			}
			stay, changed = append(stay, r), true
			kept = append(kept, &GroupInUseError{Name: r.name, ID: r.id(), ReferencedBy: by})
		}
		ready = still
	}
	return ready, kept
}

// revokeCrossReferences revokes, in each group of ready, every rule naming
// another group of ready, so the deletes do not depend on their order: the
// clients group can go even when the proxy group's delete fails.
func (g groupDeleter) revokeCrossReferences(ctx context.Context, ready []deleteTarget) []error {
	var errs []error
	for _, t := range ready {
		naming := func(perms []types.IpPermission) []rule {
			var out []rule
			for _, r := range rulesOf(perms) {
				for _, o := range ready {
					if o.id() != t.id() && r.group == o.id() {
						out = append(out, r)
					}
				}
			}
			return out
		}
		id := aws.ToString(t.sg.GroupId)
		if in := naming(t.sg.IpPermissions); len(in) > 0 {
			if _, err := g.d.EC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
				GroupId: aws.String(id), IpPermissions: permissions(in, "")}); err != nil && !isNotFound(err) {
				errs = append(errs, fmt.Errorf("revoke ingress %s on %s (%s) before deleting it: %w", rulesString(in), t.name, id, err))
			}
		}
		if out := naming(t.sg.IpPermissionsEgress); len(out) > 0 {
			if _, err := g.d.EC2.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
				GroupId: aws.String(id), IpPermissions: permissions(out, "")}); err != nil && !isNotFound(err) {
				errs = append(errs, fmt.Errorf("revoke egress %s on %s (%s) before deleting it: %w", rulesString(out), t.name, id, err))
			}
		}
	}
	return errs
}

// isDependencyViolation reports EC2's refusal to delete a group something
// still references.
func isDependencyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "DependencyViolation")
}

// deleteGroup deletes t, retrying with backoff until deadline while EC2
// refuses with DependencyViolation. It reports false when the group was
// already gone.
func (g groupDeleter) deleteGroup(ctx context.Context, t deleteTarget, deadline time.Time) (bool, error) {
	wait := deleteBackoffMin
	for attempt := 1; ; attempt++ {
		_, err := g.d.EC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(t.id())})
		switch {
		case err == nil:
			return true, nil
		case strings.Contains(err.Error(), "InvalidGroup.NotFound"):
			return false, nil
		case !isDependencyViolation(err):
			return false, fmt.Errorf("delete security group %s (%s): %w", t.name, t.id(), err)
		}
		left := deadline.Sub(g.d.now())
		if left <= 0 {
			return false, fmt.Errorf("delete security group %s (%s): still refused after %d attempt(s) over %v: %w",
				t.name, t.id(), attempt, g.deleteWait, err)
		}
		if serr := g.d.sleep(ctx, minDuration(wait, left)); serr != nil {
			return false, fmt.Errorf("delete security group %s (%s): %w (stopped retrying: %v)", t.name, t.id(), err, serr)
		}
		wait = minDuration(2*wait, deleteBackoffMax)
	}
}

// groupInterfaces are the network interfaces, in any status, carrying
// groupID, sorted.
func groupInterfaces(ctx context.Context, api EC2, groupID string) ([]string, error) {
	ids := map[string]bool{}
	p := ec2.NewDescribeNetworkInterfacesPaginator(api, &ec2.DescribeNetworkInterfacesInput{
		Filters: []types.Filter{{Name: aws.String("group-id"), Values: []string{groupID}}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the network interfaces using %s: %w", groupID, err)
		}
		for _, ni := range page.NetworkInterfaces {
			ids[aws.ToString(ni.NetworkInterfaceId)] = true
		}
	}
	return sortedKeys(ids), nil
}

// groupUsers are the instances, in any state but terminated, carrying groupID,
// and how many of their network interfaces carry it. An instance listed with
// no interfaces (EC2 always lists them for a VPC instance) counts as one.
func groupUsers(ctx context.Context, api EC2, groupID string) ([]string, int, error) {
	users := map[string]bool{}
	enis := 0
	in := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("instance.group-id"), Values: []string{groupID}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "shutting-down", "stopping", "stopped"}},
	}}
	p := ec2.NewDescribeInstancesPaginator(api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("list the instances using %s: %w", groupID, err)
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				id := aws.ToString(i.InstanceId)
				if users[id] {
					continue
				}
				users[id] = true
				if len(i.NetworkInterfaces) == 0 {
					enis++
					continue
				}
				for _, ni := range i.NetworkInterfaces {
					for _, g := range ni.Groups {
						if aws.ToString(g.GroupId) == groupID {
							enis++
							break
						}
					}
				}
			}
		}
	}
	return sortedKeys(users), enis, nil
}

// GroupState is one of a VPC's two groups as it stands: read-only, for
// `spawn egress-proxy status` and the plans of delete and upgrade.
type GroupState struct {
	Name string
	// ID is empty when no group of Name is in the VPC.
	ID string
	// Foreign is why a group of Name is not spawn's ([ErrForeignGroup]); ""
	// when it is spawn's or absent.
	Foreign string
	// Users are the instances, in any state but terminated, carrying it, and
	// ENIs how many of their network interfaces do. Not read for a foreign
	// group.
	Users []string
	ENIs  int
}

// Exists reports whether a group of the name is in the VPC.
func (g GroupState) Exists() bool { return g.ID != "" }

// Owned reports whether the group exists and is spawn's.
func (g GroupState) Owned() bool { return g.ID != "" && g.Foreign == "" }

// GroupsState is a VPC's two groups, read through ec2:DescribeSecurityGroups
// and ec2:DescribeInstances only.
type GroupsState struct {
	Proxy, Clients GroupState
}

// Groups are the IDs of the two, when both are spawn's.
func (s GroupsState) Groups() Groups {
	g := Groups{}
	if s.Proxy.Owned() {
		g.ProxyID = s.Proxy.ID
	}
	if s.Clients.Owned() {
		g.ClientsID = s.Clients.ID
	}
	return g
}

// DescribeGroups reads the VPC's two groups and who uses them. It creates and
// changes nothing.
func DescribeGroups(ctx context.Context, api EC2, vpcID string) (GroupsState, error) {
	var out GroupsState
	for _, n := range []struct {
		name, purpose string
		into          *GroupState
	}{{ProxyGroupName(vpcID), PurposeProxy, &out.Proxy}, {ClientsGroupName(vpcID), PurposeClients, &out.Clients}} {
		n.into.Name = n.name
		sg, err := describeGroup(ctx, api, vpcID, n.name)
		if err != nil {
			return out, err
		}
		if sg == nil {
			continue
		}
		n.into.ID = aws.ToString(sg.GroupId)
		if err := ownGroup(*sg, vpcID, n.purpose); err != nil {
			n.into.Foreign = err.Error()
			continue
		}
		if n.into.Users, n.into.ENIs, err = groupUsers(ctx, api, n.into.ID); err != nil {
			return out, err
		}
	}
	return out, nil
}

// ErrMemberEgress is wrapped by [CheckMemberEgress] when no group of a
// member's allows it to reach the proxy.
var ErrMemberEgress = errors.New("the member's security groups do not allow outbound traffic to the egress proxy")

// CheckMemberEgress refuses member groups none of whose egress rules lets a
// member reach the proxy at proxyIP on TCP [egressproxy.Port] (#1515 phase
// 4): the clients group's ingress rule on the proxy admits it, but a member's
// OWN outbound rules must also allow it, and a user's restrictive
// --security-group-ids may not. Security groups are a union, so one rule in
// one group is enough: all traffic or a TCP range holding the port, to the
// clients group, the proxy group, or an IPv4 CIDR holding proxyIP (0.0.0.0/0
// among them). A prefix-list destination is not counted, since its entries
// are not read; the clients group itself has no rules. spawn's own groups
// (the VPC default, the MPI, Windows and --efa-group groups) keep EC2's
// allow-all egress and pass. The read is ec2:DescribeSecurityGroups, which
// launch already uses.
func CheckMemberEgress(ctx context.Context, api EC2, groupIDs []string, g Groups, proxyIP string) error {
	ip, err := netip.ParseAddr(proxyIP)
	if err != nil {
		return fmt.Errorf("egress proxy IP %q: %w", proxyIP, err)
	}
	out, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: groupIDs})
	if err != nil {
		return fmt.Errorf("read the member security groups %s to check their egress to the proxy: %w",
			strings.Join(groupIDs, ", "), err)
	}
	var named []string
	for _, sg := range out.SecurityGroups {
		for _, r := range rulesOf(sg.IpPermissionsEgress) {
			if reachesProxy(r, g, ip) {
				return nil
			}
		}
		id := aws.ToString(sg.GroupId)
		if id == g.ClientsID {
			continue
		}
		named = append(named, fmt.Sprintf("%s (%s)", id, aws.ToString(sg.GroupName)))
	}
	sort.Strings(named)
	return fmt.Errorf("%w at %s:%d: none of %s allows outbound tcp %d to %s, %s, %s or a CIDR holding %s. "+
		"Add such an egress rule to one of them, or name a group that has one in --security-group-ids",
		ErrMemberEgress, proxyIP, egressproxy.Port, strings.Join(named, ", "), egressproxy.Port,
		g.ClientsID, g.ProxyID, proxyIP, proxyIP)
}

// reachesProxy reports whether egress rule r lets a member reach the proxy.
func reachesProxy(r rule, g Groups, ip netip.Addr) bool {
	switch r.proto {
	case "-1":
	case "tcp", "6":
		if r.from > egressproxy.Port || r.to < egressproxy.Port {
			return false
		}
	default:
		return false
	}
	switch {
	case r.group != "":
		return r.group == g.ClientsID || r.group == g.ProxyID
	case r.cidr != "":
		p, err := netip.ParsePrefix(r.cidr)
		return err == nil && p.Contains(ip)
	}
	return false
}
