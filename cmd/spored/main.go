package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/buildinfo"
	"github.com/spore-host/spawn/pkg/observability/metrics"
	"github.com/spore-host/spawn/pkg/observability/tracing"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/pluginruntime"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/tagprefix"
	"github.com/spore-host/spawn/pkg/ttl"
)

// detectLang reads the system locale from environment variables and returns a
// two-letter language code, defaulting to "en".
func detectLang() string {
	for _, env := range []string{"LANG", "LC_ALL", "LC_MESSAGES"} {
		if v := os.Getenv(env); v != "" {
			// "en_US.UTF-8" → "en"
			lang := strings.Split(strings.Split(v, ".")[0], "_")[0]
			if len(lang) == 2 {
				return lang
			}
		}
	}
	return "en"
}

// Version is injected at build time by GoReleaser (-X main.Version={{.Version}}).
// Empty by default, not a number: this said "0.1.0" through 97 releases, and
// every non-release spored — including one hand-built and uploaded to S3 — tagged
// its instance with that. Unset falls back to the build metadata Go stamps in
// (see pkg/buildinfo), which nobody has to remember to bump.
var Version = ""

// version is the resolved version spored reports and tags instances with.
func version() string { return buildinfo.Version(Version) }

// newRootCmd builds the spored command tree. The root command itself, run with
// no subcommand, is the lifecycle daemon (systemd ExecStart=/usr/local/bin/spored).
// Subcommand names, flags, and exit codes are load-bearing: spawn shells out to
// `spored status --check-complete`, `spored config get/set/list`, `spored reload`,
// and `spored complete --status/--message`, and the user-data bootstrap runs the
// bare binary as the daemon — all must keep working unchanged.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "spored",
		Short:         "Spawn EC2 instance agent",
		Long:          "spored monitors an instance's lifecycle (spot interruption, TTL, idle, completion) and is also the on-instance control CLI.",
		Version:       version(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// No subcommand → run as the lifecycle daemon.
		RunE: func(cmd *cobra.Command, args []string) error {
			runDaemon()
			return nil
		},
	}
	root.SetVersionTemplate("spored version {{.Version}}\n")

	root.AddCommand(
		newRunQueueCmd(),
		newPoolWorkerCmd(),
		newRunPipelineStageCmd(),
		newStatusCmd(),
		newReloadCmd(),
		newConfigCmd(),
		newCompleteCmd(),
		// The bootstrap gates the user's workload on this exit code (#1007).
		newIsSpotReplacementCmd(),
		// Explicit `version` subcommand preserves the historical contract
		// (`spored version` → "spored version X"); cobra's --version flag alone
		// would break callers that parse it: `spawn upgrade-spored`
		// (cmd/upgrade_spored.go) and the release version guard
		// (scripts/check-release-version.sh).
		&cobra.Command{
			Use:   "version",
			Short: "Show version",
			Args:  cobra.NoArgs,
			Run: func(cmd *cobra.Command, args []string) {
				fmt.Printf("spored version %s\n", version())
			},
		},
	)
	return root
}

func main() {
	// On Windows, if the Service Control Manager launched us, run under the SCM
	// (svc.Run → daemon) instead of the CLI. No-op on other platforms.
	if runAsServiceIfManaged() {
		return
	}
	root := newRootCmd()
	registerPlatformCommands(root) // adds `service` subcommand on Windows
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// runDaemon is the lifecycle-monitoring daemon (the bare `spored` invocation).
func runDaemon() {
	// Setup logging (platform-specific path: /var/log on Unix, %PROGRAMDATA% on Windows).
	logFile, err := os.OpenFile(sporedLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Warning: Could not open log file: %v", err)
		log.SetOutput(os.Stderr)
	} else {
		defer func() { _ = logFile.Close() }()
		log.SetOutput(logFile)
	}

	log.Printf("spored v%s starting...", version())

	// Initialize tag prefix from SPORED_TAG_PREFIX env var (default: "spawn")
	tagprefix.Init()
	if p := tagprefix.Prefix(); p != "spawn" {
		log.Printf("Using tag prefix: %s", p)
	}

	// Initialize i18n so agent lifecycle warnings are translated
	if err := i18n.Init(i18n.Config{Language: detectLang()}); err != nil {
		log.Printf("Warning: failed to initialize i18n: %v", err)
	}

	// Create agent with provider
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Auto-detect provider (EC2 or local)
	prov, err := provider.NewProvider(ctx)
	if err != nil {
		log.Fatalf("Failed to create provider: %v", err)
	}

	identity, _ := prov.GetIdentity(ctx)
	log.Printf("Running on provider: %s", identity.Provider)

	// A replacement EC2 launched from a leaked spawn persistent Spot request runs
	// this same bootstrap but has no spawn tags, so nothing else would end it
	// (#995). Off the critical path; it does nothing unless sure.
	if ep, ok := prov.(*provider.EC2Provider); ok {
		go replacementCheck{
			r:        ep,
			shutdown: powerOff,
			notify:   replacementNotifier(identity),
			sleep:    sleepCtx,
		}.run(ctx)
	}

	agent, err := agent.NewAgent(ctx, prov)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}

	// Record the running spored version in the spawn:spored-version tag so
	// `spawn status` / `spawn upgrade-spored` can read it without execing in, and
	// so an in-place upgrade can confirm the new binary took effect (#232/#234).
	// Off the critical path (own goroutine), EC2-only, best-effort — never gates
	// the lifecycle loop (#65).
	if identity.Provider == "ec2" {
		go agent.WriteVersionTag(context.Background(), version())
	}

	// Publish the compute clock so `spored status` — a separate, fresh process —
	// counts real running time instead of the last spawn:compute-seconds
	// checkpoint, which is 0 on a brand-new instance (#896). Best-effort: a
	// status without it falls back to the checkpoint and says so.
	publishComputeClock(agent)
	// Record this boot's compute start on disk now, not at the first tick, so a
	// stop in the first minute still carries it into the next boot (#928).
	agent.WriteComputeCheckpoint()

	// Get config and identity for observability
	agentConfig := agent.GetConfig()
	agentIdentity := agent.GetIdentity()

	// Initialize tracer if enabled
	var tracer *tracing.Tracer
	if agentConfig.Observability.Tracing.Enabled {
		log.Printf("Initializing tracer: exporter=%s, sampling=%.2f",
			agentConfig.Observability.Tracing.Exporter,
			agentConfig.Observability.Tracing.SamplingRate)

		var err error
		tracer, err = tracing.NewTracer(ctx, agentConfig.Observability.Tracing,
			"spored", agentIdentity.InstanceID, agentIdentity.Region)
		if err != nil {
			log.Printf("Warning: Failed to initialize tracer: %v", err)
		}
	}

	// Start metrics server if enabled
	var metricsServer *metrics.Server
	if agentConfig.Observability.Metrics.Enabled {
		log.Printf("Starting metrics server on %s:%d%s",
			agentConfig.Observability.Metrics.Bind,
			agentConfig.Observability.Metrics.Port,
			agentConfig.Observability.Metrics.Path)

		registry := metrics.NewRegistry()
		collector := metrics.NewCollector(agent)
		if err := registry.Register(collector); err != nil {
			log.Printf("Warning: Failed to register metrics collector: %v", err)
		} else {
			metricsServer = metrics.NewServer(agentConfig.Observability.Metrics, registry)
			if err := metricsServer.Start(ctx); err != nil {
				log.Printf("Warning: Failed to start metrics server: %v", err)
			}
		}
	}

	// Start the push API server (plugin key/value delivery from local controller).
	pushAPI, err := pluginruntime.NewPushAPIServer(agent.GetPluginRuntime())
	if err != nil {
		log.Printf("Warning: Failed to start push API server: %v", err)
	} else {
		if err := pushAPI.Start(ctx); err != nil {
			log.Printf("Warning: Push API server failed to bind: %v", err)
		}
	}

	// Start monitoring
	go agent.Monitor(ctx)

	// Block until a stop is requested. On Unix this is SIGINT/SIGTERM; on Windows
	// it's the Service Control Manager stop/shutdown (or signals when run
	// interactively). The platform-specific waitForShutdown returns when stop is
	// requested, then we run graceful cleanup below.
	waitForShutdown()
	log.Printf("Shutting down...")

	// Graceful shutdown - run cleanup tasks
	cancel()

	// Run cleanup with a timeout context
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()

	// Shutdown tracer if running
	if tracer != nil {
		log.Printf("Flushing traces...")
		if err := tracer.Shutdown(cleanupCtx); err != nil {
			log.Printf("Warning: Tracer shutdown error: %v", err)
		}
	}

	// Shutdown metrics server if running
	if metricsServer != nil {
		log.Printf("Shutting down metrics server...")
		if err := metricsServer.Shutdown(cleanupCtx); err != nil {
			log.Printf("Warning: Metrics server shutdown error: %v", err)
		}
	}

	agent.Cleanup(cleanupCtx)

	log.Printf("spored stopped")
}

func newRunQueueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run-queue <queue-file>",
		Short: "Execute a batch job queue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			runner, err := agent.NewQueueRunner(ctx, args[0])
			if err != nil {
				return fmt.Errorf("initialize queue runner: %w", err)
			}
			if err := runner.Run(); err != nil {
				return fmt.Errorf("queue execution failed: %w", err)
			}
			return nil
		},
	}
}

func newRunPipelineStageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run-pipeline-stage",
		Short: "Run this instance's pipeline stage",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runPipelineStage() },
	}
}

func newReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload configuration from EC2 tags",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return handleReload() },
	}
}

func newStatusCmd() *cobra.Command {
	var checkComplete, verbose bool
	var outputFormat string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show configuration and monitoring status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			log.SetOutput(statusLogSink(verbose, os.Stderr))
			return runStatus(checkComplete, outputFormat)
		},
	}
	cmd.Flags().BoolVar(&checkComplete, "check-complete", false,
		"Exit with standardized codes: 0=complete 1=failed 2=running 3=error")
	cmd.Flags().StringVarP(&outputFormat, "output", "o", "table",
		"Output format (table, json). JSON goes to stdout only (spawn#540).")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false,
		"Print the agent's diagnostic log lines (initialization, idle checks) to stderr")
	return cmd
}

// statusLogSink is where `spored status` sends the agent's Go `log` diagnostics.
// Building the query agent logs its initialization and runs an idle check, which
// printed lines such as "Not idle: CPU usage could not be measured ..." — and,
// in "Agent initialized ...", the AWS account ID — into the middle of a status
// display that is a human report (spawn#895). They are diagnostics of a
// read-only query, not of the daemon (whose log is the durable record), so they
// are discarded unless asked for.
// runStatus is handleStatus behind a seam, so a test can drive the real status
// command — flag parsing and log routing — without an instance to query.
var runStatus = handleStatus

func statusLogSink(verbose bool, stderr io.Writer) io.Writer {
	if verbose {
		return stderr
	}
	return io.Discard
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration settings (get, set, list)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "get <key>",
			Short: "Get a configuration value",
			Args:  cobra.ExactArgs(1),
			RunE:  func(cmd *cobra.Command, args []string) error { return handleConfigGet(args[0]) },
		},
		&cobra.Command{
			Use:   "set <key> <value>",
			Short: "Set a configuration value",
			Args:  cobra.ExactArgs(2),
			RunE:  func(cmd *cobra.Command, args []string) error { return handleConfigSet(args[0], args[1]) },
		},
		&cobra.Command{
			Use:   "list",
			Short: "List all configuration",
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return handleConfigList() },
		},
	)
	return cmd
}

func newCompleteCmd() *cobra.Command {
	var file, status, message string
	cmd := &cobra.Command{
		Use:   "complete",
		Short: "Signal completion to trigger the on-complete action",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleComplete(file, status, message)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "/tmp/SPAWN_COMPLETE", "Completion file path")
	cmd.Flags().StringVarP(&status, "status", "s", "", "Optional status (e.g., success, failed)")
	cmd.Flags().StringVarP(&message, "message", "m", "", "Optional message")
	return cmd
}

// launchTimeSourceTag/IMDS/Estimated label which source resolveLaunchTime
// used, most to least authoritative. Exported as constants so callers can
// compare against them without restating the literal strings (and so a typo
// in one place can't silently desync the comparison from the label).
const (
	launchTimeSourceTag       = "spawn:launch-time tag"
	launchTimeSourceIMDS      = "EC2 instance identity (IMDS)"
	launchTimeSourceEstimated = "estimated (status agent start — actual instance age unavailable)"
)

// resolveLaunchTime picks the most authoritative available source for the
// instance's actual launch time, most to least authoritative (spawn#508):
//  1. tagLaunchTime — the spawn:launch-time tag, read via ec2:DescribeTags.
//     Authoritative when readable.
//  2. pendingTime — EC2's own record of when this instance's launch was
//     requested, read from the instance identity document via IMDS. Needs NO
//     IAM permission, so it survives exactly the failure mode (#502-style
//     DescribeTags denial) that makes (1) unavailable — this is what turns
//     "Elapsed: 0s" on a 7h-old instance into the instance's real age even
//     when tags can't be read.
//  3. statusAgentStart — this status-agent invocation's own start time. Last
//     resort; only reached when both the tag AND IMDS are unavailable. Using
//     this as "Started"/"Elapsed" (unlabelled) was the spawn#508 bug.
func resolveLaunchTime(tagLaunchTime, pendingTime, statusAgentStart time.Time) (time.Time, string) {
	if !tagLaunchTime.IsZero() {
		return tagLaunchTime, launchTimeSourceTag
	}
	if !pendingTime.IsZero() {
		return pendingTime, launchTimeSourceIMDS
	}
	return statusAgentStart, launchTimeSourceEstimated
}

// statusReport is the full data spored status computes, independent of how
// it's rendered. Building it once and rendering it two ways (table/JSON) keeps
// the two outputs from drifting: the JSON marshal is of THIS struct, not a
// second hand-maintained representation of the same facts (spawn#540).
type statusReport struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Region     string `json:"region"`
	Version    string `json:"spored_version"`

	StartedAt       time.Time `json:"started_at"`
	StartedAtSource string    `json:"started_at_source"`
	ElapsedSeconds  float64   `json:"elapsed_seconds"`
	ComputeSeconds  float64   `json:"compute_seconds"`
	// ComputeClock says what ComputeSeconds measures: "live" (the daemon's own
	// running clock) or "checkpoint" (the last spawn:compute-seconds tag write,
	// which lags and is 0 on a new instance) (#896).
	ComputeClock string `json:"compute_clock"`
	// BilledSeconds is the running time the compute cost is charged on:
	// ComputeSeconds plus the boot gap, which EC2 bills although spored was not
	// yet up to count it.
	BilledSeconds  float64 `json:"billed_seconds"`
	StoppedSeconds float64 `json:"stopped_seconds"`
	// BootSeconds is the gap between the instance launching and spored starting
	// — boot, cloud-init, package installs. Reported separately because it used
	// to be folded into StoppedSeconds by subtraction (#735).
	BootSeconds float64 `json:"boot_seconds"`
	// HibernatedSeconds is time this boot spent hibernated (#945). Like
	// stopped time it is part of elapsed, bills EBS and not compute, and is
	// excluded from ComputeSeconds; it is reported apart from StoppedSeconds
	// because it is measured directly rather than left over by subtraction.
	HibernatedSeconds float64 `json:"hibernated_seconds"`

	ConfigLoadError string `json:"config_load_error,omitempty"`

	TTL statusTTL `json:"ttl"`

	IdleTimeout statusIdle `json:"idle"`

	OnComplete statusOnComplete `json:"on_complete"`

	Cost *statusCost `json:"cost,omitempty"` // nil when no price is known (nothing to report)

	CPUPercent float64 `json:"cpu_percent"`
	// CPUMeasured is false when CPUPercent is an assumption rather than a
	// reading — no sample interval yet, or /proc/stat unreadable (#734). A
	// consumer must not plot or threshold an unmeasured value.
	CPUMeasured        bool  `json:"cpu_measured"`
	NetworkBytesPerMin int64 `json:"network_bytes_per_min"`
	// NetworkMeasured is CPUMeasured's twin: false when NetworkBytesPerMin is
	// the assume-active placeholder, not a reading, and NetworkUnknown says why
	// (#919). A one-shot `spored status` has no earlier sample, so this is the
	// common case there.
	NetworkMeasured bool   `json:"network_measured"`
	NetworkUnknown  string `json:"network_unknown,omitempty"`
	// DiskBytesPerMin, DiskMeasured and DiskUnknown are the disk I/O signal,
	// with the network fields' semantics (#937).
	DiskBytesPerMin int64  `json:"disk_bytes_per_min"`
	DiskMeasured    bool   `json:"disk_measured"`
	DiskUnknown     string `json:"disk_unknown,omitempty"`
	PreStopHook     string `json:"pre_stop_hook,omitempty"`

	// Peers is the job-array peers file, for a job-array member only (#933).
	Peers *statusPeers `json:"peers,omitempty"`
}

// statusPeers reports the peers file a job-array member's workload reads
// (provider.JobArrayPeersFile). `spored status` runs in its own process, so it
// reads the file the daemon wrote rather than the daemon's memory; the file's
// modification time is when the daemon last confirmed the list.
type statusPeers struct {
	JobArrayID string `json:"job_array_id"`
	File       string `json:"file"`
	// MaintainedBySpored is true for a hybrid member (burst or local node),
	// whose file spored rewrites every RefreshIntervalSeconds. For --mpi the
	// cohort Assembler writes it once, at assembly.
	MaintainedBySpored     bool    `json:"maintained_by_spored"`
	RefreshIntervalSeconds float64 `json:"refresh_interval_seconds,omitempty"`

	Count      int                 `json:"count"`
	Peers      []provider.PeerInfo `json:"peers"`
	UpdatedAt  *time.Time          `json:"updated_at,omitempty"`
	AgeSeconds float64             `json:"age_seconds,omitempty"`
	// Stale is true when spored maintains the file but has not rewritten it
	// for three refresh intervals: discovery or the write is failing, and the
	// list may be missing members that joined or still list ones that left.
	Stale bool `json:"stale"`
	// Error is why the file could not be read (for example, not written yet).
	Error string `json:"error,omitempty"`
}

// peersStaleAfter is how many missed refreshes make a spored-maintained peers
// file stale.
const peersStaleAfter = 3

// buildPeersStatus reads the peers file at path. maintained says whether spored
// rewrites it every interval.
func buildPeersStatus(jobArrayID, path string, maintained bool, interval time.Duration, now time.Time) *statusPeers {
	st := &statusPeers{
		JobArrayID:         jobArrayID,
		File:               path,
		MaintainedBySpored: maintained,
		Peers:              []provider.PeerInfo{},
	}
	if maintained {
		st.RefreshIntervalSeconds = interval.Seconds()
	}
	peers, updated, err := provider.ReadPeersFile(path)
	if !updated.IsZero() {
		u := updated.UTC()
		st.UpdatedAt = &u
		st.AgeSeconds = now.Sub(updated).Seconds()
		if st.AgeSeconds < 0 {
			st.AgeSeconds = 0
		}
		st.Stale = maintained && now.Sub(updated) > peersStaleAfter*interval
	}
	switch {
	case os.IsNotExist(err) && maintained:
		st.Error = "not written yet"
	case os.IsNotExist(err):
		st.Error = "no peers file: spored writes one only for hybrid (burst or local) members, and spawn launch --mpi at assembly"
	case err != nil:
		st.Error = err.Error()
	default:
		st.Peers = peers
		if st.Peers == nil {
			st.Peers = []provider.PeerInfo{}
		}
	}
	st.Count = len(st.Peers)
	return st
}

type statusTTL struct {
	Configured       bool       `json:"configured"`
	RemainingSeconds float64    `json:"remaining_seconds,omitempty"`
	TerminateAt      *time.Time `json:"terminate_at,omitempty"` // the TTL deadline (spawn#540: explicitly requested field)
}

type statusIdle struct {
	Configured       bool    `json:"configured"`
	TimeoutSeconds   float64 `json:"timeout_seconds,omitempty"`
	IsIdle           bool    `json:"is_idle"`
	IdleSeconds      float64 `json:"idle_seconds,omitempty"`
	Action           string  `json:"action,omitempty"` // "stops" or "hibernates"
	RemainingSeconds float64 `json:"remaining_seconds,omitempty"`
}

type statusOnComplete struct {
	Action          string `json:"action,omitempty"`
	CompletionFile  string `json:"completion_file,omitempty"`
	SentinelPresent bool   `json:"sentinel_present"` // spawn#540: explicitly requested field
}

type statusCost struct {
	// PricePerHour is the ON-DEMAND rate (spawn:price-per-hour): the comparison
	// baseline, and what --cost-limit is enforced against.
	PricePerHour float64 `json:"price_per_hour"`
	// RatePerHour is the rate the cost figures are charged at — the spot price
	// for a spot instance when spawn recorded one, else the on-demand rate — and
	// RateBasis says which (#896).
	RatePerHour float64 `json:"rate_per_hour"`
	RateBasis   string  `json:"rate_basis"`
	// ComputeCostKnown is false until any running time has been recorded; a $0.00
	// then is "no data", not "free".
	ComputeCostKnown bool `json:"compute_cost_known"`

	EBSHourlyCost float64 `json:"ebs_hourly_cost,omitempty"`
	EBSCostKnown  bool    `json:"ebs_cost_known"`

	ComputeCost    float64 `json:"compute_cost"`
	StorageCost    float64 `json:"storage_cost,omitempty"`
	CumulativeCost float64 `json:"cumulative_cost"`

	// EffectiveRate is the blended $/hr the instance has actually cost so far
	// (spawn#540: explicitly requested field) — nil when it can't yet be
	// computed (EBS cost lookup still pending).
	EffectiveRate  *float64 `json:"effective_rate,omitempty"`
	SavingsPercent float64  `json:"savings_percent,omitempty"`

	// CostLimit fields are omitted entirely when no limit is configured.
	CostLimit          float64 `json:"cost_limit,omitempty"`
	CostLimitUsed      float64 `json:"cost_limit_used,omitempty"` // compute + EBS, from the enforcement function (spawn#540: "cost-limit consumed"; #959)
	CostLimitUsedPct   float64 `json:"cost_limit_used_pct,omitempty"`
	CostLimitRemaining float64 `json:"cost_limit_remaining,omitempty"`
}

// Rate bases for statusCost.RateBasis.
const (
	rateBasisOnDemand      = "on-demand"
	rateBasisSpot          = "spot"
	rateBasisSpotUnknown   = "on-demand (spot price unknown)"
	computeClockLive       = "live"
	computeClockCheckpoint = "checkpoint"
)

// minEffectiveRateWindow is how much elapsed time an effective rate needs.
const minEffectiveRateWindow = time.Minute

func computeClockLabel(live bool) string {
	if live {
		return computeClockLive
	}
	return computeClockCheckpoint
}

// statusComputeRate picks the rate spored status charges compute at. A spot
// instance is charged at the spot price spawn recorded at launch; the row used to
// say "(spot)" and charge the on-demand rate, overstating cost about 2.7x on the
// reported m8a.4xlarge (#896). A spot instance with no recorded price falls back
// to on-demand and says so — an overstatement, labelled, rather than a guess. The
// SpotPricePerHour tag alone also marks the instance as spot, so a failed IMDS
// read does not discard a price spawn did record.
func statusComputeRate(config *provider.Config, isSpot bool) (float64, string) {
	switch {
	case config.SpotPricePerHour > 0:
		return config.SpotPricePerHour, rateBasisSpot
	case isSpot:
		return config.PricePerHour, rateBasisSpotUnknown
	default:
		return config.PricePerHour, rateBasisOnDemand
	}
}

// buildStatusReport gathers every fact `spored status` reports, from the same
// agent/config accessors the table renderer used to read directly. This is the
// single source of truth both renderTable and renderJSON draw from.
//
// isSpot is whether the instance runs on the spot market (IMDS
// instance-life-cycle); it decides which rate the cost figures use.
func buildStatusReport(ag *agent.Agent, config *provider.Config, identity *provider.Identity, isSpot bool) *statusReport {
	// One evaluation, and every figure below is read from it: the decision and
	// the readings come from the same samples. Two samples microseconds apart is
	// what produced `Not idle: CPU usage 100.00%` beside `CPU: 0.0%` in one
	// status output (#734). This process owns its Agent and runs no monitor
	// loop, so it is the sampler here (#952); a one-shot has one sample, so the
	// counter readings are "unknown (single sample)".
	isIdle := ag.EvaluateIdle()
	cpuPercent, cpuMeasured := ag.GetCPUUsageMeasured()
	netBytes, netMeasured, netUnknown := ag.GetNetworkBytesMeasured()
	diskBytes, diskMeasured, diskUnknown := ag.GetDiskIOBytesMeasured()

	completionFileExists := false
	if config.CompletionFile != "" {
		if _, err := os.Stat(config.CompletionFile); err == nil {
			completionFileExists = true
		}
	}

	var idleTime time.Duration
	if isIdle {
		idleTime = time.Since(ag.GetLastActivityTime())
	}

	// Every clock reading is taken AFTER the samples above, never before. The
	// samples are not instant: on Windows each is a PowerShell process, and the
	// whole set takes seconds. Reading uptime before them and "now" after made
	// startTime — and so the boot term and billed time — late by however long
	// sampling took, so a status 47s after launch said 52s billed (#971).
	uptime := ag.GetUptime()

	// Calculate start time — this is the STATUS AGENT INVOCATION's own start
	// (this process, which runs fresh on every `spored status` call), not the
	// instance's. It is only ever used as a last-resort fallback below: using
	// it as "Started"/"Elapsed" is exactly the spawn#508 bug (a 7h39m-old
	// instance reported "Elapsed: 0s" because this was the only source
	// available when the tag-authoritative LaunchTime couldn't be read).
	startTime := time.Now().Add(-uptime)

	launchTime, launchTimeSource := resolveLaunchTime(config.LaunchTime, identity.PendingTime, startTime)
	elapsed := time.Since(launchTime)

	// liveClock is whether computeTime is real running time. It is for the
	// daemon, and for this status process once it has adopted the daemon's
	// published clock (handleStatus). Without that, computeTime is only the last
	// spawn:compute-seconds checkpoint — 0 on a brand-new instance, which is how
	// "Compute cost: $0.00 (0s × ...)" appeared at 47s elapsed (#896) — and
	// startTime is this process's own start, so the boot/stopped split below
	// would be arithmetic on the wrong clock.
	liveClock := ag.ComputeClockLive()
	computeSecs := ag.TotalComputeSeconds()
	if !liveClock {
		// Exactly the checkpoint. TotalComputeSeconds adds this agent's own
		// running time, which for an unadopted query agent is how long THIS
		// status process has existed — not compute. On Windows, which publishes
		// no clock and always takes this path (#916), that was the seconds spent
		// sampling, reported "as of the last checkpoint" when none existed (#971).
		computeSecs = ag.ComputeClock().BaseSeconds
	}
	computeTime := time.Duration(computeSecs) * time.Second

	// Split the unaccounted remainder into BOOT and STOPPED rather than calling
	// all of it stopped (#735).
	//
	// elapsed runs from the instance's launch time; computeTime accumulates only
	// from when spored started. The difference is therefore boot + cloud-init +
	// package installs, PLUS any genuine stopped time — and the old code labelled
	// the whole residue "stopped", asserting a specific checkable fact (the
	// instance was stopped for 2m 13s) from arithmetic that cannot tell being
	// stopped from never having started.
	//
	// bootTime is directly computable: spored knows both the launch time and its
	// own start. Whatever is left after that is stopped time, and only an
	// instance with prior compute time carried over through the tag has any.
	//
	// Since #928 the daemon counts each boot from the kernel's boot time, so
	// the gap is launch → counting on the FIRST boot only. On a later boot the
	// earlier boots are inside computeTime, and measuring from the original
	// launch to this boot's start would bill the stopped interval as boot time.
	var bootTime time.Duration
	if !ag.HasPriorComputeTime() {
		bootTime = ag.ComputeStart().Sub(launchTime)
	}
	if bootTime < 0 {
		bootTime = 0 // clock skew between the tag and local uptime
	}
	// A hibernate-and-resume stays in the same boot, so it is neither boot
	// time nor a stop the carried-over base would reveal. The kernel measures
	// it (#945): it is part of elapsed, and is not compute.
	hibernatedTime := ag.HibernatedTime()
	stoppedTime := elapsed - computeTime - bootTime - hibernatedTime
	if stoppedTime < 0 || !ag.HasPriorComputeTime() {
		// No carried-over compute time means the instance has never been stopped,
		// so there is no stopped time to report however the subtraction lands.
		stoppedTime = 0
	}
	if !liveClock {
		// startTime is this status process's own start, not the daemon's, so
		// "boot" would be all of elapsed and "stopped" would be noise. Report
		// neither rather than a split computed against the wrong clock.
		bootTime, stoppedTime, hibernatedTime = 0, 0, 0
	}
	// EBS bills while stopped and while hibernated alike.
	notRunningTime := stoppedTime + hibernatedTime

	// billedTime is the running time EC2 charges for. Billing starts when the
	// instance enters running, before the OS boots, so the boot gap is billed
	// compute even though spored was not yet counting it.
	billedTime := computeTime + bootTime

	var ttlRemaining time.Duration
	if config.TTL > 0 {
		ttlRemaining = config.TTL - uptime
		if ttlRemaining < 0 {
			ttlRemaining = 0
		}
	}

	var terminateAt time.Time
	if !config.TTLDeadline.IsZero() {
		terminateAt = config.TTLDeadline
		ttlRemaining = time.Until(terminateAt)
		if ttlRemaining < 0 {
			ttlRemaining = 0
		}
	} else if config.TTL > 0 {
		terminateAt = launchTime.Add(config.TTL)
	}

	report := &statusReport{
		InstanceID:         identity.InstanceID,
		Name:               identity.Name,
		Region:             identity.Region,
		Version:            version(),
		StartedAt:          launchTime.UTC(),
		StartedAtSource:    launchTimeSource,
		ElapsedSeconds:     elapsed.Seconds(),
		ComputeSeconds:     computeTime.Seconds(),
		ComputeClock:       computeClockLabel(liveClock),
		BilledSeconds:      billedTime.Seconds(),
		StoppedSeconds:     stoppedTime.Seconds(),
		BootSeconds:        bootTime.Seconds(),
		HibernatedSeconds:  hibernatedTime.Seconds(),
		ConfigLoadError:    config.ConfigLoadError,
		CPUPercent:         cpuPercent,
		CPUMeasured:        cpuMeasured,
		NetworkBytesPerMin: netBytes,
		NetworkMeasured:    netMeasured,
		NetworkUnknown:     netUnknown,
		DiskBytesPerMin:    diskBytes,
		DiskMeasured:       diskMeasured,
		DiskUnknown:        diskUnknown,
		PreStopHook:        config.PreStop,
	}

	if !terminateAt.IsZero() {
		utc := terminateAt.UTC()
		report.TTL = statusTTL{Configured: true, RemainingSeconds: ttlRemaining.Seconds(), TerminateAt: &utc}
	} else if config.TTL > 0 {
		report.TTL = statusTTL{Configured: true, RemainingSeconds: ttlRemaining.Seconds()}
	}

	if config.IdleTimeout > 0 {
		idle := statusIdle{Configured: true, TimeoutSeconds: config.IdleTimeout.Seconds(), IsIdle: isIdle}
		if isIdle {
			idleAction := "stops"
			if config.HibernateOnIdle {
				idleAction = "hibernates"
			}
			remaining := config.IdleTimeout - idleTime
			if remaining < 0 {
				remaining = 0
			}
			idle.IdleSeconds = idleTime.Seconds()
			idle.Action = idleAction
			idle.RemainingSeconds = remaining.Seconds()
		}
		report.IdleTimeout = idle
	}

	report.OnComplete = statusOnComplete{
		Action:          config.OnComplete,
		CompletionFile:  config.CompletionFile,
		SentinelPresent: completionFileExists,
	}

	if config.JobArrayID != "" {
		report.Peers = buildPeersStatus(config.JobArrayID, ag.PeersFilePath(), ag.MaintainsPeersFile(),
			agent.PeerRefreshInterval, time.Now())
	}

	if rate, basis := statusComputeRate(config, isSpot); rate > 0 {
		ebsHourlyCost := config.EBSHourlyCost
		ebsCostKnown := ebsHourlyCost > 0
		computeCost := rate * billedTime.Hours()

		var storageCost float64
		if ebsCostKnown {
			storageCost = ebsHourlyCost * notRunningTime.Hours()
		}
		total := computeCost + storageCost
		displayCompute := math.Round(computeCost*100) / 100
		displayEBS := math.Round(storageCost*100) / 100

		cost := &statusCost{
			PricePerHour:     config.PricePerHour,
			RatePerHour:      rate,
			RateBasis:        basis,
			EBSHourlyCost:    ebsHourlyCost,
			EBSCostKnown:     ebsCostKnown,
			ComputeCostKnown: billedTime >= time.Second,
			ComputeCost:      displayCompute,
			StorageCost:      displayEBS,
			CumulativeCost:   displayCompute + displayEBS,
		}

		// The effective rate is a ratio, and a ratio of zeros is not a saving:
		// with no running time recorded it came out as $0.0000/hr, "100% lower
		// than continuous on-demand" (#896). It needs real running time on the
		// live clock (a checkpoint lags elapsed by minutes, which reads as a
		// saving too), a known EBS rate, and a minute of elapsed time — before
		// that, whole-second compute over a sub-minute elapsed is noise. It is
		// computed from the unrounded figures: a cents-rounded total divided by
		// a few minutes is noise too.
		if elapsed >= minEffectiveRateWindow && billedTime >= time.Second && liveClock && ebsCostKnown {
			effectiveRate := total / elapsed.Hours()
			cost.EffectiveRate = &effectiveRate
			if config.PricePerHour > 0 {
				savingsPct := (1 - effectiveRate/config.PricePerHour) * 100
				if savingsPct > 0.5 {
					cost.SavingsPercent = savingsPct
				}
			}
		}

		if config.CostLimit > 0 {
			// "Used" is read from the function the enforcement loop terminates on
			// (compute + EBS since launch, #616), never re-derived here: a copy of
			// that arithmetic went stale once already and overstated the headroom,
			// worst on a stopped instance whose EBS kept accruing (#959).
			used := math.Round(ag.CostLimitSpend()*100) / 100
			cost.CostLimit = config.CostLimit
			cost.CostLimitUsed = used
			cost.CostLimitUsedPct = (used / config.CostLimit) * 100
			cost.CostLimitRemaining = config.CostLimit - used
		}

		report.Cost = cost
	}

	return report
}

func handleStatus(checkComplete bool, outputFormat string) error {
	// Create agent to get configuration and metrics
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		if checkComplete {
			os.Exit(3) // error querying status
		}
		return fmt.Errorf("initialize provider: %w", err)
	}

	ag, err := agent.NewAgentForQuery(ctx, prov)
	if err != nil {
		if checkComplete {
			os.Exit(3)
		}
		return fmt.Errorf("initialize agent: %w", err)
	}

	// Get configuration
	config := ag.GetConfig()

	// --check-complete: report completion via standardized exit codes rather than
	// the human-readable display (0=complete, 1=failed, 2=running, 3=error).
	if checkComplete {
		exitCheckComplete(config.CompletionFile)
	}

	// Count compute on the daemon's clock, not this process's (#896). When the
	// daemon has not published one (an older spored, Windows, or a daemon that
	// is not running) the report falls back to the spawn:compute-seconds
	// checkpoint and labels it.
	if err := ag.AdoptPublishedComputeClock(); err != nil && !os.IsNotExist(err) {
		log.Printf("compute clock unavailable, using the last checkpoint: %v", err)
	}

	identity := ag.GetIdentity()
	report := buildStatusReport(ag, config, identity, prov.IsSpotInstance(ctx))

	if outputFormat == "json" {
		return renderStatusJSON(report)
	}
	return renderStatusTable(report)
}

// publishComputeClock writes the daemon's compute clock where `spored status`
// reads it (#896). Best-effort and logged: a status without it falls back to the
// spawn:compute-seconds checkpoint and labels the figure as such.
func publishComputeClock(ag *agent.Agent) {
	p := agent.ComputeClockPath()
	if p == "" {
		return
	}
	if err := agent.WriteComputeClock(p, ag.ComputeClock()); err != nil {
		log.Printf("Warning: could not publish compute clock to %s: %v", p, err)
	}
}

// notRunningLabel names the not-running time a storage cost covers.
func notRunningLabel(stopped, hibernated time.Duration) string {
	switch {
	case hibernated > 0 && stopped > 0:
		return "stopped or hibernated"
	case hibernated > 0:
		return "hibernated"
	default:
		return "stopped"
	}
}

// renderStatusJSON prints ONLY the JSON-encoded report to stdout — nothing
// else. This is the machine-readable contract: a caller doing
// json.Unmarshal(output) must get valid JSON with no leading/trailing bytes.
// The agent-initialization log.Printf calls triggered by agent.NewAgent above
// never reach stdout: they are discarded, or sent to stderr under --verbose
// (statusLogSink, spawn#895). spawn#540's leak was an explicit `2>&1` in the
// SSH transport that cmd/status.go used to invoke this.
func renderStatusJSON(report *statusReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// renderStatusTable prints the human-readable table — unchanged from the
// pre-#540 output, just reading from the report struct instead of local
// variables.
func renderStatusTable(report *statusReport) error {
	// ── Identity ──────────────────────────────────────────────────────────────
	fmt.Printf("\n  %s  (%s)\n", report.Name, report.InstanceID)
	fmt.Printf("  %s\n\n", strings.Repeat("─", 46))
	fmt.Printf("  spored:           v%s\n", report.Version)

	elapsed := time.Duration(report.ElapsedSeconds * float64(time.Second))
	computeTime := time.Duration(report.ComputeSeconds * float64(time.Second))
	stoppedTime := time.Duration(report.StoppedSeconds * float64(time.Second))
	bootTime := time.Duration(report.BootSeconds * float64(time.Second))
	hibernatedTime := time.Duration(report.HibernatedSeconds * float64(time.Second))

	fmt.Printf("  Started:          %s\n", report.StartedAt.Format("2006-01-02 15:04 UTC"))
	fmt.Printf("  Elapsed:          %s", formatDuration(elapsed))
	// Each term appears only when it is non-zero, so a first-boot instance that
	// was never stopped no longer claims to have been. Boot time is worth seeing
	// in its own right: a slow boot is a real thing to notice.
	if computeTime > 0 && (bootTime > 0 || stoppedTime > 0 || hibernatedTime > 0) {
		parts := []string{formatDuration(computeTime) + " compute"}
		if bootTime > 0 {
			parts = append(parts, formatDuration(bootTime)+" boot")
		}
		if stoppedTime > 0 {
			parts = append(parts, formatDuration(stoppedTime)+" stopped")
		}
		if hibernatedTime > 0 {
			parts = append(parts, formatDuration(hibernatedTime)+" hibernated")
		}
		fmt.Printf("  (%s)", strings.Join(parts, " · "))
	}
	if report.StartedAtSource != launchTimeSourceTag {
		// Only annotate the fallback paths — the common case (the tag read
		// cleanly) stays unadorned. Surfacing the source is exactly what #508
		// needed: "Elapsed: 0s" with no indication it was the status agent's
		// own age, not the instance's.
		fmt.Printf("  (source: %s)", report.StartedAtSource)
	}
	fmt.Println()

	// config.ConfigLoadError is set only when the tag read itself failed
	// (ec2:DescribeTags denied, etc, spawn#502/#508) — in that case every
	// zero-value config field is UNKNOWN, not unset, and rendering "TTL: none"
	// asserts a specific, possibly-false fact ("will not auto-terminate")
	// from data that was never actually read.
	switch {
	case report.ConfigLoadError != "":
		fmt.Printf("  TTL:              UNKNOWN — could not read config (%s)\n", report.ConfigLoadError)
	case report.TTL.TerminateAt != nil:
		fmt.Printf("  TTL:              %s remaining — terminates %s\n",
			formatDuration(time.Duration(report.TTL.RemainingSeconds*float64(time.Second))),
			report.TTL.TerminateAt.Format("2006-01-02 15:04 UTC"))
	default:
		fmt.Println("  TTL:              none — instance will not auto-terminate")
	}

	if report.IdleTimeout.Configured {
		idle := report.IdleTimeout
		timeoutDur := time.Duration(idle.TimeoutSeconds * float64(time.Second))
		if idle.IsIdle {
			fmt.Printf("  Idle timeout:     %s  (%s for %s — %s in %s)\n",
				formatDuration(timeoutDur), idle.Action, formatDuration(time.Duration(idle.IdleSeconds*float64(time.Second))),
				idle.Action, formatDuration(time.Duration(idle.RemainingSeconds*float64(time.Second))))
		} else {
			fmt.Printf("  Idle timeout:     %s  (currently active)\n", formatDuration(timeoutDur))
		}
	}

	if report.OnComplete.Action != "" {
		fileStatus := "watching"
		if report.OnComplete.SentinelPresent {
			fileStatus = "✓ file present — acting on next check"
		}
		fmt.Printf("  On complete:      %s (%s)\n", report.OnComplete.Action, fileStatus)
	}

	// ── Cost ──────────────────────────────────────────────────────────────────
	if report.Cost != nil {
		cost := report.Cost
		fmt.Println()

		billedTime := time.Duration(report.BilledSeconds * float64(time.Second))
		live := report.ComputeClock == computeClockLive
		rateLabel := fmt.Sprintf("$%.4f/hr %s", cost.RatePerHour, cost.RateBasis)
		switch {
		case !cost.ComputeCostKnown:
			// "$0.00 (0s × ...)" on a running instance reads as "free"; it means
			// nothing has been recorded yet (#896).
			fmt.Printf("  Compute cost:     —  (no running time recorded yet; %s)\n", rateLabel)
		case !live:
			fmt.Printf("  Compute cost:     $%.2f  (%s × %s, as of the last checkpoint)\n",
				cost.ComputeCost, formatDuration(billedTime), rateLabel)
		default:
			fmt.Printf("  Compute cost:     $%.2f  (%s × %s)\n",
				cost.ComputeCost, formatDuration(billedTime), rateLabel)
		}
		if notRunning := stoppedTime + hibernatedTime; notRunning >= time.Minute {
			if cost.EBSCostKnown {
				fmt.Printf("  Storage cost:     $%.2f  (%s × $%.4f/hr EBS)\n",
					cost.StorageCost, formatDuration(notRunning), cost.EBSHourlyCost)
			} else {
				fmt.Printf("  Storage cost:     not yet available  (%s %s)\n",
					formatDuration(notRunning), notRunningLabel(stoppedTime, hibernatedTime))
			}
		}
		if cost.ComputeCostKnown {
			fmt.Printf("  Cumulative cost:  $%.2f\n", cost.CumulativeCost)
		} else {
			fmt.Println("  Cumulative cost:  —")
		}

		switch {
		case cost.EffectiveRate != nil && cost.SavingsPercent > 0.5:
			fmt.Printf("  Effective rate:   $%.4f/hr  (%.0f%% lower than continuous on-demand)\n",
				*cost.EffectiveRate, cost.SavingsPercent)
		case cost.EffectiveRate != nil:
			fmt.Printf("  Effective rate:   $%.4f/hr\n", *cost.EffectiveRate)
		case !cost.ComputeCostKnown:
			fmt.Println("  Effective rate:   —  (no running time recorded yet)")
		case !live:
			fmt.Println("  Effective rate:   —  (the last checkpoint lags elapsed time)")
		case report.ElapsedSeconds < minEffectiveRateWindow.Seconds():
			fmt.Println("  Effective rate:   —  (needs a minute of running time)")
		case report.ElapsedSeconds > 0 && !cost.EBSCostKnown:
			fmt.Println("  Effective rate:   not yet available  (EBS cost lookup pending)")
		}

		if cost.CostLimit > 0 {
			fmt.Printf("  Cost limit:       $%.2f  ($%.2f used, compute + EBS, %.0f%% — $%.2f remaining)\n",
				cost.CostLimit, cost.CostLimitUsed, cost.CostLimitUsedPct, cost.CostLimitRemaining)
		}

		if cost.EBSCostKnown {
			fmt.Printf("  On-demand rate:   $%.4f/hr compute  +  $%.4f/hr EBS storage  (%s)\n",
				cost.PricePerHour, cost.EBSHourlyCost, report.Region)
		} else {
			fmt.Printf("  On-demand rate:   $%.4f/hr compute  +  EBS storage pending  (%s)\n",
				cost.PricePerHour, report.Region)
		}
		switch cost.RateBasis {
		case rateBasisSpot:
			fmt.Printf("  Spot rate:        $%.4f/hr compute  (spot price when launched; spot prices move)\n", cost.RatePerHour)
		case rateBasisSpotUnknown:
			fmt.Println("  Spot rate:        unknown — costs above use the on-demand rate, so they overstate")
		}
		fmt.Println()
		fmt.Println("  * Cost figures are estimates. Definitive billing is from your cloud provider.")
	}

	// ── Live metrics (brief) ──────────────────────────────────────────────────
	fmt.Println()
	if report.CPUMeasured {
		fmt.Printf("  CPU:              %.1f%%\n", report.CPUPercent)
	} else {
		// An honest unknown beats a confident 0.0% on a box running at 100%.
		// A one-shot `spored status` has no prior sample to delta against, so
		// this is the COMMON case there, not an edge one.
		fmt.Printf("  CPU:              unknown (single sample; assuming active)\n")
	}
	if report.NetworkMeasured {
		fmt.Printf("  Network:          %s/min\n", formatBytes(report.NetworkBytesPerMin))
	} else {
		// Not "976.6 KB/min": that was the placeholder, printed as traffic (#919).
		fmt.Printf("  Network:          unknown (%s; assuming active)\n", report.NetworkUnknown)
	}
	if report.DiskMeasured {
		fmt.Printf("  Disk I/O:         %s/min\n", formatBytes(report.DiskBytesPerMin))
	} else {
		fmt.Printf("  Disk I/O:         unknown (%s; assuming active)\n", report.DiskUnknown)
	}
	if report.PreStopHook != "" {
		fmt.Printf("  Pre-stop hook:    %s\n", report.PreStopHook)
	}
	if report.Peers != nil {
		renderPeersTable(report.Peers)
	}
	fmt.Println()
	return nil
}

// renderPeersTable prints the job-array peers section of `spored status`.
func renderPeersTable(p *statusPeers) {
	fmt.Println()
	fmt.Printf("  Job array:        %s\n", p.JobArrayID)
	if p.Error != "" {
		fmt.Printf("  Peers:            unknown — %s (%s)\n", p.Error, p.File)
		return
	}
	line := fmt.Sprintf("  Peers:            %d in %s", p.Count, p.File)
	if p.UpdatedAt != nil {
		line += fmt.Sprintf("  (updated %s ago", formatDuration(time.Duration(p.AgeSeconds*float64(time.Second))))
		if p.MaintainedBySpored {
			line += fmt.Sprintf(", refreshed every %s", formatDuration(time.Duration(p.RefreshIntervalSeconds*float64(time.Second))))
		}
		line += ")"
	}
	fmt.Println(line)
	if p.Stale {
		fmt.Println("  ⚠️  Peer list is stale: spored has not refreshed it for several intervals; see the spored log")
	}
	for _, peer := range p.Peers {
		addr := peer.IP
		if peer.DNS != "" {
			addr += "  " + peer.DNS
		}
		fmt.Printf("    [%d] %-20s %-6s %s\n", peer.Index, peer.InstanceID, peer.Provider, addr)
	}
}

// exitCheckComplete inspects the completion file and exits with the standardized
// codes used by `spawn status --check-complete`:
//
//	0 = complete   — file present (status not "failed")
//	1 = failed     — file present with JSON {"status":"failed"} (or "error")
//	2 = running    — file absent
//	3 = error      — could not determine (e.g. no completion file configured)
//
// This is the single-instance counterpart to the sweep check; previously the
// instance path always exited 0 (#26).
func exitCheckComplete(completionFile string) {
	os.Exit(checkCompleteCode(completionFile))
}

// checkCompleteCode returns the standardized exit code for --check-complete:
// 0=complete, 1=failed, 2=running (file absent), 3=error. Pure (no os.Exit) so
// it is unit-testable.
func checkCompleteCode(completionFile string) int {
	if completionFile == "" {
		completionFile = "/tmp/SPAWN_COMPLETE"
	}

	data, err := os.ReadFile(completionFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 2 // still running — workload hasn't signaled completion
		}
		return 3 // unexpected error reading the file
	}

	// File present. If it carries JSON metadata with a failure status, report
	// failed; otherwise treat its presence as completion.
	var meta struct {
		Status string `json:"status"`
	}
	if len(data) > 0 && json.Unmarshal(data, &meta) == nil {
		switch strings.ToLower(meta.Status) {
		case "failed", "failure", "error":
			return 1
		}
	}
	return 0
}

func handleReload() error {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		return fmt.Errorf("initialize provider: %w", err)
	}

	ag, err := agent.NewAgentForQuery(ctx, prov)
	if err != nil {
		return fmt.Errorf("initialize agent: %w", err)
	}

	fmt.Println("Reloading configuration...")

	if err := ag.Reload(ctx); err != nil {
		return fmt.Errorf("reload configuration: %w", err)
	}

	fmt.Println("✓ Configuration reloaded successfully")

	// Show new config
	config := ag.GetConfig()
	fmt.Println("\nCurrent configuration:")
	fmt.Printf("  TTL:              %v\n", config.TTL)
	fmt.Printf("  Idle Timeout:     %v\n", config.IdleTimeout)
	fmt.Printf("  On Complete:      %s\n", config.OnComplete)
	fmt.Printf("  Hibernate:        %v\n", config.HibernateOnIdle)
	return nil
}

func handleConfigGet(key string) error {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		return fmt.Errorf("initialize provider: %w", err)
	}

	ag, err := agent.NewAgentForQuery(ctx, prov)
	if err != nil {
		return fmt.Errorf("initialize agent: %w", err)
	}

	config := ag.GetConfig()

	switch key {
	case "ttl":
		// Report the field that actually governs termination
		// (spawn:ttl-deadline, read into config.TTLDeadline), not the raw
		// spawn:ttl duration tag in isolation. Before this fix, `config get
		// ttl` echoed spawn:ttl and `spored status` computed remaining time
		// from spawn:ttl-deadline — the same instance, read at the same
		// moment, could give two different answers whenever a `config set
		// ttl` had written spawn:ttl without moving the deadline
		// (spawn#553's "the reporting is the sharpest part"). Deriving both
		// from the deadline here keeps `config get ttl` and `status` in
		// permanent agreement, because they are now the same calculation
		// (formatTTLLine, shared with handleConfigSetTTL's own read-back).
		fmt.Println(formatTTLLine(config, time.Now()))
	case "idle-timeout":
		if config.IdleTimeout > 0 {
			fmt.Println(formatDuration(config.IdleTimeout))
		} else {
			fmt.Println("disabled")
		}
	case "on-complete":
		if config.OnComplete != "" {
			fmt.Println(config.OnComplete)
		} else {
			fmt.Println("disabled")
		}
	case "hibernate":
		fmt.Println(config.HibernateOnIdle)
	case "completion-file":
		if config.CompletionFile != "" {
			fmt.Println(config.CompletionFile)
		} else {
			fmt.Println("not set")
		}
	case "completion-delay":
		if config.CompletionDelay > 0 {
			fmt.Println(formatDuration(config.CompletionDelay))
		} else {
			fmt.Println("0s")
		}
	default:
		return fmt.Errorf("unknown config key: %s\nValid keys: ttl, idle-timeout, on-complete, hibernate, completion-file, completion-delay", key)
	}
	return nil
}

func handleConfigSet(key, value string) error {
	ctx := context.Background()

	// Validate before the provider, IMDS or any AWS call (#990).
	if _, _, err := configSetTag(key, value); err != nil {
		return err
	}

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		return fmt.Errorf("initialize provider: %w", err)
	}

	// Config set only works for EC2 instances
	if prov.GetProviderType() != "ec2" {
		return fmt.Errorf("config set is only supported on EC2 instances\nFor local instances, edit the config file: /etc/spawn/local.yaml")
	}

	ag, err := agent.NewAgentForQuery(ctx, prov)
	if err != nil {
		return fmt.Errorf("initialize agent: %w", err)
	}

	identity := ag.GetIdentity()
	instanceID, region := identity.InstanceID, identity.Region

	// ttl is handled separately from the generic single-tag path below: it
	// has TWO tags (spawn:ttl and spawn:ttl-deadline), and spawn:ttl-deadline
	// — not spawn:ttl — is the field pkg/agent's enforcement loop actually
	// reads (agent.go prefers TTLDeadline whenever it is non-zero, which is
	// true for every instance a current spawn has ever launched). Writing
	// spawn:ttl alone, as this command used to do, reported full success
	// (tag written, reload triggered, "TTL changed" logged, checkmark
	// printed) while the enforced deadline never moved — especially
	// dangerous for SHORTENING a TTL, since `spawn extend` can only ever
	// move the deadline later and so offered no way to undo an
	// over-provisioned TTL (spawn#553).
	if key == "ttl" {
		return handleConfigSetTTL(ctx, ag, instanceID, region, value)
	}

	tagKey, value, err := configSetTag(key, value)
	if err != nil {
		return err
	}

	// Get EC2 client
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	// Update tag
	fmt.Printf("Updating %s to %s...\n", key, value)
	_, err = ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags: []types.Tag{
			{Key: aws.String(tagKey), Value: aws.String(value)},
		},
	})
	if err != nil {
		return fmt.Errorf("update tag: %w", err)
	}

	// Reload configuration
	fmt.Println("Reloading configuration...")
	if err := ag.Reload(ctx); err != nil {
		return fmt.Errorf("reload configuration: %w", err)
	}

	fmt.Printf("✓ Configuration updated: %s = %s\n", key, value)
	return nil
}

// configSetTag validates a `spored config set <key> <value>` pair and returns
// the tag it writes and the value to write. Pure, so handleConfigSet can call
// it before any AWS call, and testable without an instance. Durations use
// pkg/ttl's shared parser, the one the agent's tag reader uses (#990).
func configSetTag(key, value string) (tagKey, tagValue string, err error) {
	switch key {
	case "ttl":
		// Written by handleConfigSetTTL, which moves spawn:ttl-deadline too.
		// The shared parser (#990): "1d" and "2d12h" work, as they do for
		// `spawn launch --ttl` and `spawn extend`; "0" disables.
		if value != "0" {
			if _, err := ttl.ParseTTL(value); err != nil {
				return "", "", fmt.Errorf("invalid duration: %s: %w", value, err)
			}
		}
		return "spawn:ttl", value, nil
	case "idle-timeout":
		// The shared parser (#990), so "1d" is accepted here exactly when the
		// agent's tag reader (pkg/provider) will honour it. "0" disables.
		v, err := ttl.TagValue(value)
		if err != nil {
			return "", "", fmt.Errorf("invalid duration: %s: %w", value, err)
		}
		return "spawn:idle-timeout", v, nil
	case "on-complete":
		if value != "terminate" && value != "stop" && value != "hibernate" && value != "" {
			return "", "", fmt.Errorf("on-complete must be: terminate, stop, hibernate, or empty to disable")
		}
		return "spawn:on-complete", value, nil
	case "hibernate":
		if value != "true" && value != "false" {
			return "", "", fmt.Errorf("hibernate must be: true or false")
		}
		return "spawn:hibernate-on-idle", value, nil
	case "completion-file":
		return "spawn:completion-file", value, nil
	case "completion-delay":
		v, err := ttl.TagValue(value)
		if err != nil {
			return "", "", fmt.Errorf("invalid duration: %s: %w", value, err)
		}
		return "spawn:completion-delay", v, nil
	default:
		return "", "", fmt.Errorf("unknown config key: %s", key)
	}
}

// handleConfigSetTTL implements `spored config set ttl <value>` (spawn#553).
// value is the new TOTAL TTL from launch (matching what spawn:ttl has always
// meant elsewhere, and what `spawn launch --ttl` sets on day one) — not an
// increment. A shorter value than the current remaining time is accepted and
// moves the deadline earlier: `spawn extend` already owns the
// "lengthen from here" case and is monotonic-forward-only by construction, so
// this is the only supported path for tightening an over-provisioned TTL, and
// the issue's whole complaint is that operators need it. "0" disables TTL
// enforcement entirely (spawn:ttl-deadline is deleted, matching a launch with
// no --ttl at all) rather than producing a already-past deadline that would
// terminate the instance on the very next monitor tick under a different,
// confusing name.
func handleConfigSetTTL(ctx context.Context, ag *agent.Agent, instanceID, region, value string) error {
	launchTime := ag.GetConfig().LaunchTime

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	if value == "0" {
		fmt.Println("Disabling TTL (removing spawn:ttl-deadline)...")
		if _, err := ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{instanceID},
			Tags:      []types.Tag{{Key: aws.String("spawn:ttl"), Value: aws.String("0")}},
		}); err != nil {
			return fmt.Errorf("update tag: %w", err)
		}
		if _, err := ec2Client.DeleteTags(ctx, &ec2.DeleteTagsInput{
			Resources: []string{instanceID},
			Tags:      []types.Tag{{Key: aws.String("spawn:ttl-deadline")}},
		}); err != nil {
			return fmt.Errorf("remove spawn:ttl-deadline tag: %w", err)
		}
	} else {
		// The shared parser (#990): "1d" and "2d12h" work, as they do for
		// `spawn launch --ttl` and `spawn extend`. Must be positive; "0" is
		// the disable case above.
		newTTL, err := ttl.ParseTTL(value)
		if err != nil {
			return fmt.Errorf("invalid duration: %s: %w", value, err)
		}

		// Refuse rather than write a partial/incoherent update (spawn#553 fix
		// #1): spawn:ttl-deadline is anchored to launch time everywhere else
		// in this codebase, and there is no coherent fallback anchor for
		// "set the new TOTAL ttl" the way `extend` has one ("add to now" when
		// there's no prior deadline) for its ADD semantics.
		tags, newDeadline, err := ttl.SetTags(launchTime, newTTL)
		if err != nil {
			return fmt.Errorf("cannot set ttl: %w (spawn:launch-time tag is missing or unparseable on this instance)", err)
		}

		fmt.Printf("Updating ttl to %s (deadline %s)...\n", value, newDeadline.UTC().Format("2006-01-02 15:04 UTC"))
		ec2Tags := make([]types.Tag, 0, len(tags))
		for k, v := range tags {
			ec2Tags = append(ec2Tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		if _, err := ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{instanceID},
			Tags:      ec2Tags,
		}); err != nil {
			return fmt.Errorf("update tag: %w", err)
		}
	}

	// Reload configuration
	fmt.Println("Reloading configuration...")
	if err := ag.Reload(ctx); err != nil {
		return fmt.Errorf("reload configuration: %w", err)
	}

	// Never report success without reading back the field enforcement
	// actually uses (spawn#553 fix #4): re-fetch the config the daemon just
	// reloaded and print what it now believes the deadline to be — via the
	// same formatTTLLine `config get ttl` uses — instead of echoing the
	// input value back at the operator. A checkmark here means the deadline
	// actually moved.
	fmt.Printf("✓ Configuration updated: ttl = %s\n", formatTTLLine(ag.GetConfig(), time.Now()))
	return nil
}

// formatTTLLine renders config's TTL the same way regardless of caller
// (`spored config get ttl`'s echo and `spored config set ttl`'s post-reload
// read-back) — prioritizing the enforced spawn:ttl-deadline field over the
// raw spawn:ttl duration tag, exactly as pkg/agent's own enforcement loop
// does. now is injected so this is testable without a real clock.
func formatTTLLine(config *provider.Config, now time.Time) string {
	switch {
	case !config.TTLDeadline.IsZero():
		remaining := config.TTLDeadline.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("%s remaining (terminates %s)",
			formatDuration(remaining), config.TTLDeadline.UTC().Format("2006-01-02 15:04 UTC"))
	case config.TTL > 0:
		return formatDuration(config.TTL)
	default:
		return "disabled"
	}
}

func handleConfigList() error {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		return fmt.Errorf("initialize provider: %w", err)
	}

	ag, err := agent.NewAgentForQuery(ctx, prov)
	if err != nil {
		return fmt.Errorf("initialize agent: %w", err)
	}

	config := ag.GetConfig()

	fmt.Println("Current configuration:")
	fmt.Println()

	if config.TTL > 0 {
		fmt.Printf("  ttl:              %s\n", formatDuration(config.TTL))
	} else {
		fmt.Println("  ttl:              disabled")
	}

	if config.IdleTimeout > 0 {
		fmt.Printf("  idle-timeout:     %s\n", formatDuration(config.IdleTimeout))
	} else {
		fmt.Println("  idle-timeout:     disabled")
	}

	if config.OnComplete != "" {
		fmt.Printf("  on-complete:      %s\n", config.OnComplete)
	} else {
		fmt.Println("  on-complete:      disabled")
	}

	fmt.Printf("  hibernate:        %v\n", config.HibernateOnIdle)

	if config.CompletionFile != "" {
		fmt.Printf("  completion-file:  %s\n", config.CompletionFile)
	}

	if config.CompletionDelay > 0 {
		fmt.Printf("  completion-delay: %s\n", formatDuration(config.CompletionDelay))
	}
	return nil
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func handleComplete(completionFile, status, message string) error {
	// Build metadata if provided
	var content []byte
	if status != "" || message != "" {
		metadata := make(map[string]string)
		if status != "" {
			metadata["status"] = status
		}
		if message != "" {
			metadata["message"] = message
		}
		metadata["timestamp"] = time.Now().Format(time.RFC3339)

		var err error
		content, err = json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			return fmt.Errorf("encode metadata: %w", err)
		}
	}

	// Write completion file
	if err := os.WriteFile(completionFile, content, 0644); err != nil {
		return fmt.Errorf("write completion file: %w", err)
	}

	// Success message
	fmt.Printf("✓ Completion signal sent to %s\n", completionFile)
	if status != "" {
		fmt.Printf("  Status: %s\n", status)
	}
	if message != "" {
		fmt.Printf("  Message: %s\n", message)
	}
	return nil
}

func runPipelineStage() error {
	ctx := context.Background()

	log.Println("Checking if instance is part of a pipeline...")

	// Check if this is a pipeline instance
	isPipeline, err := pipeline.IsPipelineInstance(ctx)
	if err != nil {
		return fmt.Errorf("check pipeline status: %w", err)
	}

	if !isPipeline {
		return fmt.Errorf("this instance is not part of a pipeline")
	}

	log.Println("Instance is part of a pipeline, initializing stage runner...")

	// Create stage runner
	runner, err := pipeline.NewStageRunner(ctx)
	if err != nil {
		return fmt.Errorf("initialize stage runner: %w", err)
	}

	log.Println("Running pipeline stage...")

	// Run stage
	if err := runner.Run(ctx); err != nil {
		return fmt.Errorf("pipeline stage execution failed: %w", err)
	}

	log.Println("Pipeline stage completed successfully")
	return nil
}
