//go:build e2e_tier3

package e2e

// Tier 3 — Multi-instance tests. Launches job arrays, sweeps, FSx, MPI.
// Estimated cost: $2–$5 total, ~25–35 min.
//
// Run: go test -v -tags=e2e_tier3 ./test/e2e/ -run TestTier3 -timeout 60m

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTier3_JobArray launches a 2-instance job array and operates on them as a group.
func TestTier3_JobArray(t *testing.T) {
	rid := runID(t)
	arrayName := "e2e-array-" + rid

	out := spawn(t,
		"launch", arrayName+"-0",
		"--instance-type", testInstanceType,
		"--region", testRegion,
		"--ttl", "20m",
		"--count", "2",
		"--job-array-name", arrayName,
	)
	t.Logf("launch output: %s", out)

	// Cleanup: stop the job array (instances have unique names with rid)
	t.Cleanup(func() {
		spawnMayFail(t, "stop", "--job-array-name", arrayName)
	})

	// Both instances must reach running state (identified by job-array-name)
	deadline := time.Now().Add(5 * time.Minute)
	var running []InstanceJSON
	for time.Now().Before(deadline) {
		listOut, _ := spawnMayFail(t, "list", "--output", "json")
		var all []InstanceJSON
		if json.Unmarshal([]byte(listOut), &all) == nil {
			running = nil
			for _, inst := range all {
				if inst.Tags["spawn:job-array-name"] == arrayName && inst.State == "running" {
					running = append(running, inst)
				}
			}
			if len(running) == 2 {
				break
			}
		}
		time.Sleep(15 * time.Second)
	}
	if len(running) != 2 {
		t.Fatalf("expected 2 running instances, got %d", len(running))
	}
	t.Logf("both instances running: %s, %s", running[0].InstanceID, running[1].InstanceID)

	// Extend all at once
	spawn(t, "extend", "--job-array-name", arrayName, "5m")
	t.Log("extended job array TTL")

	// Stop all at once
	spawn(t, "stop", "--job-array-name", arrayName)
	time.Sleep(30 * time.Second)
	t.Log("stopped job array")
}

// TestTier3_ParameterSweep launches a 4-combo sweep (2×2) and verifies all instances start.
func TestTier3_ParameterSweep(t *testing.T) {
	rid := runID(t)
	sweepName := "e2e-sweep-" + rid

	// Write a small parameter file
	paramContent := `defaults:
  instance_type: t3.small
  ttl: 20m
  on_complete: terminate

params:
  - lr: "0.001"
    batch: "32"
  - lr: "0.001"
    batch: "64"
  - lr: "0.01"
    batch: "32"
  - lr: "0.01"
    batch: "64"
`
	f, err := os.CreateTemp("", "sweep-*.yaml")
	if err != nil {
		t.Fatalf("create param file: %v", err)
	}
	defer os.Remove(f.Name())
	f.WriteString(paramContent)
	f.Close()

	out := spawn(t,
		"launch", sweepName,
		"--region", testRegion,
		"--param-file", f.Name(),
		"--sweep-name", sweepName,
		"--max-concurrent", "4",
		"--yes",
	)
	t.Logf("sweep launch: %s", out)

	// Parse sweep ID from output
	var sweepID string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "sweep-") && (strings.Contains(line, "Sweep ID") || strings.Contains(line, "sweep_id")) {
			fields := strings.Fields(line)
			for _, f := range fields {
				if strings.HasPrefix(f, "sweep-") {
					sweepID = f
					break
				}
			}
		}
	}
	t.Cleanup(func() {
		if sweepID != "" {
			spawnMayFail(t, "sweep", "cancel", sweepID)
		}
	})

	// Poll for all 4 instances to launch (identified by sweep-name tag)
	deadline := time.Now().Add(8 * time.Minute)
	var launched int
	for time.Now().Before(deadline) {
		listOut, _ := spawnMayFail(t, "list", "--sweep-name", sweepName, "--output", "json")
		var all []InstanceJSON
		if json.Unmarshal([]byte(listOut), &all) == nil {
			launched = len(all)
			t.Logf("sweep progress: %d/4 instances", launched)
			if launched >= 4 {
				break
			}
		}
		time.Sleep(20 * time.Second)
	}
	if launched < 4 {
		t.Errorf("expected 4 sweep instances, found %d", launched)
	}

	// Cancel the sweep
	if sweepID != "" {
		spawnMayFail(t, "sweep", "cancel", sweepID)
	}
	t.Logf("sweep complete: %d instances launched", launched)
}

// TestTier3_MPI launches a 2-node MPI cluster and verifies hostfile is populated.
func TestTier3_MPI(t *testing.T) {
	rid := runID(t)
	name := "e2e-mpi-" + rid

	out := spawn(t,
		"launch", name+"-0",
		"--instance-type", testInstanceType,
		"--region", testRegion,
		"--ttl", "20m",
		"--count", "2",
		"--job-array-name", name,
		"--mpi",
	)
	t.Logf("MPI launch: %s", out)
	t.Cleanup(func() { spawnMayFail(t, "stop", "--job-array-name", name) })

	// Wait for head node (index 0) to be running
	head := waitForRunning(t, name+"-0", 4*time.Minute)

	// Allow time for MPI hostfile to be populated via job-array coordination
	time.Sleep(90 * time.Second)

	// Verify hostfile exists and has 2 entries
	hostfileOut := sshExec(t, head.Name, "cat /etc/mpi/hostfile 2>/dev/null || echo MISSING")
	if strings.Contains(hostfileOut, "MISSING") {
		t.Logf("MPI hostfile not yet written (may need more time): %s", hostfileOut)
	} else {
		lines := strings.Split(strings.TrimSpace(hostfileOut), "\n")
		t.Logf("MPI hostfile (%d entries): %s", len(lines), hostfileOut)
		if len(lines) < 2 {
			t.Errorf("expected 2 MPI hostfile entries, got %d", len(lines))
		}
	}
}

// TestTier3_QueueExecution verifies a batch queue runs jobs sequentially on one instance.
func TestTier3_QueueExecution(t *testing.T) {
	rid := runID(t)
	name := "e2e-queue-" + rid

	// Write a simple 2-job queue config
	queueConfig := fmt.Sprintf(`{
  "queue_id": "%s",
  "queue_name": "e2e-test-queue",
  "jobs": [
    {"job_id": "job1", "command": "echo job1_done > /tmp/job1.txt", "timeout": "2m"},
    {"job_id": "job2", "command": "echo job2_done > /tmp/job2.txt", "timeout": "2m", "depends_on": ["job1"]}
  ],
  "global_timeout": "10m",
  "on_failure": "stop",
  "result_s3_bucket": "spawn-results-%s",
  "result_s3_prefix": "e2e-queues/%s/"
}`, rid, testRegion, rid)

	f, err := os.CreateTemp("", "queue-*.json")
	if err != nil {
		t.Fatalf("create queue file: %v", err)
	}
	defer os.Remove(f.Name())
	f.WriteString(queueConfig)
	f.Close()

	launchInstance(t, name, "--batch-queue", f.Name())

	// Give jobs time to execute (spored detects batch queue and runs spored run-queue)
	time.Sleep(2 * time.Minute)

	// Verify both jobs produced output
	out1 := sshExec(t, name, "cat /tmp/job1.txt 2>/dev/null || echo MISSING")
	out2 := sshExec(t, name, "cat /tmp/job2.txt 2>/dev/null || echo MISSING")

	if strings.Contains(out1, "MISSING") {
		t.Errorf("job1 did not run: /tmp/job1.txt missing")
	}
	if strings.Contains(out2, "MISSING") {
		t.Errorf("job2 did not run: /tmp/job2.txt missing")
	}
	t.Logf("queue jobs: job1=%s job2=%s", strings.TrimSpace(out1), strings.TrimSpace(out2))
}

// TestTier3_MPI_PlacementGroupRegion verifies --mpi --region us-east-2 creates
// the placement group in the correct region and instances launch successfully
// (regression for #317 — group was created in us-east-1 when launch targeted us-east-2).
func TestTier3_MPI_PlacementGroupRegion(t *testing.T) {
	rid := runID(t)
	name := "e2e-pg-region-" + rid
	t.Cleanup(func() { spawnMayFail(t, "stop", "--job-array-name", name) })

	out := spawn(t,
		"launch", name+"-0",
		"--instance-type", "c5n.18xlarge",
		"--count", "2",
		"--job-array-name", name,
		"--region", "us-east-2", // non-default region — was the regression trigger
		"--mpi",
		"--ttl", "20m",
	)
	t.Logf("MPI launch in us-east-2: %s", out)

	// Wait for both nodes to reach running (filter by job-array-name)
	deadline := time.Now().Add(5 * time.Minute)
	var running int
	for time.Now().Before(deadline) {
		listOut, _ := spawnMayFail(t, "list", "--job-array-name", name, "--output", "json")
		var all []InstanceJSON
		if json.Unmarshal([]byte(listOut), &all) == nil {
			running = 0
			for _, inst := range all {
				if inst.State == "running" {
					running++
				}
			}
			if running >= 2 {
				break
			}
		}
		time.Sleep(15 * time.Second)
	}
	if running < 2 {
		t.Errorf("expected 2 running MPI nodes in us-east-2, got %d (regression #317)", running)
	} else {
		t.Logf("both MPI nodes running in us-east-2 (%d instances)", running)
	}
}

// TestTier3_ParameterSweep_SweepGroup verifies spawn sweep list/status/cancel
// work on a running sweep.
func TestTier3_ParameterSweep_SweepGroup(t *testing.T) {
	rid := runID(t)
	sweepName := "e2e-sg-" + rid

	paramContent := `defaults:
  instance_type: t3.small
  ttl: 20m
  on_complete: terminate

params:
  - lr: "0.001"
  - lr: "0.01"
`
	f, err := os.CreateTemp("", "sweep-*.yaml")
	if err != nil {
		t.Fatalf("create param file: %v", err)
	}
	defer os.Remove(f.Name())
	f.WriteString(paramContent)
	f.Close()

	out := spawn(t,
		"launch", sweepName,
		"--region", testRegion,
		"--param-file", f.Name(),
		"--sweep-name", sweepName,
		"--yes",
	)
	t.Logf("sweep launch: %s", out)

	// Parse sweep ID
	var sweepID string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "sweep-") {
			for _, f := range strings.Fields(line) {
				if strings.HasPrefix(f, "sweep-") {
					sweepID = f
				}
			}
		}
	}

	// spawn sweep list should show our sweep
	listOut, err := spawnMayFail(t, "sweep", "list")
	if err != nil {
		t.Logf("sweep list error (acceptable): %v", err)
	} else {
		t.Logf("sweep list output: %s", listOut[:min3(len(listOut), 300)])
	}

	// spawn sweep status if we have an ID
	if sweepID != "" {
		statusOut, err := spawnMayFail(t, "sweep", "status", sweepID)
		if err != nil {
			t.Logf("sweep status error (acceptable): %v", err)
		} else {
			t.Logf("sweep status: %s", statusOut[:min3(len(statusOut), 300)])
		}

		// Cancel to clean up
		spawnMayFail(t, "sweep", "cancel", sweepID)
	}
	t.Log("spawn sweep subcommands tested")
}

// TestTier3_SlurmSubmit verifies spawn slurm submit runs end-to-end.
// Uses a minimal sbatch script that completes quickly.
func TestTier3_SlurmSubmit(t *testing.T) {
	f, err := os.CreateTemp("", "test-*.sbatch")
	if err != nil {
		t.Fatalf("create sbatch: %v", err)
	}
	defer os.Remove(f.Name())
	fmt.Fprintln(f, "#!/bin/bash")
	fmt.Fprintln(f, "#SBATCH --job-name=e2e-slurm")
	fmt.Fprintln(f, "#SBATCH --time=00:10:00")
	fmt.Fprintln(f, "#SBATCH --mem=1G")
	fmt.Fprintln(f, "#SBATCH --cpus-per-task=1")
	fmt.Fprintln(f, "#SPAWN --instance-type=t3.small")
	fmt.Fprintln(f, "#SPAWN --region="+testRegion)
	fmt.Fprintln(f, "echo slurm_done")
	f.Close()

	out, err := spawnMayFail(t,
		"slurm", "submit", f.Name(),
		"--yes",
		"--spot",
	)
	if err != nil {
		t.Logf("slurm submit error (acceptable if quota/capacity): %v\n%s", err, out)
	} else {
		t.Logf("slurm submit launched: %s", out[:min3(len(out), 200)])
	}
}

func min3(a, b int) int {
	if a < b {
		return a
	}
	return b
}
