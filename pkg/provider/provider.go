package provider

import (
	"context"
	"log"
	"time"

	"github.com/spore-host/spawn/pkg/observability"
)

// Identity represents the instance's identity information
type Identity struct {
	InstanceID string // EC2 instance ID or local hostname
	Name       string // EC2 Name tag / local config name
	Region     string // AWS region or "local"
	AccountID  string // AWS account ID or organization name
	PublicIP   string // Public IP address
	PrivateIP  string // Private IP address
	Provider   string // "ec2" or "local"
}

// PluginDeclaration references a plugin to install at instance startup.
type PluginDeclaration struct {
	Ref    string            `json:"ref" yaml:"ref"`
	Config map[string]string `json:"config,omitempty" yaml:"config,omitempty"`
}

// Config represents the agent configuration
type Config struct {
	TTL             time.Duration
	TTLDeadline     time.Time // absolute deadline = launch_time + TTL; authoritative across stop/wake cycles
	LaunchTime      time.Time // original launch time; never resets on stop/wake
	ComputeSeconds  int64     // accumulated compute seconds since launch (updated by spored)
	EBSHourlyCost   float64   // actual EBS cost per hour (queried at first start, tagged for reuse)
	IdleTimeout     time.Duration
	HibernateOnIdle bool
	CostLimit       float64
	PricePerHour    float64 // On-demand price per hour (recorded at launch; used for cost-limit enforcement)
	IdleCPUPercent  float64

	// Pre-stop hook
	PreStop        string        // Shell command to run before any lifecycle-triggered stop
	PreStopTimeout time.Duration // Max time to wait (default: 5m)
	LocalUsername  string        // Instance's primary user; pre-stop runs as this user, not root (#63). Empty = run as root (older instances).

	// Ephemeral FSx (#194): when an FSx is created asynchronously alongside the
	// instance, the launch path tags spawn:fsx-pending=<fs-id> + spawn:fsx-mount-point.
	// spored polls until the filesystem is AVAILABLE, mounts it, then flips the
	// tag to spawn:fsx-id (so the reaper's refcount, #192, sees a live user).
	FSxPending    string // pending FSx filesystem id to mount once AVAILABLE (empty = nothing pending)
	FSxMountPoint string // mount point for the pending FSx (default: /fsx)
	FSxImportPath string // S3 import path for the pending FSx's data-repository association
	FSxExportPath string // S3 export path for the pending FSx's data-repository association

	// Completion signal settings
	OnComplete      string        // Action: terminate, stop, hibernate, exit
	CompletionFile  string        // File path to watch
	CompletionDelay time.Duration // Grace period before action

	// DNS settings
	DNSName string

	// Notification settings — populated from EC2 tags at startup
	NotifyURL        string   // spore-bot Lambda Function URL for lifecycle notifications
	SlackWorkspaceID string   // Slack workspace ID (e.g. "T03NE3GTY")
	NotifyCommand    string   // Slash command for workspace config lookup (e.g. "/spore", "/prism")
	NotifyPlatform   string   // Chat platform for notifications: "slack" (default), "teams", or "discord" (#2)
	AccountBase36    string   // base36-encoded account ID for full DNS FQDN (name.base36.spore.host)
	AccountName      string   // DNS-safe slug of the account's friendly name; drives the alias FQDN name.account-name.spore.host (#121). Empty => base36 only.
	ActivePorts      []int    // TCP ports to monitor for active connections (e.g. 8787 for RStudio)
	ActiveProcesses  []string // Process names to check; if any running, instance is not idle (e.g. "rsession")

	// DCVSessionID is the NICE DCV session ID for application streaming instances.
	// When set, spored uses DCV client connectivity as the primary idle signal,
	// short-circuiting CPU/network/disk checks (DCV streaming generates those itself).
	DCVSessionID string

	// Job array settings
	JobArrayID    string
	JobArrayName  string
	JobArrayIndex int
	JobArraySize  int

	// Plugin declarations to install at instance startup.
	Plugins []PluginDeclaration

	// Observability settings
	Observability observability.Config
}

// PeerInfo represents information about a peer instance
type PeerInfo struct {
	Index      int    `json:"index"`
	InstanceID string `json:"instance_id"`
	IP         string `json:"ip"`
	DNS        string `json:"dns"`
	Provider   string `json:"provider"` // "ec2" or "local"
}

// InterruptionInfo represents Spot instance interruption details
type InterruptionInfo struct {
	Action string    // "terminate" or "stop"
	Time   time.Time // When interruption will occur
}

// Provider abstracts the compute environment (EC2 vs local)
type Provider interface {
	// GetIdentity returns instance identity information
	GetIdentity(ctx context.Context) (*Identity, error)

	// GetConfig returns agent configuration (from tags or config file)
	GetConfig(ctx context.Context) (*Config, error)

	// RefreshConfig re-reads configuration from the source (EC2 tags or config file)
	// and updates the cached config. Called periodically to pick up tag changes.
	RefreshConfig(ctx context.Context) error

	// Terminate shuts down the instance (EC2) or exits process (local)
	Terminate(ctx context.Context, reason string) error

	// Stop stops the instance (EC2 only, no-op for local)
	Stop(ctx context.Context, reason string) error

	// Hibernate hibernates the instance (EC2 only, no-op for local)
	Hibernate(ctx context.Context) error

	// DiscoverPeers finds peer instances in the same job array
	DiscoverPeers(ctx context.Context, jobArrayID string) ([]PeerInfo, error)

	// IsSpotInstance returns true if running on a Spot instance
	IsSpotInstance(ctx context.Context) bool

	// CheckSpotInterruption checks for Spot interruption notice
	CheckSpotInterruption(ctx context.Context) (*InterruptionInfo, error)

	// GetProviderType returns the provider type ("ec2" or "local")
	GetProviderType() string

	// LookupAndTagEBSCost returns the hourly EBS storage cost, querying AWS if
	// not already cached in the spawn:ebs-hourly-cost tag.
	LookupAndTagEBSCost(ctx context.Context) float64
}

// NewProvider creates a provider based on the environment
// It auto-detects EC2 by trying IMDS, falls back to local
func NewProvider(ctx context.Context) (Provider, error) {
	// Try EC2 first with a fresh context (not the cancelled one from check)
	ec2Provider, err := NewEC2Provider(context.Background())
	if err == nil {
		return ec2Provider, nil
	}

	// Fall back to local provider
	log.Printf("EC2 provider failed (%v), using local provider", err)
	return NewLocalProvider(context.Background())
}
