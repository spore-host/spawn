package agent

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// The probe tests need a child process that does not exit on its own, on every
// OS the agent builds for. The test binary itself is that child: run with
// slowProbeEnv set, TestHelperSlowProbe sleeps instead of testing anything.
const slowProbeEnv = "SPAWN_AGENT_SLOW_PROBE"

// slowProbePIDFileEnv names a file the "grandchild" helper writes its
// grandchild's PID to, so the test can kill it in cleanup.
const slowProbePIDFileEnv = "SPAWN_AGENT_SLOW_PROBE_PIDFILE"

// slowProbeSleep is how long the helper child sleeps. Long enough that a probe
// returning "promptly" cannot be the child exiting by itself.
const slowProbeSleep = 30 * time.Second

// probeReturnBound is how long a cancelled or timed-out probe may take to
// return: the kill, plus sysProbeWaitDelay, plus a slow runner's slack.
const probeReturnBound = sysProbeWaitDelay + 5*time.Second

func TestHelperSlowProbe(t *testing.T) {
	switch os.Getenv(slowProbeEnv) {
	case "":
		t.Skip("helper process for the probe tests, not a test")
	case "grandchild":
		// Start a sleeping grandchild that inherits this process's stdout, the
		// probe's pipe, so killing this process alone does not close it.
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- re-runs this test binary as a sleeping helper; no input reaches it.
		gc := exec.Command(os.Args[0], "-test.run=^TestHelperSlowProbe$")
		gc.Env = append(os.Environ(), slowProbeEnv+"=sleep")
		gc.Stdout = os.Stdout
		if err := gc.Start(); err != nil {
			os.Exit(3)
		}
		// Tell the test which process to reap. An orphaned grandchild would keep
		// the test binary's image open, and on Windows `go test` then cannot
		// delete it ("unlinkat … agent.test.exe: Access is denied").
		if f := os.Getenv(slowProbePIDFileEnv); f != "" {
			_ = os.WriteFile(f, []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
		}
	}
	time.Sleep(slowProbeSleep)
	os.Exit(0)
}

// slowProbe returns a command that runs until it is killed. mode "grandchild"
// also leaves a grandchild holding the stdout pipe.
func slowProbe(t *testing.T, mode string) (string, []string) {
	t.Helper()
	t.Setenv(slowProbeEnv, mode)
	return os.Args[0], []string{"-test.run=^TestHelperSlowProbe$"}
}

// probeReturns runs probeOutput in a goroutine and fails if it has not
// returned within probeReturnBound of start.
func probeReturns(t *testing.T, ctx context.Context, name string, args []string) error {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := probeOutput(ctx, name, args...); done <- err }()
	select {
	case err := <-done:
		t.Logf("probe returned after %v: %v", time.Since(start).Round(time.Millisecond), err)
		return err
	case <-time.After(probeReturnBound):
		t.Fatalf("probe still running %v after its context ended; a cancelled monitor "+
			"would wait for it (#1104)", probeReturnBound)
		return nil
	}
}

// A probe stops when the monitor's context is cancelled. Before #1104 every
// Windows reader was exec.Command with no context, so Monitor returned only
// once the tick's PowerShell processes had finished, ~14s after cancel.
func TestProbeOutput_StopsWhenContextCancelled(t *testing.T) {
	name, args := slowProbe(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if err := probeReturns(t, ctx, name, args); err == nil {
		t.Fatal("a probe killed by its context reported success")
	}
}

// A probe nobody cancels is still bounded: a wedged PowerShell or WMI query
// must not hold a tick for ever.
func TestProbeOutput_TimesOut(t *testing.T) {
	name, args := slowProbe(t, "sleep")
	orig := sysProbeTimeout
	sysProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sysProbeTimeout = orig })
	if err := probeReturns(t, context.Background(), name, args); err == nil {
		t.Fatal("a probe killed by its timeout reported success")
	}
}

// Killing the probe does not close a stdout pipe a grandchild inherited, and
// without WaitDelay Output waits for the grandchild too.
func TestProbeOutput_GrandchildHoldingPipe(t *testing.T) {
	name, args := slowProbe(t, "grandchild")
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Setenv(slowProbePIDFileEnv, pidFile)
	t.Cleanup(func() { killGrandchild(t, pidFile) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	if err := probeReturns(t, ctx, name, args); err == nil {
		t.Fatal("a probe killed by its context reported success")
	}
}

// Monitor returns promptly when cancelled in the middle of a tick whose idle
// readers are still running: the cancellation reaches the readers (#1104). The
// stub CPU reader stands in for a slow PowerShell sample; it returns only when
// the context it was given ends, or after 20s.
func TestMonitor_CancelReachesIdleReaders(t *testing.T) {
	entered := make(chan struct{})
	var once bool
	origCPU, origSessions := readCPUTimes, sessionCounter
	t.Cleanup(func() { readCPUTimes, sessionCounter = origCPU, origSessions })
	sessionCounter = func(context.Context) int { return 0 }
	readCPUTimes = func(ctx context.Context) (int64, int64, error) {
		if !once {
			once = true
			close(entered)
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-time.After(20 * time.Second):
			return 0, 0, errors.New("reader was never cancelled")
		}
	}

	identity := &provider.Identity{InstanceID: "i-cancel", Region: "us-east-1", Provider: "stub"}
	cfg := &provider.Config{}
	a := &Agent{
		provider:              &stubProvider{identity: identity, config: cfg},
		identity:              identity,
		config:                cfg,
		startTime:             time.Now(),
		lastActivityTime:      time.Now(),
		monitorInterval:       10 * time.Millisecond,
		lastSessionTagWrite:   time.Now().Add(time.Hour),
		lastComputeTagWrite:   time.Now().Add(time.Hour),
		lastHeartbeatTagWrite: time.Now().Add(time.Hour),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Monitor(ctx) }()
	t.Cleanup(func() {
		cancel()
		select { // bounded, so a failure below is reported rather than hung on
		case <-done:
		case <-time.After(time.Minute):
		}
	})

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the monitor never sampled CPU")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Monitor did not return within 5s of cancel while a tick's reader was " +
			"running: the reader is not getting the monitor's context (#1104)")
	}
}

// TestNoContextlessExecInAgent is the gate for #1104: no file in this package,
// for any GOOS, may start a process with exec.Command, which takes no context.
// Use probeOutput (a context plus sysProbeTimeout) or
// exec.CommandContext. A process the monitor's cancellation cannot stop holds
// up Monitor's return, and spored's shutdown with it.
//
// It parses the files rather than building them, so the Windows and Linux
// files are checked on whichever OS runs the test, and it resolves the import
// name of os/exec per file, so an aliased import is still caught and a comment
// or string mentioning exec.Command is not.
func TestNoContextlessExecInAgent(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed, withContext int
	var platforms []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		parsed++
		if f == "sys_windows.go" || f == "sys_linux.go" || f == "sys_other.go" {
			platforms = append(platforms, f)
		}
		execName := ""
		for _, imp := range file.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == "os/exec" {
				execName = "exec"
				if imp.Name != nil {
					execName = imp.Name.Name
				}
			}
		}
		if execName == "" || execName == "_" {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != execName {
				return true
			}
			switch sel.Sel.Name {
			case "Command":
				t.Errorf("%s: %s.Command starts a process no context can stop; use probeOutput "+
					"or exec.CommandContext with the caller's context (#1104)", fset.Position(call.Pos()), execName)
			case "CommandContext":
				withContext++
			}
			return true
		})
	}
	// A discovery that found nothing has broken, not proven the package clean.
	if parsed == 0 || withContext == 0 {
		t.Fatalf("parsed %d files and found %d exec.CommandContext calls; the scan is broken", parsed, withContext)
	}
	if len(platforms) != 3 {
		t.Fatalf("scanned platform files %v, want sys_linux.go, sys_other.go and sys_windows.go", platforms)
	}
}

// killGrandchild kills the helper's grandchild and waits for it to exit, so no
// process outlives the test holding the test binary open.
func killGrandchild(t *testing.T, pidFile string) {
	t.Helper()
	var pid int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			pid, _ = strconv.Atoi(string(b))
			break
		}
	}
	if pid <= 0 {
		t.Errorf("helper never reported its grandchild's PID; it may outlive the test")
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Kill()
	done := make(chan struct{})
	go func() { _, _ = p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
