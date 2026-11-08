package taskproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A cost-limit termination's terminal record carries cost_basis, from the JSON
// spored passes in CostBasisEnv (spawn#1497); without it the record has no
// such field. Parsed, not string-matched: the hook builds the record with
// printf, so a stray comma is a real risk.
func TestFlushHookRecordsTheCostBasis(t *testing.T) {
	skipOnWindows(t, "runs the Linux flush hook under bash")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	_ = os.Remove(CompletionRecordPath())
	script := genFlush(flushSpec(), "results-bucket", "us-west-2", "run-1497")

	run := func(env ...string) *CompletionRecord {
		t.Helper()
		_ = os.Remove(localPath(terminalFileName))
		dir := t.TempDir()
		path := filepath.Join(dir, "task-flush.sh")
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture, needs +x
			t.Fatal(err)
		}
		bin := t.TempDir()
		writeFakeExe(t, filepath.Join(bin, "aws"), "#!/bin/bash\n"+awsHeadMissing)
		cmd := exec.Command("bash", path, string(ExitCostLimitExceeded))
		cmd.Env = append(append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH")), env...)
		out, _ := cmd.CombinedOutput()
		data, err := os.ReadFile(localPath(terminalFileName))
		if err != nil {
			t.Fatalf("no terminal record: %v\n%s", err, out)
		}
		rec, err := ParseCompletionRecord(data)
		if err != nil {
			t.Fatalf("%v\n%s", err, data)
		}
		return rec
	}

	rec := run(CostBasisEnv + `={"cost_limit":7,"accrued":7.01,"compute_basis":"on-demand","compute_per_hour":3.418,` +
		`"rate_collected_at":"2026-11-08T10:00:00Z","rate_source":"live","fallback":"could not read the Spot price: AccessDenied"}`)
	if rec.TerminalReason != ExitCostLimitExceeded {
		t.Errorf("terminal_reason = %q", rec.TerminalReason)
	}
	b := rec.CostBasis
	if b == nil || b.ComputeBasis != "on-demand" || b.ComputePerHour != 3.418 || b.Fallback == "" ||
		b.RateCollectedAt != "2026-11-08T10:00:00Z" || b.RateSource != "live" || b.Limit != 7 {
		t.Errorf("cost_basis = %+v", b)
	}
	if rec := run(); rec.CostBasis != nil {
		t.Errorf("with no %s the record has cost_basis %+v", CostBasisEnv, rec.CostBasis)
	}
}
