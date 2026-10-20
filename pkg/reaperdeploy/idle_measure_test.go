package reaperdeploy

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// msNow is now truncated to the millisecond CloudWatch timestamps carry, so
// durations come out exact.
func msNow() time.Time { return time.UnixMilli(time.Now().UnixMilli()) }

// #1155: IdleFor returned the whole 30-day window for any reaper that had run
// without reaping, so a reaper deployed an hour ago read as idle for 30 days
// and `teardown --if-idle-for 720h` removed it.
func TestIdleForMeasuresFromTheFirstRun(t *testing.T) {
	now := msNow()
	d := idleDeployer(&fakeLogs{events: runs(now.Add(-time.Hour), now.Add(-time.Minute), 10*time.Minute)})
	got := d.DetectIdleness(context.Background(), now)
	if !got.Determined || !got.Ran || got.EverWorked {
		t.Fatalf("want determined, ran, never worked; got %+v", got)
	}
	if idle := got.IdleFor(now); idle != time.Hour {
		t.Errorf("a reaper first run an hour ago is idle for %s, want 1h", idle)
	}
	if got.IdleFor(now) >= 720*time.Hour {
		t.Error("--if-idle-for 720h would remove a reaper deployed an hour ago")
	}
	advice := IdlenessAdvice(got, now)
	if strings.Contains(advice, "30 days") || !strings.Contains(advice, "1 hour") {
		t.Errorf("status text should state the measured hour, not the window:\n%s", advice)
	}
}

// A reaper running for the whole window is idle for (about) the whole window,
// so --if-idle-for at or below it can trigger.
func TestIdleForAReaperRunningAllWindow(t *testing.T) {
	now := msNow()
	d := idleDeployer(&fakeLogs{events: runs(now.Add(-45*24*time.Hour), now, 6*time.Hour)})
	got := d.DetectIdleness(context.Background(), now).IdleFor(now)
	if got < idleWindow-6*time.Hour || got > idleWindow {
		t.Errorf("IdleFor = %s, want within one interval of the %s window", got, idleWindow)
	}
}

// Runs logged before this deployment's creation belong to an earlier one (a
// torn-down reaper whose log group was left behind), so they must not count.
func TestIdleForIsBoundedByTheDeploymentsCreation(t *testing.T) {
	now := msNow()
	created := now.Add(-2 * time.Hour)
	d := &Deployer{
		Logs: &fakeLogs{events: runs(now.Add(-10*24*time.Hour), now, time.Hour)},
		IAM:  &fakeIAM{roleExists: true, created: created},
	}
	if got := d.DetectIdleness(context.Background(), now).IdleFor(now); got != 2*time.Hour {
		t.Errorf("IdleFor = %s, want 2h: measured from the role's creation, not an older run", got)
	}
}

// If the deployment's age cannot be read, a run-but-idle reaper is
// undetermined, never idle for the whole of whatever logs exist.
func TestIdlenessWithoutACreationDateIsUndetermined(t *testing.T) {
	now := msNow()
	d := &Deployer{
		Logs: &fakeLogs{events: runs(now.Add(-10*24*time.Hour), now, time.Hour)},
		IAM:  &fakeIAM{roleExists: true}, // no CreateDate
	}
	got := d.DetectIdleness(context.Background(), now)
	if got.Determined || got.IdleFor(now) != 0 {
		t.Errorf("want undetermined, got %+v", got)
	}
	if (&Deployer{Logs: &fakeLogs{}}).DetectIdleness(context.Background(), now).Determined {
		t.Error("a Deployer with no IAM client must report undetermined")
	}
}

// IdleFor's own invariant: Ran with no FirstRan claims nothing.
func TestIdleForWithoutAFirstRunIsZero(t *testing.T) {
	if got := (Idleness{Determined: true, Ran: true}).IdleFor(time.Now()); got != 0 {
		t.Errorf("IdleFor with Ran but no FirstRan = %s, want 0", got)
	}
}

var (
	// A log call's format string. Matched as a call so a sentinel quoted in a
	// comment does not count.
	logFormat = regexp.MustCompile(`log\.Print(?:f|ln)?\("([^"]*)"`)
	// A sentinel is an all-caps word of 4+ letters among a format's first three
	// tokens: "REAPED fsx %s", "DNS lease: EXPIRING %s", "DNS lease WOULD %s".
	capsWord = regexp.MustCompile(`^[A-Z]{4,}:?$`)
	// The alarm sentinels the reaper logs through constants (failures.go).
	sentinelConst = regexp.MustCompile(`sentinel\w*\s*=\s*"([^"]+)"`)
)

// nonActionSentinels are the leading words of reaper log lines that record
// something it did NOT do: a dry run, a report, an unparseable input.
var nonActionSentinels = map[string]bool{"WOULD": true, "UNMANAGED": true, "UNPARSEABLE": true}

type reaperLogLine struct {
	file, format, sentinel string
}

// discoverReaperLogLines reads the reaper's source for every log line led by
// a sentinel word. Discovered rather than listed, so a sentinel added to the
// reaper later is seen by this test without anyone remembering it.
func discoverReaperLogLines(t *testing.T) ([]reaperLogLine, []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "lambda", "ttl-reaper", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []reaperLogLine
	var alarms []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range logFormat.FindAllStringSubmatch(string(b), -1) {
			toks := strings.Fields(m[1])
			for i := 0; i < len(toks) && i < 3; i++ {
				if capsWord.MatchString(toks[i]) {
					lines = append(lines, reaperLogLine{filepath.Base(f), m[1], strings.TrimSuffix(toks[i], ":")})
					break
				}
			}
		}
		for _, m := range sentinelConst.FindAllStringSubmatch(string(b), -1) {
			alarms = append(alarms, m[1])
		}
	}
	if len(lines) < 5 || len(alarms) == 0 {
		t.Fatalf("found %d sentinel log lines and %d alarm sentinels in lambda/ttl-reaper — the discovery is broken",
			len(lines), len(alarms))
	}
	return lines, alarms
}

// render fills a format's verbs so the line reads like what CloudWatch holds,
// with the log package's default timestamp prefix.
func render(format string) string {
	verbs := regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)
	return "2026/10/19 12:00:00 " + verbs.ReplaceAllString(format, "x")
}

// TestActionSentinelsCoverTheReapersLogLines is the #1155 gate: idleness must
// count every action the reaper logs, not only REAPED. Every sentinel-led log
// line in the reaper must be classified, and every action line, rendered as
// the reaper writes it, must make DetectIdleness report work.
func TestActionSentinelsCoverTheReapersLogLines(t *testing.T) {
	lines, alarms := discoverReaperLogLines(t)
	actions := map[string]bool{}
	for _, s := range ActionSentinels {
		actions[s] = true
	}
	seenAction := map[string]bool{}
	for _, l := range lines {
		switch {
		case actions[l.sentinel]:
			seenAction[l.sentinel] = true
			now := msNow()
			msg := render(l.format)
			d := idleDeployer(&fakeLogs{events: append(runs(now.Add(-time.Hour), now, 10*time.Minute),
				logEvent{now.Add(-time.Minute), msg})})
			if got := d.DetectIdleness(context.Background(), now); !got.EverWorked {
				t.Errorf("%s: the reaper logs %q when it acts, and idleness did not count it as work", l.file, msg)
			}
		case nonActionSentinels[l.sentinel]:
			now := msNow()
			msg := render(l.format)
			d := idleDeployer(&fakeLogs{events: append(runs(now.Add(-time.Hour), now, 10*time.Minute),
				logEvent{now.Add(-time.Minute), msg})})
			if got := d.DetectIdleness(context.Background(), now); got.EverWorked {
				t.Errorf("%s: %q records something NOT done, and idleness counted it as work", l.file, msg)
			}
		default:
			t.Errorf("%s logs a line led by %q (%q) that is neither an ActionSentinel nor a known non-action — "+
				"classify it, or idleness will misjudge it", l.file, l.sentinel, l.format)
		}
	}
	for s := range actions {
		if !seenAction[s] {
			t.Errorf("ActionSentinels lists %q but the reaper logs no line led by it", s)
		}
	}
	// The alarm lines are not work, even when they contain an action word
	// ("SPOT REPLACEMENT NOT TERMINATED").
	for _, a := range alarms {
		if isActionLine(render(a + ": x")) {
			t.Errorf("alarm sentinel %q would count as work done", a)
		}
	}
}
