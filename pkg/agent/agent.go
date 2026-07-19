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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/pluginruntime"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/registry"
	"github.com/spore-host/spawn/pkg/security"
)

type Agent struct {
	provider provider.Provider
	identity *provider.Identity
	// config is replaced wholesale by the monitor loop's periodic tag refresh
	// while other goroutines (FSx mount, spot monitor) read it concurrently, so
	// access goes through cfg()/setConfig() under configMu (#175). Don't read the
	// field directly from code that can run off the monitor goroutine.
	config              *provider.Config
	configMu            sync.RWMutex
	dnsClient           *dns.Client
	dnsDomain           string // DNS domain (e.g. "spore.host" or "prismcloud.host")
	registry            *registry.PeerRegistry
	pluginRuntime       *pluginruntime.Runtime
	notifier            *Notifier // Slack lifecycle notifications (nil if not configured)
	startTime           time.Time
	lastActivityTime    time.Time
	preStopDone         bool      // guards against running pre-stop hook more than once
	spotWebhookFired    bool      // fire-once guard for the spot-interruption webhook (#228); the spot monitor re-enters every 5s
	prevCPUIdle         int64     // /proc/stat idle jiffies at last getCPUUsage call
	prevCPUTotal        int64     // /proc/stat total jiffies at last getCPUUsage call
	lastSessionTagWrite time.Time // throttle spawn:logged-in-count tag writes
	lastComputeTagWrite time.Time // throttle spawn:compute-seconds tag writes
	computeSecondsBase  int64     // compute-seconds already accumulated before this spored start
	prevNetRx           int64     // /proc/net/dev RX bytes at last getNetworkBytes call
	prevNetTx           int64     // /proc/net/dev TX bytes at last getNetworkBytes call
	idleWarned          bool      // send idle_warning notification only once

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

	// dcv runs the `dcv` CLI shell-outs (list/describe sessions). Defaults to the
	// real exec-based runner; tests inject a fake so the handshake + idle logic is
	// exercisable without a DCV server (spawn#282 phase 3).
	dcv dcvRunner

	// tagger writes the spawn:ready-* tags. Defaults to the real EC2 CreateTags
	// impl; tests inject a fake so the handshake retry/terminal logic is testable
	// without real EC2 (spawn#282 phase 3).
	tagger tagPutter

	// fsxMountStarted guards the async FSx mount (#194/#221) to run at most once.
	// The mount is (re)triggered from the monitor loop whenever spawn:fsx-pending
	// becomes visible — it may not be set at spored startup, because the headless
	// launch path tags the instance AFTER RunInstances (the create + Lustre-port
	// setup add seconds) and EC2 tags are eventually consistent. The original
	// run-once-at-startup invocation lost that race and silently never mounted.
	fsxMountStarted bool

	// monitorInterval is the lifecycle ticker period. Zero means the production
	// default (1 minute); tests set it small to exercise the loop quickly.
	monitorInterval time.Duration

	// configRefreshTick counts monitoring cycles for the periodic tag refresh
	// (tags are re-read every 5 ticks). Per-Agent rather than a package global so
	// it isn't shared across agents and resets cleanly per instance (#175).
	configRefreshTick int

	// regionVacatedSettle is how long the last-instance check waits before
	// re-confirming the region is empty (#260). Zero means the production default
	// (60s); tests set it small.
	regionVacatedSettle time.Duration
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

func NewAgent(ctx context.Context, prov provider.Provider) (*Agent, error) {
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
	agent.tagger = &ec2TagPutter{region: identity.Region} // real EC2 CreateTags; tests override

	log.Printf("Agent initialized for instance %s in %s (account: %s, provider: %s)",
		identity.InstanceID, identity.Region, identity.AccountID, identity.Provider)
	log.Printf("Config: TTL=%v, IdleTimeout=%v, Hibernate=%v",
		config.TTL, config.IdleTimeout, config.HibernateOnIdle)

	// Look up actual EBS volume cost on first start; caches result in the
	// spawn:ebs-hourly-cost tag. Do NOT write agent.config here: this goroutine
	// would race the monitor loop, which both reads agent.config fields and
	// replaces the whole pointer on its periodic tag refresh (#175). The cost is
	// persisted to the tag (and the provider's config) by LookupAndTagEBSCost, so
	// the monitor's next config refresh (≤5 min) picks it up without a shared
	// write from here.
	if identity.Provider == "ec2" && config.EBSHourlyCost == 0 {
		go func() {
			if ebsCost := prov.LookupAndTagEBSCost(context.Background()); ebsCost > 0 {
				log.Printf("EBS hourly cost: $%.4f/hr", ebsCost)
			}
		}()
	}

	// Log DCV idle detection if this is an application streaming instance. The DCV
	// handshake (verifier + session-wait + token + ready-url tag) is driven from
	// the monitor loop via maybeSetupDCVAuth, not once here — so a transient
	// failure recovers (spawn#282 phase 2), mirroring the FSx retry discipline.
	if config.DCVSessionID != "" {
		log.Printf("DCV idle detection enabled for session %s", config.DCVSessionID)
	}

	// Initialize lifecycle notifier (Slack notifications via spore-bot Lambda)
	if config.NotifyURL != "" {
		agent.notifier = NewNotifier(config, identity)
		log.Printf("Slack lifecycle notifications enabled for workspace %s", config.SlackWorkspaceID)
	}

	// Initialize DNS client and register if DNS name is configured
	// Skip DNS for local provider (Phase 1 decision)
	dnsDomain := os.Getenv("SPORED_DNS_DOMAIN")
	if dnsDomain == "" {
		dnsDomain = "spore.host"
	}
	agent.dnsDomain = dnsDomain

	if config.DNSName != "" && identity.PublicIP != "" && identity.Provider == "ec2" {
		dnsClient, err := dns.NewClient(ctx, dnsDomain, "")
		if err != nil {
			log.Printf("Warning: Failed to create DNS client: %v", err)
		} else {
			agent.dnsClient = dnsClient

			// Pass the friendly account-name slug so the updater also registers
			// the alias FQDN {name}.{account-name}.spore.host (#121). Empty when
			// the account has no name — base36 only, unchanged.
			dnsClient.SetAccountName(config.AccountName)

			// Register DNS (use job array method if part of a job array)
			if config.JobArrayID != "" && config.JobArrayName != "" {
				log.Printf("Registering job array DNS: %s -> %s (array: %s)",
					config.DNSName, identity.PublicIP, config.JobArrayName)
				resp, err := dnsClient.RegisterJobArrayDNS(ctx, config.DNSName, identity.PublicIP,
					config.JobArrayID, config.JobArrayName)
				if err != nil {
					log.Printf("Warning: Failed to register job array DNS: %v", err)
				} else {
					fqdn := dns.GetFullDNSName(config.DNSName, identity.AccountID, dnsDomain)
					log.Printf("✓ Job array DNS registered: %s -> %s (change: %s)", fqdn, identity.PublicIP, resp.ChangeID)
					if resp.Message != "" {
						log.Printf("  %s", resp.Message)
					}
				}
			} else {
				log.Printf("Registering DNS: %s -> %s", config.DNSName, identity.PublicIP)
				resp, err := dnsClient.RegisterDNS(ctx, config.DNSName, identity.PublicIP)
				if err != nil {
					log.Printf("Warning: Failed to register DNS: %v", err)
				} else {
					fqdn := dns.GetFullDNSName(config.DNSName, identity.AccountID, dnsDomain)
					log.Printf("✓ DNS registered: %s -> %s (change: %s)", fqdn, identity.PublicIP, resp.ChangeID)
				}
			}
		}
	} else if config.DNSName != "" && identity.Provider == "local" {
		log.Printf("DNS registration skipped for local provider")
	} else if config.DNSName != "" {
		log.Printf("Warning: DNS name configured (%s) but no public IP available", config.DNSName)
	}

	// Initialize hybrid registry if part of a job array
	if config.JobArrayID != "" {
		reg, err := registry.NewPeerRegistry(ctx, identity)
		if err != nil {
			log.Printf("Warning: Failed to initialize registry: %v (continuing without hybrid mode)", err)
		} else {
			agent.registry = reg

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

		// Discover peers
		peers, err := prov.DiscoverPeers(ctx, config.JobArrayID)
		if err != nil {
			log.Printf("Warning: Failed to discover peers: %v", err)
		} else if len(peers) > 0 {
			log.Printf("✓ Discovered %d peers in job array %s", len(peers), config.JobArrayID)
		}
	}

	// Initialize plugin runtime (always, so the push API can route to it). Pass
	// the local login user so plugin steps that opt into as_user (e.g. Globus
	// Connect Personal, which refuses to run as root) run as that user.
	rt := pluginruntime.NewRuntime(identity, config.LocalUsername)
	agent.pluginRuntime = rt

	if len(config.Plugins) > 0 {
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

func (a *Agent) Monitor(ctx context.Context) {
	interval := a.monitorInterval
	if interval <= 0 {
		interval = 1 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("Monitoring started")

	// Spot-interruption monitoring runs in its own goroutine and must NEVER gate
	// the main lifecycle ticker below. Spot detection (IsSpotInstance) makes a
	// blocking IMDS call; if that ever stalls, gating the ticker on it silently
	// kills TTL/idle/on-complete/pre-stop enforcement — i.e. instances run
	// forever (#65). So we start the lifecycle loop unconditionally and do the
	// spot decision off the critical path.
	go a.monitorSpotInterruptions(ctx)

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

	// 0. Periodically refresh config from EC2 tags (every 5 ticks = 5 min).
	// EC2 tag API has eventual consistency — tags set at launch may not be
	// visible within the seconds before spored starts. Refreshing here ensures
	// on-complete, pre-stop, and other lifecycle tags are eventually picked up.
	a.configRefreshTick++
	if a.configRefreshTick == 1 || a.configRefreshTick%5 == 0 {
		if err := a.provider.RefreshConfig(ctx); err != nil {
			log.Printf("Warning: failed to refresh config from tags: %v", err)
		} else {
			if fresh, err2 := a.provider.GetConfig(ctx); err2 == nil && fresh != nil {
				a.setConfig(fresh)
			}
		}
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

	// 0a. Keep spawn:logged-in-count tag current (throttled to 5/min).
	a.writeSessionCountTag(ctx, countActiveSessions()+countActivePortConnections(a.config.ActivePorts))

	// 0b. Keep spawn:compute-seconds tag current (throttled to 5/min).
	a.writeComputeSecondsTag(ctx)

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
			a.terminate(ctx, "TTL expired")
			return
		}

		// Warn once when 5 minutes remain before TTL
		if remaining <= 5*time.Minute && !a.ttlWarned {
			a.ttlWarned = true
			a.warnUsers(i18n.Tf("spawn.agent.ttl_warning", map[string]interface{}{
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
			a.terminate(ctx, fmt.Sprintf("cost limit reached ($%.2f)", a.config.CostLimit))
			return
		}

		// Warn when 90%+ of budget consumed
		if accumulated/a.config.CostLimit >= 0.90 {
			a.warnUsers(i18n.Tf("spawn.agent.cost_limit_warning", map[string]interface{}{
				"Accumulated": fmt.Sprintf("%.4f", accumulated),
				"Limit":       fmt.Sprintf("%.2f", a.config.CostLimit),
				"Percentage":  fmt.Sprintf("%.0f", (accumulated/a.config.CostLimit)*100),
			}))
		}
	}

	// 4. Check idle
	if a.config.IdleTimeout > 0 {
		idle := a.isIdle()
		if idle {
			idleTime := time.Since(a.lastActivityTime)

			if idleTime >= a.config.IdleTimeout {
				log.Printf("Idle timeout reached (%v)", idleTime)

				// Send event name that reflects the actual action.
				// Default: stop the instance (compute billing pauses, instance preserved).
				// --hibernate-on-idle: hibernate instead (RAM saved to disk).
				// Only TTL causes termination — idle timeout never destroys data.
				if a.config.HibernateOnIdle {
					a.notifier.Notify(ctx, "idle_hibernated", "")
					a.hibernate(ctx)
				} else {
					a.notifier.Notify(ctx, "idle_stopped", "")
					a.stop(ctx, "Idle timeout")
				}
				return
			}

			// Warn once when 5 minutes remain before idle timeout
			remaining := a.config.IdleTimeout - idleTime
			if remaining > 0 && remaining <= 5*time.Minute && !a.idleWarned {
				a.idleWarned = true
				a.warnUsers(i18n.Tf("spawn.agent.idle_warning", map[string]interface{}{
					"IdleDuration": idleTime.Round(time.Minute),
					"Remaining":    remaining.Round(time.Minute),
				}))
				a.notifier.Notify(ctx, "idle_warning", remaining.Round(time.Minute).String())
			}
		} else {
			// Activity detected — reset idle timer and re-arm the warning
			a.lastActivityTime = time.Now()
			a.idleWarned = false
		}
	}
}

// Sentinels for the OS CPU-times reader (sysReadCPUTimes).
var (
	errEmptyProcStat = fmt.Errorf("empty cpu stat")
	errBadProcStat   = fmt.Errorf("unexpected cpu stat format")
)

// countActivePortConnections counts ESTABLISHED connections on the given ports —
// detects browser-based app users (RStudio, Jupyter) that don't appear in the
// session list because they connect via HTTP, not SSH. OS-specific source.
func countActivePortConnections(ports []int) int {
	return sysCountActivePortConnections(ports)
}

// countActiveSessions returns the number of login sessions with recent keyboard
// activity (input within the last 5 minutes), so idle SSH/RDP connections don't
// block idle detection. OS-specific source.
func countActiveSessions() int {
	return sysCountActiveSessions()
}

// isRecentActivity returns true if the idle duration string from `w` is less than maxIdle.
func isRecentActivity(idle string, maxIdle time.Duration) bool {
	// "?" means no tty activity measured — treat as not active
	if idle == "?" {
		return false
	}
	// Seconds only: "Xs"
	if strings.HasSuffix(idle, "s") {
		secs, err := strconv.ParseFloat(strings.TrimSuffix(idle, "s"), 64)
		if err == nil {
			return time.Duration(secs)*time.Second < maxIdle
		}
	}
	// Minutes:seconds "MM:SS"
	if strings.Contains(idle, ":") && !strings.Contains(idle, "m") {
		parts := strings.Split(idle, ":")
		if len(parts) == 2 {
			mins, e1 := strconv.ParseFloat(parts[0], 64)
			secs, e2 := strconv.ParseFloat(parts[1], 64)
			if e1 == nil && e2 == nil {
				d := time.Duration(mins)*time.Minute + time.Duration(secs)*time.Second
				return d < maxIdle
			}
		}
	}
	// Minutes with suffix "MMm" or "HH:MMm"
	if strings.HasSuffix(idle, "m") {
		s := strings.TrimSuffix(idle, "m")
		if strings.Contains(s, ":") {
			parts := strings.Split(s, ":")
			if len(parts) == 2 {
				hrs, e1 := strconv.ParseFloat(parts[0], 64)
				mins, e2 := strconv.ParseFloat(parts[1], 64)
				if e1 == nil && e2 == nil {
					d := time.Duration(hrs)*time.Hour + time.Duration(mins)*time.Minute
					return d < maxIdle
				}
			}
		}
		mins, err := strconv.ParseFloat(s, 64)
		if err == nil {
			return time.Duration(mins)*time.Minute < maxIdle
		}
	}
	// "Xdays" — definitely not recent
	if strings.Contains(idle, "day") {
		return false
	}
	// Unknown format — conservatively treat as recent to avoid false idle
	return true
}

// findActiveProcess returns the first configured process name that is currently
// running, or "" if none are found. OS-specific source.
func (a *Agent) findActiveProcess() string {
	for _, name := range a.config.ActiveProcesses {
		if sysIsProcessRunning(name) {
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

// writeComputeSecondsTag persists the total compute seconds (base + current uptime) to an EC2 tag.
// Throttle: every 1 minute for the first 10 minutes (fast feedback on fresh instances),
// then every 5 minutes thereafter.
func (a *Agent) writeComputeSecondsTag(ctx context.Context) {
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

// flushComputeSecondsTag writes the spawn:compute-seconds tag NOW, bypassing the
// throttle in writeComputeSecondsTag. It is called on graceful shutdown (see
// Cleanup) so an in-place spored upgrade (`spawn upgrade-spored`, #234) — which
// stops the daemon to swap the binary — doesn't discard the up-to-5-minutes of
// compute time accumulated since the last throttled write. The next spored boot
// reads this tag into computeSecondsBase, so the compute clock continues across
// the restart rather than losing the tail.
// accumulatedComputeCost is the compute-only spend the cost limit is enforced
// against: the on-demand rate × TOTAL compute time (base + this boot's uptime).
// Using the total — not just this boot — is what stops the cost clock resetting
// on a stop/start, mirroring how TTL uses an absolute deadline. EBS/storage is
// deliberately excluded (the --cost-limit flag is documented as compute-only).
func (a *Agent) accumulatedComputeCost() float64 {
	totalCompute := time.Duration(a.TotalComputeSeconds()) * time.Second
	return a.config.PricePerHour * totalCompute.Hours()
}

func (a *Agent) flushComputeSecondsTag(ctx context.Context) {
	a.lastComputeTagWrite = time.Now()
	total := a.TotalComputeSeconds()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.identity.Region))
	if err != nil {
		return
	}
	client := ec2.NewFromConfig(cfg)
	_, _ = client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{a.identity.InstanceID},
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:compute-seconds"), Value: aws.String(strconv.FormatInt(total, 10))},
		},
	})
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

// TotalComputeSeconds returns accumulated compute time across all start/stop cycles.
func (a *Agent) TotalComputeSeconds() int64 {
	return a.computeSecondsBase + int64(time.Since(a.startTime).Seconds())
}

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

// installDCVCert downloads the wildcard TLS cert for this account from S3 and
// configures DCV to use it, then restarts dcvserver. Fails gracefully — if the
// cert is not in S3, DCV continues with its self-signed cert.
func (a *Agent) installDCVCert(ctx context.Context) {
	region := a.identity.Region
	accountBase36 := dns.EncodeAccountID(a.identity.AccountID)
	bucket := "spawn-certs-" + region

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		log.Printf("DCV cert: skipping — cannot load AWS config: %v", err)
		return
	}
	s3c := s3.NewFromConfig(cfg)

	if err := s3GetFile(ctx, s3c, bucket, accountBase36+"/cert.pem", "/etc/dcv/dcv.pem", 0644); err != nil {
		log.Printf("DCV cert: cert not found in S3 (%v) — using self-signed", err)
		return
	}
	if err := s3GetFile(ctx, s3c, bucket, accountBase36+"/key.pem", "/etc/dcv/dcv.key", 0600); err != nil {
		log.Printf("DCV cert: key download failed (%v) — reverting", err)
		os.Remove("/etc/dcv/dcv.pem")
		return
	}

	// Inject TLS directives into the [security] section of dcv.conf (idempotent).
	// Must be inside [security] — appending at end of file won't work.
	if dcvConf, err := os.ReadFile("/etc/dcv/dcv.conf"); err == nil && !bytes.Contains(dcvConf, []byte("tls-certificate")) {
		const tlsLines = "tls-certificate=/etc/dcv/dcv.pem\ntls-private-key=/etc/dcv/dcv.key\n"
		updated := bytes.Replace(dcvConf,
			[]byte("[security]"),
			[]byte("[security]\n"+tlsLines),
			1)
		if !bytes.Equal(updated, dcvConf) {
			_ = os.WriteFile("/etc/dcv/dcv.conf", updated, 0644)
		}
	}

	if out, err := exec.CommandContext(ctx, "systemctl", "restart", "dcvserver").CombinedOutput(); err != nil {
		log.Printf("DCV cert: dcvserver restart failed: %v\n%s", err, out)
		return
	}
	time.Sleep(3 * time.Second)
	log.Printf("DCV cert: installed wildcard cert for *.%s.spore.host", accountBase36)
}

// s3GetFile downloads an S3 object and writes it to destPath with the given mode.
func s3GetFile(ctx context.Context, client *s3.Client, bucket, key, destPath string, mode os.FileMode) error {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, data, mode)
}

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
	host := a.identity.PublicIP
	if a.config.DNSName != "" && a.dnsDomain != "" {
		host = dns.GetFullDNSName(a.config.DNSName, a.identity.AccountID, a.dnsDomain)
	}
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
		k, v := k, v
		ec2Tags = append(ec2Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err = client.CreateTags(ctx, &ec2.CreateTagsInput{
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

func (a *Agent) isIdle() bool {
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
			connCount = a.getDCVConnectionCount()
		}
		idle, fallThrough := dcvIdleDecision(fileExists, activityAge, connCount,
			time.Since(a.startTime), dcvGraceTimeout, a.config.IdleTimeout)
		if !fallThrough {
			if idle {
				log.Printf("DCV session %s: idle (file=%v age=%v conns=%d)",
					a.config.DCVSessionID, fileExists, activityAge.Round(time.Second), connCount)
			} else {
				log.Printf("Not idle: DCV session %s (file=%v age=%v conns=%d)",
					a.config.DCVSessionID, fileExists, activityAge.Round(time.Second), connCount)
			}
			return idle
		}
		log.Printf("DCV session %s never became ready within %v — falling back to standard idle checks",
			a.config.DCVSessionID, dcvGraceTimeout)
		// fall through to the standard checks below
	}

	// Active SSH/terminal sessions reset the idle timer.
	if sessions := countActiveSessions(); sessions > 0 {
		log.Printf("Not idle: %d active session(s)", sessions)
		return false
	}

	// Check configured process names — if any are running, instance is not idle.
	if proc := a.findActiveProcess(); proc != "" {
		log.Printf("Not idle: process %q is running", proc)
		return false
	}

	// Note: we intentionally do NOT check active port connections here.
	// An open browser tab maintains an ESTABLISHED TCP connection even when
	// the user is idle or away — treating it as "active" would permanently
	// block idle termination for abandoned tabs. The existing CPU and network
	// delta checks correctly distinguish real activity from idle keep-alives.

	// Check CPU usage
	cpuUsage := a.getCPUUsage()
	if cpuUsage >= a.config.IdleCPUPercent {
		log.Printf("Not idle: CPU usage %.2f%% >= %.2f%%", cpuUsage, a.config.IdleCPUPercent)
		return false
	}

	// Check network traffic
	networkBytes := a.getNetworkBytes()
	if networkBytes > 100000 { // 100KB/min threshold — filters out spored's own EC2/IMDS API calls (~25KB/min)
		log.Printf("Not idle: Network traffic %d bytes", networkBytes)
		return false
	}

	// Check disk I/O
	diskIO := a.getDiskIO()
	if diskIO > 100000 { // 100KB/min threshold
		log.Printf("Not idle: Disk I/O %d bytes", diskIO)
		return false
	}

	// Check GPU utilization
	gpuUtilization := a.getGPUUtilization()
	if gpuUtilization > 5 { // 5% GPU usage threshold
		log.Printf("Not idle: GPU utilization %.2f%%", gpuUtilization)
		return false
	}

	// Check for active terminals
	if a.hasActiveTerminals() {
		log.Printf("Not idle: Active terminals present")
		return false
	}

	// Check for logged-in users
	if a.hasLoggedInUsers() {
		log.Printf("Not idle: Users logged in")
		return false
	}

	// Check for recent user activity
	if a.hasRecentUserActivity() {
		log.Printf("Not idle: Recent user activity detected")
		return false
	}

	log.Printf("System is idle (CPU: %.2f%%, Network: %d bytes, Disk: %d bytes, GPU: %.2f%%)",
		cpuUsage, networkBytes, diskIO, gpuUtilization)
	return true
}

func (a *Agent) getCPUUsage() float64 {
	idle, total, err := sysReadCPUTimes()
	if err != nil {
		return 100.0 // Assume active if can't read
	}

	// Delta CPU usage since last call — avoids cumulative-since-boot bias
	// that makes a freshly-booted instance look busy for its entire uptime.
	prevIdle, prevTotal := a.prevCPUIdle, a.prevCPUTotal
	a.prevCPUIdle, a.prevCPUTotal = idle, total

	if prevTotal == 0 {
		return 100.0 // First call; no delta available; assume active
	}
	deltaIdle := idle - prevIdle
	deltaTotal := total - prevTotal
	if deltaTotal == 0 {
		return 0.0
	}
	return 100.0 - (float64(deltaIdle)/float64(deltaTotal))*100.0
}

func (a *Agent) getNetworkBytes() int64 {
	// OS-specific cumulative counters; compute delta since last call rather than
	// cumulative-since-boot. A negative reading means "unknown" → assume active.
	rx, tx := sysReadNetworkBytes()
	if rx < 0 || tx < 0 {
		return 1000000 // can't read; assume active
	}
	prevRx, prevTx := a.prevNetRx, a.prevNetTx
	a.prevNetRx, a.prevNetTx = rx, tx
	if prevRx == 0 && prevTx == 0 {
		return 1000000 // First call — no delta; assume active
	}
	return (rx - prevRx) + (tx - prevTx)
}

func (a *Agent) getDiskIO() int64 {
	return sysReadDiskIOBytes()
}

func (a *Agent) getGPUUtilization() float64 {
	// Check if nvidia-smi is available
	_, err := exec.LookPath("nvidia-smi")
	if err != nil {
		// No GPU or nvidia-smi not installed
		return 0
	}

	// Query GPU utilization
	// nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits
	cmd := exec.Command("nvidia-smi", "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	// Parse output (can have multiple GPUs, one per line)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var maxUtilization float64

	for _, line := range lines {
		utilization, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err == nil && utilization > maxUtilization {
			maxUtilization = utilization
		}
	}

	return maxUtilization
}

// getDCVConnectionCount returns the DCV session's connected-client count, or -1
// when DCV isn't ready (binary missing, server down, or unparseable output) — the
// caller's "unknown" signal. The parsing is in parseDCVConnections (pure/tested);
// the shell-out goes through the dcvRunner seam so it's testable (#282).
func (a *Agent) getDCVConnectionCount() int {
	out, err := a.dcv.describeSession(context.Background(), a.config.DCVSessionID)
	if err != nil {
		return -1 // DCV not ready or session not found
	}
	n, err := parseDCVConnections(out)
	if err != nil {
		return -1
	}
	return n
}

func (a *Agent) hasLoggedInUsers() bool {
	// Delegate to countActiveSessions which checks for recent keyboard activity, not just presence
	return countActiveSessions() > 0
}

func (a *Agent) hasRecentUserActivity() bool {
	return sysHasRecentUserActivity()
}

func (a *Agent) hasActiveTerminals() bool {
	return sysHasActiveTerminals()
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
	a.warnUsers(i18n.T("spawn.agent.spot_interruption.title") + "\n" +
		i18n.Tf("spawn.agent.spot_interruption.message", map[string]interface{}{
			"Action": info.Action,
			"Time":   info.Time.Format("15:04:05"),
		}))

	// Run pre-stop hook with shortened timeout (stay within the 2-min window)
	a.runPreStop(true)

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

	// Log for posterity
	log.Printf("Spot interruption: action=%s, time=%s", info.Action, info.Time.Format(time.RFC3339))

	// Continue monitoring for remaining time
	return false // Return false to allow normal monitoring to continue
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
		LastActivityTime:     a.lastActivityTime.UTC().Format(time.RFC3339),
		Correlation:          cfg.WebhookCorrelation,
		EmittedAt:            time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Spot webhook: marshal failed, dropping: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.SpotWebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("Spot webhook: request build failed, dropping: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Spot webhook: POST to %s failed, dropping (best-effort): %v", cfg.SpotWebhookURL, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("Spot webhook: endpoint %s returned %d, dropping (best-effort)", cfg.SpotWebhookURL, resp.StatusCode)
		return
	}
	log.Printf("Spot webhook: notice POSTed to %s (action=%s)", cfg.SpotWebhookURL, info.Action)
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

func (a *Agent) warnUsers(message string) {
	// Broadcast to logged-in sessions (OS-specific: wall on Linux, msg on Windows).
	sysWarnUsers(message)

	// Also write a warning file in the OS temp dir for external pickup.
	_ = os.WriteFile(filepath.Join(os.TempDir(), "SPAWN_WARNING"), []byte(message+"\n"), 0600) // nosemgrep: go.lang.security.bad_tmp.bad-tmp-file-creation

	log.Printf("Warning sent to users: %s", message)
}

func (a *Agent) checkCompletion(ctx context.Context) bool {
	// Check if completion file exists
	if _, err := os.Stat(a.config.CompletionFile); err == nil {
		log.Printf("Completion signal detected: file %s exists", a.config.CompletionFile)

		// Read completion file for metadata (optional)
		content, err := os.ReadFile(a.config.CompletionFile)
		if err == nil && len(content) > 0 {
			log.Printf("Completion metadata: %s", strings.TrimSpace(string(content)))
		}

		// Notify via Slack before the grace period
		a.notifier.Notify(ctx, "completion", "")

		// Warn users with grace period
		delay := a.config.CompletionDelay
		a.warnUsers(i18n.Tf("spawn.agent.workload_complete", map[string]interface{}{
			"Action": a.config.OnComplete,
			"Delay":  delay,
		}))

		log.Printf("Grace period: waiting %v before action", delay)
		time.Sleep(delay)

		// Execute action based on configuration
		switch strings.ToLower(a.config.OnComplete) {
		case "terminate":
			a.terminate(ctx, "Completion signal received")
		case "stop":
			a.stop(ctx, "Completion signal received")
		case "hibernate":
			a.hibernate(ctx)
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

func (a *Agent) stop(ctx context.Context, reason string) {
	log.Printf("Stopping instance (reason: %s)", reason)

	a.runPreStop(false)

	// Clean up DNS before stopping
	a.Cleanup(ctx)

	a.warnUsers(i18n.Tf("spawn.agent.stopping", map[string]interface{}{
		"Reason": reason,
	}))

	// Wait a moment for users to see warning
	time.Sleep(5 * time.Second)

	err := a.provider.Stop(ctx, reason)
	if err != nil {
		log.Printf("Failed to stop instance: %v", err)
	}
}

func (a *Agent) hibernate(ctx context.Context) {
	log.Printf("Hibernating instance")

	a.runPreStop(false)

	// Clean up DNS before hibernating
	a.Cleanup(ctx)

	a.warnUsers(i18n.T("spawn.agent.hibernating"))

	// Wait a moment for users to see warning
	time.Sleep(5 * time.Second)

	err := a.provider.Hibernate(ctx)
	if err != nil {
		log.Printf("Failed to hibernate: %v", err)
	}
}

// Cleanup performs cleanup tasks before shutdown (plugins, DNS, registry).
func (a *Agent) Cleanup(ctx context.Context) {
	log.Printf("Running cleanup tasks...")

	// Flush the compute-seconds tag before we stop, bypassing the periodic
	// throttle. Without this, a graceful stop (including an in-place spored
	// upgrade, #234) would discard up to ~5 minutes of compute time since the
	// last throttled write; the next boot reads the tag into computeSecondsBase,
	// so flushing here keeps the compute clock continuous across the restart.
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
	if a.dnsClient != nil && a.config.DNSName != "" && a.identity.PublicIP != "" {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		// Use job array DNS deletion if part of a job array
		if a.config.JobArrayID != "" && a.config.JobArrayName != "" {
			log.Printf("Deleting job array DNS record: %s (array: %s)", a.config.DNSName, a.config.JobArrayName)
			resp, err := a.dnsClient.DeleteJobArrayDNS(cleanupCtx, a.config.DNSName, a.identity.PublicIP,
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
			_, err := a.dnsClient.DeleteDNS(cleanupCtx, a.config.DNSName, a.identity.PublicIP)
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
func (a *Agent) runPreStop(spotMode bool) {
	if a.config.PreStop == "" || a.preStopDone {
		return
	}
	a.preStopDone = true

	timeout := 5 * time.Minute
	if a.config.PreStopTimeout > 0 {
		timeout = a.config.PreStopTimeout
	} else if spotMode {
		timeout = 90 * time.Second // stay within the 2-min spot window
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
	a.warnUsers(i18n.Tf("spawn.agent.pre_stop_running", map[string]interface{}{
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
			a.warnUsers(fmt.Sprintf("⚠️ Pre-stop task timed out after %v and was killed — output may be incomplete.", timeout))
			a.notifier.Notify(context.Background(), "pre_stop_timeout", preStopDetail(timeout.String(), detail))
		} else {
			log.Printf("Pre-stop hook exited with error: %v — proceeding with shutdown", err)
			a.warnUsers(fmt.Sprintf("⚠️ Pre-stop task failed (%v) — output may not have been saved.", err))
			a.notifier.Notify(context.Background(), "pre_stop_failed", preStopDetail(err.Error(), detail))
		}
		return
	}

	log.Printf("Pre-stop hook completed successfully")
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

func (a *Agent) terminate(ctx context.Context, reason string) {
	log.Printf("Terminating instance (reason: %s)", reason)

	a.runPreStop(false)

	// Clean up DNS before terminating
	a.Cleanup(ctx)

	a.warnUsers(i18n.Tf("spawn.agent.terminating", map[string]interface{}{
		"Reason": reason,
	}))

	// Wait a moment for users to see warning
	time.Sleep(5 * time.Second)

	// Last-instance check: if no other spawn-managed instances remain running in
	// this region, notify that the region is vacated (#260). Notify-only by
	// default; auto-cleanup is a separate opt-in handled control-plane side.
	a.checkRegionVacated(ctx)

	err := a.provider.Terminate(ctx, reason)
	if err != nil {
		log.Printf("Failed to terminate: %v", err)
	}
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

// Reload re-reads configuration from provider without restarting the daemon
func (a *Agent) Reload(ctx context.Context) error {
	log.Printf("Reloading configuration...")

	// Refresh the provider's cached config from EC2 tags first
	if err := a.provider.RefreshConfig(ctx); err != nil {
		log.Printf("Warning: failed to refresh config from tags: %v", err)
	}

	// Re-read config from provider
	newConfig, err := a.provider.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// Log changes
	if newConfig.TTL != a.config.TTL {
		log.Printf("TTL changed: %v → %v", a.config.TTL, newConfig.TTL)
	}
	if newConfig.IdleTimeout != a.config.IdleTimeout {
		log.Printf("Idle timeout changed: %v → %v", a.config.IdleTimeout, newConfig.IdleTimeout)
	}
	if newConfig.OnComplete != a.config.OnComplete {
		log.Printf("On-complete changed: %s → %s", a.config.OnComplete, newConfig.OnComplete)
	}
	if newConfig.HibernateOnIdle != a.config.HibernateOnIdle {
		log.Printf("Hibernate-on-idle changed: %v → %v", a.config.HibernateOnIdle, newConfig.HibernateOnIdle)
	}

	// Update config (but keep startTime - TTL is absolute)
	a.config = newConfig

	log.Printf("Configuration reloaded successfully")
	log.Printf("New config: TTL=%v, IdleTimeout=%v, OnComplete=%s, Hibernate=%v",
		newConfig.TTL, newConfig.IdleTimeout, newConfig.OnComplete, newConfig.HibernateOnIdle)

	return nil
}

// Public getter methods for status reporting

// GetPluginRuntime returns the agent's plugin runtime (always non-nil).
func (a *Agent) GetPluginRuntime() *pluginruntime.Runtime { return a.pluginRuntime }

func (a *Agent) GetConfig() *provider.Config {
	return a.config
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

func (a *Agent) GetCPUUsage() float64 {
	return a.getCPUUsage()
}

func (a *Agent) GetNetworkBytes() int64 {
	return a.getNetworkBytes()
}

func (a *Agent) IsIdle() bool {
	return a.isIdle()
}

func (a *Agent) GetLastActivityTime() time.Time {
	return a.lastActivityTime
}

// UX detection methods
func (a *Agent) HasActiveTerminals() bool {
	return a.hasActiveTerminals()
}

func (a *Agent) HasLoggedInUsers() bool {
	return a.hasLoggedInUsers()
}

func (a *Agent) HasRecentUserActivity() bool {
	return a.hasRecentUserActivity()
}
