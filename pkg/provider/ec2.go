package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"io"
	"log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/costmodel"
	"github.com/spore-host/spawn/pkg/elasticip"
	"github.com/spore-host/spawn/pkg/instancestate"
	"github.com/spore-host/spawn/pkg/launchconfig"
	"github.com/spore-host/spawn/pkg/observability"
	"github.com/spore-host/spawn/pkg/observability/tracing"
	"github.com/spore-host/spawn/pkg/spotrequest"
	"github.com/spore-host/spawn/pkg/tagprefix"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

// EC2Provider implements Provider for EC2 instances
type EC2Provider struct {
	imdsClient *imds.Client
	ec2Client  *ec2.Client
	identity   *Identity
	config     *Config

	// registryClient reads the hybrid registry. Only a burst member
	// (Config.Burst) uses it, to find the local nodes of its job array (#918).
	registryClient registryQueryAPI
	registryTable  string

	// launch loads the launch config object once per boot (#1359).
	launch *launchConfigLoader
}

// NewEC2Provider creates an EC2 provider
func NewEC2Provider(ctx context.Context) (*EC2Provider, error) {
	// Create a context with short timeout for IMDS check
	// This makes detection fast when not on EC2
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(checkCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	if err := spawnconfig.CheckAccount(checkCtx, cfg); err != nil { // --account / SPORE_ACCOUNT (#1118)
		return nil, err
	}

	imdsClient := imds.NewFromConfig(cfg)

	// Get instance identity document
	idDoc, err := imdsClient.GetInstanceIdentityDocument(checkCtx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance identity (not EC2?): %w", err)
	}

	instanceID := idDoc.InstanceID
	region := idDoc.Region

	// Before anything below reads plugins.json or the peers file: discard the
	// per-instance state an instance launched from an image inherits (#1241).
	discardInheritedState(instanceID)
	accountID := idDoc.AccountID
	availabilityZone := idDoc.AvailabilityZone

	// Get public IP from metadata
	publicIPResult, err := imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "public-ipv4",
	})
	var publicIP string
	if err == nil {
		ipBytes, _ := io.ReadAll(publicIPResult.Content)
		publicIP = strings.TrimSpace(string(ipBytes))
	}

	// Get private IP from metadata
	privateIPResult, err := imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "local-ipv4",
	})
	var privateIP string
	if err == nil {
		ipBytes, _ := io.ReadAll(privateIPResult.Content)
		privateIP = strings.TrimSpace(string(ipBytes))
	}

	identity := &Identity{
		InstanceID:       instanceID,
		Region:           region,
		AvailabilityZone: availabilityZone,
		AccountID:        accountID,
		PublicIP:         publicIP,
		PrivateIP:        privateIP,
		Provider:         "ec2",
		PendingTime:      idDoc.PendingTime,
	}

	// Update config with region
	cfg.Region = region

	// Load config from tags first to check if tracing is enabled
	ec2Client := ec2.NewFromConfig(cfg)
	launch := &launchConfigLoader{
		s3:        s3.NewFromConfig(cfg),
		bucket:    launchconfig.Bucket(accountID, region),
		account:   accountID,
		cachePath: launchconfig.CachePath,
	}
	providerConfig, instanceName, err := loadConfig(ctx, ec2Client, instanceID, launch)
	if err != nil {
		log.Printf("Warning: Could not load config from tags: %v", err)
		providerConfig = &Config{
			IdleCPUPercent: 5.0,
			Observability:  observability.DefaultConfig(),
			// Every field above is a default, not a measurement — a caller
			// (spawn status/spored status) must not present TTL=0/IdleTimeout=0
			// as "no TTL configured" when the real story is "couldn't read the
			// config that says otherwise" (spawn#508).
			ConfigLoadError: err.Error(),
		}
		instanceName = ""
	}
	identity.Name = instanceName

	// Instrument AWS SDK with tracing if enabled
	if providerConfig.Observability.Tracing.Enabled {
		tracing.InstrumentAWSConfig(&cfg)
		// Recreate EC2 client with instrumented config
		ec2Client = ec2.NewFromConfig(cfg)
		log.Printf("AWS SDK instrumented with tracing")
	}

	return &EC2Provider{
		imdsClient: imdsClient,
		ec2Client:  ec2Client,
		identity:   identity,
		config:     providerConfig,
		// Same config, and so the same region, as the EC2 client: the
		// instance's own region, which is where `spawn burst` creates the
		// registry table and launches its members.
		registryClient: dynamodb.NewFromConfig(cfg),
		registryTable:  HybridRegistryTable,
		launch:         launch,
	}, nil
}

func (p *EC2Provider) GetIdentity(ctx context.Context) (*Identity, error) {
	return p.identity, nil
}

func (p *EC2Provider) GetConfig(ctx context.Context) (*Config, error) {
	return p.config, nil
}

// RefreshConfig re-reads configuration from EC2 tags and updates the cached config.
// Called periodically by the agent to pick up tag changes (e.g. spawn extend).
//
// The launch config object is not fetched again (#1359): it is fixed at
// launch, so the copy loaded at boot is reused. Only one that could not be
// loaded is tried again.
func (p *EC2Provider) RefreshConfig(ctx context.Context) error {
	fresh, _, err := loadConfig(ctx, p.ec2Client, p.identity.InstanceID, p.launch)
	if err != nil {
		return err
	}
	p.config = fresh
	return nil
}

func (p *EC2Provider) Terminate(ctx context.Context, reason string) error {
	log.Printf("Terminating EC2 instance %s (reason: %s)", p.identity.InstanceID, reason)

	// Cancel this instance's Spot request first (#922). A persistent request —
	// what a --spot launch with a stop/hibernate lifecycle gets — reopens when its
	// instance terminates and EC2 launches a replacement. Best-effort: spored
	// terminates at the TTL, on-complete and the cost limit, and an instance past
	// any of those must die even if the cancel is refused.
	err := spotrequest.Terminate(ctx, p.ec2Client, p.identity.Region, p.identity.InstanceID, spotrequest.BestEffort,
		func(msg string) { log.Printf("Warning: %s", msg) })

	if err != nil {
		return fmt.Errorf("failed to terminate: %w", err)
	}

	log.Printf("Terminate request sent")

	// The launch Elastic IP (#661), released only now: before the terminate,
	// losing the address would cut the egress the TerminateInstances call
	// itself needs, and an instance that failed to terminate would run on
	// without it. Never fails the terminate.
	p.ReleaseOwnElasticIPs(ctx)
	return nil
}

// ReleaseOwnElasticIPs releases the Elastic IP a multi-interface EFA launch
// allocated for this instance (#661), if there is one. Only `spawn terminate`
// used to release it, so an instance ending by its TTL, idle or cost limit,
// on-complete or a Spot reclaim left it allocated and billing.
//
// One ReleaseAddress per address and no DisassociateAddress: the instance
// reaches EC2 through that address, so after a disassociate it could not send
// the release. In a default VPC the release disassociates too; in a nondefault
// VPC EC2 answers InUse and the address is left to the TTL reaper, which
// releases it once EC2 has detached it. Best-effort and logged, never an error:
// it runs while the instance is going away.
func (p *EC2Provider) ReleaseOwnElasticIPs(ctx context.Context) {
	if p.ec2Client == nil || p.identity == nil {
		return
	}
	releaseOwnElasticIPs(ctx, p.ec2Client, p.identity.InstanceID)
}

func releaseOwnElasticIPs(ctx context.Context, api elasticip.API, instanceID string) {
	// Bounded: this runs on the way down, and a hung call must not hold it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	res, err := elasticip.ReleaseForInstance(ctx, api, instanceID)
	for _, a := range res.Released {
		log.Printf("Released Elastic IP %s allocated for %s", a, instanceID)
	}
	for _, a := range res.InUse {
		log.Printf("Elastic IP %s is still associated (nondefault VPC); the TTL reaper releases it once %s is gone", a, instanceID)
	}
	if err != nil {
		log.Printf("Warning: could not release the Elastic IP allocated for %s: %v (the TTL reaper releases it later)", instanceID, err)
	}
}

// DetectSpotReplacement reports whether this instance is a replacement EC2
// launched from a leaked spawn persistent Spot request (#995). See
// spotrequest.DetectReplacement for the rule; any read failure is
// VerdictUnknown.
func (p *EC2Provider) DetectSpotReplacement(ctx context.Context) spotrequest.Detection {
	return spotrequest.DetectReplacement(ctx, p.ec2Client, p.identity.InstanceID)
}

// RetireSpotReplacement cancels a detected replacement's Spot request and then
// terminates this instance, falling back to shutdown only if the terminate is
// refused (#1080). It refuses anything but a VerdictReplacement detection.
func (p *EC2Provider) RetireSpotReplacement(ctx context.Context, d spotrequest.Detection, shutdown func(context.Context) error) (spotrequest.RetireOutcome, error) {
	return spotrequest.RetireReplacement(ctx, p.ec2Client, d, p.identity.InstanceID, shutdown)
}

func (p *EC2Provider) Stop(ctx context.Context, reason string) error {
	log.Printf("Stopping EC2 instance %s (reason: %s)", p.identity.InstanceID, reason)

	_, err := p.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{p.identity.InstanceID},
	})

	if err != nil {
		return fmt.Errorf("failed to stop: %w", err)
	}

	log.Printf("Stop request sent")
	return nil
}

func (p *EC2Provider) Hibernate(ctx context.Context) error {
	log.Printf("Hibernating EC2 instance %s", p.identity.InstanceID)

	_, err := p.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{p.identity.InstanceID},
		Hibernate:   aws.Bool(true),
	})

	if err != nil {
		log.Printf("Failed to hibernate: %v, falling back to stop", err)
		// Fall back to regular stop
		return p.Stop(ctx, "hibernate failed")
	}

	log.Printf("Hibernate request sent")
	return nil
}

// DiscoverPeers finds the members of jobArrayID.
//
// Every member finds its EC2 siblings by the spawn:job-array-id tag. A burst
// member (spawn:burst=true) also reads the hybrid registry, because the local
// nodes it was launched to work alongside have no EC2 tags and are only listed
// there (#918). The two sets are merged with each instance listed once. An
// ordinary `spawn launch --count` member stays tag-only: it is not in the
// registry and nothing else of its array is either (#904).
//
// A registry read that fails does not fail discovery. The member keeps its tag
// peers and the failure is logged, as the local provider does.
func (p *EC2Provider) DiscoverPeers(ctx context.Context, jobArrayID string) ([]PeerInfo, error) {
	if jobArrayID == "" {
		return nil, nil
	}

	peers, err := p.discoverPeersByTag(ctx, jobArrayID)
	if err != nil {
		return nil, err
	}

	if !p.readsHybridRegistry() {
		return peers, nil
	}

	registryPeers, err := queryRegistryPeers(ctx, p.registryClient, p.registryTable, jobArrayID)
	if err != nil {
		log.Printf("Warning: could not read hybrid registry peers for job array %s: %v (using EC2 peers only)", jobArrayID, err)
		return peers, nil
	}

	merged := mergePeers(peers, registryPeers)
	log.Printf("✓ Discovered %d peers in job array %s (%d by tag, %d from the hybrid registry)",
		len(merged), jobArrayID, len(peers), len(registryPeers))
	return merged, nil
}

// readsHybridRegistry reports whether this member's discovery includes the
// hybrid registry: only a burst member does, and only with a client to read it.
func (p *EC2Provider) readsHybridRegistry() bool {
	return p.config != nil && p.config.Burst && p.registryClient != nil
}

// discoverPeersByTag finds the running members of jobArrayID through their
// spawn:job-array-id tag.
func (p *EC2Provider) discoverPeersByTag(ctx context.Context, jobArrayID string) ([]PeerInfo, error) {
	log.Printf("Discovering peers for job array: %s", jobArrayID)

	// Query for all instances with the same job-array-id
	instances, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String(tagprefix.FilterTag("job-array-id")),
				Values: []string{jobArrayID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running"},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query job array instances: %w", err)
	}

	// Build peer list
	var peers []PeerInfo
	accountBase36 := intToBase36(p.identity.AccountID)

	for _, reservation := range instances.Reservations {
		for _, instance := range reservation.Instances {
			// Extract job array index from tags
			var index int
			var name string
			for _, tag := range instance.Tags {
				if *tag.Key == tagprefix.Tag("job-array-index") {
					index, _ = strconv.Atoi(*tag.Value)
				}
				if *tag.Key == "Name" {
					name = *tag.Value
				}
			}

			publicIP := ""
			if instance.PublicIpAddress != nil {
				publicIP = *instance.PublicIpAddress
			}

			// Generate DNS name: {name}.{account-base36}.spore.host
			dnsName := fmt.Sprintf("%s.%s.spore.host", name, accountBase36)

			peer := PeerInfo{
				Index:      index,
				InstanceID: *instance.InstanceId,
				IP:         publicIP,
				DNS:        dnsName,
				Provider:   "ec2",
			}
			peers = append(peers, peer)
		}
	}

	// Sort by index
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].Index < peers[j].Index
	})

	// NOTE: the /etc/spawn/job-array-peers.json file is no longer written here.
	// MPI peer distribution moved to the cohort control-plane Assembler, which
	// pushes that file to every node over SSM (pkg/mpicohort). spored no longer
	// self-discovers peers for MPI; DiscoverPeers remains for the hybrid registry
	// and diagnostics.
	log.Printf("✓ Discovered %d peers in job array %s", len(peers), jobArrayID)
	return peers, nil
}

func (p *EC2Provider) IsSpotInstance(ctx context.Context) bool {
	// Bound the IMDS call: a hung metadata endpoint must never block the caller.
	// spored calls this on the monitor's startup path, so an unbounded stall here
	// would silently kill TTL/idle/on-complete/pre-stop enforcement (#65).
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := p.imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "instance-life-cycle",
	})
	if err != nil {
		return false // assume on-demand if we can't tell
	}

	body, err := io.ReadAll(result.Content)
	if err != nil {
		return false
	}

	lifecycle := strings.TrimSpace(string(body))
	return lifecycle == "spot"
}

func (p *EC2Provider) CheckSpotInterruption(ctx context.Context) (*InterruptionInfo, error) {
	// Bound the IMDS call so a hung endpoint can't stall the spot-monitor goroutine (#65).
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := p.imdsClient.GetMetadata(ctx, &imds.GetMetadataInput{
		Path: "spot/instance-action",
	})

	if err != nil {
		// 404 means no interruption notice
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		// A timeout/cancellation is "couldn't tell" — treat as no interruption
		// rather than erroring, so the 5s poll loop doesn't log-spam (#65).
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, nil
		}
		return nil, err
	}

	// Parse the response
	body, err := io.ReadAll(result.Content)
	if err != nil {
		return nil, fmt.Errorf("error reading Spot interruption response: %w", err)
	}

	var action struct {
		Action string `json:"action"`
		Time   string `json:"time"`
	}

	if err := json.Unmarshal(body, &action); err != nil {
		return nil, fmt.Errorf("error parsing Spot interruption JSON: %w", err)
	}

	interruptTime, err := time.Parse(time.RFC3339, action.Time)
	if err != nil {
		return nil, fmt.Errorf("error parsing interruption time: %w", err)
	}

	return &InterruptionInfo{
		Action: action.Action,
		Time:   interruptTime,
	}, nil
}

func (p *EC2Provider) GetProviderType() string {
	return "ec2"
}

// CountOtherManagedInstances counts spawn:managed instances in this region that
// are running or pending, excluding this instance (#260).
func (p *EC2Provider) CountOtherManagedInstances(ctx context.Context) int {
	out, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:spawn:managed"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{"running", "pending"}},
		},
	})
	if err != nil {
		log.Printf("region-vacated check: DescribeInstances failed: %v", err)
		return -1
	}
	count := 0
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if aws.ToString(inst.InstanceId) == p.identity.InstanceID {
				continue // don't count ourselves
			}
			count++
		}
	}
	return count
}

// MeasureCostFootprint measures what this instance pays for by the hour
// beside compute — every attached EBS volume (size, IOPS, throughput) and every
// public IPv4 address on any of its interfaces — and records it in the
// spawn:cost-footprint tag that --cost-limit is enforced from (pkg/costmodel, #51;
// one tag since #1428, which had three).
//
// It measures every time it is called, not once: a volume attached or an
// Elastic IP associated after the first boot (the launch EIP of a
// multi-interface EFA launch is associated once the instance is running, so
// spored's first look can precede it) used to be missed for the instance's
// whole life. A recorded value never goes DOWN: the cost model prices the
// current footprint over the instance's whole life, so lowering it after a
// detach would retroactively forgive cost already incurred, and the cap would
// fire late. Keeping the maximum can overstate after a detach — the safe
// direction.
//
// The bool distinguishes a real measurement from a failure (spawn#517):
// measured is false whenever DescribeInstances or DescribeVolumes failed or the
// instance had no volumes, and nothing is tagged then.
func (p *EC2Provider) MeasureCostFootprint(ctx context.Context) (CostFootprint, bool) {
	descOut, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{p.identity.InstanceID},
	})
	if err != nil || len(descOut.Reservations) == 0 || len(descOut.Reservations[0].Instances) == 0 {
		log.Printf("Warning: could not describe instance for cost footprint: %v", err)
		return CostFootprint{}, false
	}
	inst := descOut.Reservations[0].Instances[0]

	var volumeIDs []string
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.Ebs != nil && bdm.Ebs.VolumeId != nil {
			volumeIDs = append(volumeIDs, *bdm.Ebs.VolumeId)
		}
	}
	if len(volumeIDs) == 0 {
		return CostFootprint{}, false
	}
	volOut, err := p.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: volumeIDs})
	if err != nil {
		log.Printf("Warning: could not describe volumes for cost footprint: %v", err)
		return CostFootprint{}, false
	}

	fp := costFootprintOf(inst, volOut.Volumes)
	log.Printf("Cost footprint: %d vol(s) $%.4f/hr EBS, %d auto-assigned + %d Elastic public IPv4",
		len(volOut.Volumes), fp.EBSHourlyCost, fp.PublicIPv4, fp.ElasticIPv4)

	tags := make([]types.Tag, 0, 1)
	for _, t := range footprintTagWrites(fp, inst.Tags) {
		tags = append(tags, types.Tag{Key: aws.String(t[0]), Value: aws.String(t[1])})
	}
	if len(tags) > 0 {
		_, _ = p.ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{p.identity.InstanceID},
			Tags:      tags,
		})
	}
	return fp, true
}

// CostFootprint is the non-compute part of what an instance pays for by the
// hour, as MeasureCostFootprint measured it.
type CostFootprint struct {
	EBSHourlyCost float64
	PublicIPv4    int // auto-assigned (Amazon-owned)
	ElasticIPv4   int
}

// costFootprintOf prices an instance's volumes and counts its public IPv4
// addresses from what DescribeInstances and DescribeVolumes returned. Split out
// so the counting is testable without EC2.
func costFootprintOf(inst types.Instance, vols []types.Volume) CostFootprint {
	var fp CostFootprint
	for _, v := range vols {
		fp.EBSHourlyCost += costmodel.VolumeHourly(costmodel.Volume{
			Type:       string(v.VolumeType),
			SizeGiB:    aws.ToInt32(v.Size),
			IOPS:       aws.ToInt32(v.Iops),
			Throughput: aws.ToInt32(v.Throughput),
		})
	}
	fp.PublicIPv4, fp.ElasticIPv4 = costmodel.CountIPv4(instanceAddresses(inst))
	return fp
}

// instanceAddresses lists every public IPv4 association on every interface of
// the instance, primary and secondary private addresses alike. With no
// interfaces reported it falls back to the instance's PublicIpAddress, owner
// unknown (counted as Elastic, i.e. billing on the wall clock: the safe side).
func instanceAddresses(inst types.Instance) []costmodel.Address {
	var out []costmodel.Address
	for _, ni := range inst.NetworkInterfaces {
		if ni.Association != nil {
			out = append(out, costmodel.Address{PublicIP: aws.ToString(ni.Association.PublicIp), IPOwnerID: aws.ToString(ni.Association.IpOwnerId)})
		}
		for _, pa := range ni.PrivateIpAddresses {
			if pa.Association != nil {
				out = append(out, costmodel.Address{PublicIP: aws.ToString(pa.Association.PublicIp), IPOwnerID: aws.ToString(pa.Association.IpOwnerId)})
			}
		}
	}
	if len(inst.NetworkInterfaces) == 0 && aws.ToString(inst.PublicIpAddress) != "" {
		out = append(out, costmodel.Address{PublicIP: aws.ToString(inst.PublicIpAddress)})
	}
	return out
}

// footprintTagWrites is the tags (key, value) to write for a measured
// footprint: the one spawn:cost-footprint tag (#1428), when a figure would RISE above
// what the instance already records (see MeasureCostFootprint for why it never
// falls) or it is absent. What it records is read as every reader reads it
// (costmodel.MeasuredFromTags), so the three tags a spored before #1428 wrote
// count too, and each figure written is the larger of the measured and the
// recorded one. Those three are never written: a launch keeps one slot for
// the footprint, not four.
func footprintTagWrites(fp CostFootprint, current []types.Tag) [][2]string {
	have := map[string]string{}
	for _, t := range current {
		have[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	recorded := costmodel.MeasuredFromTags(have, tagprefix.Prefix())
	value := costmodel.EncodeFootprint(recorded.Max(costmodel.Measured{
		EBSPerHour: fp.EBSHourlyCost, PublicIPv4: fp.PublicIPv4, ElasticIPv4: fp.ElasticIPv4,
	}))
	key := tagprefix.Tag(costmodel.TagFootprint)
	if old, ok := have[key]; ok && old == value {
		return nil
	}
	return [][2]string{{key, value}}
}

// loadConfig is spored's configuration: the instance's tags, then the
// launch-fixed settings from the launch config object the spawn:config tag
// names (#1359). launch may be nil (no object is read).
func loadConfig(ctx context.Context, client *ec2.Client, instanceID string, launch *launchConfigLoader) (*Config, string, error) {
	cfg, name, tags, err := loadConfigFromEC2Tags(ctx, client, instanceID)
	if err != nil {
		return nil, "", err
	}
	if launch != nil {
		launch.apply(ctx, cfg, tags)
	}
	return cfg, name, nil
}

// loadConfigFromEC2Tags loads configuration from EC2 instance tags.
// It returns the Config, the value of the EC2 Name tag (may be empty), and the
// tags read.
func loadConfigFromEC2Tags(ctx context.Context, client *ec2.Client, instanceID string) (*Config, string, []types.TagDescription, error) {
	output, err := client.DescribeTags(ctx, &ec2.DescribeTagsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("resource-id"),
				Values: []string{instanceID},
			},
		},
	})
	if err != nil {
		return nil, "", nil, err
	}
	config, instanceName := configFromTags(output.Tags)

	// Load plugin declarations written to /etc/spawn/plugins.json by user-data.
	config.Plugins = loadPluginDeclarations()

	return config, instanceName, output.Tags, nil
}

// configFromTags builds spored's Config from the instance's tags, and returns
// the EC2 Name tag (may be empty). Split from loadConfigFromEC2Tags so the
// parsing is testable without EC2. Every duration tag is read with pkg/ttl's
// shared parser — the one launch and the TTL reaper use — so a value the CLI
// accepted (e.g. "1d") is never silently ignored here (#990).
func configFromTags(tags []types.TagDescription) (*Config, string) {
	config := &Config{
		IdleCPUPercent: 5.0, // Default
		Observability:  observability.DefaultConfig(),
	}

	var instanceName string
	footprint := map[string]string{}
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			continue
		}

		switch *tag.Key {
		case "Name":
			instanceName = *tag.Value
		case tagprefix.Tag(launchconfig.TagSuffix):
			// The launch config object (#1359): the launch-fixed settings,
			// which have no case here. launchConfigLoader.apply reads them.
			config.LaunchConfigSHA = *tag.Value
		case tagprefix.Tag("ttl"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.TTL = duration
			}
		case tagprefix.Tag("ttl-deadline"):
			if t, err := time.Parse(time.RFC3339, *tag.Value); err == nil {
				config.TTLDeadline = t
			}
		case tagprefix.Tag("task-id"):
			config.TaskID = *tag.Value
		case tagprefix.Tag("launch-time"):
			if t, err := time.Parse(time.RFC3339, *tag.Value); err == nil {
				config.LaunchTime = t
			}
		case tagprefix.Tag("compute-seconds"):
			if s, err := strconv.ParseInt(*tag.Value, 10, 64); err == nil {
				config.ComputeSeconds = s
			}
		case tagprefix.Tag(costmodel.TagFootprint), tagprefix.Tag(costmodel.TagEBSHourlyCost),
			tagprefix.Tag(costmodel.TagPublicIPv4), tagprefix.Tag(costmodel.TagElasticIPv4):
			// The measured footprint: spawn:cost-footprint, or the three tags a
			// spored before #1428 wrote. Read after the loop, as every
			// reader reads it.
			footprint[*tag.Key] = *tag.Value
		case tagprefix.Tag("idle-timeout"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.IdleTimeout = duration
			}
		case tagprefix.Tag("hibernate-on-idle"):
			config.HibernateOnIdle = *tag.Value == "true"
		case tagprefix.Tag("terminate-on-idle"):
			config.TerminateOnIdle = *tag.Value == "true"
		case tagprefix.Tag("cost-limit"):
			if limit, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.CostLimit = limit
			}
		case tagprefix.Tag("price-per-hour"):
			if price, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.PricePerHour = price
			}
		case tagprefix.Tag("spot-price-per-hour"):
			if price, err := strconv.ParseFloat(*tag.Value, 64); err == nil && price > 0 {
				config.SpotPricePerHour = price
			}
		case tagprefix.Tag("dns-name"):
			config.DNSName = *tag.Value
		case tagprefix.Tag("dns-domain"):
			config.DNSDomain = *tag.Value
		case tagprefix.Tag("dns-endpoint"):
			config.DNSEndpoint = *tag.Value
		case tagprefix.Tag("account-base36"):
			config.AccountBase36 = *tag.Value
		case tagprefix.Tag("account-name"):
			config.AccountName = *tag.Value
		case tagprefix.Tag("dcv-session-id"):
			config.DCVSessionID = *tag.Value
		case tagprefix.Tag("pre-stop"):
			config.PreStop = *tag.Value
		case tagprefix.Tag("pre-stop-timeout"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.PreStopTimeout = duration
			}
		case tagprefix.Tag("local-username"):
			config.LocalUsername = *tag.Value
		case tagprefix.Tag("fsx-pending"):
			config.FSxPending = *tag.Value
		case tagprefix.Tag("fsx-mount-point"):
			config.FSxMountPoint = *tag.Value
		case tagprefix.Tag("fsx-s3-import-path"):
			config.FSxImportPath = *tag.Value
		case tagprefix.Tag("fsx-s3-export-path"):
			config.FSxExportPath = *tag.Value
		case tagprefix.Tag("on-complete"):
			config.OnComplete = *tag.Value
		case tagprefix.Tag("completion-file"):
			config.CompletionFile = *tag.Value
		case tagprefix.Tag("completion-delay"):
			if duration, err := ttlpkg.ParseDuration(*tag.Value); err == nil {
				config.CompletionDelay = duration
			}
		case tagprefix.Tag("job-array-id"):
			config.JobArrayID = *tag.Value
		case tagprefix.Tag("job-array-name"):
			config.JobArrayName = *tag.Value
		case tagprefix.Tag("job-array-size"):
			if size, err := strconv.Atoi(*tag.Value); err == nil {
				config.JobArraySize = size
			}
		case tagprefix.Tag("job-array-index"):
			if index, err := strconv.Atoi(*tag.Value); err == nil {
				config.JobArrayIndex = index
			}
		case tagprefix.Tag("burst"):
			config.Burst = *tag.Value == "true"

		// Observability - Metrics
		case tagprefix.Tag("metrics-enabled"):
			config.Observability.Metrics.Enabled = *tag.Value == "true"

		// Observability - Tracing
		case tagprefix.Tag("tracing-endpoint"):
			// No launch writes this; a tag a user may set by hand.
			config.Observability.Tracing.Endpoint = *tag.Value
		}
	}
	m := costmodel.MeasuredFromTags(footprint, tagprefix.Prefix())
	config.EBSHourlyCost, config.PublicIPv4Count, config.ElasticIPv4Count = m.EBSPerHour, m.PublicIPv4, m.ElasticIPv4

	// Set default completion file if on-complete is set but file isn't specified
	if config.OnComplete != "" && config.CompletionFile == "" {
		config.CompletionFile = DefaultCompletionFile
	}

	// Set default completion delay if on-complete is set but delay isn't specified
	if config.OnComplete != "" && config.CompletionDelay == 0 {
		config.CompletionDelay = 30 * time.Second
	}

	return config, instanceName
}

// reconcileInstanceState is instancestate.ReconcileHost, stripClusterKeys
// instancestate.StripClusterKeysHost, and the two
// predicates below the platform checks; variables so tests can count the calls.
var (
	reconcileInstanceState = instancestate.ReconcileHost
	stripClusterKeys       = instancestate.StripClusterKeysHost
	stateOnLinux           = runtime.GOOS == "linux"
	stateAsRoot            = func() bool { return os.Geteuid() == 0 }
)

// discardInheritedState removes the state this instance inherited from the
// image it was launched from (#1241). Linux only, where those paths are, and
// only as root: an unprivileged `spored` subcommand could not remove them, and
// the daemon and the bootstrap run as root.
func discardInheritedState(instanceID string) {
	if !stateOnLinux || !stateAsRoot() {
		return
	}
	removed, from, err := reconcileInstanceState(instanceID)
	if from != "" {
		log.Printf("%s was launched from an image of %s; discarded %d inherited state file(s): %v", instanceID, from, len(removed), removed)
	}
	if err != nil {
		log.Printf("Warning: checking for inherited instance state: %v", err)
	}
	// The MPI cluster key root's authorized_keys inherited (#1343). Here too,
	// so an instance launched from the image by something other than spawn
	// strips it when the image's spored starts.
	n, err := stripClusterKeys(instanceID)
	if n > 0 {
		log.Printf("Removed %d inherited MPI cluster-key line(s) from %s", n, instancestate.RootAuthorizedKeys)
	}
	if err != nil {
		log.Printf("Warning: removing inherited MPI cluster keys: %v", err)
	}
}

// loadPluginDeclarations reads /etc/spawn/plugins.json if present.
func loadPluginDeclarations() []PluginDeclaration {
	data, err := os.ReadFile("/etc/spawn/plugins.json")
	if err != nil {
		return nil // File is optional.
	}
	var decls []PluginDeclaration
	if err := json.Unmarshal(data, &decls); err != nil {
		log.Printf("Warning: failed to parse /etc/spawn/plugins.json: %v", err)
		return nil
	}
	return decls
}

// intToBase36 converts an AWS account ID to base36
func intToBase36(accountID string) string {
	num, err := strconv.ParseUint(accountID, 10, 64)
	if err != nil {
		return accountID
	}
	return strconv.FormatUint(num, 36)
}
