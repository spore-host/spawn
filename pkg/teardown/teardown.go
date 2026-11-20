// Package teardown is spawn's footprint.Deleter (spawn#1653, part of the
// leave-no-trace epic spore-host/spore-host#752): the per-type Describe and
// Delete that github.com/spore-host/libs/footprint.Execute calls to carry out
// a plan built from `spawn footprint -o json`.
//
// It adds no new way to delete anything. Each type reuses the deleter spawn
// already has for it:
//
//	AWS::EC2::Instance          pkg/aws RemoveResource (terminate)
//	AWS::EC2::Volume            pkg/aws RemoveResource
//	AWS::EC2::SecurityGroup     pkg/egressproxy/deploy Delete for an egress
//	                            proxy's groups (they name each other), else
//	                            pkg/aws RemoveResource
//	AWS::EC2::PlacementGroup    pkg/aws RemoveResource (the retrying delete)
//	AWS::EC2::KeyPair           pkg/aws RemoveResource, session keys only
//	                            (pkg/keypairsweep.IsSessionKey)
//	AWS::EC2::EIP               pkg/aws RemoveResource, launch-allocated only
//	AWS::IAM::InstanceProfile   pkg/aws RemoveResource
//	AWS::IAM::Role              pkg/instancerole Delete for a per-task role,
//	                            else pkg/aws RemoveResource
//	AWS::IAM::ManagedPolicy     DeletePolicy, spored managed policies only
//	                            (pkg/instancerole.IsSporedPolicyName)
//	AWS::S3::Bucket             pkg/bucketsweep Sweep (empty, past its grace)
//	AWS::SQS::Queue             DeleteQueue once pkg/poolqueue.Decide allows
//
// The reaper's network sweeps are package main of the reaper module, so they
// are reached through RemoveResource, which makes the same EC2 calls.
//
// # Nothing in use is deleted
//
// footprint.Deletable lets an active resource through: the idle backstop
// (spawn#1654) only plans a teardown once nothing is active. This is also
// the human path, run on demand, so Describe re-derives "in use" from the
// live account for every resource, with the same checks the footprint uses,
// and Delete refuses one Describe found in use, with an error wrapping
// ErrInUse. A step refused that way is never deleted, and footprint.Execute
// skips everything it depends on.
package teardown

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/spore-host/libs/footprint"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/bucketsweep"
	"github.com/spore-host/spawn/pkg/egressproxy/deploy"
	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/keypairsweep"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// ErrInUse is wrapped by Delete for a resource its Describe found in use.
var ErrInUse = errors.New("in use")

// Options configures New.
type Options struct {
	// Account is the account the plan is for: the expected owner of every
	// bucket, and the account every ARN must name.
	Account string
	// Now is the clock; default time.Now.
	Now func() time.Time
	// RoleReferences are the deferred launchers (schedules) that name
	// per-task roles (instancerole.CollectReferences). When they were not
	// collected, no per-task role or profile is deleted.
	RoleReferences instancerole.References
	// Workers returns what poolqueue.WorkersInUse finds across every
	// enabled region. Default: computed once from the config. An error
	// makes every role, instance profile and pool queue in use.
	Workers func(ctx context.Context) (poolqueue.InUse, error)
	// Egress bounds the egress-proxy group teardown's waits.
	Egress deploy.DeleteOptions
}

// New returns spawn's Deleter over cfg's credentials: one per type in
// footprintspawn.DeletableTypes.
func New(cfg aws.Config, opts Options) footprint.ByType {
	d := buildDeleter(cfg, opts)
	return footprint.ByType{
		fs.TypeInstance:        typed{d, d.describeInstance, d.deleteInstance},
		fs.TypeVolume:          typed{d, d.describeVolume, d.deleteVolume},
		fs.TypeSecurityGroup:   typed{d, d.describeSecurityGroup, d.deleteSecurityGroup},
		fs.TypePlacementGroup:  typed{d, d.describePlacementGroup, d.deletePlacementGroup},
		fs.TypeKeyPair:         typed{d, d.describeKeyPair, d.deleteKeyPair},
		fs.TypeEIP:             typed{d, d.describeEIP, d.deleteEIP},
		fs.TypeInstanceProfile: typed{d, d.describeProfile, d.deleteProfile},
		fs.TypeRole:            typed{d, d.describeRole, d.deleteRole},
		fs.TypeManagedPolicy:   typed{d, d.describePolicy, d.deletePolicy},
		fs.TypeBucket:          typed{d, d.describeBucket, d.deleteBucket},
		fs.TypeQueue:           typed{d, d.describeQueue, d.deleteQueue},
	}
}

// ErrReportOnly is what a report-only Deleter's Delete returns: it was built
// to re-check a plan, never to carry one out.
var ErrReportOnly = errors.New("report-only deleter: nothing is deleted")

// NewReportOnly is New without the deletes, for the reaper's account-idle
// sweep in report mode (spawn#1654): Describe re-reads every resource exactly
// as New's does, and Delete refuses with ErrReportOnly, making no AWS call.
//
// It is a separate constructor, not New with a flag, so that no delete is
// reachable from a caller that only reports. The IAM coverage of a program is
// derived from its call graph (internal/iamcoverage), and that graph counts a
// method value as callable wherever it is created: New creates every
// d.deleteX, so a program that calls New is held to grant every delete. This
// function never names one, so the reaper's role is asked for the reads
// alone, and a report-mode reaper cannot delete even through a bug.
func NewReportOnly(cfg aws.Config, opts Options) footprint.ByType {
	d := buildDeleter(cfg, opts)
	return footprint.ByType{
		fs.TypeInstance:        typed{d, d.describeInstance, refuseDelete},
		fs.TypeVolume:          typed{d, d.describeVolume, refuseDelete},
		fs.TypeSecurityGroup:   typed{d, d.describeSecurityGroup, refuseDelete},
		fs.TypePlacementGroup:  typed{d, d.describePlacementGroup, refuseDelete},
		fs.TypeKeyPair:         typed{d, d.describeKeyPair, refuseDelete},
		fs.TypeEIP:             typed{d, d.describeEIP, refuseDelete},
		fs.TypeInstanceProfile: typed{d, d.describeProfile, refuseDelete},
		fs.TypeRole:            typed{d, d.describeRole, refuseDelete},
		fs.TypeManagedPolicy:   typed{d, d.describePolicy, refuseDelete},
		fs.TypeBucket:          typed{d, d.describeBucket, refuseDelete},
		fs.TypeQueue:           typed{d, d.describeQueue, refuseDelete},
	}
}

func refuseDelete(_ context.Context, r footprint.Resource, _ seen) error {
	return fmt.Errorf("%s: %w", r.ARN, ErrReportOnly)
}

func buildDeleter(cfg aws.Config, opts Options) *deleter {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	d := &deleter{cfg: cfg, opts: opts, client: spawnaws.NewClientFromConfig(cfg), seen: map[string]seen{}}
	if d.opts.Workers == nil {
		d.opts.Workers = d.defaultWorkers
	}
	return d
}

// seen is what the last Describe of a resource found, for its Delete.
type seen struct {
	inUse string
	mr    spawnaws.ManagedResource
	// profileExists: a per-task role's same-name profile (Inspect).
	profileExists bool
	queueURL      string
	vpcID         string
}

type deleter struct {
	cfg    aws.Config
	opts   Options
	client *spawnaws.Client

	mu   sync.Mutex
	seen map[string]seen

	// The workers, read once per teardown. A mutex rather than a sync.Once:
	// the IAM inventory's call graph (internal/iamcoverage) cannot tell one
	// Once.Do closure from another, and a closure here made every command
	// look as if it read every region's instances.
	workersMu   sync.Mutex
	workersRead bool
	workers     poolqueue.InUse
	workersErr  error
}

// typed adapts one type's describe and delete to footprint.Deleter. describe
// returns the live resource and what Delete needs; typed records it, and sets
// the live class to active when it is in use.
type typed struct {
	d        *deleter
	describe func(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error)
	del      func(ctx context.Context, r footprint.Resource, s seen) error
}

func (t typed) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	if err := t.d.checkAccount(r); err != nil {
		return footprint.Resource{}, err
	}
	live, s, err := t.describe(ctx, r)
	if err != nil {
		return footprint.Resource{}, err
	}
	if s.inUse != "" && live.Class == "" {
		live.Class = footprint.ClassActive
	}
	// Ownership from what the account says now, by the footprint's own
	// rule: a resource that has since gained another tool's tag, or that
	// carries another tool's name, is not spawn's to delete.
	name := live.Name
	if name == "" {
		name = r.Name
	}
	live.Ownership = fs.OwnershipOf(name, live.Tags)
	t.d.mu.Lock()
	t.d.seen[r.ARN] = s
	t.d.mu.Unlock()
	return live, nil
}

func (t typed) Delete(ctx context.Context, r footprint.Resource) error {
	t.d.mu.Lock()
	s, ok := t.d.seen[r.ARN]
	delete(t.d.seen, r.ARN)
	t.d.mu.Unlock()
	switch {
	case !ok:
		return fmt.Errorf("refusing to delete %s: it was not re-read first", r.ARN)
	case s.inUse != "":
		return fmt.Errorf("kept %s: %w: %s", r.Name, ErrInUse, s.inUse)
	}
	return t.del(ctx, r, s)
}

// checkAccount refuses a resource whose ARN names another account: a plan is
// for one account, and a plan file edited to point elsewhere is not followed.
func (d *deleter) checkAccount(r footprint.Resource) error {
	parts := strings.SplitN(r.ARN, ":", 6)
	if len(parts) < 6 || !strings.HasPrefix(parts[0], "arn") {
		return fmt.Errorf("refusing %q: not an ARN", r.ARN)
	}
	if acct := parts[4]; acct != "" && acct != d.opts.Account {
		return fmt.Errorf("refusing %s: it is in account %s, the plan is for %s", r.ARN, acct, d.opts.Account)
	}
	return nil
}

func (d *deleter) regional(region string) aws.Config {
	c := d.cfg.Copy()
	if region != "" && region != fs.RegionGlobal {
		c.Region = region
	}
	return c
}

func (d *deleter) ec2(r footprint.Resource) *ec2.Client {
	return ec2.NewFromConfig(d.regional(r.Region))
}

func (d *deleter) defaultWorkers(ctx context.Context) (poolqueue.InUse, error) {
	clients, err := poolqueue.NewClients(ctx, d.cfg)
	if err != nil {
		return poolqueue.InUse{}, err
	}
	return poolqueue.WorkersInUse(ctx, clients.EC2)
}

// liveWorkers is opts.Workers, read once per teardown.
func (d *deleter) liveWorkers(ctx context.Context) (poolqueue.InUse, error) {
	d.workersMu.Lock()
	defer d.workersMu.Unlock()
	if !d.workersRead {
		d.workers, d.workersErr = d.opts.Workers(ctx)
		d.workersRead = true
	}
	return d.workers, d.workersErr
}

func idOf(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

func errCode(err error, codes ...string) bool {
	if err == nil {
		return false
	}
	for _, c := range codes {
		if strings.Contains(err.Error(), c) {
			return true
		}
	}
	return false
}

func notFound(what string) error { return fmt.Errorf("%s: %w", what, footprint.ErrNotFound) }

func tagMap(ts []ec2types.Tag) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// liveStates are the instance states in which an instance holds what it uses.
var liveStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

func isLive(state string) bool {
	for _, s := range liveStates {
		if s == state {
			return true
		}
	}
	return false
}

// --- EC2 ---------------------------------------------------------------------

func (d *deleter) describeInstance(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	id := idOf(r.ARN)
	out, err := d.ec2(r).DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{id}}},
	})
	if err != nil {
		return r, seen{}, err
	}
	var insts []ec2types.Instance
	for _, rv := range out.Reservations {
		insts = append(insts, rv.Instances...)
	}
	if len(insts) > 0 {
		inst := insts[0]
		state := ""
		if inst.State != nil {
			state = string(inst.State.Name)
		}
		if state != "terminated" {
			s := seen{mr: spawnaws.ManagedResource{Service: "ec2", ResourceType: "instance", ID: id, Region: r.Region, State: state}}
			if isLive(state) {
				s.inUse = "instance is " + state
			}
			return footprint.Resource{Tags: tagMap(inst.Tags)}, s, nil
		}
	}
	return r, seen{}, notFound("instance " + id)
}

func (d *deleter) deleteInstance(ctx context.Context, _ footprint.Resource, s seen) error {
	return d.client.RemoveResource(ctx, s.mr)
}

func (d *deleter) describeVolume(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	id := idOf(r.ARN)
	out, err := d.ec2(r).DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{{Name: aws.String("volume-id"), Values: []string{id}}},
	})
	if err != nil {
		return r, seen{}, err
	}
	if len(out.Volumes) > 0 {
		v := out.Volumes[0]
		s := seen{mr: spawnaws.ManagedResource{Service: "ec2", ResourceType: "volume", ID: id, Region: r.Region, State: string(v.State)}}
		if v.State != ec2types.VolumeStateAvailable {
			s.inUse = "volume is " + string(v.State)
		}
		return footprint.Resource{Tags: tagMap(v.Tags)}, s, nil
	}
	return r, seen{}, notFound("volume " + id)
}

func (d *deleter) deleteVolume(ctx context.Context, _ footprint.Resource, s seen) error {
	return d.client.RemoveResource(ctx, s.mr)
}

// Egress proxy groups (pkg/egressproxy/deploy): their rules name each other,
// so they are deleted together by the egress proxy's own teardown.
func isEgressGroup(name string) bool {
	return strings.HasPrefix(name, "spawn-egress-proxy-") || strings.HasPrefix(name, "spawn-egress-clients-")
}

func (d *deleter) describeSecurityGroup(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	id := idOf(r.ARN)
	ec := d.ec2(r)
	out, err := ec.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{id}}},
	})
	if err != nil {
		return r, seen{}, err
	}
	if len(out.SecurityGroups) == 0 {
		return r, seen{}, notFound("security group " + id)
	}
	g := out.SecurityGroups[0]
	s := seen{vpcID: aws.ToString(g.VpcId), mr: spawnaws.ManagedResource{Service: "ec2", ResourceType: "security-group", ID: id, Region: r.Region}}
	nis, err := ec.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{id}}},
	})
	switch {
	case err != nil:
		s.inUse = "its network interfaces could not be read: " + err.Error()
	case len(nis.NetworkInterfaces) > 0:
		s.inUse = fmt.Sprintf("on %d network interface(s)", len(nis.NetworkInterfaces))
	}
	if aws.ToString(g.GroupName) == "default" {
		s.inUse = "a VPC's default group"
	}
	return footprint.Resource{Name: aws.ToString(g.GroupName), Tags: tagMap(g.Tags)}, s, nil
}

func (d *deleter) deleteSecurityGroup(ctx context.Context, r footprint.Resource, s seen) error {
	if !isEgressGroup(r.Name) {
		return d.client.RemoveResource(ctx, s.mr)
	}
	ec := d.ec2(r)
	inv, err := deploy.Inventory(ctx, ec, s.vpcID, deploy.Want{})
	if err != nil {
		return err
	}
	for _, v := range inv {
		if v.VPCID != s.vpcID {
			continue
		}
		plan := deploy.PlanDelete(v)
		if len(plan.Terminate)+len(plan.Await)+len(plan.Refused) > 0 {
			return fmt.Errorf("kept %s: %w: the VPC's egress proxy is still there", r.Name, ErrInUse)
		}
		_, err := deploy.Delete(ctx, deploy.Deps{EC2: ec}, plan, d.opts.Egress)
		return err
	}
	// The inventory found nothing of this VPC's: the group is already gone.
	return notFound("security group " + idOf(r.ARN))
}

func (d *deleter) describePlacementGroup(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	ec := d.ec2(r)
	out, err := ec.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{GroupNames: []string{r.Name}})
	if errCode(err, "InvalidPlacementGroup.Unknown") {
		return r, seen{}, notFound("placement group " + r.Name)
	}
	if err != nil {
		return r, seen{}, err
	}
	if len(out.PlacementGroups) == 0 {
		return r, seen{}, notFound("placement group " + r.Name)
	}
	s := seen{mr: spawnaws.ManagedResource{Service: "ec2", ResourceType: "placement-group", ID: r.Name, Region: r.Region}}
	insts, err := ec.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("placement-group-name"), Values: []string{r.Name}},
		{Name: aws.String("instance-state-name"), Values: liveStates},
	}})
	switch {
	case err != nil:
		s.inUse = "its instances could not be read: " + err.Error()
	default:
		for _, rv := range insts.Reservations {
			if len(rv.Instances) > 0 {
				s.inUse = "an instance is in the group"
			}
		}
	}
	return footprint.Resource{Tags: tagMap(out.PlacementGroups[0].Tags)}, s, nil
}

func (d *deleter) deletePlacementGroup(ctx context.Context, _ footprint.Resource, s seen) error {
	return d.client.RemoveResource(ctx, s.mr)
}

func (d *deleter) describeKeyPair(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	id := idOf(r.ARN)
	out, err := d.ec2(r).DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
		Filters: []ec2types.Filter{{Name: aws.String("key-pair-id"), Values: []string{id}}},
	})
	if err != nil {
		return r, seen{}, err
	}
	if len(out.KeyPairs) == 0 {
		return r, seen{}, notFound("key pair " + id)
	}
	k := out.KeyPairs[0]
	live := footprint.Resource{Name: aws.ToString(k.KeyName), Tags: tagMap(k.Tags)}
	if !keypairsweep.IsSessionKey(k.Tags) {
		// A workstation key, or one spawn did not import for a session: the
		// user's.
		live.Class = footprint.ClassUserData
	}
	return live, seen{mr: spawnaws.ManagedResource{Service: "ec2", ResourceType: "key-pair", ID: id, Region: r.Region}}, nil
}

func (d *deleter) deleteKeyPair(ctx context.Context, _ footprint.Resource, s seen) error {
	return d.client.RemoveResource(ctx, s.mr)
}

func (d *deleter) describeEIP(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	id := idOf(r.ARN)
	ec := d.ec2(r)
	out, err := ec.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []string{id}})
	if errCode(err, "InvalidAllocationID.NotFound") {
		return r, seen{}, notFound("Elastic IP " + id)
	}
	if err != nil {
		return r, seen{}, err
	}
	if len(out.Addresses) == 0 {
		return r, seen{}, notFound("Elastic IP " + id)
	}
	a := out.Addresses[0]
	tags := tagMap(a.Tags)
	mr := spawnaws.ManagedResource{Service: "ec2", ResourceType: "address", ID: id, Region: r.Region, Tags: tags,
		PublicIP: aws.ToString(a.PublicIp), AssociationID: aws.ToString(a.AssociationId), State: "unassociated"}
	live := footprint.Resource{Tags: tags}
	s := seen{mr: mr}
	switch {
	case !spawnaws.IsSpawnAllocatedAddress(mr):
		live.Class = footprint.ClassUserData
	case mr.AssociationID != "":
		s.inUse = "associated with " + aws.ToString(a.InstanceId)
	default:
		inst := tags[spawnaws.ElasticIPInstanceTagKey]
		got, err := ec.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{inst}}},
		})
		if err != nil {
			s.inUse = "the instance it was allocated for could not be read: " + err.Error()
			break
		}
		s.mr.LaunchInstanceState = spawnaws.LaunchInstanceGone
		for _, rv := range got.Reservations {
			for _, in := range rv.Instances {
				if in.State != nil {
					s.mr.LaunchInstanceState = string(in.State.Name)
				}
			}
		}
		if isLive(s.mr.LaunchInstanceState) {
			s.inUse = "its instance " + inst + " is " + s.mr.LaunchInstanceState
		}
	}
	return live, s, nil
}

func (d *deleter) deleteEIP(ctx context.Context, _ footprint.Resource, s seen) error {
	return d.client.RemoveResource(ctx, s.mr)
}

// --- IAM ---------------------------------------------------------------------

func (d *deleter) iam() *iam.Client { return iam.NewFromConfig(d.regional("")) }

// perTaskInUse decides a per-task role (or its same-name profile) with the
// reaper's rule, instancerole.Inspect. "" when it may go.
func (d *deleter) perTaskInUse(ctx context.Context, name string, inUse map[string]bool) (string, bool) {
	dec, profileExists, err := instancerole.Inspect(ctx, d.iam(), name, inUse, d.opts.RoleReferences, d.opts.Now(), instancerole.Options{LegacyPoolWorkers: true})
	switch {
	case err != nil:
		return "the per-task rule could not be applied: " + err.Error(), profileExists
	case !dec.Delete:
		return dec.Reason, profileExists
	}
	return "", profileExists
}

func (d *deleter) describeProfile(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	got, err := d.iam().GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(r.Name)})
	if errCode(err, "NoSuchEntity") {
		return r, seen{}, notFound("instance profile " + r.Name)
	}
	if err != nil {
		return r, seen{}, err
	}
	tags := map[string]string{}
	for _, t := range got.InstanceProfile.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	s := seen{mr: spawnaws.ManagedResource{Service: "iam", ResourceType: "instance-profile", ID: r.Name}}
	u, err := d.liveWorkers(ctx)
	switch {
	case err != nil:
		s.inUse = "the instances that may use it could not all be read: " + err.Error()
	case u.Profiles[r.Name]:
		s.inUse = "attached to a live instance"
	case instancerole.IsCandidate(r.Name):
		// A per-task profile goes with its role, by the role's rule.
		s.inUse, _ = d.perTaskInUse(ctx, r.Name, u.Profiles)
	}
	return footprint.Resource{Tags: tags}, s, nil
}

func (d *deleter) deleteProfile(ctx context.Context, _ footprint.Resource, s seen) error {
	return d.client.RemoveResource(ctx, s.mr)
}

func (d *deleter) describeRole(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	ic := d.iam()
	got, err := ic.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(r.Name)})
	if errCode(err, "NoSuchEntity") {
		return r, seen{}, notFound("role " + r.Name)
	}
	if err != nil {
		return r, seen{}, err
	}
	tags := map[string]string{}
	for _, t := range got.Role.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	s := seen{mr: spawnaws.ManagedResource{Service: "iam", ResourceType: "role", ID: r.Name}}
	u, err := d.liveWorkers(ctx)
	if err != nil {
		s.inUse = "the instances that may use it could not all be read: " + err.Error()
		return footprint.Resource{Tags: tags}, s, nil
	}
	profs, err := ic.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String(r.Name)})
	if err != nil {
		s.inUse = "its instance profiles could not be read: " + err.Error()
		return footprint.Resource{Tags: tags}, s, nil
	}
	for _, p := range profs.InstanceProfiles {
		if n := aws.ToString(p.InstanceProfileName); u.Profiles[n] {
			s.inUse = "its instance profile " + n + " is attached to a live instance"
			return footprint.Resource{Tags: tags}, s, nil
		}
	}
	if instancerole.IsCandidate(r.Name) {
		s.inUse, s.profileExists = d.perTaskInUse(ctx, r.Name, u.Profiles)
	}
	return footprint.Resource{Tags: tags}, s, nil
}

func (d *deleter) deleteRole(ctx context.Context, r footprint.Resource, s seen) error {
	if instancerole.IsCandidate(r.Name) {
		return instancerole.Delete(ctx, d.iam(), r.Name, s.profileExists)
	}
	return d.client.RemoveResource(ctx, s.mr)
}

func (d *deleter) describePolicy(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	ic := d.iam()
	got, err := ic.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(r.ARN)})
	if errCode(err, "NoSuchEntity") {
		return r, seen{}, notFound("policy " + r.ARN)
	}
	if err != nil {
		return r, seen{}, err
	}
	tags := map[string]string{}
	for _, t := range got.Policy.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	var s seen
	if n := aws.ToInt32(got.Policy.AttachmentCount) + aws.ToInt32(got.Policy.PermissionsBoundaryUsageCount); n > 0 {
		s.inUse = fmt.Sprintf("attached to, or the boundary of, %d role(s)", n)
	}
	return footprint.Resource{Name: aws.ToString(got.Policy.PolicyName), Tags: tags}, s, nil
}

func (d *deleter) deletePolicy(ctx context.Context, r footprint.Resource, _ seen) error {
	if !strings.HasSuffix(r.ARN, ":policy"+instancerole.SporedPolicyPath+r.Name) || !instancerole.IsSporedPolicyName(r.Name) {
		return fmt.Errorf("refusing to delete %s: not a spored managed policy at %s", r.ARN, instancerole.SporedPolicyPath)
	}
	_, err := d.iam().DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: aws.String(r.ARN)})
	switch {
	case errCode(err, "NoSuchEntity"):
		return notFound("policy " + r.ARN)
	case errCode(err, "DeleteConflict"):
		return fmt.Errorf("kept %s: %w: IAM reports it attached", r.Name, ErrInUse)
	}
	return err
}

// --- S3 ----------------------------------------------------------------------

func bucketKind(name string) (bucketsweep.Kind, bool) {
	switch {
	case strings.HasPrefix(name, taskproto.StagingBucketPrefix):
		return bucketsweep.KindStaging, true
	case strings.HasPrefix(name, "spawn-results-"):
		return bucketsweep.KindResults, true
	}
	return "", false
}

func (d *deleter) describeBucket(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	sc := s3.NewFromConfig(d.regional(r.Region))
	out, err := sc.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(r.Name), ExpectedBucketOwner: aws.String(d.opts.Account)})
	tags := map[string]string{}
	switch {
	case errCode(err, "NoSuchBucket"):
		return r, seen{}, notFound("bucket " + r.Name)
	case errCode(err, "NoSuchTagSet"):
	case err != nil:
		return r, seen{}, err
	default:
		for _, t := range out.TagSet {
			tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	live := footprint.Resource{Tags: tags}
	if _, ok := bucketKind(r.Name); !ok {
		live.Class = footprint.ClassUserData
	}
	return live, seen{}, nil
}

func (d *deleter) deleteBucket(ctx context.Context, r footprint.Resource, _ seen) error {
	kind, ok := bucketKind(r.Name)
	if !ok {
		return fmt.Errorf("refusing to delete bucket %s: not spawn's staging or results bucket", r.Name)
	}
	res := bucketsweep.Sweep(ctx, bucketsweep.ClientsFor(d.cfg)(r.Region), bucketsweep.Bucket{Name: r.Name, Region: r.Region, Kind: kind},
		bucketsweep.Options{Account: d.opts.Account, Now: d.opts.Now(), Writes: true, Delete: true})
	switch res.Action {
	case bucketsweep.Deleted:
		return nil
	case bucketsweep.Absent:
		return notFound("bucket " + r.Name)
	case bucketsweep.InUse:
		return fmt.Errorf("kept %s: %w: %s", r.Name, ErrInUse, res.Reason)
	case bucketsweep.Stamped, bucketsweep.Waiting:
		return fmt.Errorf("kept %s: empty, but not yet for the %s grace (%s since %s); a later teardown deletes it",
			r.Name, bucketsweep.Grace, bucketsweep.EmptySinceTagKey, res.EmptySince.Format(time.RFC3339))
	case bucketsweep.Failed:
		return fmt.Errorf("bucket %s: %s: %w", r.Name, res.Reason, res.Err)
	}
	return fmt.Errorf("kept %s: %s (%s)", r.Name, res.Action, res.Reason)
}

// --- SQS ---------------------------------------------------------------------

func (d *deleter) describeQueue(ctx context.Context, r footprint.Resource) (footprint.Resource, seen, error) {
	qc := sqs.NewFromConfig(d.regional(r.Region))
	url, err := qc.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(r.Name), QueueOwnerAWSAccountId: aws.String(d.opts.Account)})
	if errCode(err, "QueueDoesNotExist", "NonExistentQueue") {
		return r, seen{}, notFound("queue " + r.Name)
	}
	if err != nil {
		return r, seen{}, err
	}
	s := seen{queueURL: aws.ToString(url.QueueUrl)}
	tg, err := qc.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: url.QueueUrl})
	if err != nil {
		return r, seen{}, err
	}
	tags := tg.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	attrs, err := qc.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: url.QueueUrl, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll}})
	if err != nil {
		return r, seen{}, err
	}
	f := poolqueue.Facts{Name: r.Name, Tags: tags, CountsKnown: true}
	for _, n := range []sqstypes.QueueAttributeName{
		sqstypes.QueueAttributeNameApproximateNumberOfMessages,
		sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
	} {
		c, ok := poolqueue.Count(attrs.Attributes, n)
		f.CountsKnown = f.CountsKnown && ok
		f.Messages += c
	}
	if v, ok := poolqueue.Count(attrs.Attributes, sqstypes.QueueAttributeNameCreatedTimestamp); ok {
		f.Created = time.Unix(int64(v), 0)
	}
	u, err := d.liveWorkers(ctx)
	if err != nil {
		s.inUse = "the instances that may work it could not all be read: " + err.Error()
		return footprint.Resource{Tags: tags}, s, nil
	}
	if id, ok := poolqueue.RunID(r.Name); ok {
		f.InUse = u.Holds(id)
	}
	if ok, why := poolqueue.Decide(f, d.opts.Now()); !ok {
		s.inUse = why
	}
	return footprint.Resource{Tags: tags}, s, nil
}

func (d *deleter) deleteQueue(ctx context.Context, r footprint.Resource, s seen) error {
	_, err := sqs.NewFromConfig(d.regional(r.Region)).DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(s.queueURL)})
	if errCode(err, "QueueDoesNotExist", "NonExistentQueue") {
		return notFound("queue " + r.Name)
	}
	return err
}
