package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Nothing in cmd/ may hand-roll a "wait for running" loop.
//
// pkg/aws.Client.WaitForRunning already wraps ec2.NewInstanceRunningWaiter and
// absorbs the #78 InvalidInstanceID.NotFound window via DescribeInstanceWithRetry.
// Three sites reimplemented it worse (#752), each in the same three ways:
//
//   - polling ListInstances(region, "running") — a WHOLE-REGION describe every
//     tick, cost scaling with fleet size, instead of one instance describe;
//   - `continue` on error with no ctx.Done() case at all, so Ctrl-C was ignored
//     for the full 90–150s;
//   - sleeping BEFORE the first check, charging 2–5s to an instance that was
//     already running.
//
// The tell is the pair: a sleep and a ListInstances(..., "running") inside one
// loop. A ListInstances poll for something else — the spawn:ready-url tag — is
// legitimate, because no SDK waiter covers a custom tag.
func TestNoHandRolledRunningWait(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	loopStart := regexp.MustCompile(`^\s*for\s`)
	sleepCall := regexp.MustCompile(`time\.Sleep\(`)
	runningList := regexp.MustCompile(`ListInstances\(ctx,[^,]+,\s*"running"\)`)

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(e.Name())
		if rerr != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if !loopStart.MatchString(line) {
				continue
			}
			// Scan the loop body for the offending pair.
			sawSleep, sawRunningList := false, false
			for j := i + 1; j < min(i+22, len(lines)); j++ {
				body := lines[j]
				if strings.HasPrefix(strings.TrimSpace(body), "//") {
					continue
				}
				if sleepCall.MatchString(body) {
					sawSleep = true
				}
				if runningList.MatchString(body) {
					sawRunningList = true
				}
			}
			if sawSleep && sawRunningList {
				t.Errorf("%s:%d hand-rolls a wait-for-running loop (a sleep plus "+
					"ListInstances(..., \"running\")).\n"+
					"Use client.WaitForRunning — it wraps the SDK waiter, absorbs the #78 "+
					"NotFound window, honours ctx, and does one instance describe instead "+
					"of a whole-region one per tick (#752).", e.Name(), i+1)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("found no loops in cmd/ — the scan is broken, not the package")
	}
	t.Logf("scanned %d loop(s)", checked)
}

// The polls that legitimately watch a custom tag must still honour ctx. A bare
// time.Sleep in a 60-iteration loop means a cancelled context is ignored for
// five minutes, which is what cmd/app.go and cmd/connect.go both did.
func TestReadyURLPollsHonourContext(t *testing.T) {
	for _, f := range []string{"app.go", "connect.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		if !strings.Contains(src, "scanDCVReady(") {
			continue // no ready-url poll in this file
		}
		// Each ready-url poll must select on ctx.Done().
		idx := strings.Index(src, "scanDCVReady(")
		window := src[max(0, idx-900):idx]
		if !strings.Contains(window, "<-ctx.Done()") {
			t.Errorf("%s polls for the ready-url without a ctx.Done() case — a cancelled "+
				"context would be ignored for the full poll budget (#752)", f)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
