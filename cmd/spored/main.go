package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
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

func main() {
	// Handle subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run-queue":
			handleRunQueue()
			os.Exit(0)
		case "run-pipeline-stage":
			handleRunPipelineStage()
			os.Exit(0)
		case "version":
			fmt.Printf("spored version %s\n", Version)
			os.Exit(0)
		case "status":
			handleStatus(os.Args[2:])
			os.Exit(0)
		case "reload":
			handleReload()
			os.Exit(0)
		case "config":
			handleConfig(os.Args[2:])
			os.Exit(0)
		case "complete":
			handleComplete(os.Args[2:])
			os.Exit(0)
		case "help", "--help", "-h":
			printHelp()
			os.Exit(0)
		}
	}

	// Setup logging
	logFile, err := os.OpenFile("/var/log/spored.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
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

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start monitoring
	go agent.Monitor(ctx)

	// Wait for signal
	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

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

func handleRunQueue() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: spored run-queue <queue-file>\n")
		os.Exit(1)
	}

	queueFile := os.Args[2]
	ctx := context.Background()

	runner, err := agent.NewQueueRunner(ctx, queueFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize queue runner: %v\n", err)
		os.Exit(1)
	}

	err = runner.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Queue execution failed: %v\n", err)
		os.Exit(1)
	}
}

func handleStatus(args []string) {
	checkComplete := false
	for _, a := range args {
		if a == "--check-complete" {
			checkComplete = true
		}
	}

	// Create agent to get configuration and metrics
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		if checkComplete {
			os.Exit(3) // error querying status
		}
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize provider: %v\n", err)
		os.Exit(1)
	}

	ag, err := agent.NewAgent(ctx, prov)
	if err != nil {
		if checkComplete {
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize agent: %v\n", err)
		os.Exit(1)
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

func handleReload() {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize provider: %v\n", err)
		os.Exit(1)
	}

	ag, err := agent.NewAgent(ctx, prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize agent: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Reloading configuration...")

	if err := ag.Reload(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to reload configuration: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✓ Configuration reloaded successfully")

	// Show new config
	config := ag.GetConfig()
	fmt.Println("\nCurrent configuration:")
	fmt.Printf("  TTL:              %v\n", config.TTL)
	fmt.Printf("  Idle Timeout:     %v\n", config.IdleTimeout)
	fmt.Printf("  On Complete:      %s\n", config.OnComplete)
	fmt.Printf("  Hibernate:        %v\n", config.HibernateOnIdle)
}

func handleConfig(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Error: config subcommand requires an action (get, set, list)\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  spored config get <key>\n")
		fmt.Fprintf(os.Stderr, "  spored config set <key> <value>\n")
		fmt.Fprintf(os.Stderr, "  spored config list\n")
		os.Exit(1)
	}

	action := args[0]
	switch action {
	case "get":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Error: config get requires a key\n")
			fmt.Fprintf(os.Stderr, "Usage: spored config get <key>\n")
			fmt.Fprintf(os.Stderr, "Keys: ttl, idle-timeout, on-complete, hibernate, completion-file, completion-delay\n")
			os.Exit(1)
		}
		handleConfigGet(args[1])

	case "set":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "Error: config set requires a key and value\n")
			fmt.Fprintf(os.Stderr, "Usage: spored config set <key> <value>\n")
			os.Exit(1)
		}
		handleConfigSet(args[1], args[2])

	case "list":
		handleConfigList()

	default:
		fmt.Fprintf(os.Stderr, "Error: unknown config action: %s\n", action)
		fmt.Fprintf(os.Stderr, "Valid actions: get, set, list\n")
		os.Exit(1)
	}
}

func handleConfigGet(key string) {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize provider: %v\n", err)
		os.Exit(1)
	}

	ag, err := agent.NewAgent(ctx, prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize agent: %v\n", err)
		os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "Error: unknown config key: %s\n", key)
		fmt.Fprintf(os.Stderr, "Valid keys: ttl, idle-timeout, on-complete, hibernate, completion-file, completion-delay\n")
		os.Exit(1)
	}
}

func handleConfigSet(key, value string) {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize provider: %v\n", err)
		os.Exit(1)
	}

	// Config set only works for EC2 instances
	if prov.GetProviderType() != "ec2" {
		fmt.Fprintf(os.Stderr, "Error: config set is only supported on EC2 instances\n")
		fmt.Fprintf(os.Stderr, "For local instances, edit the config file: /etc/spawn/local.yaml\n")
		os.Exit(1)
	}

	ag, err := agent.NewAgent(ctx, prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize agent: %v\n", err)
		os.Exit(1)
	}

	identity := ag.GetIdentity()
	instanceID, region := identity.InstanceID, identity.Region

	// Map key to tag name
	tagKey := ""
	switch key {
	case "ttl":
		// Validate duration
		if _, err := time.ParseDuration(value); err != nil && value != "0" {
			fmt.Fprintf(os.Stderr, "Error: invalid duration: %s\n", value)
			os.Exit(1)
		}
		tagKey = "spawn:ttl"
	case "idle-timeout":
		if _, err := time.ParseDuration(value); err != nil && value != "0" {
			fmt.Fprintf(os.Stderr, "Error: invalid duration: %s\n", value)
			os.Exit(1)
		}
		tagKey = "spawn:idle-timeout"
	case "on-complete":
		if value != "terminate" && value != "stop" && value != "hibernate" && value != "" {
			fmt.Fprintf(os.Stderr, "Error: on-complete must be: terminate, stop, hibernate, or empty to disable\n")
			os.Exit(1)
		}
		tagKey = "spawn:on-complete"
	case "hibernate":
		if value != "true" && value != "false" {
			fmt.Fprintf(os.Stderr, "Error: hibernate must be: true or false\n")
			os.Exit(1)
		}
		tagKey = "spawn:hibernate-on-idle"
	case "completion-file":
		tagKey = "spawn:completion-file"
	case "completion-delay":
		if _, err := time.ParseDuration(value); err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid duration: %s\n", value)
			os.Exit(1)
		}
		tagKey = "spawn:completion-delay"
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown config key: %s\n", key)
		os.Exit(1)
	}

	// Get EC2 client
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to load AWS config: %v\n", err)
		os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "Error: Failed to update tag: %v\n", err)
		os.Exit(1)
	}

	// Reload configuration
	fmt.Println("Reloading configuration...")
	if err := ag.Reload(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to reload configuration: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Configuration updated: %s = %s\n", key, value)
}

func handleConfigList() {
	ctx := context.Background()

	prov, err := provider.NewProvider(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize provider: %v\n", err)
		os.Exit(1)
	}

	ag, err := agent.NewAgent(ctx, prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize agent: %v\n", err)
		os.Exit(1)
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

func handleComplete(args []string) {
	// Default completion file
	completionFile := "/tmp/SPAWN_COMPLETE"
	var status, message string

	// Simple flag parsing
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--file", "-f":
			if i+1 < len(args) {
				completionFile = args[i+1]
				i++
			}
		case "--status", "-s":
			if i+1 < len(args) {
				status = args[i+1]
				i++
			}
		case "--message", "-m":
			if i+1 < len(args) {
				message = args[i+1]
				i++
			}
		case "--help", "-h":
			fmt.Println("Usage: spored complete [options]")
			fmt.Println()
			fmt.Println("Signal completion to trigger on-complete action (terminate/stop/hibernate)")
			fmt.Println()
			fmt.Println("Options:")
			fmt.Println("  -f, --file PATH      Completion file path (default: /tmp/SPAWN_COMPLETE)")
			fmt.Println("  -s, --status STATUS  Optional status (e.g., 'success', 'failed')")
			fmt.Println("  -m, --message MSG    Optional message")
			fmt.Println("  -h, --help           Show this help")
			fmt.Println()
			fmt.Println("Examples:")
			fmt.Println("  spored complete")
			fmt.Println("  spored complete --status success")
			fmt.Println("  spored complete --status success --message 'Job completed successfully'")
			os.Exit(0)
		}
	}

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
			fmt.Fprintf(os.Stderr, "Error encoding metadata: %v\n", err)
			os.Exit(1)
		}
	}

	// Write completion file
	if err := os.WriteFile(completionFile, content, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing completion file: %v\n", err)
		os.Exit(1)
	}

	// Success message
	fmt.Printf("✓ Completion signal sent to %s\n", completionFile)
	if status != "" {
		fmt.Printf("  Status: %s\n", status)
	}
	if message != "" {
		fmt.Printf("  Message: %s\n", message)
	}
}

func printHelp() {
	fmt.Printf("spored v%s - Spawn EC2 instance agent\n\n", Version)
	fmt.Println("Usage:")
	fmt.Println("  spored                Run as daemon (monitors instance lifecycle)")
	fmt.Println("  spored run-queue      Execute a batch job queue")
	fmt.Println("  spored status         Show configuration and monitoring status")
	fmt.Println("  spored reload         Reload configuration from EC2 tags")
	fmt.Println("  spored config         Manage configuration settings")
	fmt.Println("  spored complete       Signal completion to trigger on-complete action")
	fmt.Println("  spored version        Show version")
	fmt.Println("  spored help           Show this help")
	fmt.Println()
	fmt.Println("Config Commands:")
	fmt.Println("  spored config get <key>         Get a configuration value")
	fmt.Println("  spored config set <key> <value> Set a configuration value")
	fmt.Println("  spored config list              List all configuration")
	fmt.Println()
	fmt.Println("Config Keys:")
	fmt.Println("  ttl               Time-to-live (e.g., 24h, 2h30m)")
	fmt.Println("  idle-timeout      Idle timeout duration")
	fmt.Println("  on-complete       Action on completion (terminate|stop|hibernate)")
	fmt.Println("  hibernate         Hibernate on idle (true|false)")
	fmt.Println("  completion-file   Path to completion signal file")
	fmt.Println("  completion-delay  Grace period before action")
	fmt.Println()
	fmt.Println("Daemon Mode:")
	fmt.Println("  Runs as a systemd service and monitors:")
	fmt.Println("  - Spot interruption warnings")
	fmt.Println("  - Completion signals (file-based)")
	fmt.Println("  - TTL (time-to-live) expiration")
	fmt.Println("  - Idle timeout detection")
	fmt.Println()
	fmt.Println("  Configuration is loaded from EC2 instance tags (set by spawn launch).")
	fmt.Println()
	fmt.Println("Complete Subcommand:")
	fmt.Println("  Signal that your workload has finished. If --on-complete was set during")
	fmt.Println("  launch, the instance will terminate/stop/hibernate after a grace period.")
	fmt.Println()
	fmt.Println("  Examples:")
	fmt.Println("    spored complete")
	fmt.Println("    spored complete --status success")
	fmt.Println("    spored complete --status success --message 'Job completed'")
	fmt.Println()
	fmt.Println("For more information: https://github.com/scttfrdmn/spore-host")
}

func handleRunPipelineStage() {
	ctx := context.Background()

	log.Println("Checking if instance is part of a pipeline...")

	// Check if this is a pipeline instance
	isPipeline, err := pipeline.IsPipelineInstance(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking pipeline status: %v\n", err)
		os.Exit(1)
	}

	if !isPipeline {
		fmt.Fprintf(os.Stderr, "Error: This instance is not part of a pipeline\n")
		os.Exit(1)
	}

	log.Println("Instance is part of a pipeline, initializing stage runner...")

	// Create stage runner
	runner, err := pipeline.NewStageRunner(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize stage runner: %v\n", err)
		os.Exit(1)
	}

	log.Println("Running pipeline stage...")

	// Run stage
	if err := runner.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Pipeline stage execution failed: %v\n", err)
		os.Exit(1)
	}

	log.Println("Pipeline stage completed successfully")
}
