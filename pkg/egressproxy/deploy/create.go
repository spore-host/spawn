package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/egressproxy"
	"github.com/spore-host/spawn/pkg/ttl"
)

// Readiness. A proxy is ready when spored is up on it and its port answers.
const (
	// ReadyTimeout bounds the wait for a proxy to become ready. spored's
	// first heartbeat comes about a minute after boot (cmd launch verification
	// allows Linux five minutes); this adds the probe.
	ReadyTimeout = 6 * time.Minute
	// readyPoll is the gap between readiness polls.
	readyPoll = 10 * time.Second
	// probeTimeout bounds one SSM probe.
	probeTimeout = 60 * time.Second
	// ProbeHost is what the readiness probe fetches through the proxy: a host
	// on the allowlist, so a proxy that answers it is listening, forwarding
	// and reaching the internet through its internet gateway.
	ProbeHost = "awscli.amazonaws.com"
)

// ProbeCommand is the shell command, run on the proxy over SSM, that succeeds
// only when the proxy at privateIP carries an HTTPS CONNECT to [ProbeHost].
// curl exits 0 on any HTTP response from the origin, and non-zero when the
// proxy refuses the tunnel or does not answer.
func ProbeCommand(privateIP string) string {
	return fmt.Sprintf("curl -sS -o /dev/null --max-time 20 --proxy http://%s:%d https://%s/",
		privateIP, egressproxy.Port, ProbeHost)
}

// Deps are find-or-create's AWS seams and clock.
type Deps struct {
	EC2    EC2
	Prober Prober
	// Now and Sleep default to the wall clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) sleep(ctx context.Context, wait time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, wait)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Spec is a new proxy.
type Spec struct {
	Region, VPCID string
	// AvailabilityZone is the launch's, which the proxy's subnet is chosen in
	// when the VPC has a public subnet there.
	AvailabilityZone string
	PublicSubnets    []spawnaws.PublicSubnet
	Groups           Groups
	AMI              string
	// InstanceProfile is the name of the spored instance profile.
	InstanceProfile string
	// UserData is the encoded user data ([UserData]).
	UserData string
	// BaseTags are the standard spawn launch tags (pkg/aws BuildLaunchTags,
	// with no TTL): Create adds the deadline and [ProxyTag].
	BaseTags []types.Tag
}

// ErrNoPublicSubnet is returned when the VPC has no subnet routed to an
// internet gateway to run a proxy in.
var ErrNoPublicSubnet = errors.New("the VPC has no public subnet routed to an internet gateway")

// PickSubnet is the subnet a proxy runs in: one of publics in az if there is
// one, else the first. publics come sorted by ID, so the choice is stable.
func PickSubnet(publics []spawnaws.PublicSubnet, az string) (spawnaws.PublicSubnet, error) {
	if len(publics) == 0 {
		return spawnaws.PublicSubnet{}, ErrNoPublicSubnet
	}
	for _, s := range publics {
		if s.AvailabilityZone == az {
			return s, nil
		}
	}
	return publics[0], nil
}

// ProxyTags are base with the proxy's lifetime and identity: spawn:ttl and
// spawn:ttl-deadline for a life of [egressproxy.MaxLife] from its
// spawn:launch-time (pkg/ttl.Tags, so the reaper backstop applies to it
// unmodified), its Name, and [ProxyTag]. A launch time missing from base is
// now. It returns the tags and the deadline.
func ProxyTags(base []types.Tag, vpcID string, now time.Time) ([]types.Tag, time.Time) {
	launch := now.UTC().Truncate(time.Second)
	set := map[string]bool{"spawn:ttl": true, "spawn:ttl-deadline": true, "Name": true, ProxyTag: true}
	var out []types.Tag
	for _, t := range base {
		k := aws.ToString(t.Key)
		if k == "spawn:launch-time" {
			if parsed, err := time.Parse(time.RFC3339, aws.ToString(t.Value)); err == nil {
				launch = parsed
			}
		}
		if !set[k] {
			out = append(out, t)
		}
	}
	if !hasTag(out, "spawn:launch-time") {
		out = append(out, types.Tag{Key: aws.String("spawn:launch-time"), Value: aws.String(launch.Format(time.RFC3339))})
	}
	deadline := launch.Add(egressproxy.MaxLife)
	for _, k := range []string{"spawn:ttl", "spawn:ttl-deadline"} {
		out = append(out, types.Tag{Key: aws.String(k), Value: aws.String(ttl.Tags(launch, deadline)[k])})
	}
	out = append(out,
		types.Tag{Key: aws.String("Name"), Value: aws.String(InstanceName(vpcID))},
		types.Tag{Key: aws.String(ProxyTag), Value: aws.String(vpcID)})
	return out, deadline
}

func hasTag(tags []types.Tag, key string) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return true
		}
	}
	return false
}

// RunInput is the RunInstances request for s: one t4g.micro in a public
// subnet with a public IP from EC2 (no Elastic IP), the proxy group as its
// only group, IMDSv2 required, the spored profile, and the proxy's tags on
// the instance and its volume. It terminates rather than stops on an OS
// shutdown: a proxy has nothing to come back to. It returns the input and the
// proxy's deadline.
func RunInput(s Spec, now time.Time) (*ec2.RunInstancesInput, time.Time, error) {
	subnet, err := PickSubnet(s.PublicSubnets, s.AvailabilityZone)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w (%s)", err, s.VPCID)
	}
	tags, deadline := ProxyTags(s.BaseTags, s.VPCID, now)
	return &ec2.RunInstancesInput{
		ImageId:      aws.String(s.AMI),
		InstanceType: types.InstanceType(InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		IamInstanceProfile: &types.IamInstanceProfileSpecification{
			Name: aws.String(s.InstanceProfile),
		},
		UserData: aws.String(s.UserData),
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			SubnetId:                 aws.String(subnet.SubnetID),
			Groups:                   []string{s.Groups.ProxyID},
			AssociatePublicIpAddress: aws.Bool(true),
			DeleteOnTermination:      aws.Bool(true),
		}},
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired,
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
			HttpPutResponseHopLimit: aws.Int32(1),
		},
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
	}, deadline, nil
}

// profileBackoff retries a RunInstances that EC2 refuses for a just-created
// instance profile, the transient pkg/aws retries on every launch (#572).
var profileBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

func isProfilePropagation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "InvalidParameterValue") &&
		strings.Contains(err.Error(), "Invalid IAM Instance Profile name")
}

// Create launches a proxy for s and returns it, not yet ready.
func Create(ctx context.Context, d Deps, s Spec) (Proxy, error) {
	now := d.now()
	in, deadline, err := RunInput(s, now)
	if err != nil {
		return Proxy{}, err
	}
	var out *ec2.RunInstancesOutput
	for attempt := 0; ; attempt++ {
		out, err = d.EC2.RunInstances(ctx, in)
		if err == nil || !isProfilePropagation(err) || attempt >= len(profileBackoff) {
			break
		}
		if serr := d.sleep(ctx, profileBackoff[attempt]); serr != nil {
			break
		}
	}
	if err != nil {
		return Proxy{}, fmt.Errorf("launch the egress proxy for %s: %w", s.VPCID, err)
	}
	if len(out.Instances) == 0 {
		return Proxy{}, fmt.Errorf("launch the egress proxy for %s: RunInstances returned no instance", s.VPCID)
	}
	inst := out.Instances[0]
	p := Proxy{
		InstanceID: aws.ToString(inst.InstanceId),
		PrivateIP:  aws.ToString(inst.PrivateIpAddress),
		SubnetID:   aws.ToString(in.NetworkInterfaces[0].SubnetId),
		Version:    tagMap(in.TagSpecifications[0].Tags)["spawn:version"],
		LaunchTime: deadline.Add(-egressproxy.MaxLife),
		Deadline:   deadline,
		Created:    true,
	}
	if inst.Placement != nil {
		p.AvailabilityZone = aws.ToString(inst.Placement.AvailabilityZone)
	}
	return p, nil
}

// Terminate ends a proxy. The launch that created one which never became
// ready calls it; it is the teardown `spawn egress-proxy delete` can reuse.
func Terminate(ctx context.Context, api EC2, instanceID string) error {
	_, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return fmt.Errorf("terminate egress proxy %s: %w", instanceID, err)
	}
	return nil
}

// ErrNotReady is wrapped when a proxy did not become ready.
var ErrNotReady = errors.New("the egress proxy did not become ready")

// terminalStates are states from which a proxy can no longer serve.
var terminalStates = map[string]bool{"shutting-down": true, "terminated": true, "stopping": true, "stopped": true}

// WaitReady waits, up to timeout, until the proxy is ready: spored is up on
// it (its spawn:last-heartbeat tag, the evidence `spawn launch` verifies every
// instance's spored by) and its port carries a CONNECT through to the
// internet ([ProbeCommand], over SSM, which reaches the proxy through its own
// public IP). It returns the proxy with its private IP.
func WaitReady(ctx context.Context, d Deps, region string, p Proxy, timeout time.Duration) (Proxy, error) {
	if timeout <= 0 {
		timeout = ReadyTimeout
	}
	deadline := d.now().Add(timeout)
	var last string
	for {
		out, err := d.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{p.InstanceID}})
		switch {
		case spawnaws.IsAccessDenied(err):
			return p, fmt.Errorf("%w: %s: cannot read it: %v", ErrNotReady, p.InstanceID, err)
		case err != nil:
			last = "DescribeInstances: " + err.Error()
		default:
			inst, ok := findInstance(out, p.InstanceID)
			if !ok {
				last = "DescribeInstances does not list it yet"
				break
			}
			state := ""
			if inst.State != nil {
				state = string(inst.State.Name)
			}
			if terminalStates[state] {
				return p, fmt.Errorf("%w: %s is %s", ErrNotReady, p.InstanceID, state)
			}
			if ip := aws.ToString(inst.PrivateIpAddress); ip != "" {
				p.PrivateIP = ip
			}
			if tagMap(inst.Tags)[spawnaws.HeartbeatTag] == "" {
				last = "spored has not written " + spawnaws.HeartbeatTag + " yet (" + orUnknown(state) + ")"
				break
			}
			if p.PrivateIP == "" {
				last = "it has no private IP yet"
				break
			}
			res, perr := d.Prober.RunShellScript(ctx, region, p.InstanceID, ProbeCommand(p.PrivateIP), probeTimeout)
			switch {
			case spawnaws.IsAccessDenied(perr):
				return p, fmt.Errorf("%w: %s: spored is up, but spawn cannot check its port over SSM: %v", ErrNotReady, p.InstanceID, perr)
			case perr != nil:
				last = "the port probe could not run over SSM: " + perr.Error()
			case res.Status == "Success":
				return p, nil
			default:
				last = fmt.Sprintf("the port probe (%s) ended %s: %s", ProbeCommand(p.PrivateIP), res.Status,
					strings.TrimSpace(strings.TrimSpace(res.Stdout)+" "+strings.TrimSpace(res.Stderr)))
			}
		}
		if !d.now().Before(deadline) {
			return p, fmt.Errorf("%w within %v: %s: %s", ErrNotReady, timeout, p.InstanceID, last)
		}
		wait := readyPoll
		if rem := deadline.Sub(d.now()); rem < wait {
			wait = rem
		}
		if err := d.sleep(ctx, wait); err != nil {
			return p, fmt.Errorf("%w: %s: %v (last: %s)", ErrNotReady, p.InstanceID, err, last)
		}
	}
}

func findInstance(out *ec2.DescribeInstancesOutput, id string) (types.Instance, bool) {
	if out == nil {
		return types.Instance{}, false
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if aws.ToString(i.InstanceId) == id {
				return i, true
			}
		}
	}
	return types.Instance{}, false
}

// Request is a launch's find-or-create.
type Request struct {
	Region, VPCID, AvailabilityZone string
	PublicSubnets                   []spawnaws.PublicSubnet
	// Version is this spawn's buildinfo version.
	Version string
	// Idle is the proxy's idle period, at most [egressproxy.MaxLife].
	Idle time.Duration
	// InstanceProfile is the name of the spored instance profile.
	InstanceProfile string
	// AMI, UserData and BaseTags are resolved only when a proxy is created.
	AMI      func(ctx context.Context) (string, error)
	UserData func(idle time.Duration) (string, error)
	BaseTags func(ctx context.Context) ([]types.Tag, error)
	// Out receives progress lines; nil discards them.
	Out io.Writer
	// ReadyTimeout bounds each readiness wait; [ReadyTimeout] when zero.
	ReadyTimeout time.Duration
}

// Result is find-or-create's outcome.
type Result struct {
	Proxy  Proxy
	Groups Groups
	// Rejected are the VPC's proxies that were not reused, and why.
	Rejected []Rejection
}

// FindOrCreate is the launch path: the VPC's groups, then a reusable proxy
// that answers, else a new one, ready.
//
// A proxy that is not reused (another spawn's version, near its deadline) is
// left running, not terminated: another launch's members may be using it, and
// it ends on its own, at its idle period or its deadline. Two launches racing
// in one VPC may each create a proxy; both serve, and the one nobody uses
// expires at its idle period. Neither is worth a lock.
//
// A proxy this call created that does not become ready is terminated before
// the error is returned, so a failed launch leaves no proxy behind. The groups
// are kept: they cost nothing, the next launch reuses them, and the reaper's
// managed-network sweep or [DeleteGroups] removes them once unused.
func FindOrCreate(ctx context.Context, d Deps, r Request) (Result, error) {
	if err := egressproxy.ValidateIdle(r.Idle, egressproxy.MaxLife); err != nil {
		return Result{}, err
	}
	groups, err := EnsureGroups(ctx, d.EC2, r.VPCID, d.now())
	if err != nil {
		return Result{}, err
	}
	res := Result{Groups: groups}
	reusable, rejected, err := Find(ctx, d.EC2, Want{
		VPCID: r.VPCID, Version: r.Version, ProxyGroupID: groups.ProxyID,
		InstanceProfile: r.InstanceProfile, Now: d.now(),
	})
	if err != nil {
		return res, err
	}
	res.Rejected = rejected
	for _, rj := range rejected {
		fprintf(r.Out, "   egress proxy %s not reused: %s\n", rj.InstanceID, rj.Reason)
	}
	for _, p := range reusable {
		ready, werr := WaitReady(ctx, d, r.Region, p, r.ReadyTimeout)
		if werr == nil {
			res.Proxy = ready
			return res, nil
		}
		// Not ours to end: it may be another launch's, still starting.
		fprintf(r.Out, "   egress proxy %s not reused: %v\n", p.InstanceID, werr)
		res.Rejected = append(res.Rejected, Rejection{InstanceID: p.InstanceID, Reason: werr.Error()})
	}

	spec := Spec{Region: r.Region, VPCID: r.VPCID, AvailabilityZone: r.AvailabilityZone,
		PublicSubnets: r.PublicSubnets, Groups: groups, InstanceProfile: r.InstanceProfile}
	if _, err := PickSubnet(spec.PublicSubnets, spec.AvailabilityZone); err != nil {
		return res, fmt.Errorf("%w (%s)", err, r.VPCID)
	}
	if spec.AMI, err = r.AMI(ctx); err != nil {
		return res, fmt.Errorf("the egress proxy's AMI (%s): %w", AMIParameter, err)
	}
	if spec.UserData, err = r.UserData(r.Idle); err != nil {
		return res, fmt.Errorf("the egress proxy's user data: %w", err)
	}
	if spec.BaseTags, err = r.BaseTags(ctx); err != nil {
		return res, fmt.Errorf("the egress proxy's tags: %w", err)
	}
	created, err := Create(ctx, d, spec)
	if err != nil {
		return res, err
	}
	fprintf(r.Out, "   launched egress proxy %s in %s; waiting for it to be ready (up to %v)\n",
		created.InstanceID, created.SubnetID, orDefault(r.ReadyTimeout, ReadyTimeout))
	ready, err := WaitReady(ctx, d, r.Region, created, r.ReadyTimeout)
	if err != nil {
		// Use a context the caller's cancellation cannot stop, so a ^C during
		// the wait still ends what this call created.
		if terr := Terminate(context.WithoutCancel(ctx), d.EC2, created.InstanceID); terr != nil {
			return res, errorsJoin(err, fmt.Errorf("and it could not be terminated, so terminate it yourself "+
				"(`spawn terminate %s`); its deadline and the TTL reaper end it otherwise: %w", created.InstanceID, terr))
		}
		return res, fmt.Errorf("%w; terminated it", err)
	}
	ready.Created = true
	res.Proxy = ready
	return res, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Client is the AWS side of find-or-create for a launch in region, through
// the launching spawn's client: its EC2 and SSM, the spored instance profile
// (created if absent, as any launch does), the AL2023 arm64 AMI, and the
// standard launch tags for a proxy launched by spawn version.
type Client struct {
	Deps Deps
	// Profile returns the spored instance profile's name.
	Profile func(ctx context.Context) (string, error)
	// AMI and BaseTags are [Request.AMI] and [Request.BaseTags].
	AMI      func(ctx context.Context) (string, error)
	BaseTags func(ctx context.Context) ([]types.Tag, error)
}

// ClientFor is [Client] through c.
func ClientFor(c *spawnaws.Client, region, version string) Client {
	cfg := c.Config().Copy()
	cfg.Region = region
	return Client{
		Deps:    Deps{EC2: ec2.NewFromConfig(cfg), Prober: c},
		Profile: c.SetupSporedIAMRole,
		AMI: func(ctx context.Context) (string, error) {
			return c.GetAL2023AMI(ctx, region, "arm64", false)
		},
		BaseTags: func(ctx context.Context) ([]types.Tag, error) {
			tagger, err := c.NewLaunchTagger(ctx)
			if err != nil {
				return nil, err
			}
			return tagger.Tags(ctx, LaunchConfig(region, version))
		},
	}
}

// LaunchConfig is the launch config a proxy's base tags are rendered from
// (pkg/aws LaunchTagger): no TTL, since [ProxyTags] adds the proxy's own.
func LaunchConfig(region, version string) spawnaws.LaunchConfig {
	return spawnaws.LaunchConfig{
		Name: "spawn-egress-proxy", InstanceType: InstanceType, Region: region,
		SpawnVersion: version, TargetOS: "linux",
	}
}
