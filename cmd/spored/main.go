package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/spore-host/spawn/pkg/observability/metrics"
	"github.com/spore-host/spawn/pkg/observability/tracing"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/pluginruntime"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/tagprefix"
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

var Version = "0.1.0"

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
		Version:       Version,
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
		newRunPipelineStageCmd(),
		newStatusCmd(),
		newReloadCmd(),
		newConfigCmd(),
		newCompleteCmd(),
		// Explicit `version` subcommand preserves the historical contract
		// (`spored version` → "spored version X"); cobra's --version flag alone
		// would break callers like scripts/install-spored.sh.
		&cobra.Command{
			Use:   "version",
			Short: "Show version",
			Args:  cobra.NoArgs,
			Run: func(cmd *cobra.Command, args []string) {
				fmt.Printf("spored version %s\n", Version)
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

	log.Printf("spored v%s starting...", Version)

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

	agent, err := agent.NewAgent(ctx, prov)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}

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
	var checkComplete bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show configuration and monitoring status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return handleStatus(checkComplete)
		},
	}
	cmd.Flags().BoolVar(&checkComplete, "check-complete", false,
		"Exit with standardized codes: 0=complete 1=failed 2=running 3=error")
	return cmd
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

func handleStatus(checkComplete bool) error {
	// Create agent to get configuration and metrics
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		if checkComplete {
			os.Exit(3) // error querying status
		}
		return fmt.Errorf("initialize provider: %w", err)
	}

	ag, err := agent.NewAgent(ctx, prov)
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

	// Get identity
	identity := ag.GetIdentity()
	instanceID, region := identity.InstanceID, identity.Region

	// Get uptime
	uptime := ag.GetUptime()

	// Get metrics
	cpuUsage := ag.GetCPUUsage()
	networkBytes := ag.GetNetworkBytes()
	isIdle := ag.IsIdle()

	// Calculate time remaining for TTL
	var ttlRemaining time.Duration
	if config.TTL > 0 {
		ttlRemaining = config.TTL - uptime
		if ttlRemaining < 0 {
			ttlRemaining = 0
		}
	}

	// Check completion file
	completionFileExists := false
	if config.CompletionFile != "" {
		if _, err := os.Stat(config.CompletionFile); err == nil {
			completionFileExists = true
		}
	}

	// Calculate idle time
	var idleTime time.Duration
	if isIdle {
		idleTime = time.Since(ag.GetLastActivityTime())
	}

	// Calculate start time
	startTime := time.Now().Add(-uptime)

	// ── Identity ──────────────────────────────────────────────────────────────
	fmt.Printf("\n  %s  (%s)\n", identity.Name, instanceID)
	fmt.Printf("  %s\n\n", strings.Repeat("─", 46))

	// Use original launch time from tag if available; fall back to startTime
	launchTime := startTime
	if !config.LaunchTime.IsZero() {
		launchTime = config.LaunchTime
	}
	elapsed := time.Since(launchTime)
	computeSecs := ag.TotalComputeSeconds()
	computeTime := time.Duration(computeSecs) * time.Second
	stoppedTime := elapsed - computeTime
	if stoppedTime < 0 {
		stoppedTime = 0
	}

	// Use absolute deadline for TTL if available
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

	// ── Lifecycle ─────────────────────────────────────────────────────────────
	fmt.Printf("  Started:          %s\n", launchTime.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Printf("  Elapsed:          %s", formatDuration(elapsed))
	if computeTime > 0 && stoppedTime > 0 {
		fmt.Printf("  (%s compute · %s stopped)", formatDuration(computeTime), formatDuration(stoppedTime))
	}
	fmt.Println()

	if !terminateAt.IsZero() {
		fmt.Printf("  TTL:              %s remaining  (terminates %s)\n",
			formatDuration(ttlRemaining), terminateAt.UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		fmt.Println("  TTL:              none — instance will not auto-terminate")
	}

	if config.IdleTimeout > 0 {
		if isIdle {
			idleAction := "stops"
			if config.HibernateOnIdle {
				idleAction = "hibernates"
			}
			remaining := config.IdleTimeout - idleTime
			if remaining < 0 {
				remaining = 0
			}
			fmt.Printf("  Idle timeout:     %s  (%s for %s — %s in %s)\n",
				formatDuration(config.IdleTimeout), idleAction, formatDuration(idleTime),
				idleAction, formatDuration(remaining))
		} else {
			fmt.Printf("  Idle timeout:     %s  (currently active)\n", formatDuration(config.IdleTimeout))
		}
	}

	if config.OnComplete != "" {
		fileStatus := "watching"
		if completionFileExists {
			fileStatus = "✓ file present — acting on next check"
		}
		fmt.Printf("  On complete:      %s (%s)\n", config.OnComplete, fileStatus)
	}

	// ── Cost ──────────────────────────────────────────────────────────────────
	if config.PricePerHour > 0 {
		fmt.Println()
		// EBS cost: looked up from actual volumes at first start, stored in spawn:ebs-hourly-cost tag.
		// If not yet available, skip the storage line rather than showing a guess.
		ebsHourlyCost := config.EBSHourlyCost
		ebsCostKnown := ebsHourlyCost > 0
		computeCost := config.PricePerHour * computeTime.Hours()
		displayCompute := math.Round(computeCost*100) / 100

		var displayEBS float64
		if ebsCostKnown {
			displayEBS = math.Round(ebsHourlyCost*stoppedTime.Hours()*100) / 100
		}
		displayTotal := displayCompute + displayEBS

		fmt.Printf("  Compute cost:     $%.2f  (%s × $%.4f/hr)\n",
			displayCompute, formatDuration(computeTime), config.PricePerHour)
		if stoppedTime >= time.Minute {
			if ebsCostKnown {
				fmt.Printf("  Storage cost:     $%.2f  (%s × $%.4f/hr EBS)\n",
					displayEBS, formatDuration(stoppedTime), ebsHourlyCost)
			} else {
				fmt.Printf("  Storage cost:     not yet available  (%s stopped)\n",
					formatDuration(stoppedTime))
			}
		}
		fmt.Printf("  Cumulative cost:  $%.2f\n", displayTotal)

		elapsedHours := elapsed.Hours()
		if elapsedHours > 0 && ebsCostKnown {
			effectiveRate := displayTotal / elapsedHours
			savingsPct := (1 - effectiveRate/config.PricePerHour) * 100
			if savingsPct > 0.5 {
				fmt.Printf("  Effective rate:   $%.4f/hr  (%.0f%% lower than continuous on-demand)\n",
					effectiveRate, savingsPct)
			} else {
				fmt.Printf("  Effective rate:   $%.4f/hr\n", effectiveRate)
			}
		} else if elapsedHours > 0 && !ebsCostKnown {
			fmt.Println("  Effective rate:   not yet available  (EBS cost lookup pending)")
		}

		if config.CostLimit > 0 {
			remaining := config.CostLimit - displayTotal
			pct := (displayTotal / config.CostLimit) * 100
			fmt.Printf("  Cost limit:       $%.2f  ($%.2f used, %.0f%% — $%.2f remaining)\n",
				config.CostLimit, displayTotal, pct, remaining)
		}

		if ebsCostKnown {
			fmt.Printf("  On-demand rate:   $%.4f/hr compute  +  $%.4f/hr EBS storage  (%s)\n",
				config.PricePerHour, ebsHourlyCost, region)
		} else {
			fmt.Printf("  On-demand rate:   $%.4f/hr compute  +  EBS storage pending  (%s)\n",
				config.PricePerHour, region)
		}
		fmt.Println()
		fmt.Println("  * Cost figures are estimates. Definitive billing is from your cloud provider.")
	}

	// ── Live metrics (brief) ──────────────────────────────────────────────────
	fmt.Println()
	fmt.Printf("  CPU:              %.1f%%\n", cpuUsage)
	fmt.Printf("  Network:          %s/min\n", formatBytes(networkBytes))
	if config.PreStop != "" {
		fmt.Printf("  Pre-stop hook:    %s\n", config.PreStop)
	}
	fmt.Println()
	return nil
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

	ag, err := agent.NewAgent(ctx, prov)
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

	ag, err := agent.NewAgent(ctx, prov)
	if err != nil {
		return fmt.Errorf("initialize agent: %w", err)
	}

	config := ag.GetConfig()

	switch key {
	case "ttl":
		if config.TTL > 0 {
			fmt.Println(formatDuration(config.TTL))
		} else {
			fmt.Println("disabled")
		}
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

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		return fmt.Errorf("initialize provider: %w", err)
	}

	// Config set only works for EC2 instances
	if prov.GetProviderType() != "ec2" {
		return fmt.Errorf("config set is only supported on EC2 instances\nFor local instances, edit the config file: /etc/spawn/local.yaml")
	}

	ag, err := agent.NewAgent(ctx, prov)
	if err != nil {
		return fmt.Errorf("initialize agent: %w", err)
	}

	identity := ag.GetIdentity()
	instanceID, region := identity.InstanceID, identity.Region

	// Map key to tag name
	tagKey := ""
	switch key {
	case "ttl":
		// Validate duration
		if _, err := time.ParseDuration(value); err != nil && value != "0" {
			return fmt.Errorf("invalid duration: %s", value)
		}
		tagKey = "spawn:ttl"
	case "idle-timeout":
		if _, err := time.ParseDuration(value); err != nil && value != "0" {
			return fmt.Errorf("invalid duration: %s", value)
		}
		tagKey = "spawn:idle-timeout"
	case "on-complete":
		if value != "terminate" && value != "stop" && value != "hibernate" && value != "" {
			return fmt.Errorf("on-complete must be: terminate, stop, hibernate, or empty to disable")
		}
		tagKey = "spawn:on-complete"
	case "hibernate":
		if value != "true" && value != "false" {
			return fmt.Errorf("hibernate must be: true or false")
		}
		tagKey = "spawn:hibernate-on-idle"
	case "completion-file":
		tagKey = "spawn:completion-file"
	case "completion-delay":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("invalid duration: %s", value)
		}
		tagKey = "spawn:completion-delay"
	default:
		return fmt.Errorf("unknown config key: %s", key)
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

func handleConfigList() error {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		return fmt.Errorf("initialize provider: %w", err)
	}

	ag, err := agent.NewAgent(ctx, prov)
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
