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

// ErrGroupsInUse is wrapped by [DeleteGroups] when an instance still uses a
// group.
var ErrGroupsInUse = errors.New("the egress proxy security groups are still in use")

// DeleteGroups is the deleter of a VPC's two groups (owner rule: every
// resource spawn creates has a named one). It deletes them only when no
// instance in any state but terminated still carries either: the proxy's
// first, since its ingress rule references the clients group, which EC2 will
// not delete while referenced. A group that is already gone is not an error;
// a group of spawn's name that is not spawn's is left alone. The TTL reaper's
// managed-network sweep is the other deleter, for groups nobody runs this on.
// It returns the IDs it deleted.
func DeleteGroups(ctx context.Context, api EC2, vpcID string) ([]string, error) {
	type named struct {
		name, purpose string
	}
	var deleted []string
	for _, n := range []named{{ProxyGroupName(vpcID), PurposeProxy}, {ClientsGroupName(vpcID), PurposeClients}} {
		sg, err := describeGroup(ctx, api, vpcID, n.name)
		if err != nil {
			return deleted, err
		}
		if sg == nil {
			continue
		}
		if err := ownGroup(*sg, vpcID, n.purpose); err != nil {
			return deleted, err
		}
		id := aws.ToString(sg.GroupId)
		users, _, err := groupUsers(ctx, api, id)
		if err != nil {
			return deleted, err
		}
		if len(users) > 0 {
			return deleted, &GroupInUseError{Name: n.name, ID: id, Users: users}
		}
		if _, err := api.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}); err != nil {
			if strings.Contains(err.Error(), "InvalidGroup.NotFound") {
				continue
			}
			return deleted, fmt.Errorf("delete security group %s (%s): %w", n.name, id, err)
		}
		deleted = append(deleted, id)
	}
	return deleted, nil
}

// GroupInUseError is [DeleteGroups]'s refusal of a group an instance still
// carries. It wraps [ErrGroupsInUse].
type GroupInUseError struct {
	Name, ID string
	// Users are the instances carrying it, sorted.
	Users []string
}

func (e *GroupInUseError) Error() string {
	return fmt.Sprintf("%v: %s (%s) is used by %s", ErrGroupsInUse, e.Name, e.ID, join(e.Users))
}

func (e *GroupInUseError) Unwrap() error { return ErrGroupsInUse }

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
