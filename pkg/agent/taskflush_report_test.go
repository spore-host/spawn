package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// flushSinks records everything spored reports about a flush (#1186): the
// tags it wrote, what went to the serial console, and the notifications.
type flushSinks struct {
	tagger  *fakeTagPutter
	mu      sync.Mutex
	console strings.Builder
	events  []string // "event|detail"
}

func (s *flushSinks) consoleText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.console.String()
}

func (s *flushSinks) eventsNamed(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		if strings.HasPrefix(e, name+"|") {
			out = append(out, strings.TrimPrefix(e, name+"|"))
		}
	}
	return out
}

// flushAgent is an agent whose flush reporting goes to recorders instead of
// EC2, /dev/console and the bot. taskID "" makes it a plain launch.
func flushAgent(t *testing.T, taskID string) (*Agent, *flushSinks) {
	t.Helper()
	s := &flushSinks{tagger: &fakeTagPutter{}}
	a := &Agent{
		identity: &provider.Identity{InstanceID: "i-flush", Region: "us-east-1", Provider: "stub"},
		config:   &provider.Config{TaskID: taskID},
		tagger:   s.tagger,
		consoleWriter: func(text string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.console.WriteString(text)
			return nil
		},
		notifier: &Notifier{record: func(event, detail string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.events = append(s.events, event+"|"+detail)
		}},
	}
	return a, s
}

// TestFlushOutcomeIsVisibleAfterTermination is #1186. A TTL-killed task left an
// empty results prefix, and the reason was invisible: the hook's output and
// status went only to spored's journal, which dies with the instance. Every way
// the flush can fail or be skipped must now reach the three places that survive
// termination — the spawn:flush-status tag, the serial console (with the hook's
// own output), and a notification (pre_stop_failed, an event spore-bot formats).
func TestFlushOutcomeIsVisibleAfterTermination(t *testing.T) {
	cases := []struct {
		name    string
		hook    string      // fake hook body; "" means no hook installed
		mode    os.FileMode // 0 keeps installFakeHook's 0700
		wantTag string
		wantOut string // text that must reach the console and the notification
	}{
		{"missing hook on a task instance", "", 0, "skipped:hook-missing", "definitely-absent.sh"},
		{"hook refused as tamperable", "exit 0\n", 0o777, "failed:hook-refused", "group- or world-writable"},
		{"hook exits non-zero", "echo 'hook broke: no credentials' >&2\nexit 9\n", 0, "failed:exit-9", "hook broke: no credentials"},
		{"uploads failed after retries", "echo 'spawn: upload failed after 3 attempt(s): AccessDenied' >&2\nexit " +
			strconv.Itoa(taskproto.FlushExitUploadFailed) + "\n", 0, "failed:upload-failed", "AccessDenied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hook == "" {
				t.Cleanup(taskproto.SetFlushScriptPathForTest(filepath.Join(t.TempDir(), "definitely-absent.sh")))
			} else {
				installFakeHook(t, tc.hook)
				if tc.mode != 0 {
					if err := os.Chmod(taskproto.FlushScriptPath(), tc.mode); err != nil {
						t.Fatal(err)
					}
				}
			}
			a, s := flushAgent(t, "cookbook-su2-real-probe0-b")

			a.flushTaskRecord(taskproto.ExitTTLExpired)

			if got := s.tagger.tags[flushStatusTag]; got != tc.wantTag {
				t.Errorf("%s = %q, want %q", flushStatusTag, got, tc.wantTag)
			}
			console := s.consoleText()
			if !strings.Contains(console, tc.wantTag) || !strings.Contains(console, tc.wantOut) ||
				!strings.Contains(console, string(taskproto.ExitTTLExpired)) {
				t.Errorf("the serial console must carry the outcome %q, the reason and %q; got:\n%s", tc.wantTag, tc.wantOut, console)
			}
			ev := s.eventsNamed("pre_stop_failed")
			if len(ev) != 1 || !strings.Contains(ev[0], "task flush "+tc.wantTag) || !strings.Contains(ev[0], tc.wantOut) {
				t.Errorf("want one pre_stop_failed notification carrying %q and %q, got %q", "task flush "+tc.wantTag, tc.wantOut, ev)
			}
		})
	}
}

// TestFlushSuccessIsTaggedQuietly: a flush that worked is tagged ok and does
// not spend console space or send a failure notification.
func TestFlushSuccessIsTaggedQuietly(t *testing.T) {
	installFakeHook(t, "echo 'spawn: terminal record written'\nexit 0\n")
	a, s := flushAgent(t, "t1")

	a.flushTaskRecord(taskproto.ExitTTLExpired)

	if got := s.tagger.tags[flushStatusTag]; got != flushOK {
		t.Errorf("%s = %q, want %q", flushStatusTag, got, flushOK)
	}
	if c := s.consoleText(); c != "" {
		t.Errorf("a successful flush wrote to the console:\n%s", c)
	}
	if ev := s.eventsNamed("pre_stop_failed"); len(ev) != 0 {
		t.Errorf("a successful flush sent %q", ev)
	}
}

// TestNoHookOnAPlainLaunchStaysSilent: a `spawn launch` instance has no hook
// because it is not a task. Reporting that as a skipped flush on every
// shutdown would be noise.
func TestNoHookOnAPlainLaunchStaysSilent(t *testing.T) {
	t.Cleanup(taskproto.SetFlushScriptPathForTest(filepath.Join(t.TempDir(), "definitely-absent.sh")))
	a, s := flushAgent(t, "")

	a.flushTaskRecord(taskproto.ExitTTLExpired)

	if s.tagger.calls != 0 || s.consoleText() != "" || len(s.eventsNamed("pre_stop_failed")) != 0 {
		t.Errorf("a plain launch must report nothing: tags=%v console=%q events=%v",
			s.tagger.writes, s.consoleText(), s.events)
	}
}

// TestClassifyFlushTimeout: a hook killed by taskFlushTimeout is a failure
// named "timeout", not a generic exit status. Driven with an already-expired
// context rather than a 45-second wait.
func TestClassifyFlushTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	o := classifyFlush(ctx, &exec.Cmd{}, []byte("partial"), context.DeadlineExceeded)
	if got := o.tagValue(); got != "failed:timeout" {
		t.Errorf("tag value = %q, want failed:timeout", got)
	}
}

// TestFlushHookOutputPipeIsBounded (#1186): the context kills the hook but not
// an `aws` child it started, and that child holds the output pipe, so without
// exec.Cmd.WaitDelay spored waits for the child instead of the cap. The hook
// here leaves a child holding stdout and exits; flushTaskRecord must return
// while that child is still alive.
func TestFlushHookOutputPipeIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: the task-flush hook is a bash script")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The child inherits stdout and stderr; it is killed below whatever happens.
	installFakeHook(t, "sleep 300 &\necho $! > "+pidFile+"\nexit 0\n")
	a, _ := flushAgent(t, "t1")
	a.taskFlushWaitDelay = 50 * time.Millisecond

	child := func() *os.Process {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return nil
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return nil
		}
		p, _ := os.FindProcess(pid)
		return p
	}
	killChild := func() {
		if p := child(); p != nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	}
	t.Cleanup(killChild)

	done := make(chan struct{})
	go func() { defer close(done); a.flushTaskRecord(taskproto.ExitTTLExpired) }()

	select {
	case <-done:
		// The child must still be running: proof flushTaskRecord did not wait
		// for it to release the pipe.
		p := child()
		if p == nil {
			t.Fatal("hook did not record its child")
		}
		if err := p.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("the child had already exited (%v), so this run proved nothing", err)
		}
	case <-time.After(60 * time.Second):
		// Only reached when the pipe is unbounded. Release it so the test ends.
		killChild()
		<-done
		t.Fatal("flushTaskRecord waited for the hook's child to close the output pipe; WaitDelay is not bounding it")
	}
}

// ttlProbeProvider is a stubProvider whose Terminate records what the flush
// hook had done by the time EC2 was asked to terminate.
type ttlProbeProvider struct {
	stubProvider
	callLog    string
	once       sync.Once
	terminated chan []string // the hook's argv lines, as seen at Terminate
}

func (p *ttlProbeProvider) Terminate(_ context.Context, _ string) error {
	p.once.Do(func() {
		var lines []string
		if b, err := os.ReadFile(p.callLog); err == nil {
			lines = strings.Fields(string(b))
		}
		p.terminated <- lines
	})
	return nil
}

// TestMonitorTTLExpiryRunsTheFlushHookBeforeTerminate drives the real monitor
// loop through a TTL expiry. The claim #632 rests on — a TTL kill runs the
// task flush hook, with the ttl_expired reason, before the instance is
// terminated — had no end-to-end test: each half was tested alone, so a
// broken link between them (#1186's open question) would pass every test.
func TestMonitorTTLExpiryRunsTheFlushHookBeforeTerminate(t *testing.T) {
	callLog := installFakeHook(t, "exit 0\n")
	identity := &provider.Identity{InstanceID: "i-ttl-task", Region: "us-east-1", Provider: "stub"}
	cfg := &provider.Config{TTLDeadline: time.Now().Add(-time.Minute), TaskID: "cookbook-su2-real-probe0-b"}
	p := &ttlProbeProvider{
		stubProvider: stubProvider{identity: identity, config: cfg},
		callLog:      callLog,
		terminated:   make(chan []string, 1),
	}
	tagger := &fakeTagPutter{}
	a := &Agent{
		provider:              p,
		identity:              identity,
		config:                cfg,
		tagger:                tagger,
		consoleWriter:         func(string) error { return nil },
		startTime:             time.Now().Add(-2 * time.Hour),
		lastActivityTime:      time.Now(),
		monitorInterval:       10 * time.Millisecond,
		warnDelay:             time.Millisecond,
		lastSessionTagWrite:   time.Now().Add(time.Hour),
		lastComputeTagWrite:   time.Now().Add(time.Hour),
		lastHeartbeatTagWrite: time.Now().Add(time.Hour),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Monitor(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case seen := <-p.terminated:
		if len(seen) != 1 || seen[0] != string(taskproto.ExitTTLExpired) {
			t.Errorf("at Terminate the flush hook had been run with %q, want exactly one run with %q",
				seen, taskproto.ExitTTLExpired)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the monitor never terminated an instance past its TTL deadline")
	}
	cancel()
	<-done
	if got := tagger.tags[flushStatusTag]; got != flushOK {
		t.Errorf("%s = %q after the TTL flush, want %q", flushStatusTag, got, flushOK)
	}
}
