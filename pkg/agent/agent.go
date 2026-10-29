package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/costmodel"
	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/pluginruntime"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/registry"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/taskproto"
)

type Agent struct {
	provider provider.Provider
	identity *provider.Identity
	// config is replaced wholesale by the monitor loop's periodic tag refresh
	// while other goroutines (FSx mount, spot monitor) read it concurrently, so
	// access goes through cfg()/setConfig() under configMu (#175). Don't read the
	// field directly from code that can run off the monitor goroutine.
	config                *provider.Config
	configMu              sync.RWMutex
	dnsClient             *dns.Client
	dnsDomain             string // DNS domain (e.g. "spore.host" or "prismcloud.host")
	dnsEndpoint           string // DNS updater URL; "" is the compiled-in spore.host one (#891)
	dnsTargetErr          error  // the domain or endpoint tag did not validate: never register (#891)
	registry              *registry.PeerRegistry
	pluginRuntime         *pluginruntime.Runtime
	notifier              *Notifier // Slack lifecycle notifications (nil if not configured)
	startTime             time.Time
	lastActivityTime      time.Time
	preStopDone           bool            // guards against running pre-stop hook more than once
	taskFlushDone         bool            // guards against writing the task terminal record twice (#632)
	spotWebhookFired      bool            // fire-once guard for the spot-interruption webhook (#228); the spot monitor re-enters every 5s
	prevCPUIdle           int64           // /proc/stat idle jiffies at last getCPUUsage call
	prevCPUTotal          int64           // /proc/stat total jiffies at last getCPUUsage call
	lastSessionTagWrite   time.Time       // throttle spawn:logged-in-count tag writes
	lastComputeTagWrite   time.Time       // throttle spawn:compute-seconds tag writes
	lastHeartbeatTagWrite time.Time       // throttle spawn:last-heartbeat tag writes (#497)
	nextDNSRefresh        time.Time       // when the DNS lease is next renewed (pkg/dns/lease.go)
	lastDNSStatus         string          // last spawn:dns-status written, so renewal only re-tags on change
	computeSecondsBase    int64           // compute-seconds already accumulated before this spored start
	liveClock             bool            // TotalComputeSeconds counts real running time (daemon, or a query that adopted its clock, #896)
	computeStart          time.Time       // when this boot's compute started counting; zero means startTime (#928)
	bootID                string          // kernel boot id, "" when unknown; tags the durable checkpoint (#928)
	kernelOrigin          kernelClocks    // the kernel clocks when this boot's compute started counting (#945)
	kernelOriginKnown     bool            // kernelOrigin is set; compute is then measured on CLOCK_MONOTONIC
	checkpointPath        string          // durable compute checkpoint; "" for a query agent or an unsupported platform (#928)
	checkpointWarned      bool            // a checkpoint write failure has been logged
	prevNetRx             int64           // /proc/net/dev RX bytes at last getNetworkBytes call
	prevNetTx             int64           // /proc/net/dev TX bytes at last getNetworkBytes call
	netSampled            bool            // prevNetRx/prevNetTx hold a real sample (a counter can legitimately be 0, #919)
	netUnknownWarned      bool            // "network cannot be measured" logged once, not every tick (#919)
	prevDiskBytes         int64           // whole-disk bytes read+written at the last diskIOBytes call (#937)
	diskSampled           bool            // prevDiskBytes holds a real sample (#937)
	diskUnknownWarned     bool            // "disk I/O cannot be measured" logged once, not every tick (#937)
	idleWarned            bool            // send idle_warning notification only once
	accelUnknownWarned    map[string]bool // accelerator families warned "cannot be measured", once each (#954)
	stopRetry             stopRetry       // backoff for a failed stop/hibernate, idle (#948) or on-complete (#978)
	completionAnnounced   bool            // completion notification, webhook and grace period done; once (#978)
	completionSpent       time.Time       // mtime of a completion file already acted on before a resume (#987)
	lastTick              tickReading     // the clocks at the last monitor tick, for resume detection (#987)
	lastTickSet           bool

	// currentIP is the public IP re-read after a resume (#987); see publicIP.
	// lookupPublicIP reads it; nil means IMDS on EC2. Tests set it.
	ipMu           sync.Mutex
	currentIP      string
	currentIPSet   bool
	lookupPublicIP func(context.Context) (string, error)

	// idleMu guards idleSnap and lastActivityTime. The monitor loop writes them;
	// the Prometheus scrape, the spot-interruption goroutine and status read
	// them (#952). The prev* sample fields above are NOT guarded: only the owner
	// of sampling touches them (sampleIdleSignals).
	idleMu   sync.Mutex
	idleSnap idleSnapshot

	// idleStateWarned limits a failing idle-state publish to one log line
	// (#1090). Touched only by the monitor loop.
	idleStateWarned bool

	// DCV auth token verifier (embedded HTTP server for seamless browser auth)
	dcvTokens          map[string]string // token → username
	dcvTokensMu        sync.Mutex
	dcvReadyURLWritten bool
	ttlWarned          bool // send ttl_warning notification only once

	// DCV handshake retry state (spawn#282 phase 2). The verifier starts once at
	// boot (must listen before DCV connects); the session-wait + token + tag write
	// are driven from the monitor loop (maybeSetupDCVAuth) so a transient failure
	// (slow dcvserver, a momentary ec2:CreateTags throttle) recovers instead of
	// being permanent — the same fix shape as the FSx run-once-but-retry guard.
	dcvVerifierStarted bool // the :8444 verifier is up (start once)
	dcvAuthDone        bool // ready-url written OR a terminal failure recorded (stop retrying)

	// Web-UI app readiness state (#590). For AppMode=="web": probe the app's HTTP
	// port, start a TLS reverse proxy on :443 (once), write spawn:ready-url.
	webProxyStarted bool   // the :443 TLS reverse proxy is up (start once)
	webReadyDone    bool   // web ready-url written OR a terminal failure recorded (stop retrying)
	webToken        string // access token gating the :443 proxy (empty when WebAuth=none); generated once

	// The written spawn:ready-url's host, and how to build it for another, so
	// refreshReadyURL can follow a public IP that changed on a resume (#1019).
	// Monitor loop only.
	readyURLHost  string
	readyURLBuild func(host string) string

	// dcv runs the `dcv` CLI shell-outs (list/describe sessions). Defaults to the
	// real exec-based runner; tests inject a fake so the handshake + idle logic is
	// exercisable without a DCV server (spawn#282 phase 3).
	dcv dcvRunner

	// tagger writes the spawn:ready-* tags. Defaults to the real EC2 CreateTags
	// impl; tests inject a fake so the handshake retry/terminal logic is testable
	// without real EC2 (spawn#282 phase 3).
	tagger tagPutter

	// The task flush report's seams (#1186). taskFlushWaitDelay is the hook's
	// exec.Cmd.WaitDelay (zero means the taskFlushWaitDelay constant);
	// consoleWriter replaces sysWriteConsole when non-nil. Tests only.
	taskFlushWaitDelay time.Duration
	consoleWriter      func(string) error

	// fsxMountStarted guards the async FSx mount (#194/#221) to run at most once.
	// The mount is (re)triggered from the monitor loop whenever spawn:fsx-pending
	// becomes visible — it may not be set at spored startup, because the headless
	// launch path tags the instance AFTER RunInstances (the create + Lustre-port
	// setup add seconds) and EC2 tags are eventually consistent. The original
	// run-once-at-startup invocation lost that race and silently never mounted.
	fsxMountStarted bool
	// fsxMounter replaces mountPendingFSx when non-nil. Tests only: the real one
	// polls the FSx API for up to 25 minutes (#968).
	fsxMounter func(context.Context)

	// monitorInterval is the lifecycle ticker period. Zero means the production
	// default (1 minute); tests set it small to exercise the loop quickly.
	monitorInterval time.Duration

	// reloadReqs carries `spored reload` requests into the monitor loop (#1278):
	// the daemon's SIGHUP handler (or, on Windows, the SCM ParamChange control)
	// calls RequestReload, and Monitor refreshes the config on its own goroutine
	// and publishes reloadMarkerPath. Created lazily by reloadRequests.
	reloadOnce sync.Once
	reloadReqs chan struct{}
	// reloadMarkerPath is where Monitor publishes its ReloadMarker; "" (a query
	// agent, an unsupported platform) publishes nothing.
	reloadMarkerPath string
	// reloadSeq counts reload requests handled. Touched only by Monitor.
	reloadSeq uint64
	// reloadHandled, when non-nil, runs after each handled reload request.
	// Tests only: it lets a test wait for the request without a timing bound.
	reloadHandled func()

	// configRefreshTick counts monitoring cycles for the periodic tag refresh
	// (tags are re-read every 5 ticks). Per-Agent rather than a package global so
	// it isn't shared across agents and resets cleanly per instance (#175).
	configRefreshTick int

	// footprintMeasuring is set while a MeasureCostFootprint goroutine runs, so
	// a slow EC2 call never stacks a second one (#51).
	footprintMeasuring atomic.Bool

	// earlyCompletionRefreshes counts tag refreshes triggered by a completion
	// signal seen before on-complete was known (#270); capped at
	// maxEarlyCompletionRefreshes.
	earlyCompletionRefreshes int
	// defaultCompletionFile replaces provider.DefaultCompletionFile when
	// non-empty. Tests only, so they need not write to /tmp.
	defaultCompletionFile string

	// peerState is the hybrid peer list spored maintains (#933, peers.go).
	peerState peerState

	// peerRefreshInterval is the peers-file refresh period. Zero means the
	// production default (PeerRefreshInterval); tests set it small.
	peerRefreshInterval time.Duration

	// warnDelay is how long stop/hibernate/terminate wait after warning
	// logged-in users. Zero means the production default (5s); tests set it
	// small.
	warnDelay time.Duration

	// graceSleep is the completion grace-period wait. A seam so tests count the
	// waits instead of timing them: on a slow runner the warnUsers probes around
	// it take long enough that wall-clock bounds flake (#978's test on Windows).
	graceSleep func(time.Duration)

	// regionVacatedSettle is how long the last-instance check waits before
	// re-confirming the region is empty (#260). Zero means the production default
	// (60s); tests set it small.
	regionVacatedSettle time.Duration

	// The launch Elastic IP release on a Spot terminate notice (#661): a
	// fire-once guard (the spot monitor re-enters every 5s), and seams for the
	// clock, the wait, and how the release goroutine is started. nil means
	// time.Now, sleepCtx and `go`. spotNow is also the clock the Spot
	// pre-stop measures its deadline with (#1311).
	spotEIPScheduled bool
	spotNow          func() time.Time
	spotEIPSleep     func(context.Context, time.Duration)
	spotEIPGo        func(func())
}

// cfg returns the current config under a read lock. Callers get a stable pointer
// to an immutable snapshot — setConfig swaps in a new pointer rather than
// mutating in place, so the returned value stays safe to read after the lock is
// released.
func (a *Agent) cfg() *provider.Config {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	return a.config
}

// setConfig swaps in a freshly-loaded config pointer under a write lock.
func (a *Agent) setConfig(c *provider.Config) {
	a.configMu.Lock()
	a.config = c
	a.configMu.Unlock()
}

// joinsHybridRegistry reports whether this instance registers with the hybrid
// registry (the spawn-hybrid-registry DynamoDB table): only a member of a hybrid
// job array does (#904).
//
// A hybrid member is either a local node (the local provider — its
// DiscoverPeers reads the registry to find the other members), or an EC2
// instance `spawn burst` launched to join one, which it marks spawn:burst=true.
// `spawn burst` is also the only thing that creates the table
// (registry.EnsureTable).
//
// spawn:job-array-id alone is not enough: buildJobArrayMemberConfig puts it on
// every `spawn launch --count` member, MPI or not. Nothing reads the registry
// for those arrays — EC2 peer discovery uses tags, and MPI peers come from the
// cohort Assembler — while registering failed on every member (the instance
// role grants no DynamoDB actions, and the table exists only where burst has
// run), leaving a warning and heartbeat noise on every boot.
func joinsHybridRegistry(identity *provider.Identity, config *provider.Config) bool {
	if identity == nil || config == nil || config.JobArrayID == "" {
		return false
	}
	return identity.Provider == "local" || config.Burst
}

// NewAgent builds the agent AND activates it: initialization logging, the EBS
// cost lookup, DNS registration, job-array registry registration with its
// heartbeat, peer discovery, and plugin loading. This is what the DAEMON wants.
//
// Query subcommands must use [NewAgentForQuery] instead — see #733.
func NewAgent(ctx context.Context, prov provider.Provider) (*Agent, error) {
	return newAgent(ctx, prov, true)
}

// NewAgentForQuery builds the agent WITHOUT any side effect: no DNS
// registration, no tag writes, no registry registration, no heartbeat
// goroutine, no plugin loading, no initialization logging.
//
// It exists because `spored status` called NewAgent, and NewAgent registers DNS
// — an HTTP POST that mutates a Route53 record, plus an EC2 tag write. So
// `spawn status`, a read-only query, performed a control-plane WRITE on every
// invocation, and polling it in a loop (the normal way to watch a job) generated
// one write per poll (#733).
//
// The reported symptom was cosmetic — `Warning: Failed to register DNS: DNS API
// returned HTTP 403` appearing above the status summary, alarming during a demo —
// but that warning was only visible BECAUSE the write was failing. Had it
// succeeded, nothing would have printed and the behaviour would still have been
// wrong.
//
// Correct long-term shape is to split construction from a Start/Activate the
// daemon calls. This is the narrow version of that: one constructor, the side
// effects behind a flag, so there is no parallel path to drift.
func NewAgentForQuery(ctx context.Context, prov provider.Provider) (*Agent, error) {
	return newAgent(ctx, prov, false)
}

// The constructors newAgent's activation reaches the outside world through. On
// an instance each is the real thing; they are variables so a test can see
// activation happen WITHOUT it happening (#968). TestNewAgent_StillActivates
// used to call the real ones, and with a developer's ambient credentials that
// was a real Route53 UPSERT through the DNS API, an EC2 CreateTags and a
// DynamoDB PutItem on every `make check` — and it passed either way, because
// NewAgent logs those failures rather than returning them. This package's
// TestMain (internal/hermetic) now fails any test that reaches the network.
var (
	newDNSClient    = dns.NewClient
	newPeerRegistry = registry.NewPeerRegistry
	newTagPutter    = func(region string) tagPutter { return &ec2TagPutter{region: region} }
)

func newAgent(ctx context.Context, prov provider.Provider, activate bool) (*Agent, error) {
	// Get identity from provider
	identity, err := prov.GetIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get identity: %w", err)
	}

	// Get config from provider
	config, err := prov.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	// If TTL is set but TTLDeadline was not written (pre-deadline instances), synthesize
	// an absolute deadline from LaunchTime so the TTL is never reset by a spored restart.
	if config.TTL > 0 && config.TTLDeadline.IsZero() {
		anchor := config.LaunchTime
		if anchor.IsZero() {
			anchor = time.Now()
		}
		config.TTLDeadline = anchor.Add(config.TTL)
	}

	agent := &Agent{
		provider:           prov,
		identity:           identity,
		config:             config,
		startTime:          time.Now(),
		lastActivityTime:   time.Now(),
		computeSecondsBase: config.ComputeSeconds, // carry over accumulated time from before this start
		dcv:                execDCVRunner{},       // real `dcv` CLI; tests override
	}
	// The daemon IS the clock; a query agent is not until it adopts the
	// daemon's (AdoptPublishedComputeClock, #896).
	agent.liveClock = activate

	// Carry compute over from earlier boots (#928). The tag alone lost whatever
	// the last pre-stop write missed, and counting from spored's own start
	// dropped every boot's OS and cloud-init time, which EC2 bills; the durable
	// checkpoint fixes both (resolveComputeStart).
	agent.bootID = readBootID()
	var checkpoint *ComputeCheckpoint
	if p := ComputeCheckpointPath(); p != "" {
		if c, err := ReadComputeCheckpoint(p); err == nil {
			checkpoint = &c
		} else if !os.IsNotExist(err) && activate {
			log.Printf("Warning: ignoring unreadable compute checkpoint: %v", err)
		}
		if activate {
			agent.checkpointPath = p
		}
	}
	if activate {
		agent.reloadMarkerPath = ReloadMarkerPath() // #1278
		kernelNow, kernelOK := readKernelClocks()
		start := resolveComputeStart(computeStartInputs{
			InstanceID: identity.InstanceID,
			BootID:     agent.bootID,
			BootTime:   sysBootTime(),
			LaunchTime: config.LaunchTime,
			SporedAt:   agent.startTime,
			TagSeconds: config.ComputeSeconds,
			Checkpoint: checkpoint,
			Kernel:     kernelNow,
			KernelOK:   kernelOK,
		})
		agent.computeSecondsBase, agent.computeStart = start.Base, start.From
		agent.kernelOrigin, agent.kernelOriginKnown = start.Origin, start.OriginKnown
		log.Printf("Compute clock: %ds carried over from earlier boots (%s); this boot counts from %s",
			start.Base, start.Source, start.From.UTC().Format(time.RFC3339))
	} else if checkpoint != nil && checkpoint.InstanceID == identity.InstanceID && checkpoint.TotalSeconds > agent.computeSecondsBase {
		// A query with no live clock reports the freshest checkpoint: the file
		// is written every tick, the tag at most every five minutes.
		agent.computeSecondsBase = checkpoint.TotalSeconds
	}
	agent.tagger = newTagPutter(identity.Region) // real EC2 CreateTags; tests override

	// Everything below this point is ACTIVATION — logging and side effects — and
	// is skipped for a query (#733). The pure state assignments (dnsDomain,
	// notifier, pluginRuntime) still happen either way, because a query may need
	// to report them.
	if activate {
		log.Printf("Agent initialized for instance %s in %s (account: %s, provider: %s)",
			identity.InstanceID, identity.Region, identity.AccountID, identity.Provider)
		log.Printf("Config: TTL=%v, IdleTimeout=%v, Hibernate=%v, TerminateOnIdle=%v",
			config.TTL, config.IdleTimeout, config.HibernateOnIdle, config.TerminateOnIdle)
	}

	// Measure the cost footprint (every attached EBS volume, every public IPv4
	// address) on every start, and again periodically from the monitor loop
	// (maybeMeasureCostFootprint): before #51 it was measured once at first
	// start and cached for life, so a volume attached or an Elastic IP
	// associated later was never counted. Do NOT write agent.config here: this
	// goroutine would race the monitor loop, which replaces the whole pointer on
	// its periodic tag refresh (#175). The footprint is persisted to tags, and
	// the monitor's next config refresh (<=5 min) picks it up.
	if activate && identity.Provider == "ec2" {
		agent.maybeMeasureCostFootprint(context.Background())
	}

	// Log DCV idle detection if this is an application streaming instance. The DCV
	// handshake (verifier + session-wait + token + ready-url tag) is driven from
	// the monitor loop via maybeSetupDCVAuth, not once here — so a transient
	// failure recovers (spawn#282 phase 2), mirroring the FSx retry discipline.
	if activate && config.DCVSessionID != "" {
		log.Printf("DCV idle detection enabled for session %s", config.DCVSessionID)
	}

	// Initialize lifecycle notifier (Slack notifications via spore-bot Lambda)
	if config.NotifyURL != "" {
		// Constructing the notifier sends nothing; only the lifecycle hooks do.
		agent.notifier = NewNotifier(config, identity)
		if activate {
			log.Printf("Slack lifecycle notifications enabled for workspace %s", config.SlackWorkspaceID)
		}
	}

	// Initialize DNS client and register if DNS name is configured
	// Skip DNS for local provider (Phase 1 decision)
	dnsDomain, dnsEndpoint, targetErr := resolveDNSTarget(config)
	agent.dnsDomain = dnsDomain
	agent.dnsEndpoint = dnsEndpoint
	agent.dnsTargetErr = targetErr

	// THE one from #733: this block POSTs to the DNS API, mutating a Route53
	// record, and writes an EC2 tag via recordDNSStatus. A query must do neither.
	if activate && config.DNSName != "" && agent.publicIP() != "" && identity.Provider == "ec2" && targetErr != nil {
		// A target that does not validate is reported, not replaced: silently
		// registering under spore.host instead would put the record somewhere
		// nobody asked for and nobody will look (#891).
		log.Printf("Warning: DNS not registered: %v", targetErr)
		agent.recordDNSStatus(ctx, "failed", targetErr.Error())
		agent.lastDNSStatus = "failed"
	} else if activate && config.DNSName != "" && agent.publicIP() != "" && identity.Provider == "ec2" {
		if agent.ensureDNSClient(ctx) {
			// The SAME registration the renewal timer calls, so boot and renewal
			// cannot drift apart into two slightly different requests.
			regErr := agent.registerDNS(ctx)
			if regErr != nil {
				agent.recordDNSStatus(ctx, "failed", regErr.Error())
				agent.lastDNSStatus = "failed"
			} else {
				agent.recordDNSStatus(ctx, "registered", "")
				agent.lastDNSStatus = "registered"
			}
			agent.nextDNSRefresh = nextLeaseRefresh(time.Now(), regErr == nil)
		}
	} else if activate && config.DNSName != "" && identity.Provider == "local" {
		log.Printf("DNS registration skipped for local provider")
	} else if activate && config.DNSName != "" {
		// refreshDNSLease registers it if a public IP appears later (#1019).
		log.Printf("Warning: DNS name configured (%s) but no public IP available yet; it will be registered if one appears", config.DNSName)
	} else if activate {
		// No DNS name configured (no spawn:dns-name tag) → nothing to register.
		// Log it explicitly: a silent skip here made a client that forgot to set
		// spawn:dns-name look like a broken spored/registration path (#435). The
		// Go `spawn launch` always sets DNSName (--dns defaults to --name), so an
		// empty value on EC2 means the launcher didn't emit the tag.
		log.Printf("DNS registration skipped: no DNS name configured (no spawn:dns-name tag on the instance)")
	}

	// Initialize hybrid registry if this is a hybrid job-array member (#904)
	if activate && joinsHybridRegistry(identity, config) {
		reg, err := newPeerRegistry(ctx, identity)
		if err != nil {
			log.Printf("Warning: Failed to initialize registry: %v (continuing without hybrid mode)", err)
		} else {
			agent.attachRegistry(reg)

			// Register with hybrid registry using index from config
			index := config.JobArrayIndex
			if err := reg.Register(ctx, config.JobArrayID, index); err != nil {
				log.Printf("Warning: Failed to register with hybrid registry: %v", err)
			} else {
				// Start heartbeat
				reg.StartHeartbeat(ctx, config.JobArrayID)
				log.Printf("✓ Registered with hybrid registry: job_array=%s, provider=%s",
					config.JobArrayID, identity.Provider)
			}
		}
	} else if activate && config.JobArrayID != "" {
		log.Printf("Hybrid registry skipped: job array %s is not hybrid (no spawn:burst tag, not a local node)",
			config.JobArrayID)
	}

	if activate && config.JobArrayID != "" {
		// Discover peers. A hybrid member's list is written to the peers file
		// for its workload now, and refreshed by the monitor (#933). Any other
		// member's discovery is diagnostic only: for --mpi the cohort Assembler
		// owns that file (see MaintainsPeersFile).
		if agent.MaintainsPeersFile() {
			_ = agent.refreshPeers(ctx) // failures are logged and recorded
		} else {
			peers, err := prov.DiscoverPeers(ctx, config.JobArrayID)
			if err != nil {
				log.Printf("Warning: Failed to discover peers: %v", err)
			} else if len(peers) > 0 {
				log.Printf("✓ Discovered %d peers in job array %s", len(peers), config.JobArrayID)
			}
		}
	}

	// Initialize plugin runtime (always, so the push API can route to it). Pass
	// the local login user so plugin steps that opt into as_user (e.g. Globus
	// Connect Personal, which refuses to run as root) run as that user.
	rt := pluginruntime.NewRuntime(identity, config.LocalUsername)
	agent.attachPluginRuntime(rt)

	if activate && len(config.Plugins) > 0 {
		// Convert provider.PluginDeclaration to plugin.Declaration.
		decls := make([]plugin.Declaration, len(config.Plugins))
		for i, pd := range config.Plugins {
			decls[i] = plugin.Declaration{Ref: pd.Ref, Config: pd.Config}
		}
		resolver := plugin.DefaultResolver()
		rt.LoadFromDeclarations(ctx, decls, resolver)
		log.Printf("Plugin runtime: loading %d declared plugin(s)", len(decls))
	}

	return agent, nil
}

// ensureDNSClient creates the DNS client if there is none yet and this instance
// should have one: an EC2 instance with a DNS name and a public IP. It reports
// whether a client is there afterwards.
//
// The boot path calls it, and so does refreshDNSLease, because the public IP is
// not fixed at boot: an instance that booted without one, then got one (an EIP
// associated later, or a resume from hibernation), would otherwise never be
// registered (#1019).
func (a *Agent) ensureDNSClient(ctx context.Context) bool {
	if a.dnsClient != nil {
		return true
	}
	if a.config.DNSName == "" || a.publicIP() == "" || a.identity == nil || a.identity.Provider != "ec2" {
		return false
	}
	// An invalid spawn:dns-domain or spawn:dns-endpoint is never replaced by the
	// default: the record would land where nobody asked for it (#891).
	if a.dnsTargetErr != nil {
		return false
	}
	c, err := newDNSClient(ctx, a.dnsDomain, a.dnsEndpoint)
	if err != nil {
		log.Printf("Warning: Failed to create DNS client: %v", err)
		return false
	}
	// Pass the friendly account-name slug so the updater also registers the
	// alias FQDN {name}.{account-name}.spore.host (#121). Empty when the
	// account has no name — base36 only, unchanged.
	c.SetAccountName(a.config.AccountName)
	// This spored renews its registration on a timer (refreshDNSLease), so it
	// may ask for a lease. The one-shot `spawn dns register` uses the same
	// client and must not — see dns.Client.renewing.
	c.SetRenewing(true)
	a.dnsClient = c
	return true
}

// attachPluginRuntime and attachRegistry hand the instance's public IP to the
// plugin runtime ({{ instance.ip }}) and the hybrid registry as a getter of
// the guarded current value, never as the boot-time identity snapshot, so a
// public IP re-read after a resume reaches them too (#1019).
func (a *Agent) attachPluginRuntime(rt *pluginruntime.Runtime) {
	rt.SetPublicIPSource(a.publicIP)
	a.pluginRuntime = rt
}

func (a *Agent) attachRegistry(reg *registry.PeerRegistry) {
	reg.SetPublicIPSource(a.publicIP)
	a.registry = reg
}

func (a *Agent) Monitor(ctx context.Context) {
	interval := a.monitorInterval
	if interval <= 0 {
		interval = 1 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("Monitoring started")

	// The clocks' baseline for resume detection (#987).
	a.checkResume(ctx)

	// Tell `spored reload` this process takes reload requests (#1278): it
	// signals only a daemon whose pid is in the marker, so it never sends
	// SIGHUP to an older spored, for which SIGHUP's default action is exit.
	reloads := a.reloadRequests()
	a.publishReloadMarker(nil)

	// Spot-interruption monitoring runs in its own goroutine and must NEVER gate
	// the main lifecycle ticker below. Spot detection (IsSpotInstance) makes a
	// blocking IMDS call; if that ever stalls, gating the ticker on it silently
	// kills TTL/idle/on-complete/pre-stop enforcement — i.e. instances run
	// forever (#65). So we start the lifecycle loop unconditionally and do the
	// spot decision off the critical path.
	go a.monitorSpotInterruptions(ctx)

	// A hybrid member's peers file is refreshed off this loop too, for the same
	// reason: discovery makes EC2 and DynamoDB calls (#933).
	peerInterval := a.peerRefreshInterval
	if peerInterval <= 0 {
		peerInterval = PeerRefreshInterval
	}
	go a.maintainPeers(ctx, peerInterval)

	// The async FSx mount (#194) is triggered from the monitor loop
	// (maybeMountPendingFSx in checkAndAct), not once here: spawn:fsx-pending may
	// not be visible at startup (the headless launch path tags AFTER RunInstances;
	// EC2 tags are eventually consistent), and a run-once-at-startup read lost that
	// race and silently never mounted (#221). Driving it from the loop re-checks
	// after each config refresh until the tag appears.

	for {
		select {
		case <-ctx.Done():
			log.Printf("Context cancelled, stopping monitor")
			return

		case <-ticker.C:
			a.checkAndAct(ctx)

		case <-reloads:
			a.handleReloadRequest(ctx)
		}
	}
}

// monitorSpotInterruptions polls for the 2-minute Spot interruption notice
// every 5s (independent of the 1-minute lifecycle ticker, for maximum lead
// time). It first determines whether this is even a Spot instance; on-demand
// instances return early. Runs in its own goroutine so neither the spot-type
// detection nor the polling can block the main lifecycle loop (#65).
func (a *Agent) monitorSpotInterruptions(ctx context.Context) {
	if !a.provider.IsSpotInstance(ctx) {
		return
	}
	spotTicker := time.NewTicker(5 * time.Second)
	defer spotTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-spotTicker.C:
			if a.checkSpotInterruption(ctx) {
				return
			}
		}
	}
}

func (a *Agent) checkAndAct(ctx context.Context) {
	// Heartbeat: a single line per tick so "Monitoring started" followed by
	// silence is immediately diagnosable from spored.log without a live repro
	// (the failure mode in #65, where the loop never ran at all).
	log.Printf("monitor tick %d", a.configRefreshTick+1)

	// Resumed from hibernation since the last tick? Then reset the per-boot
	// state first, so this tick's pre-stop, DNS and idle checks see a fresh
	// boot (#987).
	a.checkResume(ctx)

	// 0. Periodically refresh config from EC2 tags (every 5 ticks = 5 min).
	// EC2 tag API has eventual consistency — tags set at launch may not be
	// visible within the seconds before spored starts. Refreshing here ensures
	// on-complete, pre-stop, and other lifecycle tags are eventually picked up.
	a.configRefreshTick++
	refreshed := false
	if a.configRefreshTick == 1 || a.configRefreshTick%5 == 0 {
		a.refreshConfigFromTags(ctx)
		refreshed = true
	}

	// 0'. A completion signal with no on-complete configured yet may only mean
	// the spawn:on-complete tag was not visible at the last refresh: a short
	// task can finish before it is (#270). Re-read the tags now rather than
	// waiting up to five ticks, so step 1 below sees the action on this tick.
	if !refreshed {
		a.refreshForEarlyCompletion(ctx)
	}

	// 0a0. Mount a pending async-created FSx once spawn:fsx-pending is visible
	// (#194/#221). Driven here (after the config refresh) rather than once at
	// startup so a tag that lands after spored boots is still picked up. Runs at
	// most once, in its own goroutine (the poll-until-AVAILABLE can block minutes
	// and must never gate this ticker, #65).
	a.maybeMountPendingFSx(ctx)

	// 0a1. Drive the DCV readiness handshake (verifier already up; this does one
	// session-check + token + ready-url tag write per tick until it succeeds or
	// hits a terminal failure). Loop-driven so a transient failure recovers
	// (spawn#282 phase 2). No-op on non-DCV instances.
	a.maybeSetupDCVAuth(ctx)

	// 0a2. Drive the web-UI readiness handshake for AppMode=="web" (#590): probe
	// the app's HTTP port, start the :443 TLS reverse proxy once it's up, and write
	// spawn:ready-url. Loop-driven like the DCV handshake. No-op on non-web instances.
	a.maybeSetupWebReady(ctx)

	// 0a3. Keep spawn:ready-url on the instance's current address: one written
	// with the public IP goes stale when a resume changes it (#1019).
	a.refreshReadyURL(ctx)

	// 0a'. Re-measure the cost footprint every footprintMeasureTicks ticks, off
	// this goroutine (#51).
	if a.configRefreshTick%footprintMeasureTicks == 0 && a.identity != nil && a.identity.Provider == "ec2" {
		a.maybeMeasureCostFootprint(ctx)
	}

	// 0a. Keep spawn:logged-in-count tag current (throttled to 5/min).
	a.writeSessionCountTag(ctx, countActiveSessions(ctx)+countActivePortConnections(ctx, a.config.ActivePorts))

	// 0b. Keep spawn:compute-seconds tag current (throttled to 5/min).
	a.writeComputeSecondsTag(ctx)

	// 0c. Keep spawn:last-heartbeat current (throttled to 1/min, matching the
	// production monitor interval — #497): an always-on liveness signal a
	// caller can poll to tell "still alive and ticking" from "hung" (spored
	// froze) or "gone" (terminated), independent of whatever completion
	// artifact the workload itself writes. No opt-in flag, unlike the webhooks
	// above — it costs nothing to a caller who never reads it.
	a.writeHeartbeatTag(ctx)

	// 0d. Renew the DNS lease (throttled to ~hourly, jittered). Without renewal a
	// registration is made once at boot and its lease would expire under a live
	// instance; with it, a record whose owner has gone away can be expired from the
	// zone alone, with no credentials in this account (pkg/dns/lease.go).
	a.refreshDNSLease(ctx)

	// 1. Check for completion signal (HIGH PRIORITY)
	if a.config.OnComplete != "" {
		if a.checkCompletion(ctx) {
			// Completion signal detected - handled in checkCompletion
			return
		}
	}

	// 2. Check TTL
	// TTLDeadline is authoritative — it is set once at launch and anchored to the
	// original launch time, so it is never reset by stop/start cycles. If not set
	// (older instances), fall back to startTime+TTL which has the reset bug.
	if !a.config.TTLDeadline.IsZero() || a.config.TTL > 0 {
		var remaining time.Duration
		if !a.config.TTLDeadline.IsZero() {
			remaining = time.Until(a.config.TTLDeadline)
		} else {
			remaining = a.config.TTL - time.Since(a.startTime)
		}

		if remaining <= 0 {
			// INVARIANT (#72): TTL expiry ALWAYS terminates. Do not add a
			// stop/hibernate branch here or honor a "ttl-action" tag — "stop" is
			// not a terminal state (it bills EBS indefinitely and runs no daemon
			// to re-check TTL, the #71 zombie). TTL is the unconditional
			// backstop; only idle/on-complete may stop or hibernate.
			log.Printf("TTL expired (deadline: %v)", a.config.TTLDeadline)
			a.notifier.Notify(ctx, "ttl_expired", "")
			a.terminate(ctx, "TTL expired", taskproto.ExitTTLExpired)
			return
		}

		// Warn once when 5 minutes remain before TTL
		if remaining <= 5*time.Minute && !a.ttlWarned {
			a.ttlWarned = true
			a.warnUsers(ctx, i18n.Tf("spawn.agent.ttl_warning", map[string]interface{}{
				"Duration": remaining.Round(time.Minute),
			}))
			a.notifier.Notify(ctx, "ttl_warning", remaining.Round(time.Minute).String())
		}
	}

	// 3. Check cost limit (fires independently of or alongside TTL — first-to-fire wins)
	if a.config.CostLimit > 0 && a.config.PricePerHour > 0 {
		// Use TOTAL compute time across the instance's life, not just this boot's
		// uptime: computeSecondsBase carries the compute accumulated before this
		// spored start (persisted in spawn:compute-seconds). Using time.Since(
		// startTime) alone would reset the cost clock on every stop/start, letting
		// a repeatedly-resumed instance blow past its --cost-limit — the same
		// reset trap TTL avoids with an absolute deadline.
		accumulated := a.accumulatedComputeCost()
		remaining := a.config.CostLimit - accumulated

		if remaining <= 0 {
			log.Printf("Cost limit reached (limit: $%.4f, accumulated: $%.4f)", a.config.CostLimit, accumulated)
			a.terminate(ctx, fmt.Sprintf("cost limit reached ($%.2f)", a.config.CostLimit), taskproto.ExitCostLimitExceeded)
			return
		}

		// Warn when 90%+ of budget consumed
		if accumulated/a.config.CostLimit >= 0.90 {
			a.warnUsers(ctx, i18n.Tf("spawn.agent.cost_limit_warning", map[string]interface{}{
				"Accumulated": fmt.Sprintf("%.4f", accumulated),
				"Limit":       fmt.Sprintf("%.2f", a.config.CostLimit),
				"Percentage":  fmt.Sprintf("%.0f", (accumulated/a.config.CostLimit)*100),
			}))
		}
	}

	// 4. Check idle. The signals are sampled and the decision published on
	// every tick, idle timeout or not, so the metrics endpoint and status read
	// this tick's evaluation rather than taking samples of their own (#952).
	snap := a.evaluateIdle(ctx)
	a.publishIdleState(snap) // for `spored status`, which cannot sample (#1090)
	if a.config.IdleTimeout > 0 {
		log.Print(snap.reason)
		if snap.idle {
			idleTime := time.Since(a.lastActivity())

			if idleTime >= a.config.IdleTimeout {

				// Send event name that reflects the actual action.
				// Default: stop the instance (compute billing pauses, instance preserved).
				// --hibernate-on-idle: hibernate instead (RAM saved to disk).
				// spawn:terminate-on-idle (`spawn service`, pool workers):
				// terminate, since those instances have nothing to come back to
				// and a stopped one keeps billing its EBS volume (#1151, #1146).
				//
				// A failed stop is retried on a backoff, not every tick, and the
				// "stopped" notification is sent only once EC2 has accepted the
				// request (#948).
				a.idleTimeoutReached(ctx, time.Now())
				return
			}

			// Warn once when 5 minutes remain before idle timeout
			remaining := a.config.IdleTimeout - idleTime
			if remaining > 0 && remaining <= 5*time.Minute && !a.idleWarned {
				a.idleWarned = true
				a.warnUsers(ctx, i18n.Tf("spawn.agent.idle_warning", map[string]interface{}{
					"IdleDuration": idleTime.Round(time.Minute),
					"Remaining":    remaining.Round(time.Minute),
				}))
				a.notifier.Notify(ctx, "idle_warning", remaining.Round(time.Minute).String())
			}
		} else {
			// Activity detected — reset idle timer and re-arm the warning
			a.setLastActivity(time.Now())
			a.idleWarned = false
		}
	}
}

// Sentinels for the OS CPU-times reader (sysReadCPUTimes).
// errEmptyProcStat is Linux-only and lives in sys_linux.go with its one reader.
var errBadProcStat = fmt.Errorf("unexpected cpu stat format")

// countActivePortConnections counts ESTABLISHED connections on the given ports —
// detects browser-based app users (RStudio, Jupyter) that don't appear in the
// session list because they connect via HTTP, not SSH. OS-specific source.
func countActivePortConnections(ctx context.Context, ports []int) int {
	return sysCountActivePortConnections(ctx, ports)
}

// countActiveSessions returns the number of login sessions with recent keyboard
// activity (input within the last 5 minutes), so idle SSH/RDP connections don't
// block idle detection. OS-specific source: on Linux the idle time of each
// terminal (ttyidle.go), on Windows quser's IDLE TIME. Presence alone, an open
// login or a PTY nobody is typing in, must never count (#953).
func countActiveSessions(ctx context.Context) int {
	return sessionCounter(ctx)
}

// sessionCounter is the OS session source behind countActiveSessions; a variable
// so a test can make a host with no active sessions on every OS (on Windows the
// real source is quser, which sees the CI runner's own console session).
var sessionCounter = sysCountActiveSessions

// findActiveProcess returns the first configured process name that is currently
// running, or "" if none are found. OS-specific source.
func (a *Agent) findActiveProcess(ctx context.Context) string {
	for _, name := range a.config.ActiveProcesses {
		if sysIsProcessRunning(ctx, name) {
			return name
		}
	}
	return ""
}

// writeSessionCountTag updates the spawn:logged-in-count EC2 tag, throttled to once per minute.
func (a *Agent) writeSessionCountTag(ctx context.Context, count int) {
	if time.Since(a.lastSessionTagWrite) < 5*time.Minute {
		return
	}
	a.lastSessionTagWrite = time.Now()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.identity.Region))
	if err != nil {
		return
	}
	client := ec2.NewFromConfig(cfg)
	_, _ = client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.identity.InstanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:logged-in-count"), Value: aws.String(strconv.Itoa(count))},
		},
	})
}

// writeHeartbeatTag stamps spawn:last-heartbeat with the current time, throttled
// to once per minute (#497): an always-on liveness signal a caller can poll to
// distinguish "still alive and ticking" from "hung" (spored crashed/froze) or
// "gone" (instance terminated, tag no longer resolvable) — independent of
// whatever completion artifact the workload itself writes. Follows the
// writeSessionCountTag/writeComputeSecondsTag throttle pattern so tests can
// push lastHeartbeatTagWrite into the future to skip the real EC2 call.
func (a *Agent) writeHeartbeatTag(ctx context.Context) {
	if time.Since(a.lastHeartbeatTagWrite) < time.Minute {
		return
	}
	a.lastHeartbeatTagWrite = time.Now()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.identity.Region))
	if err != nil {
		return
	}
	client := ec2.NewFromConfig(cfg)
	_, _ = client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.identity.InstanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:last-heartbeat"), Value: aws.String(time.Now().UTC().Format(time.RFC3339))},
		},
	})
}

// writeComputeSecondsTag persists the total compute seconds (base + current uptime) to an EC2 tag.
// Throttle: every 1 minute for the first 10 minutes (fast feedback on fresh instances),
// then every 5 minutes thereafter.
func (a *Agent) writeComputeSecondsTag(ctx context.Context) {
	// The local checkpoint is cheap and needs no network, so it is written on
	// every tick, unthrottled: a stop can then lose at most one tick (#928).
	a.WriteComputeCheckpoint()

	uptime := time.Since(a.startTime)
	interval := 5 * time.Minute
	if uptime < 10*time.Minute {
		interval = 1 * time.Minute
	}
	if time.Since(a.lastComputeTagWrite) < interval {
		return
	}
	a.flushComputeSecondsTag(ctx)
}

// accumulatedComputeCost is what --cost-limit is measured against: the
// instance's compute, its EBS and its public IPv4 addresses (#616, #51),
// accrued by pkg/costmodel.Accrue — the same function `spawn status`, `spawn
// cost` and `spawn extend` use, so what they show is what terminates the
// instance. The component/state table is in pkg/costmodel's package doc.
//
// Compute is the on-demand rate x TOTAL compute time (base + this boot's
// uptime); using the total, not just this boot, stops the cost clock resetting
// on a stop/start. EBS and Elastic IPs bill on the WALL CLOCK since the
// original launch (a stopped instance still pays for them); auto-assigned
// public IPv4 is released on stop, so it bills with compute.
//
// FSx is deliberately NOT included. Terminating does not delete a filesystem — a
// durable one is never reclaimed automatically, and an ephemeral one only by the
// out-of-band reaper, in an account it covers (#624/#625). Counting it here would let
// the cap fire and terminate the instance while the filesystem it was reacting to kept
// billing: an action that looks like enforcement and achieves nothing. That commitment
// is refused up front instead, by costLimitPreflight at launch.
//
// The name is kept for continuity with the spawn:compute-seconds tag and the existing
// cost reporting; the doc comment is the contract.
func (a *Agent) accumulatedComputeCost() float64 {
	return costmodel.Accrue(a.costFootprint(),
		time.Duration(a.TotalComputeSeconds())*time.Second, a.wallSinceLaunch()).Total
}

// costFootprint is the enforced Footprint from the agent's config.
func (a *Agent) costFootprint() costmodel.Footprint {
	return costmodel.Footprint{
		ComputePerHour: a.config.PricePerHour,
		EBSPerHour:     a.config.EBSHourlyCost,
		PublicIPv4:     a.config.PublicIPv4Count,
		ElasticIPv4:    a.config.ElasticIPv4Count,
	}
}

// wallSinceLaunch is the wall clock since the original launch, which EBS and
// Elastic IPs bill on. LaunchTime (spawn:launch-time) never resets on
// stop/wake. Without it, EC2's pending time from IMDS is the anchor rather than
// none at all: before #51 a missing tag silently dropped EBS from the cap.
func (a *Agent) wallSinceLaunch() time.Duration {
	anchor := a.config.LaunchTime
	if anchor.IsZero() && a.identity != nil {
		anchor = a.identity.PendingTime
	}
	if anchor.IsZero() {
		return 0
	}
	return time.Since(anchor)
}

// footprintMeasureTicks is how often (in monitor ticks, one a minute) the
// cost footprint is re-measured: two read-only EC2 calls every 15 minutes.
const footprintMeasureTicks = 15

// maybeMeasureCostFootprint measures the cost footprint in its own goroutine,
// unless one is already running, and reports whether it started one. Never
// blocks the caller (#65).
func (a *Agent) maybeMeasureCostFootprint(ctx context.Context) bool {
	if !a.footprintMeasuring.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer a.footprintMeasuring.Store(false)
		fp, measured := a.provider.MeasureCostFootprint(context.WithoutCancel(ctx))
		if measured {
			log.Printf("Cost footprint: EBS $%.4f/hr, public IPv4 %d auto-assigned + %d Elastic ($%.3f/hr each)",
				fp.EBSHourlyCost, fp.PublicIPv4, fp.ElasticIPv4, costmodel.PublicIPv4PerHour)
		} else {
			// Never logged in the shape of a measurement (spawn#517).
			log.Printf("Cost footprint: unknown (lookup failed or denied); EBS and public IPv4 are not counted until it succeeds")
		}
	}()
	return true
}

// CostLimitSpend is the spend --cost-limit is enforced against, exactly as the
// enforcement loop measures it (accumulatedComputeCost: compute + EBS + public
// IPv4, #616, #51).
// `spored status` reports cost-limit used/remaining from this rather than
// re-deriving it, so the headroom it shows is the headroom that terminates the
// instance (#959).
func (a *Agent) CostLimitSpend() float64 {
	return a.accumulatedComputeCost()
}

// flushComputeSecondsTag writes the spawn:compute-seconds tag NOW, bypassing the
// throttle in writeComputeSecondsTag. It is called on graceful shutdown (see
// Cleanup) so an in-place spored upgrade (`spawn upgrade-spored`, #234) — which
// stops the daemon to swap the binary — doesn't discard the up-to-5-minutes of
// compute time accumulated since the last throttled write. The next spored boot
// reads this tag into computeSecondsBase, so the compute clock continues across
// the restart rather than losing the tail.
func (a *Agent) flushComputeSecondsTag(ctx context.Context) {
	a.lastComputeTagWrite = time.Now()
	total := a.TotalComputeSeconds()
	if err := a.putComputeSecondsTag(ctx, total); err != nil {
		// Logged, not swallowed: a lost write here is how compute vanished across
		// a stop (#928). The local checkpoint still holds the total.
		log.Printf("Warning: could not write spawn:compute-seconds=%d: %v", total, err)
	}
}

// WriteVersionTag records the running spored version in the spawn:spored-version
// EC2 tag (#232/#234) so `spawn status` and `spawn upgrade-spored` can read the
// on-instance version without an exec into the box, and so an upgrade can confirm
// the new binary actually took effect. Best-effort: a tag-write failure is logged
// and ignored (it must never gate the lifecycle loop, per #65). version is the
// spored binary's build version (main.Version), passed in from cmd/spored.
func (a *Agent) WriteVersionTag(ctx context.Context, version string) {
	if version == "" {
		return
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.identity.Region))
	if err != nil {
		return
	}
	client := ec2.NewFromConfig(cfg)
	if _, err := client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.identity.InstanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:spored-version"), Value: aws.String(version)},
		},
	}); err != nil {
		log.Printf("Warning: failed to write spawn:spored-version tag: %v", err)
	}
}

// HasPriorComputeTime reports whether this spored inherited accumulated compute
// time from a previous run — i.e. whether the instance has genuinely been stopped
// and started again.
//
// It is the honest discriminator for the "stopped" term in a status breakdown.
// That term used to be computed by SUBTRACTION — elapsed-since-launch minus
// compute-since-spored-started — so on a first boot it absorbed the boot,
// cloud-init and package-install time and reported it as time spent stopped. The
// reported case was `22m 13s (20m 0s compute · 2m 13s stopped)` on an instance
// that had never been stopped (#735).
//
// computeSecondsBase is non-zero only when a previous run's total was carried
// over through the tag, so zero means "no evidence of a prior run".
func (a *Agent) HasPriorComputeTime() bool {
	return a.computeSecondsBase > 0
}

// TotalComputeSeconds returns accumulated compute time across all start/stop cycles.
//
// This boot counts from computeFrom: the kernel's boot time where it is known
// to be safe (resolveComputeStart), since EC2 bills from there; otherwise
// spored's own start. Time spent hibernated is not counted (thisBootRunning).
func (a *Agent) TotalComputeSeconds() int64 {
	return a.computeSecondsBase + int64(a.thisBootRunning().Seconds())
}

// computeFrom is when this boot's compute started counting.
func (a *Agent) computeFrom() time.Time {
	if a.computeStart.IsZero() {
		return a.startTime
	}
	return a.computeStart
}

// ComputeStart is when this boot's compute started counting — the daemon's, for
// a query agent that adopted its clock. `spored status` measures the launch-to-
// counting gap from it (#928).
func (a *Agent) ComputeStart() time.Time { return a.computeFrom() }

// ── DCV auth token verifier ───────────────────────────────────────────────────

// startDCVAuthVerifier starts a tiny HTTP server on 127.0.0.1:8444 that verifies
// one-time auth tokens for NICE DCV. DCV calls this endpoint when a browser connects
// with ?authToken=<token> in the URL. The protocol is specified by AWS DCV:
// POST body: sessionId=<id>&authenticationToken=<token>&clientAddress=<ip>
// Response XML: <auth result="yes"><username>ec2-user</username></auth>
func (a *Agent) startDCVAuthVerifier(ctx context.Context) {
	a.dcvTokens = make(map[string]string)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		token := r.FormValue("authenticationToken")
		a.dcvTokensMu.Lock()
		username, ok := a.dcvTokens[token]
		// Token is kept valid for the session lifetime so reconnects work.
		// spored generates a new token if it restarts (new spawn:ready-url tag).
		a.dcvTokensMu.Unlock()
		w.Header().Set("Content-Type", "text/xml")
		type authResp struct {
			XMLName  xml.Name `xml:"auth"`
			Result   string   `xml:"result,attr"`
			Username string   `xml:"username,omitempty"`
			Message  string   `xml:"message,omitempty"`
		}
		var resp authResp
		if ok {
			resp = authResp{Result: "yes", Username: username}
		} else {
			resp = authResp{Result: "no", Message: "invalid or expired token"}
		}
		_ = xml.NewEncoder(w).Encode(resp)
	})
	srv := &http.Server{Addr: "127.0.0.1:8444", Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Printf("DCV: auth token verifier listening on 127.0.0.1:8444")
}

// The DCV wildcard cert is installed by the LAUNCH USER-DATA, not here (#809).
//
// There used to be an installDCVCert on this type, plus an s3GetFile helper it
// was the only caller of, which fetched spawn-certs-<region>/<base36>/{cert,key}
// into /etc/dcv/, injected tls-certificate into /etc/dcv/dcv.conf and restarted
// dcvserver. Nothing ever called it — the definition and its own doc comment were
// the only references in the tree, across all three GOOS values.
//
// It was dead because it had been SUPERSEDED, not forgotten: cmd/app.go's DCV
// user-data does this at boot, and writes to
// /var/lib/dcv/.config/NICE/dcv/private/dcv.{pem,key} — which is where DCV
// actually reads them. webProxyCertPath's comment in webready.go records the
// current arrangement: "the launch user-data drops the wildcard TLS cert for the
// reverse proxy (same spawn-certs source as DCV, but DCV owns its own copy under
// /var/lib/dcv)".
//
// So the audit's reading of this — "DCV sessions always use a self-signed cert,
// and spawn-certs is a read-only bucket with no live end" — was wrong in both
// halves, because the live reader is `aws s3 cp` in shell and a Go-only grep
// cannot see it. Wiring the function up would have been worse than leaving it:
// the wrong destination path, a dcvserver restart that drops live sessions, and
// two writers fighting over the same cert state.

// dcvSessionWaitTimeout bounds how long spored waits for the DCV session to
// appear before recording session-never-created. Measured from spored start;
// longer than the CLI's 5-min poll so a slow-but-eventual session still wins.
const dcvSessionWaitTimeout = 4 * time.Minute

// dcvGraceTimeout bounds the idle-detection startup grace for a DCV instance
// whose DCV server never becomes ready (spawn#282). Within the grace we assume
// not-idle (DCV is still coming up); past it, isIdle falls through to the
// standard CPU/network checks so a permanently-unhealthy DCV instance still idle-
// stops instead of billing until TTL. Comfortably exceeds dcvSessionWaitTimeout.
const dcvGraceTimeout = 8 * time.Minute

// maybeSetupDCVAuth drives the DCV readiness handshake from the monitor loop
// (spawn#282 phase 2): it starts the :8444 token verifier once, then on each tick
// does ONE session check and, when the session is present, issues the token and
// writes spawn:ready-url/ready-token/ready-status. Loop-driven (not a one-shot
// goroutine) so a transient failure — slow dcvserver, a momentary
// ec2:CreateTags throttle — recovers on the next tick instead of being permanent
// (the old fire-once bug). This also collapses the CLI-vs-spored timer race:
// spored keeps retrying within the CLI's poll window.
//
// Stops (sets dcvAuthDone) once a ready-url is written OR a terminal failure is
// recorded. No-op on non-DCV instances and on non-EC2 providers.
func (a *Agent) maybeSetupDCVAuth(ctx context.Context) {
	if a.config.DCVSessionID == "" || a.identity.Provider != "ec2" || a.dcvAuthDone {
		return
	}
	sessionID := a.config.DCVSessionID

	// Verifier must be listening before DCV connects; start it once.
	if !a.dcvVerifierStarted {
		a.startDCVAuthVerifier(ctx)
		a.dcvVerifierStarted = true
	}

	// One session check this tick. "exhausted" (→ terminal session-never-created)
	// is a wall-clock deadline from spored start, since each call is one poll.
	out, listErr := a.dcv.listSessions(ctx)
	exhausted := time.Since(a.startTime) > dcvSessionWaitTimeout
	status := classifyDCVStatus(a.dcv.installed(), listErr, out, sessionID, exhausted)

	if status != dcvReady {
		if status.terminal() {
			// Record the named reason (no fake ready-url) and stop retrying.
			log.Printf("DCV: session %q not ready: %s (giving up)", sessionID, status)
			a.writeReadyTags(ctx, map[string]string{"spawn:ready-status": string(status)})
			a.dcvAuthDone = true
		}
		// dcvWaiting: leave dcvAuthDone false → retry next tick.
		return
	}

	// Session is present — issue the one-time token.
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Printf("DCV: failed to generate token: %v", err)
		return // retry next tick
	}
	token := hex.EncodeToString(b)
	a.dcvTokensMu.Lock()
	a.dcvTokens[token] = "ec2-user"
	a.dcvTokensMu.Unlock()

	// Ready URL — DNS FQDN when available, else public IP.
	host := a.readyHost()
	readyURL := buildReadyURL(host, token, sessionID)

	// spawn:ready-status carries the machine-readable enum ("ready"); the friendly
	// app label lives in spawn:app-name, which the CLI reads for display.
	a.writeReadyTags(ctx, map[string]string{
		"spawn:ready-url":    readyURL,
		"spawn:ready-token":  token,
		"spawn:ready-status": string(dcvReady),
	})
	if a.dcvReadyURLWritten {
		log.Printf("DCV: spawn:ready-url written (session %s, host %s)", sessionID, host)
		a.dcvAuthDone = true
		// The token stays valid for the session's lifetime (see the verifier),
		// so a rewrite for a new host keeps it (#1019).
		a.noteReadyURL(host, func(h string) string { return buildReadyURL(h, token, sessionID) })
	}
	// If the tag write failed (writeReadyTags didn't latch), retry next tick.
}

// writeReadyTags writes spawn:ready-* tags to the instance. The run-once guard
// (dcvReadyURLWritten) is set ONLY on a successful ready write — a status-only
// failure write (e.g. dcv-waiting → session-never-created) must not latch the
// guard, so Phase 2's monitor-loop retry can still write the real ready-url once
// DCV comes up. A CreateTags AccessDenied is surfaced as tag-write-denied so the
// CLI can name the cause (spawn#282). Follows the writeSessionCountTag pattern.
func (a *Agent) writeReadyTags(ctx context.Context, tags map[string]string) {
	if a.dcvReadyURLWritten {
		return
	}
	if err := a.tagger.putTags(ctx, a.identity.InstanceID, tags); err != nil {
		log.Printf("DCV: failed to write ready tags: %v", err)
		// Best-effort: record the IAM denial as the status so the CLI can say
		// "spored couldn't write its tag" rather than a generic timeout. Use a
		// minimal second write for just the status (it may also fail — then
		// nothing more we can do, and the CLI's generic timeout is the fallback).
		if isAccessDenied(err) {
			_ = a.tagger.putTags(ctx, a.identity.InstanceID, map[string]string{"spawn:ready-status": string(dcvTagWriteDenied)})
		}
		return
	}
	// Only latch the guard once the real ready-url is written — not for a
	// status-only failure record (which Phase 2 retry should be able to supersede).
	if _, ok := tags["spawn:ready-url"]; ok {
		a.dcvReadyURLWritten = true
	}
}

// recordDNSStatus records the outcome of DNS registration as instance tags so it
// is visible to `spawn status`, not just buried in spored's on-instance journal.
// Before this, a Function-URL auth rejection (403 post-#173 AWS_IAM cutover) or any
// other failure left the FQDN silently unresolvable with no user-facing signal
// (#435). Best-effort: a tag-write failure here is itself only logged — we never
// let DNS bookkeeping block the agent. status is "registered" or "failed"; detail
// (truncated to fit an EC2 tag value) is written only on failure. Both values are
// ALWAYS written from the DNS API call's actual, already-resolved outcome — the
// caller only invokes this after RegisterDNS/RegisterJobArrayDNS has returned —
// never optimistically ahead of it.
//
// #551: EC2's CreateTags (which putTags wraps) only ever adds or overwrites the
// keys given to it — it never removes a key that isn't in the call. NewAgent (and
// therefore this DNS-registration side effect) reruns on every `spored status`,
// `spored reload`, and `spored config` invocation, not just once at boot, so a
// transient early failure (spawn:dns-status=failed + spawn:dns-error=<detail>)
// followed by a later successful attempt used to leave the stale dns-error tag
// sitting right next to the new dns-status=registered — a live instance could
// carry both simultaneously, contradicting itself. Explicitly delete dns-error
// on success so the tag set can never claim both outcomes at once.
func (a *Agent) recordDNSStatus(ctx context.Context, status, detail string) {
	tags := map[string]string{"spawn:dns-status": status}
	if status == "registered" {
		if err := a.tagger.deleteTags(ctx, a.identity.InstanceID, []string{"spawn:dns-error"}); err != nil {
			log.Printf("DNS: failed to clear stale dns-error tag: %v", err)
		}
	} else if detail != "" {
		if len(detail) > 255 {
			detail = detail[:255]
		}
		tags["spawn:dns-error"] = detail
	}
	if err := a.tagger.putTags(ctx, a.identity.InstanceID, tags); err != nil {
		log.Printf("DNS: failed to write dns-status tag: %v", err)
	}
}

// ec2TagPutter is the production tagPutter: it writes tags via EC2 CreateTags.
// The region is captured at construction.
type ec2TagPutter struct {
	region string
}

func (p *ec2TagPutter) putTags(ctx context.Context, instanceID string, tags map[string]string) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(p.region))
	if err != nil {
		return fmt.Errorf("load AWS config for tag write: %w", err)
	}
	client := ec2.NewFromConfig(cfg)
	var ec2Tags []ec2types.Tag
	for k, v := range tags {
		ec2Tags = append(ec2Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err = client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags:      ec2Tags,
	})
	return err
}

// deleteTags removes the named tags entirely via EC2 DeleteTags (#551) — the
// counterpart putTags/CreateTags can't provide, since CreateTags only ever adds
// or overwrites keys and never clears ones it isn't given.
func (p *ec2TagPutter) deleteTags(ctx context.Context, instanceID string, keys []string) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(p.region))
	if err != nil {
		return fmt.Errorf("load AWS config for tag delete: %w", err)
	}
	client := ec2.NewFromConfig(cfg)
	ec2Tags := make([]ec2types.Tag, 0, len(keys))
	for _, k := range keys {
		ec2Tags = append(ec2Tags, ec2types.Tag{Key: aws.String(k)})
	}
	_, err = client.DeleteTags(ctx, &ec2.DeleteTagsInput{
		Resources: []string{instanceID},
		Tags:      ec2Tags,
	})
	return err
}

// isAccessDenied reports whether an AWS SDK error is an authorization failure
// (UnauthorizedOperation / AccessDenied), so a missing ec2:CreateTags grant is
// reported as tag-write-denied rather than a silent timeout.
func isAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UnauthorizedOperation") || strings.Contains(s, "AccessDenied")
}

// x11ActivityFile is written by kiosk-wm on every key/mouse event.
// spored reads its mtime to detect genuine user activity in DCV sessions.
const x11ActivityFile = "/run/spore/x11-last-activity"

// idleSnapshot is one evaluation of the idle signals: the counter readings,
// taken together on one tick, and the decision made from them (#952).
type idleSnapshot struct {
	taken        time.Time // zero until the first evaluation
	idle         bool
	reason       string // the "Not idle: ..." or "System is idle ..." log line
	cpu          float64
	cpuMeasured  bool
	net          int64
	netMeasured  bool
	netWhy       string
	disk         int64
	diskMeasured bool
	diskWhy      string
}

// noSampleYet is the unknown reason a reader sees before the first evaluation.
const noSampleYet = "no sample yet"

// sampleIdleSignals reads every counter-based idle signal once.
//
// Sampling has exactly one owner: the monitor loop in the daemon, or the
// one-shot `spored status`, which owns its own Agent. The prev* fields it
// advances are touched by nothing else, so they need no lock, and readers on
// other goroutines (the Prometheus scrape, the spot-interruption webhooks)
// read the published idleSnapshot instead. Before #952 the scrape called
// IsIdle and the CPU reader itself, from the HTTP goroutine: that was a data
// race, and worse, it moved the sample baseline, so the monitor loop's next
// delta covered only the seconds since the scrape and real traffic read as
// under the per-tick threshold — a false idle-stop.
//
// All three are read on every call, before any check can return. When the
// decision returned early (a session, a process, busy CPU), the network and
// disk counters used to go unsampled, so the next delta spanned several ticks.
func (a *Agent) sampleIdleSignals(ctx context.Context) idleSnapshot {
	s := idleSnapshot{taken: time.Now()}
	s.cpu, s.cpuMeasured = a.cpuUsage(ctx)
	s.net, s.netMeasured, s.netWhy = a.networkBytes(ctx)
	s.disk, s.diskMeasured, s.diskWhy = a.diskIOBytes(ctx)
	return s
}

// evaluateIdle samples, decides and publishes the result for readers. Only the
// owner of sampling may call it (see sampleIdleSignals).
func (a *Agent) evaluateIdle(ctx context.Context) idleSnapshot {
	s := a.sampleIdleSignals(ctx)
	s.idle, s.reason = a.idleDecision(ctx, s)
	a.idleMu.Lock()
	a.idleSnap = s
	a.idleMu.Unlock()
	return s
}

// idleSnapshotNow returns the last published evaluation. Safe from any goroutine.
func (a *Agent) idleSnapshotNow() idleSnapshot {
	a.idleMu.Lock()
	defer a.idleMu.Unlock()
	return a.idleSnap
}

// isIdle is the monitor loop's evaluation, logged.
func (a *Agent) isIdle(ctx context.Context) bool {
	s := a.evaluateIdle(ctx)
	log.Print(s.reason)
	return s.idle
}

// idleDecision decides from one tick's readings plus the stateless signals
// (DCV, sessions, processes, GPU). It takes no counter sample itself.
func (a *Agent) idleDecision(ctx context.Context, r idleSnapshot) (bool, string) {
	// The launch config object holds the activity signals (--active-ports,
	// --active-processes) and the idle CPU threshold (#1359). Until it loads,
	// an instance in use could look idle, so no idle action is taken; TTL,
	// cost limit and completion are tags and are enforced as ever.
	if a.config.LaunchConfigUnavailable() {
		return false, "Not idle: the launch config object has not loaded (" + a.config.LaunchConfigError + ")"
	}
	// DCV application streaming: use X11 activity file (written by kiosk-wm on
	// key/mouse events) as the authoritative idle signal. Falls back to DCV
	// connection count if the file doesn't exist (pre-kiosk-wm instances).
	if a.config.DCVSessionID != "" {
		// Decide via the pure dcvIdleDecision (spawn#282): X11 activity file is the
		// accurate signal when DCV is up; else the connection count; else (DCV not
		// ready) a BOUNDED startup grace — past which we fall through to the
		// standard CPU/network checks rather than returning not-idle forever (the
		// old unbounded grace billed until TTL if DCV never came up).
		info, statErr := os.Stat(x11ActivityFile)
		fileExists := statErr == nil
		var activityAge time.Duration
		if fileExists {
			activityAge = time.Since(info.ModTime())
		}
		connCount := -1
		if !fileExists {
			connCount = a.getDCVConnectionCount(ctx)
		}
		idle, fallThrough := dcvIdleDecision(fileExists, activityAge, connCount,
			time.Since(a.startTime), dcvGraceTimeout, a.config.IdleTimeout)
		if !fallThrough {
			if idle {
				return true, fmt.Sprintf("DCV session %s: idle (file=%v age=%v conns=%d)",
					a.config.DCVSessionID, fileExists, activityAge.Round(time.Second), connCount)
			}
			return false, fmt.Sprintf("Not idle: DCV session %s (file=%v age=%v conns=%d)",
				a.config.DCVSessionID, fileExists, activityAge.Round(time.Second), connCount)
		}
		log.Printf("DCV session %s never became ready within %v — falling back to standard idle checks",
			a.config.DCVSessionID, dcvGraceTimeout)
		// fall through to the standard checks below
	}

	// Active SSH/terminal sessions reset the idle timer.
	if sessions := countActiveSessions(ctx); sessions > 0 {
		return false, fmt.Sprintf("Not idle: %d active session(s)", sessions)
	}

	// Check configured process names — if any are running, instance is not idle.
	if proc := a.findActiveProcess(ctx); proc != "" {
		return false, fmt.Sprintf("Not idle: process %q is running", proc)
	}

	// Note: we intentionally do NOT check active port connections here.
	// An open browser tab maintains an ESTABLISHED TCP connection even when
	// the user is idle or away — treating it as "active" would permanently
	// block idle termination for abandoned tabs. The existing CPU and network
	// delta checks correctly distinguish real activity from idle keep-alives.

	// CPU, network and disk come from r, sampled together this tick
	// (sampleIdleSignals). An unmeasured reading is the conservative
	// assume-active value and is logged as what it is, not as a number (#734,
	// #919, #937).
	if r.cpu >= a.config.IdleCPUPercent {
		if r.cpuMeasured {
			return false, fmt.Sprintf("Not idle: CPU usage %.2f%% >= %.2f%%", r.cpu, a.config.IdleCPUPercent)
		}
		return false, fmt.Sprintf("Not idle: CPU usage could not be measured (no sample interval yet); "+
			"assuming active (%.2f%%)", r.cpu)
	}

	if !r.netMeasured {
		return false, fmt.Sprintf("Not idle: network traffic could not be measured (%s); assuming active", r.netWhy)
	}
	if r.net > idleNetworkBytesThreshold {
		return false, fmt.Sprintf("Not idle: Network traffic %d bytes", r.net)
	}

	if !r.diskMeasured {
		return false, fmt.Sprintf("Not idle: disk I/O could not be measured (%s); assuming active", r.diskWhy)
	}
	if r.disk > idleDiskBytesThreshold {
		return false, fmt.Sprintf("Not idle: Disk I/O %d bytes", r.disk)
	}

	// Check GPU utilization
	// GPU, Neuron, Gaudi: unknown is active, as for the counters (#954).
	gpuUtilization, gpuMeasured, gpuWhy := a.gpuUtilization(ctx)
	if !gpuMeasured {
		return false, fmt.Sprintf("Not idle: accelerator utilization could not be measured (%s); assuming active", gpuWhy)
	}
	if gpuUtilization > idleGPUPercent {
		return false, fmt.Sprintf("Not idle: GPU utilization %.2f%%", gpuUtilization)
	}

	// No presence check follows. The session check at the top already counted
	// every terminal with recent input; a terminal that merely exists (an idle
	// SSH login, a detached tmux) must not block idle-stop (#953).

	return true, fmt.Sprintf("System is idle (CPU: %.2f%%, Network: %d bytes, Disk: %d bytes, GPU: %.2f%%)",
		r.cpu, r.net, r.disk, gpuUtilization)
}

// lastActivity returns when the idle check last saw activity. Safe from any
// goroutine (#952).
func (a *Agent) lastActivity() time.Time {
	a.idleMu.Lock()
	defer a.idleMu.Unlock()
	return a.lastActivityTime
}

func (a *Agent) setLastActivity(t time.Time) {
	a.idleMu.Lock()
	a.lastActivityTime = t
	a.idleMu.Unlock()
}

// readCPUTimes is the CPU-times source, indirected through a var so the delta
// logic above can be tested on any platform.
//
// It needs to be: sysReadCPUTimes is build-tagged, and the darwin implementation
// always returns errBadProcStat — so on the platform most development happens
// on, every branch of cpuUsage except the first is unreachable, and the
// zero-delta bug (#734) could not be reproduced locally at all.
var readCPUTimes = sysReadCPUTimes

// cpuUsage returns CPU utilisation and whether it is a real MEASUREMENT.
//
// ok=false means "could not tell", and the returned value is the conservative
// assumption (100%, active) rather than a reading. Three distinct states used to
// collapse into one float, two of them returning sentinels a caller could not
// distinguish from data (#734):
//
//   - /proc/stat unreadable            -> 100.0, "assume active"
//   - no previous sample (first call)  -> 100.0, "assume active"
//   - zero time between samples        -> 0.0   <- the broken one
//
// That last branch returned the most REASSURING possible value from a
// no-information state, while its two siblings returned the most conservative
// one. It produced the reported contradiction: `spored status` calls this twice
// microseconds apart (once via IsIdle, once for the report), so the log line said
// `Not idle: CPU usage 100.00%` and the summary said `CPU: 0.0%` — on the same
// instance, in the same process, neither a measurement.
//
// It is also the sentinel that feeds the idle decision. In the daemon the ticker
// spaces calls a second or more apart so deltaTotal > 0 and the branch is not
// currently reachable from isIdle — but "one call site away from stopping a busy
// instance" is not a property worth keeping.
func (a *Agent) cpuUsage(ctx context.Context) (float64, bool) {
	idle, total, err := readCPUTimes(ctx)
	if err != nil {
		return 100.0, false // cannot read: assume active
	}

	// Delta CPU usage since last call — avoids cumulative-since-boot bias
	// that makes a freshly-booted instance look busy for its entire uptime.
	prevIdle, prevTotal := a.prevCPUIdle, a.prevCPUTotal
	a.prevCPUIdle, a.prevCPUTotal = idle, total

	if prevTotal == 0 {
		return 100.0, false // first call, no delta: assume active
	}
	deltaIdle := idle - prevIdle
	deltaTotal := total - prevTotal
	if deltaTotal == 0 {
		// No jiffies elapsed between samples. Unknown, and now reported as such
		// AND as "active" rather than as 0% — consistent with the two branches
		// above, and with networkBytes, which returns its assume-active value
		// on an unreadable counter.
		return 100.0, false
	}
	if deltaTotal < 0 || deltaIdle < 0 || deltaIdle > deltaTotal {
		// A counter went backwards (the kernel documents iowait, which is in the
		// total, as able to decrease) or the two came from different time bases.
		// The ratio means nothing, and the old code turned a negative one into a
		// "usage" under every idle threshold (#937 audit).
		return 100.0, false
	}
	return 100.0 - (float64(deltaIdle)/float64(deltaTotal))*100.0, true
}

// readNetworkBytes is the network-counter source, indirected through a var for
// the same reason as readCPUTimes: the darwin stub always says "unknown", so the
// delta logic below is otherwise untestable where development happens.
var readNetworkBytes = sysReadNetworkBytes

// networkAssumedActive is what an unmeasured network reading reports: far over
// idleNetworkBytesThreshold, so "could not tell" never reads as "idle".
const networkAssumedActive int64 = 64 * idleNetworkBytesThreshold

// networkBytes returns network bytes since the previous sample and whether that
// is a real MEASUREMENT. When measured is false the value is the conservative
// networkAssumedActive and why says what was missing.
//
// The semantics of "unknown" are the CPU check's (cpuUsage, #734): the idle
// decision treats it as active, and status displays it as unknown rather than as
// a number. That is the safe direction for a check that stops instances, and the
// cost of it is the reason for the rest of this function: an unknown that never
// resolves disables idle-stop, so it must be rare and it must be loud.
//
// It used to be neither (#919). The first-sample test was prev == 0, and a host
// whose interface the old name filter did not recognise read 0 bytes on every
// call, so EVERY call was a "first call" and returned the assume-active value —
// indistinguishable in the log from 976.6 KB/min of real traffic. Now the first
// sample is tracked explicitly, a counter that cannot be read is reported as
// unknown (and warned about once), and the reader picks interfaces by structure
// (readNetDevBytes) so the unreadable case is genuinely exceptional.
func (a *Agent) networkBytes(ctx context.Context) (bytes int64, measured bool, why string) {
	rx, tx := readNetworkBytes(ctx)
	if rx < 0 || tx < 0 {
		const reason = "no physical network interface counters could be read"
		if !a.netUnknownWarned {
			a.netUnknownWarned = true
			log.Printf("WARNING: %s; network traffic is treated as active, so idle-stop "+
				"will not trigger on this host (#919)", reason)
		}
		return networkAssumedActive, false, reason
	}
	prevRx, prevTx, sampled := a.prevNetRx, a.prevNetTx, a.netSampled
	a.prevNetRx, a.prevNetTx, a.netSampled = rx, tx, true
	if !sampled {
		return networkAssumedActive, false, "single sample"
	}
	delta := (rx - prevRx) + (tx - prevTx)
	if delta < 0 {
		// The counted interfaces changed (an ENI detached) or a counter reset. The
		// delta means nothing, and a negative one would read as idle.
		return networkAssumedActive, false, "interface counters went backwards"
	}
	return delta, true, ""
}

// readDiskIOBytes is the disk-counter source, indirected through a var for the
// same reason as readNetworkBytes: the darwin stub always says "unknown".
var readDiskIOBytes = sysReadDiskIOBytes

// The per-tick (one minute) network and disk volumes at or under which those
// signals count as idle. They are set from what an instance nobody is using
// actually does, measured on the supported AMIs (#1093), not from what idle
// ought to look like. Untouched for 20 minutes each:
//
//	                 disk I/O per minute   network per minute
//	AL2023 arm64     1.3 - 7.8 MB          up to ~146 KB
//	AL2023 x86_64    1.3 - 3.9 MB          up to ~148 KB
//	Ubuntu 24.04     0.14 - 3.7 MB         21 - 55 KB
//
// The disk volume is almost all systemd-journald (rsyslog too on Ubuntu); the
// network is the SSM agent, AL2023's chronicled and spored's own API calls.
// Both thresholds used to be 100 KB, which every one of those minutes exceeded,
// so --idle-timeout could never fire on any of them. Before #937 counted NVMe
// disks the disk reading was ~0 on Nitro instances, which hid this.
//
// A minute over a threshold only restarts the idle window, so the thresholds
// need to clear the steady background with margin, not every rare spike. Work
// that a person or a job is actually doing clears them easily; CPU, sessions
// and accelerators are judged separately.
const (
	idleNetworkBytesThreshold int64 = 1 << 20  // 1 MiB/min, ~5x the highest idle reading
	idleDiskBytesThreshold    int64 = 16 << 20 // 16 MiB/min, ~2x the highest idle spike
)

// diskAssumedActive is what an unmeasured disk reading reports: far over
// idleDiskBytesThreshold, so "could not tell" never reads as "idle".
const diskAssumedActive int64 = 64 * idleDiskBytesThreshold

// diskIOBytes returns disk bytes read and written since the previous sample and
// whether that is a real MEASUREMENT. When measured is false the value is the
// conservative diskAssumedActive and why says what was missing. Unknown has the
// network check's semantics (networkBytes, #919): the idle decision treats it as
// active, status shows it as unknown, and an unreadable counter is warned about
// once.
//
// It used to be no delta at all (#937). The idle check compared the since-boot
// total with a per-minute threshold, and the device filter dropped every NVMe
// disk. So on Nitro it read 0 for ever, a dead signal that let a disk-bound,
// low-CPU workload be idle-stopped; and on xvd/sd/vd hosts it read the boot-time
// total, always over the threshold, so idle-stop could never fire.
func (a *Agent) diskIOBytes(ctx context.Context) (bytes int64, measured bool, why string) {
	total := readDiskIOBytes(ctx)
	if total < 0 {
		const reason = "no whole-disk I/O counters could be read"
		if !a.diskUnknownWarned {
			a.diskUnknownWarned = true
			log.Printf("WARNING: %s; disk I/O is treated as active, so idle-stop "+
				"will not trigger on this host (#937)", reason)
		}
		return diskAssumedActive, false, reason
	}
	prev, sampled := a.prevDiskBytes, a.diskSampled
	a.prevDiskBytes, a.diskSampled = total, true
	if !sampled {
		return diskAssumedActive, false, "single sample"
	}
	delta := total - prev
	if delta < 0 {
		// A disk was detached (the counted set shrank) or a counter reset. The
		// delta means nothing, and a negative one would read as idle.
		return diskAssumedActive, false, "disk counters went backwards"
	}
	return delta, true, ""
}

// getDCVConnectionCount returns the DCV session's connected-client count, or -1
// when DCV isn't ready (binary missing, server down, or unparseable output) — the
// caller's "unknown" signal. The parsing is in parseDCVConnections (pure/tested);
// the shell-out goes through the dcvRunner seam so it's testable (#282).
func (a *Agent) getDCVConnectionCount(ctx context.Context) int {
	out, err := a.dcv.describeSession(ctx, a.config.DCVSessionID)
	if err != nil {
		return -1 // DCV not ready or session not found
	}
	n, err := parseDCVConnections(out)
	if err != nil {
		return -1
	}
	return n
}

func (a *Agent) checkSpotInterruption(ctx context.Context) bool {
	// Query provider for interruption info
	info, err := a.provider.CheckSpotInterruption(ctx)
	if err != nil {
		log.Printf("Error checking Spot interruption: %v", err)
		return false
	}

	// No interruption
	if info == nil {
		return false
	}

	// Spot interruption detected
	log.Printf("🚨 SPOT INTERRUPTION DETECTED: action=%s, time=%s", info.Action, info.Time.Format(time.RFC3339))

	// Clean up DNS immediately to avoid stale records
	log.Printf("Spot interruption: Running cleanup tasks")
	cleanupCtx := context.Background()
	a.Cleanup(cleanupCtx)

	// Alert users immediately
	a.warnUsers(ctx, i18n.T("spawn.agent.spot_interruption.title")+"\n"+
		i18n.Tf("spawn.agent.spot_interruption.message", map[string]interface{}{
			"Action": info.Action,
			"Time":   info.Time.Format("15:04:05"),
		}))

	// Run the pre-stop path against one deadline taken from the notice, so the
	// task flush and the user's hook together end inside it (#1311).
	a.runPreStop(spotPreStopDeadline(info), taskproto.ExitSpotInterruption)

	// Send Slack notification
	a.notifier.Notify(cleanupCtx, "spot_interrupt",
		fmt.Sprintf("action: %s, interruption at %s", info.Action, info.Time.Format("15:04")))

	// Send file-based notifications (legacy)
	a.sendSpotInterruptionNotification(info.Action, info.Time.Format(time.RFC3339))

	// Fire the optional off-node webhook LAST and time-boxed, so a slow or dead
	// endpoint can never delay the survival work above (#228, same #65 discipline
	// that keeps the spot monitor off the critical path). Fire-once: the monitor
	// re-enters this handler every 5s until the node dies.
	if !a.spotWebhookFired {
		a.spotWebhookFired = true
		a.emitSpotInterruptionWebhook(info)
	}

	// Release the launch Elastic IP (#661) near the end of the window, once the
	// survival work above is done. Last, because the instance loses its public
	// address with it.
	a.scheduleSpotElasticIPRelease(ctx, info)

	// Log for posterity
	log.Printf("Spot interruption: action=%s, time=%s", info.Action, info.Time.Format(time.RFC3339))

	// Continue monitoring for remaining time
	return false // Return false to allow normal monitoring to continue
}

// elasticIPReleaser is the optional provider capability to release the
// instance's own launch Elastic IP (#661). EC2Provider implements it; the local
// provider has no address to release.
type elasticIPReleaser interface {
	ReleaseOwnElasticIPs(ctx context.Context)
}

// spotEIPReleaseLead is how long before the interruption time spored releases
// its launch Elastic IP. The address carries the instance's only egress on a
// multi-interface instance, so releasing it at the notice would cut off, for up
// to two minutes, anything the user's own checkpoint logic is still sending.
// Releasing it after the instance is gone is impossible from here. This is the
// compromise: most of the window keeps its network, and 20s leaves room for the
// call. A miss is not a leak: the TTL reaper releases it.
const spotEIPReleaseLead = 20 * time.Second

// scheduleSpotElasticIPRelease releases the launch Elastic IP once per
// interruption, and only for a terminate: a stop or hibernate interruption
// brings the instance back, and it keeps its address while stopped.
//
// It waits until spotEIPReleaseLead before info.Time, or less if spored is
// being shut down first (ctx done), which on a reclaim means the OS is already
// going away. Off the monitor goroutine, through a.spotEIPGo, so the wait never
// delays the next poll.
func (a *Agent) scheduleSpotElasticIPRelease(ctx context.Context, info *provider.InterruptionInfo) {
	if info == nil || !strings.EqualFold(info.Action, "terminate") || a.spotEIPScheduled {
		return
	}
	releaser, ok := a.provider.(elasticIPReleaser)
	if !ok {
		return
	}
	a.spotEIPScheduled = true
	now := time.Now
	if a.spotNow != nil {
		now = a.spotNow
	}
	wait := info.Time.Add(-spotEIPReleaseLead).Sub(now())
	if wait < 0 {
		wait = 0
	}
	run := func() {
		if wait > 0 {
			sleep := a.spotEIPSleep
			if sleep == nil {
				sleep = sleepCtx
			}
			sleep(ctx, wait) // returns early when spored is stopping
		}
		log.Printf("Spot interruption: releasing the launch Elastic IP (%s before the interruption time)", spotEIPReleaseLead)
		releaser.ReleaseOwnElasticIPs(context.WithoutCancel(ctx))
	}
	if a.spotEIPGo != nil {
		a.spotEIPGo(run)
		return
	}
	go run()
}

// sleepCtx sleeps for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// spotWebhookPayload is the fixed, stable on-node fact-struct spored POSTs on a
// spot interruption (#228). Every field is a projection of knowledge the node
// already holds mid-reclamation — there is no caller-supplied "include X" field
// except Correlation, which is echoed verbatim and never parsed. A consumer
// takes the fields it cares about and ignores the rest; correlation back to the
// consumer's own entity/record rides on Correlation (NOT the RunInstances
// ClientToken, which the node cannot see — see issue #228).
type spotWebhookPayload struct {
	Event                string `json:"event"`                 // always "spot_interruption"
	InstanceID           string `json:"instance_id"`           //
	Region               string `json:"region"`                //
	AZ                   string `json:"az,omitempty"`          //
	Action               string `json:"action"`                // AWS verbatim: "terminate" or "stop"
	InterruptionDeadline string `json:"interruption_deadline"` // AWS-provided window end (RFC3339)
	NameTag              string `json:"name_tag,omitempty"`    // spawn:name
	ComputeSeconds       int64  `json:"compute_seconds"`       // accumulated compute time
	LastActivityTime     string `json:"last_activity_time"`    // RFC3339
	Correlation          string `json:"correlation,omitempty"` // opaque caller blob, verbatim
	EmittedAt            string `json:"emitted_at"`            // RFC3339, when spored sent this
}

// emitSpotInterruptionWebhook POSTs the fixed payload to the launch-configured
// URL exactly once, best-effort, time-boxed by WebhookTimeout (default 2s). Any
// failure — disabled, timeout, DNS, non-2xx — is logged at most and dropped: a
// node mid-reclamation must never block on this any more than on a tag write.
// The durable source of truth remains the EC2 state + spawn:* tags; this is the
// in-window upgrade over poll-and-infer, not a delivery guarantee.
func (a *Agent) emitSpotInterruptionWebhook(info *provider.InterruptionInfo) {
	cfg := a.cfg()
	if cfg == nil || cfg.SpotWebhookURL == "" {
		return // opt-in; empty URL = today's behavior
	}

	timeout := cfg.WebhookTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	payload := spotWebhookPayload{
		Event:                "spot_interruption",
		InstanceID:           a.identity.InstanceID,
		Region:               a.identity.Region,
		AZ:                   a.identity.AvailabilityZone,
		Action:               info.Action,
		InterruptionDeadline: info.Time.UTC().Format(time.RFC3339),
		NameTag:              a.identity.Name,
		ComputeSeconds:       a.TotalComputeSeconds(),
		LastActivityTime:     a.lastActivity().UTC().Format(time.RFC3339),
		Correlation:          cfg.WebhookCorrelation,
		EmittedAt:            time.Now().UTC().Format(time.RFC3339),
	}

	postWebhook("Spot webhook", cfg.SpotWebhookURL, timeout, payload)
}

// postWebhook marshals payload and POSTs it to url, best-effort, time-boxed by
// timeout. Any failure — marshal, build, timeout, DNS, non-2xx — is logged at
// most and dropped; the caller (spot/completion webhook) must never block the
// lifecycle action it precedes on a slow or dead endpoint. label prefixes log
// lines so spored.log can distinguish which webhook fired.
func postWebhook(label, url string, timeout time.Duration, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("%s: marshal failed, dropping: %v", label, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("%s: request build failed, dropping: %v", label, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("%s: POST to %s failed, dropping (best-effort): %v", label, url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("%s: endpoint %s returned %d, dropping (best-effort)", label, url, resp.StatusCode)
		return
	}
	log.Printf("%s: notice POSTed to %s", label, url)
}

// completionWebhookPayload is the fixed, stable on-node fact-struct spored
// POSTs when the completion sentinel fires (#497) — the caller-facing signal
// that was previously only reachable by parsing spored's Slack notification or
// by the caller reinventing its own artifact-polling loop (as calque's
// WaitForSummary did). Mirrors spotWebhookPayload's shape: Correlation is the
// only caller-supplied field, echoed verbatim and never parsed.
type completionWebhookPayload struct {
	Event            string `json:"event"`                 // always "completion"
	InstanceID       string `json:"instance_id"`           //
	Region           string `json:"region"`                //
	NameTag          string `json:"name_tag,omitempty"`    // spawn:name
	OnComplete       string `json:"on_complete"`           // the configured action: terminate/stop/hibernate/exit
	ComputeSeconds   int64  `json:"compute_seconds"`       // accumulated compute time
	LastActivityTime string `json:"last_activity_time"`    // RFC3339
	Correlation      string `json:"correlation,omitempty"` // opaque caller blob, verbatim
	EmittedAt        string `json:"emitted_at"`            // RFC3339, when spored sent this
}

// emitCompletionWebhook POSTs the fixed payload to the launch-configured URL
// exactly once, best-effort, time-boxed by WebhookTimeout (default 2s) — the
// same fire-and-forget discipline as emitSpotInterruptionWebhook (#228),
// applied to the completion sentinel instead of a spot notice (#497). Called
// from checkCompletion BEFORE the grace-period sleep and lifecycle action, so
// a caller learns of completion as early as spored itself does.
func (a *Agent) emitCompletionWebhook(ctx context.Context) {
	cfg := a.cfg()
	if cfg == nil || cfg.CompletionWebhookURL == "" {
		return // opt-in; empty URL = today's behavior
	}

	timeout := cfg.WebhookTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	payload := completionWebhookPayload{
		Event:            "completion",
		InstanceID:       a.identity.InstanceID,
		Region:           a.identity.Region,
		NameTag:          a.identity.Name,
		OnComplete:       cfg.OnComplete,
		ComputeSeconds:   a.TotalComputeSeconds(),
		LastActivityTime: a.lastActivity().UTC().Format(time.RFC3339),
		Correlation:      cfg.WebhookCorrelation,
		EmittedAt:        time.Now().UTC().Format(time.RFC3339),
	}

	postWebhook("Completion webhook", cfg.CompletionWebhookURL, timeout, payload)
}

func (a *Agent) sendSpotInterruptionNotification(action, interruptTime string) {
	// Log to spored logs (always)
	log.Printf("📢 NOTIFICATION: Spot interruption detected - action=%s time=%s", action, interruptTime)

	// Write to a file that can be picked up by external systems (OS temp dir).
	notificationFile := filepath.Join(os.TempDir(), "spawn-spot-interruption.json")
	notification := fmt.Sprintf(`{
  "event": "spot-interruption",
  "instance_id": "%s",
  "action": "%s",
  "time": "%s",
  "detected_at": "%s"
}`, a.identity.InstanceID, action, interruptTime, time.Now().UTC().Format(time.RFC3339))

	if err := os.WriteFile(notificationFile, []byte(notification), 0600); err != nil { // nosemgrep: go.lang.security.bad_tmp.bad-tmp-file-creation
		log.Printf("Failed to write notification file: %v", err)
	}

	// Future enhancement: Support webhooks, email, SNS, etc.
	// For now, the notification file can be picked up by external monitoring
}

func (a *Agent) warnUsers(ctx context.Context, message string) {
	// Broadcast to logged-in sessions (OS-specific: wall on Linux, msg on Windows).
	sysWarnUsers(ctx, message)

	// Also write a warning file in the OS temp dir for external pickup.
	_ = os.WriteFile(filepath.Join(os.TempDir(), "SPAWN_WARNING"), []byte(message+"\n"), 0600) // nosemgrep: go.lang.security.bad_tmp.bad-tmp-file-creation

	log.Printf("Warning sent to users: %s", message)
}

// refreshConfigFromTags re-reads the spawn:* tags and swaps in the result. A
// failed read keeps the current config.
func (a *Agent) refreshConfigFromTags(ctx context.Context) {
	if err := a.provider.RefreshConfig(ctx); err != nil {
		log.Printf("Warning: failed to refresh config from tags: %v", err)
		return
	}
	if fresh, err := a.provider.GetConfig(ctx); err == nil && fresh != nil {
		a.setConfig(fresh)
	}
}

// maxEarlyCompletionRefreshes caps the out-of-cadence tag refreshes a
// completion signal can trigger. Tag visibility settles in seconds, so a few
// ticks covers the race; past that, a workload that writes the file with no
// on-complete configured falls back to the normal every-5-ticks refresh rather
// than costing a DescribeTags every minute forever.
const maxEarlyCompletionRefreshes = 5

// refreshForEarlyCompletion refreshes config from tags when the completion
// signal is present but no on-complete action is known yet (#270).
//
// spored learns on-complete from EC2 tags, which are eventually consistent and
// re-read only on tick 1 and every 5th tick. A task that writes its completion
// file before spawn:on-complete is visible used to be ignored until the next
// scheduled refresh, or, when that refresh was before the file appeared, ran
// to its TTL. The signal itself is now the trigger to look again.
//
// The file checked is the configured one or, when the completion-file tag is
// not visible either, the default every launch path uses. A custom
// completion-file whose tag is also invisible is still picked up at the next
// scheduled refresh.
func (a *Agent) refreshForEarlyCompletion(ctx context.Context) {
	if a.config.OnComplete != "" || a.earlyCompletionRefreshes >= maxEarlyCompletionRefreshes {
		return
	}
	path := a.config.CompletionFile
	if path == "" {
		path = a.defaultCompletionFile
		if path == "" {
			path = provider.DefaultCompletionFile
		}
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	a.earlyCompletionRefreshes++
	log.Printf("Completion signal %s present but no on-complete action known yet; re-reading tags (#270)", path)
	a.refreshConfigFromTags(ctx)
}

func (a *Agent) checkCompletion(ctx context.Context) bool {
	// Check if completion file exists
	if fi, err := os.Stat(a.config.CompletionFile); err == nil {
		// Acted on before a hibernation, and not written again since: the
		// instance was started again on purpose (#987).
		if !a.completionSpent.IsZero() && fi.ModTime().Equal(a.completionSpent) {
			return false
		}
		// Announce once. The file stays on disk, so every tick after a
		// failed stop/hibernate comes back here; before #978 each one sent
		// another `completion` notification, POSTed the webhook again (a
		// caller may treat each POST as "done"), copied the command log to the
		// console again and slept the grace period again, holding up the TTL
		// and cost-limit checks below.
		if !a.completionAnnounced {
			a.completionAnnounced = true
			a.announceCompletion(ctx)
		}

		// Execute action based on configuration
		switch strings.ToLower(a.config.OnComplete) {
		case "terminate":
			// Retried every tick on failure, deliberately: terminate is the
			// TTL's action too, and there is nothing to back off to.
			a.terminate(ctx, "Completion signal received", taskproto.ExitCompleted)
		case "stop", "hibernate":
			at := stopAttempt{trigger: "on-complete", action: "stop", failEvent: "completion_stop_failed",
				run: func() error { return a.stop(ctx, "Completion signal received", taskproto.ExitCompleted) }}
			if strings.EqualFold(a.config.OnComplete, "hibernate") {
				at.action = "hibernate"
				at.run = func() error { return a.hibernate(ctx, taskproto.ExitCompleted) }
			}
			attempted, _ := a.stopWithBackoff(ctx, time.Now(), at)
			if !attempted {
				// Backing off after a failed attempt. Let the TTL and cost-limit
				// checks run: they are the backstop for an instance that cannot
				// be stopped. The idle check shares the backoff, so it does not
				// try the same stop in between.
				return false
			}
		case "exit":
			// For local provider - just exit
			log.Printf("Exiting on completion signal")
			a.Cleanup(ctx)
			os.Exit(0)
		default:
			log.Printf("Unknown on-complete action: %s (doing nothing)", a.config.OnComplete)
			return false
		}

		return true
	}

	return false
}

// announceCompletion is the once-per-signal part of the completion path: the
// `completion` notification, the completion webhook, the user warning and the
// grace period (#978).
func (a *Agent) announceCompletion(ctx context.Context) {
	log.Printf("Completion signal detected: file %s exists", a.config.CompletionFile)

	// Read completion file for metadata (optional)
	content, err := os.ReadFile(a.config.CompletionFile)
	if err == nil && len(content) > 0 {
		log.Printf("Completion metadata: %s", strings.TrimSpace(string(content)))
	}

	// Did the workload FAIL? The record the bootstrap writes carries
	// {"status": "...", "exit_code": N}, so this needs no new plumbing —
	// only reading what was already being logged (#736).
	//
	// On a failure the command log is the only record of WHY, and it dies
	// with the instance, so copy its tail to the serial console before the
	// terminate below. Gated on failure: a successful job's log is rarely
	// read and console space is shared with the boot messages.
	if completionFailed(content) {
		a.writeCommandLogToConsole("workload failed")
	}

	// Notify via Slack before the grace period
	a.notifier.Notify(ctx, "completion", "")

	// Fire the optional off-node completion webhook (#497), same
	// fire-and-forget discipline as the spot-interruption webhook (#228) —
	// so a caller waiting on ITS OWN target learns of completion as early
	// as spored itself does, before the grace-period sleep below.
	a.emitCompletionWebhook(ctx)

	// Warn users with grace period
	delay := a.config.CompletionDelay
	a.warnUsers(ctx, i18n.Tf("spawn.agent.workload_complete", map[string]interface{}{
		"Action": a.config.OnComplete,
		"Delay":  delay,
	}))

	log.Printf("Grace period: waiting %v before action", delay)
	if a.graceSleep != nil {
		a.graceSleep(delay)
	} else {
		time.Sleep(delay)
	}
}

func (a *Agent) shutdownWarnDelay() time.Duration {
	if a.warnDelay > 0 {
		return a.warnDelay
	}
	return 5 * time.Second
}

// stopRetryBase and stopRetryMax bound the backoff between attempts at a
// failed stop or hibernate, idle (#948) or on-complete (#978).
const (
	stopRetryBase = 5 * time.Minute
	stopRetryMax  = time.Hour
)

// stopRetryDelay is how long to wait after the n-th consecutive failure
// (n >= 1): 5, 10, 20, 40 minutes, then hourly.
func stopRetryDelay(failures int) time.Duration {
	d := stopRetryBase
	for i := 1; i < failures && d < stopRetryMax; i++ {
		d *= 2
	}
	if d > stopRetryMax {
		d = stopRetryMax
	}
	return d
}

// stopRetry is the backoff state for stop/hibernate. There is one per agent,
// shared by the idle and on-complete triggers: whether EC2 will stop this
// instance does not depend on why spored asked, so a refusal for one trigger
// also holds off the other, and the failure is announced once.
type stopRetry struct {
	failures int       // consecutive failed attempts
	retryAt  time.Time // no attempt before this
}

// backingOff reports whether an attempt at now must wait.
func (r *stopRetry) backingOff(now time.Time) bool { return now.Before(r.retryAt) }

// stopAttempt describes one trigger's stop or hibernate for stopWithBackoff.
// run is the a.stop or a.hibernate call itself, so each call site names its
// own taskproto.Exit* token (TestEveryLifecycleExitPathCarriesAReasonToken).
type stopAttempt struct {
	trigger   string       // "idle", "on-complete": for the log and the failure detail
	action    string       // "stop" or "hibernate"
	run       func() error // the a.stop / a.hibernate call
	failEvent string       // notification sent once, on the first failure
}

// stopWithBackoff stops or hibernates the instance unless a previous attempt
// failed and its backoff has not run out. It reports
// whether it attempted, and the provider's error.
//
// Before #948 (idle) and #978 (on-complete) a failed stop was simply logged, so
// the next tick ran the whole sequence again: once a minute until TTL. The
// common cause is permanent (a one-time Spot instance cannot be stopped; a role
// without ec2:StopInstances), so a retry every minute buys nothing. A transient
// failure (throttling) is retried soon enough by the backoff. The failure is
// announced once, on the first attempt that fails: the first failure is the
// news. Success is the caller's to announce.
func (a *Agent) stopWithBackoff(ctx context.Context, now time.Time, at stopAttempt) (bool, error) {
	r := &a.stopRetry
	if r.backingOff(now) {
		return false, nil // backing off after a failed attempt; logged when it failed
	}
	err := at.run()
	if err == nil {
		*r = stopRetry{}
		return true, nil
	}

	r.failures++
	delay := stopRetryDelay(r.failures)
	r.retryAt = now.Add(delay)
	log.Printf("%s %s failed (attempt %d): %v; the instance is still running, "+
		"next attempt in %v", at.trigger, at.action, r.failures, err, delay)
	if r.failures == 1 {
		a.notifier.Notify(ctx, at.failEvent,
			fmt.Sprintf("%s %s failed, instance still running; retrying with backoff: %v", at.trigger, at.action, err))
	}
	return true, err
}

// idleTimeoutReached stops or hibernates the instance once the idle timeout has
// passed, on stopWithBackoff's backoff, and announces it only once EC2 has
// accepted the request (#948).
func (a *Agent) idleTimeoutReached(ctx context.Context, now time.Time) {
	if a.stopRetry.backingOff(now) {
		return
	}
	log.Printf("Idle timeout reached (%v)", now.Sub(a.lastActivity()))

	if a.config.TerminateOnIdle {
		// Like the TTL's terminate, retried every tick on failure: there is
		// nothing to back off to. Announced, like the stop, only once EC2 has
		// accepted the request, so a failing terminate is not reported as done
		// and a retry that succeeds is (#1205).
		//
		// The event is ttl_expired, spore-bot's one "has terminated" message,
		// because spore-bot lists no idle_terminated event (its Slack and
		// Discord formatters keep one only for older spored, and SMS has none).
		// idle_stopped would be worse: it tells the user the instance can be
		// woken. Switch to idle_terminated once spore-bot lists it
		// (spore-host/spore-host#612).
		if err := a.terminate(ctx, "Idle timeout", taskproto.ExitIdleTimeout); err == nil {
			a.notifier.Notify(ctx, "ttl_expired", "idle timeout reached")
		}
		return
	}

	event := "idle_stopped"
	at := stopAttempt{trigger: "idle", action: "stop", failEvent: "idle_stop_failed",
		run: func() error { return a.stop(ctx, "Idle timeout", taskproto.ExitIdleTimeout) }}
	if a.config.HibernateOnIdle {
		event = "idle_hibernated"
		at.action = "hibernate"
		at.run = func() error { return a.hibernate(ctx, taskproto.ExitIdleTimeout) }
	}
	if _, err := a.stopWithBackoff(ctx, now, at); err == nil {
		a.notifier.Notify(ctx, event, "")
	}
}

// restoreAfterCleanup puts back what Cleanup removed, for an instance that is
// running after all: the stop or hibernate Cleanup prepared for failed, so it
// keeps its DNS name and job-array registration (#948) and its plugins (#979);
// or it has resumed from the hibernation (#987). Cleanup runs before the EC2
// call because a hibernating guest is frozen, not shut down, and gets no later
// chance to deregister.
func (a *Agent) restoreAfterCleanup(ctx context.Context) {
	if a.registry != nil && a.config.JobArrayID != "" {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := a.registry.Register(rctx, a.config.JobArrayID, a.config.JobArrayIndex); err != nil {
			log.Printf("Warning: failed to re-register with hybrid registry after a failed stop: %v", err)
		}
		cancel()
	}
	// Cleanup recorded every plugin as stopped and ended its health loop; the
	// instance is still running, so put both back (#979).
	if a.pluginRuntime != nil {
		a.pluginRuntime.ResumeStopped(ctx)
	}
	// The lease renewal is the registration path; making it due now re-registers
	// the record Cleanup deleted, and keeps spawn:dns-status honest if it fails.
	a.nextDNSRefresh = time.Time{}
	a.refreshDNSLease(ctx)
}

// stop runs the pre-stop hook and cleanup, then asks the provider to stop the
// instance. It returns the provider's error; on failure the DNS name and
// registry entry are restored, since the instance is still running (#948).
func (a *Agent) stop(ctx context.Context, reason string, why taskproto.ExitReason) error {
	log.Printf("Stopping instance (reason: %s)", reason)

	a.runPreStop(time.Time{}, why)

	// Clean up DNS before stopping
	a.Cleanup(ctx)

	a.warnUsers(ctx, i18n.Tf("spawn.agent.stopping", map[string]interface{}{
		"Reason": reason,
	}))

	// Wait a moment for users to see warning
	time.Sleep(a.shutdownWarnDelay())

	err := a.provider.Stop(ctx, reason)
	if err != nil {
		log.Printf("Failed to stop instance: %v", err)
		a.restoreAfterCleanup(ctx)
	}
	return err
}

// hibernate is stop's twin for --hibernate-on-idle and on-complete hibernate.
func (a *Agent) hibernate(ctx context.Context, why taskproto.ExitReason) error {
	log.Printf("Hibernating instance")

	a.runPreStop(time.Time{}, why)

	// Clean up DNS before hibernating
	a.Cleanup(ctx)

	a.warnUsers(ctx, i18n.T("spawn.agent.hibernating"))

	// Wait a moment for users to see warning
	time.Sleep(a.shutdownWarnDelay())

	err := a.provider.Hibernate(ctx)
	if err != nil {
		log.Printf("Failed to hibernate: %v", err)
		a.restoreAfterCleanup(ctx)
	}
	return err
}

// Cleanup performs cleanup tasks before shutdown (plugins, DNS, registry).
func (a *Agent) Cleanup(ctx context.Context) {
	log.Printf("Running cleanup tasks...")

	// Flush the compute-seconds tag before we stop, bypassing the periodic
	// throttle. Without this, a graceful stop (including an in-place spored
	// upgrade, #234) would discard up to ~5 minutes of compute time since the
	// last throttled write; the next boot reads the tag into computeSecondsBase,
	// so flushing here keeps the compute clock continuous across the restart.
	//
	// The local checkpoint goes first: it needs no network, and a stop's
	// shutdown is exactly when the network write is likeliest to fail (#928).
	a.WriteComputeCheckpoint()
	if a.identity != nil && a.identity.Provider == "ec2" {
		a.flushComputeSecondsTag(ctx)
	}

	// Stop all running plugins before deregistering from infrastructure.
	if a.pluginRuntime != nil {
		log.Printf("Stopping plugins...")
		a.pluginRuntime.StopAll(ctx)
		log.Printf("Plugins stopped")
	}

	// Deregister from hybrid registry
	if a.registry != nil && a.config.JobArrayID != "" {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := a.registry.Deregister(cleanupCtx, a.config.JobArrayID); err != nil {
			log.Printf("Warning: Failed to deregister from hybrid registry: %v", err)
		} else {
			log.Printf("✓ Deregistered from hybrid registry")
		}
	}

	// Clean up DNS (EC2 only)
	if ip := a.publicIP(); a.dnsClient != nil && a.config.DNSName != "" && ip != "" {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		// Use job array DNS deletion if part of a job array
		if a.config.JobArrayID != "" && a.config.JobArrayName != "" {
			log.Printf("Deleting job array DNS record: %s (array: %s)", a.config.DNSName, a.config.JobArrayName)
			resp, err := a.dnsClient.DeleteJobArrayDNS(cleanupCtx, a.config.DNSName, ip,
				a.config.JobArrayID, a.config.JobArrayName)
			if err != nil {
				log.Printf("Warning: Failed to delete job array DNS: %v", err)
			} else {
				fqdn := dns.GetFullDNSName(a.config.DNSName, a.identity.AccountID, a.dnsDomain)
				log.Printf("✓ Job array DNS deleted: %s", fqdn)
				if resp.Message != "" {
					log.Printf("  %s", resp.Message)
				}
			}
		} else {
			log.Printf("Deleting DNS record: %s", a.config.DNSName)
			_, err := a.dnsClient.DeleteDNS(cleanupCtx, a.config.DNSName, ip)
			if err != nil {
				log.Printf("Warning: Failed to delete DNS: %v", err)
			} else {
				fqdn := dns.GetFullDNSName(a.config.DNSName, a.identity.AccountID, a.dnsDomain)
				log.Printf("✓ DNS deleted: %s", fqdn)
			}
		}
	}

	log.Printf("Cleanup complete")
}

// runPreStop executes the user-configured pre-stop command before any
// lifecycle-triggered shutdown. It runs at most once (guarded by preStopDone).
// The default timeout is 5 minutes; spot interruptions use 90 seconds.
//
// spotDeadline is the Spot notice's pre-stop deadline (spotPreStopDeadline),
// or the zero time on every other path. With one, the flush and the user's
// hook share it (#1311): the flush is sized to it first, then the hook gets
// at most what the flush left, and none once it has passed. That holds even
// for a configured PreStopTimeout, since nothing can run past the reclaim.
//
// It also runs the task terminal-flush hook (spawn#632) first. `why` is a stable
// token, not the human reason string spored logs, so a reworded log message can
// never change a recorded retry class.
//
// The flush is hung here rather than at each exit site on purpose: every path
// that ends an instance's life already calls this function, so a future exit
// path gets the record for free instead of being a place the two can drift
// apart. It goes BEFORE the user's hook because that hook is allowed to take
// minutes (PreStopTimeout) and may itself fail — the diagnostics should already
// be in S3 by then.
func (a *Agent) runPreStop(spotDeadline time.Time, why taskproto.ExitReason) {
	a.flushTaskRecord(why, spotDeadline)

	if a.config.PreStop == "" || a.preStopDone {
		return
	}
	a.preStopDone = true

	timeout, ok := preStopTimeout(a.config.PreStopTimeout, spotDeadline, a.spotClock())
	if !ok {
		log.Printf("Pre-stop hook skipped: the Spot notice left no time after the task flush (deadline %s)", spotDeadline.Format(time.RFC3339))
		a.notifier.Notify(context.Background(), "pre_stop_failed", "skipped: the Spot interruption notice left no time after the task flush")
		return
	}

	// Run the hook as the instance's primary user (not root) so ~/$HOME/PATH and
	// credential resolution match how the workload ran (#63). The username comes
	// from the spawn:local-username tag; validate it before handing it to `su`
	// (defense-in-depth — never interpolate an untrusted tag into a shell user).
	// An empty/invalid/missing username falls back to the legacy root shell.
	runAsUser := a.config.LocalUsername
	if runAsUser != "" {
		if err := security.ValidateUsername(runAsUser); err != nil {
			log.Printf("pre-stop: ignoring invalid spawn:local-username %q (%v) — running as root", runAsUser, err)
			runAsUser = ""
		}
	}

	if runAsUser != "" {
		log.Printf("Running pre-stop hook as %s (timeout: %v): %s", runAsUser, timeout, a.config.PreStop)
	} else {
		log.Printf("Running pre-stop hook (timeout: %v): %s", timeout, a.config.PreStop)
	}
	a.warnUsers(context.Background(), i18n.Tf("spawn.agent.pre_stop_running", map[string]interface{}{
		"Timeout": timeout,
	}))
	a.notifier.Notify(context.Background(), "pre_stop_start", a.config.PreStop)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Tee the hook's output to the local console AND a bounded tail buffer, so a
	// failure/timeout notification can carry the last few lines of stderr (e.g.
	// "fatal error: Unable to locate credentials", or an `aws s3 sync` summary) —
	// otherwise a silently-failed flush looks identical to a successful one (#186).
	tail := newTailBuffer(2048)
	cmd := sysShellCommand(ctx, a.config.PreStop, runAsUser)
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	if err := cmd.Run(); err != nil {
		// Either branch still proceeds with shutdown — pre-stop is best-effort and
		// must never block the lifecycle action. But make the outcome LOUD: a
		// terminal-state notification so a partial/no-op flush isn't mistaken for
		// success (the #184 data-loss shape).
		detail := tail.String()
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("Pre-stop hook timed out after %v — proceeding with shutdown", timeout)
			a.warnUsers(context.Background(), fmt.Sprintf("⚠️ Pre-stop task timed out after %v and was killed — output may be incomplete.", timeout))
			a.notifier.Notify(context.Background(), "pre_stop_timeout", preStopDetail(timeout.String(), detail))
		} else {
			log.Printf("Pre-stop hook exited with error: %v — proceeding with shutdown", err)
			a.warnUsers(context.Background(), fmt.Sprintf("⚠️ Pre-stop task failed (%v) — output may not have been saved.", err))
			a.notifier.Notify(context.Background(), "pre_stop_failed", preStopDetail(err.Error(), detail))
		}
		return
	}

	log.Printf("Pre-stop hook completed successfully")
}

// spotPreStopTimeout is the user hook's default bound on a Spot notice.
const spotPreStopTimeout = 90 * time.Second

// preStopTimeout is how long the user's pre-stop hook may run: the configured
// timeout, or 5 minutes, or on a Spot notice spotPreStopTimeout, and on a Spot
// notice never past spotDeadline (#1311). ok is false when that deadline
// leaves less than a second, and the hook is then not started.
func preStopTimeout(configured time.Duration, spotDeadline, now time.Time) (timeout time.Duration, ok bool) {
	timeout = 5 * time.Minute
	if configured > 0 {
		timeout = configured
	} else if !spotDeadline.IsZero() {
		timeout = spotPreStopTimeout
	}
	if spotDeadline.IsZero() {
		return timeout, true
	}
	left := spotDeadline.Sub(now).Truncate(time.Second)
	if left < time.Second {
		return 0, false
	}
	return min(timeout, left), true
}

// tailBuffer is a thread-safe io.Writer that retains only the last `max` bytes
// written to it (a ring of sorts) — used to capture the tail of a pre-stop
// hook's output for a failure notification without unbounded memory if the hook
// is chatty. exec.Cmd writes stdout/stderr from separate goroutines, so the
// mutex is required.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// preStopDetail builds the notification detail for a failed/timed-out pre-stop:
// the summary (timeout or error) plus a trimmed tail of the hook's output, so
// the user can see WHY the flush didn't complete without digging into logs.
func preStopDetail(summary, outputTail string) string {
	outputTail = strings.TrimSpace(outputTail)
	if outputTail == "" {
		return summary
	}
	return summary + " — " + outputTail
}

// terminate runs the pre-stop hook and cleanup, then asks the provider to
// terminate the instance, returning the provider's error.
func (a *Agent) terminate(ctx context.Context, reason string, why taskproto.ExitReason) error {
	log.Printf("Terminating instance (reason: %s)", reason)

	a.runPreStop(time.Time{}, why)

	// Clean up DNS before terminating
	a.Cleanup(ctx)

	a.warnUsers(ctx, i18n.Tf("spawn.agent.terminating", map[string]interface{}{
		"Reason": reason,
	}))

	// Wait a moment for users to see warning
	time.Sleep(a.shutdownWarnDelay())

	// Last-instance check: if no other spawn-managed instances remain running in
	// this region, notify that the region is vacated (#260). Notify-only by
	// default; auto-cleanup is a separate opt-in handled control-plane side.
	a.checkRegionVacated(ctx)

	err := a.provider.Terminate(ctx, reason)
	if err != nil {
		log.Printf("Failed to terminate: %v", err)
	}
	return err
}

// checkRegionVacated fires a region_vacated notification when this is the last
// spawn-managed instance running in the region. It re-checks after 60s to avoid
// a false alarm during a rapid relaunch (e.g. a job array rolling). Best-effort:
// an unknown count (-1) is treated as "not vacated" so we never cry wolf (#260).
func (a *Agent) checkRegionVacated(ctx context.Context) {
	if n := a.provider.CountOtherManagedInstances(ctx); n != 0 {
		return // others remain, or the count is unknown (-1)
	}

	// Confirm the region is still empty after a short settle window — a rapid
	// relaunch (another instance coming up) means it isn't really vacated.
	settle := a.regionVacatedSettle
	if settle == 0 {
		settle = 60 * time.Second
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(settle):
	}
	if n := a.provider.CountOtherManagedInstances(ctx); n != 0 {
		log.Printf("region-vacated: re-check found %d other instance(s); not vacated", n)
		return
	}

	region := a.identity.Region
	log.Printf("region-vacated: no spawn-managed instances remain in %s", region)
	a.notifier.Notify(ctx, "region_vacated", region)
}

// Reload re-reads configuration from provider without restarting the daemon.
// Returns an error when the tag refresh itself failed (spawn#505) — callers
// (handleReload, and spawn extend's "reloaded on instance" message) must not
// report success when the daemon's config could not actually be refreshed;
// a discarded refresh error previously let both report success while the
// daemon kept running on stale (possibly zero-value) config indefinitely.
func (a *Agent) Reload(ctx context.Context) error {
	log.Printf("Reloading configuration...")

	// Refresh the provider's cached config from EC2 tags first.
	if err := a.provider.RefreshConfig(ctx); err != nil {
		return fmt.Errorf("failed to refresh config from tags: %w", err)
	}

	// Re-read config from provider
	newConfig, err := a.provider.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// Log changes. Read and swap under configMu: the daemon's monitor loop
	// runs this while the metrics scrape and other goroutines read the config
	// (#175, #1278).
	old := a.cfg()
	if newConfig.TTL != old.TTL {
		log.Printf("TTL changed: %v → %v", old.TTL, newConfig.TTL)
	}
	// TTLDeadline is the field enforcement actually reads (see the remaining-
	// time calculation below in monitorLoop) — log it separately from
	// spawn:ttl so a reload where the tag changed but the deadline did not
	// (the exact spawn#553 symptom, before that command was fixed to write
	// both together) is visible in the daemon's own log, not just inferred
	// from its absence.
	if !newConfig.TTLDeadline.Equal(old.TTLDeadline) {
		log.Printf("TTL deadline changed: %v → %v", old.TTLDeadline, newConfig.TTLDeadline)
	}
	if newConfig.IdleTimeout != old.IdleTimeout {
		log.Printf("Idle timeout changed: %v → %v", old.IdleTimeout, newConfig.IdleTimeout)
	}
	if newConfig.OnComplete != old.OnComplete {
		log.Printf("On-complete changed: %s → %s", old.OnComplete, newConfig.OnComplete)
	}
	if newConfig.HibernateOnIdle != old.HibernateOnIdle {
		log.Printf("Hibernate-on-idle changed: %v → %v", old.HibernateOnIdle, newConfig.HibernateOnIdle)
	}
	if newConfig.TerminateOnIdle != old.TerminateOnIdle {
		log.Printf("Terminate-on-idle changed: %v → %v", old.TerminateOnIdle, newConfig.TerminateOnIdle)
	}

	// Update config (but keep startTime - TTL is absolute)
	a.setConfig(newConfig)

	log.Printf("Configuration reloaded successfully")
	log.Printf("New config: TTL=%v, IdleTimeout=%v, OnComplete=%s, Hibernate=%v",
		newConfig.TTL, newConfig.IdleTimeout, newConfig.OnComplete, newConfig.HibernateOnIdle)

	return nil
}

// Public getter methods for status reporting

// GetPluginRuntime returns the agent's plugin runtime (always non-nil).
func (a *Agent) GetPluginRuntime() *pluginruntime.Runtime { return a.pluginRuntime }

// GetConfig returns the current config. The metrics scrape calls it from the
// HTTP goroutine while the monitor loop swaps the config, so it reads under
// configMu like every other off-loop reader (#175, #952).
func (a *Agent) GetConfig() *provider.Config {
	return a.cfg()
}

func (a *Agent) GetIdentity() *provider.Identity {
	return a.identity
}

func (a *Agent) GetInstanceInfo() (string, string, string) {
	return a.identity.InstanceID, a.identity.Region, a.identity.AccountID
}

func (a *Agent) GetUptime() time.Duration {
	return time.Since(a.startTime)
}

// EvaluateIdle samples every idle signal and decides, as one monitor tick
// does, and returns the decision. It is for a caller that owns its Agent and
// runs no monitor loop — the one-shot `spored status` — and must never be
// called while Monitor is running, because it takes samples (#952).
func (a *Agent) EvaluateIdle(ctx context.Context) bool {
	return a.evaluateIdle(ctx).idle
}

// The readers below return the last evaluation (a monitor tick, or
// EvaluateIdle) and take no sample, so they are safe from any goroutine and
// cannot shorten the monitor loop's measurement window (#952).

// GetCPUUsageMeasured returns the last CPU utilisation reading and whether it
// is a real measurement. A display must use the flag, so a "could not tell" is
// not printed as a reading (#734).
func (a *Agent) GetCPUUsageMeasured() (float64, bool) {
	s := a.idleSnapshotNow()
	if s.taken.IsZero() {
		return 100.0, false
	}
	return s.cpu, s.cpuMeasured
}

// GetNetworkBytesMeasured returns the network bytes over the last sample
// interval, whether that is a real measurement, and if not, why (#919).
func (a *Agent) GetNetworkBytesMeasured() (int64, bool, string) {
	s := a.idleSnapshotNow()
	if s.taken.IsZero() {
		return networkAssumedActive, false, noSampleYet
	}
	return s.net, s.netMeasured, s.netWhy
}

// GetDiskIOBytesMeasured returns disk bytes read and written over the last
// sample interval, whether that is a real measurement, and if not, why (#937).
func (a *Agent) GetDiskIOBytesMeasured() (int64, bool, string) {
	s := a.idleSnapshotNow()
	if s.taken.IsZero() {
		return diskAssumedActive, false, noSampleYet
	}
	return s.disk, s.diskMeasured, s.diskWhy
}

// IsIdle returns the last idle decision; false before the first one.
func (a *Agent) IsIdle() bool {
	return a.idleSnapshotNow().idle
}

func (a *Agent) GetLastActivityTime() time.Time {
	return a.lastActivity()
}
