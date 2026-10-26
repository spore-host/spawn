package agent

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// noticeProvider is a stubProvider whose instance has a Spot notice.
type noticeProvider struct {
	stubProvider
	info *provider.InterruptionInfo
}

func (n *noticeProvider) CheckSpotInterruption(_ context.Context) (*provider.InterruptionInfo, error) {
	return n.info, nil
}

// spotPreStopRun is one Spot notice handled end to end with a fake clock. The
// fake task-flush hook "takes" flushTakes: it advances the clock by writing
// the seconds to a file the clock reads, so no test waits on a wall clock.
type spotPreStopRun struct {
	hookArgv []string // the task flush hook's argv, one entry per call
	log      string   // spored's log
	events   []string // notifications, "event: detail"
	ranHook  bool     // the user's PreStop command ran
}

func runSpotNotice(t *testing.T, declared int, flushTakes time.Duration, preStopTimeout time.Duration) spotPreStopRun {
	t.Helper()
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir) // the notice handler writes its notification files there
	clockFile := filepath.Join(dir, "elapsed")
	callLog := installFakeHook(t, "# spawn-flush-outputs-seconds: "+strconv.Itoa(declared)+"\n"+
		"echo "+strconv.Itoa(int(flushTakes/time.Second))+" > "+clockFile+"\nexit 0\n")
	marker := filepath.Join(dir, "user-hook-ran")

	a := newTestAgent(t, &provider.Config{PreStop: "touch " + marker, PreStopTimeout: preStopTimeout})
	a.provider = &noticeProvider{stubProvider: *a.provider.(*stubProvider),
		info: &provider.InterruptionInfo{Action: "terminate", Time: t0.Add(2 * time.Minute)}}
	a.consoleWriter = discardConsole
	a.spotNow = func() time.Time {
		b, _ := os.ReadFile(clockFile) //nolint:gosec // test fixture path
		s, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return t0.Add(time.Duration(s) * time.Second)
	}
	a.spotEIPGo = func(func()) {}
	var r spotPreStopRun
	a.notifier = &Notifier{record: func(event, detail string) { r.events = append(r.events, event+": "+detail) }}

	var buf bytes.Buffer
	var mu sync.Mutex
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&syncWriter{w: &buf, mu: &mu})
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	a.checkSpotInterruption(context.Background())

	mu.Lock()
	r.log = buf.String()
	mu.Unlock()
	r.hookArgv = hookCalls(t, callLog)
	_, err := os.Stat(marker)
	r.ranHook = err == nil
	return r
}

// The task flush and the user's PreStop hook share one deadline, the
// interruption time less spotPreStopMargin (#1311). The flush is sized to it
// first (record 45 s, then outputs), and the hook gets what the flush left,
// not a fresh 90 s.
func TestSpotPreStopSharesOneDeadline(t *testing.T) {
	// 120 s notice - 25 s margin = 95 s: 45 s record + 5 s slack leaves 45 s
	// of the declared 60 for outputs. The flush takes 70 s, leaving 25 s.
	r := runSpotNotice(t, 60, 70*time.Second, 0)
	if len(r.hookArgv) != 1 || r.hookArgv[0] != "spot_interruption 45" {
		t.Errorf("flush argv = %v, want [spot_interruption 45]", r.hookArgv)
	}
	if !r.ranHook {
		t.Fatal("the user's PreStop hook did not run with 25 s left")
	}
	if !strings.Contains(r.log, "Running pre-stop hook (timeout: 25s)") {
		t.Errorf("the PreStop hook was not bounded by the 25 s the flush left; log:\n%s", r.log)
	}
}

// A configured PreStopTimeout does not take the hook past the reclaim.
func TestSpotPreStopClampsAConfiguredTimeout(t *testing.T) {
	r := runSpotNotice(t, 0, 10*time.Second, 10*time.Minute)
	if len(r.hookArgv) != 1 || r.hookArgv[0] != "spot_interruption" {
		t.Errorf("flush argv = %v, want [spot_interruption]", r.hookArgv)
	}
	if !strings.Contains(r.log, "Running pre-stop hook (timeout: 1m25s)") {
		t.Errorf("a 10m PreStopTimeout was not clamped to the 85 s left; log:\n%s", r.log)
	}
}

// When the flush used the whole deadline the hook is not started, and that is
// reported rather than left to be cut off silently.
func TestSpotPreStopWithNoTimeLeftSkipsTheHookLoudly(t *testing.T) {
	r := runSpotNotice(t, 60, 95*time.Second, 0)
	if r.ranHook {
		t.Error("the user's PreStop hook started with no time left before the reclaim")
	}
	found := false
	for _, e := range r.events {
		if strings.HasPrefix(e, "pre_stop_failed: skipped") {
			found = true
		}
	}
	if !found {
		t.Errorf("no pre_stop_failed notification for the skipped hook; events: %v", r.events)
	}
}

// Off Spot nothing changes: no deadline, the configured or 5-minute bound.
func TestPreStopTimeoutWithoutADeadline(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		configured time.Duration
		deadline   time.Time
		want       time.Duration
		ok         bool
	}{
		{0, time.Time{}, 5 * time.Minute, true},
		{20 * time.Minute, time.Time{}, 20 * time.Minute, true},
		{0, now.Add(200 * time.Second), spotPreStopTimeout, true},
		{0, now.Add(40 * time.Second), 40 * time.Second, true},
		{0, now.Add(500 * time.Millisecond), 0, false},
	} {
		got, ok := preStopTimeout(tc.configured, tc.deadline, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("preStopTimeout(%v, %v) = %v, %v; want %v, %v", tc.configured, tc.deadline, got, ok, tc.want, tc.ok)
		}
	}
}
