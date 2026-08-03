package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spore-host/spawn/pkg/buildinfo"
	"github.com/spore-host/spawn/pkg/orchestrator"
)

// Version is injected at build time (-X main.Version=...). It was a CONST
// pinned at "0.1.0", which an -X ldflag cannot write at all — so this binary
// could only ever report 0.1.0, no matter how it was built. Now a var, and empty
// by default so the Go build stamp fills it in (see pkg/buildinfo).
//
// Nothing in .goreleaser.yaml or the Makefile builds this binary today; it is
// built by hand when used. That makes the build stamp the only version source it
// has, which is exactly the case the fallback is for.
var Version = ""

// version is the resolved version spawn-orchestrator reports.
func version() string { return buildinfo.Version(Version) }

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "run":
		runOrchestrator()
	case "status":
		showStatus()
	case "version":
		fmt.Printf("spawn-orchestrator version %s\n", version())
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func runOrchestrator() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: spawn-orchestrator run <config-file>\n")
		os.Exit(1)
	}

	configFile := os.Args[2]

	// Load configuration
	cfg, err := orchestrator.LoadConfig(configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Setup logging
	logFile, err := os.OpenFile("/var/log/spawn-orchestrator.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Warning: Could not open log file: %v", err)
		log.SetOutput(os.Stderr)
	} else {
		defer func() { _ = logFile.Close() }()
		log.SetOutput(logFile)
	}

	log.Printf("spawn-orchestrator v%s starting...", version())
	log.Printf("Job array: %s", cfg.JobArrayID)
	log.Printf("Queue: %s", cfg.QueueURL)
	log.Printf("Mode: %s", cfg.BurstPolicy.Mode)

	// Create orchestrator
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := orchestrator.New(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to create orchestrator: %v", err)
	}

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start orchestrator in goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- orch.Run(ctx)
	}()

	// Wait for signal or error
	select {
	case sig := <-sigChan:
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	case err := <-errChan:
		if err != nil {
			log.Fatalf("Orchestrator error: %v", err)
		}
	}

	log.Printf("Orchestrator stopped")
}

func showStatus() {
	fmt.Println("spawn-orchestrator status")
	fmt.Println("(Status reporting not yet implemented)")
	// TODO: Query orchestrator state via file or API
}

func printUsage() {
	fmt.Printf("spawn-orchestrator v%s - Automatic cloud burst orchestrator\n\n", version())
	fmt.Println("Usage:")
	fmt.Println("  spawn-orchestrator run <config-file>    Start orchestrator daemon")
	fmt.Println("  spawn-orchestrator status               Show orchestrator status")
	fmt.Println("  spawn-orchestrator version              Show version")
	fmt.Println("  spawn-orchestrator help                 Show this help")
	fmt.Println()
	fmt.Println("The orchestrator monitors queue depth and automatically bursts to")
	fmt.Println("AWS EC2 when capacity is needed, then scales down when queue drains.")
	fmt.Println()
	fmt.Println("Example config file:")
	fmt.Println("  job_array_id: my-pipeline")
	fmt.Println("  queue_url: https://sqs.us-east-1.amazonaws.com/.../my-queue")
	fmt.Println("  burst_policy:")
	fmt.Println("    mode: auto")
	fmt.Println("    queue_depth_threshold: 100")
	fmt.Println("    max_cloud_instances: 50")
	fmt.Println("    instance_type: c5.4xlarge")
	fmt.Println("    spot: true")
}
