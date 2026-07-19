package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/observability"
	"github.com/spore-host/spawn/pkg/observability/tracing"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// EC2Provider implements Provider for EC2 instances
type EC2Provider struct {
	imdsClient *imds.Client
	ec2Client  *ec2.Client
	identity   *Identity
	config     *Config
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

	imdsClient := imds.NewFromConfig(cfg)

	// Get instance identity document
	idDoc, err := imdsClient.GetInstanceIdentityDocument(checkCtx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance identity (not EC2?): %w", err)
	}

	instanceID := idDoc.InstanceID
	region := idDoc.Region
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
	}

	// Update config with region
	cfg.Region = region

	// Load config from tags first to check if tracing is enabled
	ec2Client := ec2.NewFromConfig(cfg)
	providerConfig, instanceName, err := loadConfigFromEC2Tags(ctx, ec2Client, instanceID)
	if err != nil {
		log.Printf("Warning: Could not load config from tags: %v", err)
		providerConfig = &Config{
			IdleCPUPercent: 5.0,
			Observability:  observability.DefaultConfig(),
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
func (p *EC2Provider) RefreshConfig(ctx context.Context) error {
	fresh, _, err := loadConfigFromEC2Tags(ctx, p.ec2Client, p.identity.InstanceID)
	if err != nil {
		return err
	}
	p.config = fresh
	return nil
}

func (p *EC2Provider) Terminate(ctx context.Context, reason string) error {
	log.Printf("Terminating EC2 instance %s (reason: %s)", p.identity.InstanceID, reason)

	_, err := p.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{p.identity.InstanceID},
	})

	if err != nil {
		return fmt.Errorf("failed to terminate: %w", err)
	}

	log.Printf("Terminate request sent")
	return nil
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

func (p *EC2Provider) DiscoverPeers(ctx context.Context, jobArrayID string) ([]PeerInfo, error) {
	if jobArrayID == "" {
		return nil, nil
	}

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

// ebsPricePerGBMonth returns the approximate per-GB-month price for common EBS volume types.
// Prices are for us-east-1; other regions are within ~10% of these.
func ebsPricePerGBMonth(volumeType string) float64 {
	switch strings.ToLower(volumeType) {
	case "gp3":
		return 0.08
	case "gp2":
		return 0.10
	case "io1", "io2":
		return 0.125
	case "st1":
		return 0.045
	case "sc1":
		return 0.015
	default:
		return 0.08 // gp3 default
	}
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

// LookupAndTagEBSCost queries the instance's attached volumes, calculates the hourly
// EBS storage cost, and stores it as spawn:ebs-hourly-cost. Called once at first
// spored start; subsequent starts read the tag instead of re-querying.
func (p *EC2Provider) LookupAndTagEBSCost(ctx context.Context) float64 {
	if p.config.EBSHourlyCost > 0 {
		return p.config.EBSHourlyCost // already known from tag
	}

	// Describe the instance to get block device mappings
	descOut, err := p.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{p.identity.InstanceID},
	})
	if err != nil || len(descOut.Reservations) == 0 || len(descOut.Reservations[0].Instances) == 0 {
		log.Printf("Warning: could not describe instance for EBS cost lookup: %v", err)
		return 0.003 // safe fallback
	}

	inst := descOut.Reservations[0].Instances[0]
	var volumeIDs []string
	for _, bdm := range inst.BlockDeviceMappings {
		if bdm.Ebs != nil && bdm.Ebs.VolumeId != nil {
			volumeIDs = append(volumeIDs, *bdm.Ebs.VolumeId)
		}
	}
	if len(volumeIDs) == 0 {
		return 0.003
	}

	// Describe volumes to get sizes and types
	volOut, err := p.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: volumeIDs,
	})
	if err != nil {
		log.Printf("Warning: could not describe volumes for EBS cost lookup: %v", err)
		return 0.003
	}

	const hoursPerMonth = 730.0
	var totalHourlyCost float64
	for _, vol := range volOut.Volumes {
		sizeGB := float64(aws.ToInt32(vol.Size))
		pricePerGBMonth := ebsPricePerGBMonth(string(vol.VolumeType))
		totalHourlyCost += sizeGB * pricePerGBMonth / hoursPerMonth
	}

	log.Printf("EBS volumes: %d vol(s), hourly cost = $%.4f/hr", len(volOut.Volumes), totalHourlyCost)

	// Store as a tag so subsequent spored starts don't need to re-query
	_, _ = p.ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{p.identity.InstanceID},
		Tags: []types.Tag{
			{
				Key:   aws.String(tagprefix.Tag("ebs-hourly-cost")),
				Value: aws.String(strconv.FormatFloat(totalHourlyCost, 'f', 6, 64)),
			},
		},
	})

	p.config.EBSHourlyCost = totalHourlyCost
	return totalHourlyCost
}

// loadConfigFromEC2Tags loads configuration from EC2 instance tags.
// It returns the Config and the value of the EC2 Name tag (may be empty).
func loadConfigFromEC2Tags(ctx context.Context, client *ec2.Client, instanceID string) (*Config, string, error) {
	output, err := client.DescribeTags(ctx, &ec2.DescribeTagsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("resource-id"),
				Values: []string{instanceID},
			},
		},
	})
	if err != nil {
		return nil, "", err
	}

	config := &Config{
		IdleCPUPercent: 5.0, // Default
		Observability:  observability.DefaultConfig(),
	}

	var instanceName string
	for _, tag := range output.Tags {
		if tag.Key == nil || tag.Value == nil {
			continue
		}

		switch *tag.Key {
		case "Name":
			instanceName = *tag.Value
		case tagprefix.Tag("ttl"):
			if duration, err := time.ParseDuration(*tag.Value); err == nil {
				config.TTL = duration
			}
		case tagprefix.Tag("ttl-deadline"):
			if t, err := time.Parse(time.RFC3339, *tag.Value); err == nil {
				config.TTLDeadline = t
			}
		case tagprefix.Tag("launch-time"):
			if t, err := time.Parse(time.RFC3339, *tag.Value); err == nil {
				config.LaunchTime = t
			}
		case tagprefix.Tag("compute-seconds"):
			if s, err := strconv.ParseInt(*tag.Value, 10, 64); err == nil {
				config.ComputeSeconds = s
			}
		case tagprefix.Tag("ebs-hourly-cost"):
			if f, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.EBSHourlyCost = f
			}
		case tagprefix.Tag("idle-timeout"):
			if duration, err := time.ParseDuration(*tag.Value); err == nil {
				config.IdleTimeout = duration
			}
		case tagprefix.Tag("hibernate-on-idle"):
			config.HibernateOnIdle = *tag.Value == "true"
		case tagprefix.Tag("cost-limit"):
			if limit, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.CostLimit = limit
			}
		case tagprefix.Tag("price-per-hour"):
			if price, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.PricePerHour = price
			}
		case tagprefix.Tag("idle-cpu"):
			if cpu, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.IdleCPUPercent = cpu
			}
		case tagprefix.Tag("dns-name"):
			config.DNSName = *tag.Value
		case tagprefix.Tag("account-base36"):
			config.AccountBase36 = *tag.Value
		case tagprefix.Tag("account-name"):
			config.AccountName = *tag.Value
		case tagprefix.Tag("notify-url"):
			config.NotifyURL = *tag.Value
		case tagprefix.Tag("notify-command"):
			config.NotifyCommand = *tag.Value
		case tagprefix.Tag("notify-platform"):
			config.NotifyPlatform = *tag.Value
		case tagprefix.Tag("active-ports"):
			for _, p := range strings.Split(*tag.Value, ",") {
				p = strings.TrimSpace(p)
				if port, err := strconv.Atoi(p); err == nil && port > 0 {
					config.ActivePorts = append(config.ActivePorts, port)
				}
			}
		case tagprefix.Tag("active-processes"):
			for _, p := range strings.Split(*tag.Value, ",") {
				if p = strings.TrimSpace(p); p != "" {
					config.ActiveProcesses = append(config.ActiveProcesses, p)
				}
			}
		case tagprefix.Tag("dcv-session-id"):
			config.DCVSessionID = *tag.Value
		case tagprefix.Tag("slack-workspace-id"):
			config.SlackWorkspaceID = *tag.Value
		case tagprefix.Tag("pre-stop"):
			config.PreStop = *tag.Value
		case tagprefix.Tag("pre-stop-timeout"):
			if duration, err := time.ParseDuration(*tag.Value); err == nil {
				config.PreStopTimeout = duration
			}
		case tagprefix.Tag("spot-webhook-url"):
			config.SpotWebhookURL = *tag.Value
		case tagprefix.Tag("webhook-correlation"):
			config.WebhookCorrelation = *tag.Value
		case tagprefix.Tag("webhook-timeout"):
			if duration, err := time.ParseDuration(*tag.Value); err == nil {
				config.WebhookTimeout = duration
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
			if duration, err := time.ParseDuration(*tag.Value); err == nil {
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

		// Observability - Metrics
		case tagprefix.Tag("metrics-enabled"):
			config.Observability.Metrics.Enabled = *tag.Value == "true"
		case tagprefix.Tag("metrics-port"):
			if port, err := strconv.Atoi(*tag.Value); err == nil && port >= 1024 && port <= 65535 {
				config.Observability.Metrics.Port = port
			}
		case tagprefix.Tag("metrics-bind"):
			config.Observability.Metrics.Bind = *tag.Value
		case tagprefix.Tag("metrics-path"):
			config.Observability.Metrics.Path = *tag.Value

		// Observability - Tracing
		case tagprefix.Tag("tracing-enabled"):
			config.Observability.Tracing.Enabled = *tag.Value == "true"
		case tagprefix.Tag("tracing-exporter"):
			config.Observability.Tracing.Exporter = *tag.Value
		case tagprefix.Tag("tracing-sampling"):
			if rate, err := strconv.ParseFloat(*tag.Value, 64); err == nil {
				config.Observability.Tracing.SamplingRate = rate
			}
		case tagprefix.Tag("tracing-endpoint"):
			config.Observability.Tracing.Endpoint = *tag.Value
		}
	}

	// Set default completion file if on-complete is set but file isn't specified
	if config.OnComplete != "" && config.CompletionFile == "" {
		config.CompletionFile = "/tmp/SPAWN_COMPLETE"
	}

	// Set default completion delay if on-complete is set but delay isn't specified
	if config.OnComplete != "" && config.CompletionDelay == 0 {
		config.CompletionDelay = 30 * time.Second
	}

	// Load plugin declarations written to /etc/spawn/plugins.json by user-data.
	config.Plugins = loadPluginDeclarations()

	return config, instanceName, nil
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
