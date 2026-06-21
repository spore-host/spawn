package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/observability"
)

// LocalProvider implements Provider for local (non-EC2) systems
type LocalProvider struct {
	identity      *Identity
	config        *Config
	configPath    string
	dynamoClient  *dynamodb.Client
	registryTable string
}

// NewLocalProvider creates a local provider
func NewLocalProvider(ctx context.Context) (*LocalProvider, error) {
	// Load local config
	configPath := os.Getenv("SPAWN_CONFIG")
	localConfig, err := config.LoadLocalConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load local config: %w", err)
	}

	// Get public IP
	publicIP := localConfig.PublicIP
	if publicIP == "" || publicIP == "auto" {
		publicIP = getPublicIP()
	}

	// Get private IP
	privateIP := localConfig.PrivateIP
	if privateIP == "" || privateIP == "auto" {
		privateIP = getPrivateIP()
	}

	identity := &Identity{
		InstanceID: localConfig.InstanceID,
		Name:       localConfig.Name,
		Region:     localConfig.Region,
		AccountID:  localConfig.AccountID,
		PublicIP:   publicIP,
		PrivateIP:  privateIP,
		Provider:   "local",
	}

	providerConfig := &Config{
		TTL:             config.ParseDuration(localConfig.TTL),
		IdleTimeout:     config.ParseDuration(localConfig.IdleTimeout),
		HibernateOnIdle: localConfig.HibernateOnIdle,
		IdleCPUPercent:  localConfig.IdleCPUPercent,
		OnComplete:      localConfig.OnComplete,
		CompletionFile:  localConfig.CompletionFile,
		CompletionDelay: config.ParseDuration(localConfig.CompletionDelay),
		DNSName:         localConfig.DNS.Name,
		JobArrayID:      localConfig.JobArray.ID,
		JobArrayName:    localConfig.JobArray.Name,
		JobArrayIndex:   localConfig.JobArray.Index,
		Observability: observability.Config{
			Metrics: observability.MetricsConfig{
				Enabled: localConfig.Observability.Metrics.Enabled,
				Port:    localConfig.Observability.Metrics.Port,
				Path:    localConfig.Observability.Metrics.Path,
				Bind:    localConfig.Observability.Metrics.Bind,
			},
			Tracing: observability.TracingConfig{
				Enabled:      localConfig.Observability.Tracing.Enabled,
				Exporter:     localConfig.Observability.Tracing.Exporter,
				SamplingRate: localConfig.Observability.Tracing.SamplingRate,
				Endpoint:     localConfig.Observability.Tracing.Endpoint,
			},
			Alerting: observability.AlertingConfig{
				PrometheusURL:   localConfig.Observability.Alerting.PrometheusURL,
				AlertmanagerURL: localConfig.Observability.Alerting.AlertmanagerURL,
			},
		},
	}

	if len(localConfig.Plugins) > 0 {
		decls := make([]PluginDeclaration, len(localConfig.Plugins))
		for i, d := range localConfig.Plugins {
			decls[i] = PluginDeclaration{Ref: d.Ref, Config: d.Config}
		}
		providerConfig.Plugins = decls
	}

	// Apply defaults if not set
	if !providerConfig.Observability.Metrics.Enabled && providerConfig.Observability.Metrics.Port == 0 {
		providerConfig.Observability = observability.DefaultConfig()
	} else {
		// Apply defaults for unset fields
		defaults := observability.DefaultConfig()
		if providerConfig.Observability.Metrics.Port == 0 {
			providerConfig.Observability.Metrics.Port = defaults.Metrics.Port
		}
		if providerConfig.Observability.Metrics.Path == "" {
			providerConfig.Observability.Metrics.Path = defaults.Metrics.Path
		}
		if providerConfig.Observability.Metrics.Bind == "" {
			providerConfig.Observability.Metrics.Bind = defaults.Metrics.Bind
		}
		if providerConfig.Observability.Tracing.Exporter == "" {
			providerConfig.Observability.Tracing.Exporter = defaults.Tracing.Exporter
		}
		if providerConfig.Observability.Tracing.SamplingRate == 0 {
			providerConfig.Observability.Tracing.SamplingRate = defaults.Tracing.SamplingRate
		}
	}

	provider := &LocalProvider{
		identity:      identity,
		config:        providerConfig,
		configPath:    configPath,
		registryTable: "spawn-hybrid-registry",
	}

	// Try to initialize DynamoDB client for hybrid coordination
	// This is optional - local-only mode works without it
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err == nil {
		provider.dynamoClient = dynamodb.NewFromConfig(cfg)
		log.Printf("DynamoDB client initialized for hybrid registry")
	} else {
		log.Printf("Warning: DynamoDB client not available: %v (local-only mode)", err)
	}

	return provider, nil
}

func (p *LocalProvider) GetIdentity(ctx context.Context) (*Identity, error) {
	return p.identity, nil
}

func (p *LocalProvider) GetConfig(ctx context.Context) (*Config, error) {
	return p.config, nil
}

func (p *LocalProvider) RefreshConfig(ctx context.Context) error {
	return nil // local provider config doesn't change at runtime
}

func (p *LocalProvider) Terminate(ctx context.Context, reason string) error {
	log.Printf("Local instance exiting (reason: %s)", reason)
	// Local mode: just exit process
	// Give a moment for logs to flush
	time.Sleep(1 * time.Second)
	os.Exit(0)
	return nil
}

func (p *LocalProvider) Stop(ctx context.Context, reason string) error {
	// Local instances don't support stop - same as terminate
	log.Printf("Local instance doesn't support stop, exiting instead (reason: %s)", reason)
	return p.Terminate(ctx, reason)
}

func (p *LocalProvider) Hibernate(ctx context.Context) error {
	// Local instances don't support hibernate - same as terminate
	log.Printf("Local instance doesn't support hibernate, exiting instead")
	return p.Terminate(ctx, "hibernate not supported")
}

func (p *LocalProvider) DiscoverPeers(ctx context.Context, jobArrayID string) ([]PeerInfo, error) {
	if jobArrayID == "" {
		return nil, nil
	}

	log.Printf("Discovering peers for job array: %s (local mode)", jobArrayID)

	// Try DynamoDB registry first (Phase 2)
	peers, err := p.discoverPeersFromDynamoDB(ctx, jobArrayID)
	if err == nil && len(peers) > 0 {
		return peers, nil
	}

	// Fall back to static peers file (Phase 1 compatibility)
	localConfig, err := config.LoadLocalConfig(p.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	if localConfig.JobArray.PeersFile != "" {
		log.Printf("Falling back to static peers file: %s", localConfig.JobArray.PeersFile)
		return loadPeersFromFile(localConfig.JobArray.PeersFile)
	}

	// No peers configured
	log.Printf("No peer discovery configured for local mode")
	return nil, nil
}

func (p *LocalProvider) discoverPeersFromDynamoDB(ctx context.Context, jobArrayID string) ([]PeerInfo, error) {
	if p.dynamoClient == nil {
		return nil, fmt.Errorf("DynamoDB client not available")
	}

	result, err := p.dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(p.registryTable),
		KeyConditionExpression: aws.String("job_array_id = :job_array_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":job_array_id": &types.AttributeValueMemberS{Value: jobArrayID},
		},
	})

	if err != nil {
		return nil, fmt.Errorf("failed to query DynamoDB registry: %w", err)
	}

	var peers []PeerInfo
	now := time.Now().Unix()

	for _, item := range result.Items {
		// Check if instance is still alive (heartbeat within TTL)
		expiresAt := getNumberValue(item["expires_at"])
		if expiresAt < now {
			// Instance expired, skip it
			continue
		}

		peer := PeerInfo{
			Index:      int(getNumberValue(item["index"])),
			InstanceID: getStringValue(item["instance_id"]),
			IP:         getStringValue(item["ip_address"]),
			DNS:        "", // Can construct if needed
			Provider:   getStringValue(item["provider"]),
		}

		peers = append(peers, peer)
	}

	log.Printf("Discovered %d peers from DynamoDB registry", len(peers))
	return peers, nil
}

// Helper functions for DynamoDB attribute parsing
func getStringValue(attr types.AttributeValue) string {
	if s, ok := attr.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func getNumberValue(attr types.AttributeValue) int64 {
	if n, ok := attr.(*types.AttributeValueMemberN); ok {
		val, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil {
			log.Printf("warning: unexpected DynamoDB number value %q: %v", n.Value, err)
			return 0
		}
		return val
	}
	return 0
}

func (p *LocalProvider) IsSpotInstance(ctx context.Context) bool {
	// Local instances are never Spot
	return false
}

func (p *LocalProvider) CheckSpotInterruption(ctx context.Context) (*InterruptionInfo, error) {
	// Local instances never have Spot interruptions
	return nil, nil
}

func (p *LocalProvider) GetProviderType() string {
	return "local"
}

func (p *LocalProvider) LookupAndTagEBSCost(_ context.Context) float64 {
	return 0 // no EBS on local provider
}

// CountOtherManagedInstances is not meaningful for the local provider — there's
// no region of managed instances to vacate. Return -1 (unknown) so the
// region-vacated check is skipped (#260).
func (p *LocalProvider) CountOtherManagedInstances(_ context.Context) int {
	return -1
}

// getPublicIP queries an external service to get the public IP
func getPublicIP() string {
	// Try multiple services in case one is down
	services := []string{
		"https://api.ipify.org",
		"https://ifconfig.me",
		"https://icanhazip.com",
	}

	client := &http.Client{Timeout: 5 * time.Second}

	for _, service := range services {
		resp, err := client.Get(service)
		if err != nil {
			continue
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			if err == nil {
				ip := strings.TrimSpace(string(body))
				if net.ParseIP(ip) != nil {
					return ip
				}
			}
		}
	}

	log.Printf("Warning: Could not determine public IP")
	return ""
}

// getPrivateIP gets the local network IP address
func getPrivateIP() string {
	// Get local IP by connecting to a remote address (doesn't actually send data)
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

// loadPeersFromFile loads peer information from a JSON file
func loadPeersFromFile(path string) ([]PeerInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read peers file: %w", err)
	}

	var peers []PeerInfo
	if err := json.Unmarshal(data, &peers); err != nil {
		return nil, fmt.Errorf("failed to parse peers file: %w", err)
	}

	log.Printf("✓ Loaded %d peers from %s", len(peers), path)
	return peers, nil
}
