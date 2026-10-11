package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/spore-host/spawn/pkg/pipeline"
)

// Flags that are accepted and discarded (#807, #775).
//
// `pipeline launch --wait` and `logs --region` each had exactly two references in
// the tree — the declaration and the registration — so neither did anything. The
// second was worse than inert: a local --region SHADOWS the persistent root one,
// so binding to a dead local flag meant the value never reached SetSharedFlags
// and there was no working --region on that command at all.

// A note on what this file does NOT gate.
//
// The first version of this file had a test requiring that no local flag shadow a
// persistent root flag, on the theory that `logs --region` broke because the local
// flag won the binding. It ran, and reported more than twenty commands — `launch`,
// `list`, `doctor`, every `array` subcommand — declaring their own `--region`.
//
// That is the repo's convention, not a defect. The gate encoded a false premise,
// and keeping it would have meant an allowlist of twenty entries each asserting
// that ordinary code is an exception: CLAUDE.md rule 4's anti-pattern turned
// inside out, where the list is what is normal and the gate is what is wrong.
//
// The actual defect in `logs --region` was not the shadowing. It was that the
// local flag was never READ — shadowing only determined how the symptom
// presented. TestEveryFlagVariableIsReadSomewhere below gates that directly, and
// would have caught it with no reference to shadowing at all.

// Every registered flag must be read somewhere.
//
// The #775/#807 shape, caught structurally: a flag variable with exactly two
// references in non-test code — its declaration and its registration — is
// accepted from the user and discarded. Reading the SOURCE is what makes this
// possible; cobra cannot tell a flag that is read from one that is not.
func TestEveryFlagVariableIsReadSomewhere(t *testing.T) {
	root := repoRootFromCmd(t)

	reg := regexp.MustCompile(`Flags\(\)\.[A-Za-z]+VarP?\(&([a-zA-Z_][a-zA-Z0-9_]*)\s*,\s*"([a-z0-9-]+)"`)

	type decl struct{ flag, file string }
	vars := map[string]decl{}
	var sources []string

	err := filepath.Walk(filepath.Join(root, "cmd"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // fixed repo subtree
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		sources = append(sources, string(b))
		for _, m := range reg.FindAllStringSubmatch(string(b), -1) {
			if _, seen := vars[m[1]]; !seen {
				vars[m[1]] = decl{flag: m[2], file: rel}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd: %v", err)
	}
	if len(vars) < 50 {
		t.Fatalf("found only %d flag variables; the registration pattern has stopped matching, which "+
			"is not the same as every flag being read", len(vars))
	}

	// A DEPRECATED flag is allowed to be a no-op: that is what deprecating it
	// means — still accepted so existing scripts do not break, deliberately
	// ignored. Three flags turned up on the first run (--use-reservation,
	// pipeline --detached, cleanup --force) and all three carry MarkDeprecated,
	// so the exemption comes from the code's own declaration of intent rather
	// than from a hand-written list (CLAUDE.md rule 4).
	//
	// Neither --wait nor logs --region was deprecated, which is exactly why they
	// were defects and not decisions.
	//
	// Limitation worth stating: this exempts by flag NAME across the whole tree,
	// so a flag deprecated on one command would also be exempt on another where it
	// is live. Narrowing that needs the command each variable belongs to, which
	// this source scan does not recover.
	deprecated := map[string]bool{}
	var collect func(c *cobra.Command)
	collect = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Deprecated != "" {
				deprecated[f.Name] = true
			}
		})
		for _, sub := range c.Commands() {
			collect(sub)
		}
	}
	collect(rootCmd)
	if len(deprecated) == 0 {
		t.Fatal("found no deprecated flags; the repo has several, so this lookup has broken and the " +
			"gate is about to report them as defects")
	}

	all := strings.Join(sources, "\n")
	for v, d := range vars {
		if deprecated[d.flag] {
			continue
		}
		// Declaration + registration = 2. Anything that reads it makes 3.
		if n := countWholeWord(all, v); n <= 2 {
			t.Errorf("%s: --%s binds to %s, which has %d reference(s) in non-test source — declaration "+
				"and registration only. The flag is accepted from the user and discarded (#807).",
				d.file, d.flag, v, n)
		}
	}
}

// ── pipeline launch --wait ───────────────────────────────────────────────────

type fakePipelineStore struct {
	states []*pipeline.PipelineState
	errs   []error
	calls  int
}

func (f *fakePipelineStore) Get(_ context.Context, _ string) (*pipeline.PipelineState, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i < len(f.states) {
		return f.states[i], nil
	}
	if len(f.states) == 0 {
		return nil, nil
	}
	return f.states[len(f.states)-1], nil
}

func state(s pipeline.PipelineStatus, done, total, failed int) *pipeline.PipelineState {
	return &pipeline.PipelineState{Status: s, CompletedStages: done, TotalStages: total, FailedStages: failed}
}

func withFastPolling(t *testing.T) {
	t.Helper()
	prev := waitForPipelinePollInterval
	waitForPipelinePollInterval = time.Millisecond
	t.Cleanup(func() { waitForPipelinePollInterval = prev })
}

// --wait blocks until terminal, and its exit status reflects the outcome.
//
// The second half is the point. A --wait that merely blocked would still let
// `spawn pipeline launch --wait && ./collect-results.sh` run the next step after a
// FAILED pipeline, which is the thing the flag exists to prevent.
func TestWaitForPipelineReportsTheOutcome(t *testing.T) {
	withFastPolling(t)

	cases := []struct {
		name    string
		states  []*pipeline.PipelineState
		wantErr bool
		wantIn  string
	}{
		{
			name:   "runs then completes",
			states: []*pipeline.PipelineState{state(pipeline.StatusRunning, 1, 3, 0), state(pipeline.StatusCompleted, 3, 3, 0)},
		},
		{
			name:    "fails",
			states:  []*pipeline.PipelineState{state(pipeline.StatusRunning, 1, 3, 0), state(pipeline.StatusFailed, 1, 3, 2)},
			wantErr: true,
			wantIn:  "FAILED",
		},
		{
			name:    "cancelled",
			states:  []*pipeline.PipelineState{state(pipeline.StatusCancelled, 0, 3, 0)},
			wantErr: true,
			wantIn:  "CANCELLED",
		},
		{
			name: "initializing is not terminal",
			states: []*pipeline.PipelineState{
				state(pipeline.StatusInitializing, 0, 3, 0),
				state(pipeline.StatusRunning, 1, 3, 0),
				state(pipeline.StatusCompleted, 3, 3, 0),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakePipelineStore{states: c.states}
			err := waitForPipeline(context.Background(), store, "p-1", 10*time.Second)
			if c.wantErr && err == nil {
				t.Fatal("wait returned nil for a pipeline that did not succeed; `launch --wait && next` " +
					"would run the next step")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("wait returned an error for a completed pipeline: %v", err)
			}
			if c.wantIn != "" && !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("error %q does not name the terminal status %q", err, c.wantIn)
			}
			if store.calls < 2 && len(c.states) > 1 {
				t.Errorf("polled %d time(s) for %d states — it did not wait", store.calls, len(c.states))
			}
		})
	}
}

// A transient read failure, or a record not yet written, must not end the wait.
//
// The orchestrator writes the pipeline record asynchronously, so "not found" is
// normal immediately after launch; treating it as failure would make --wait
// useless exactly when it is used.
func TestWaitForPipelineToleratesTransientReadProblems(t *testing.T) {
	withFastPolling(t)

	store := &fakePipelineStore{
		errs: []error{errors.New("throttled"), nil, nil},
		states: []*pipeline.PipelineState{
			nil, // record not written yet
			nil,
			state(pipeline.StatusCompleted, 2, 2, 0),
		},
	}
	if err := waitForPipeline(context.Background(), store, "p-1", 10*time.Second); err != nil {
		t.Fatalf("a throttle and a missing record ended the wait: %v", err)
	}
	if store.calls < 3 {
		t.Errorf("polled %d times; it should have retried past both problems", store.calls)
	}
}

// The wait must be bounded, and say the pipeline is still running when it gives up.
//
// An unbounded wait on a wedged pipeline hangs CI forever, which is a different
// failure from the one being fixed but no better.
func TestWaitForPipelineGivesUpAndSaysSo(t *testing.T) {
	withFastPolling(t)

	store := &fakePipelineStore{states: []*pipeline.PipelineState{state(pipeline.StatusRunning, 1, 3, 0)}}
	err := waitForPipeline(context.Background(), store, "p-1", 20*time.Millisecond)
	if err == nil {
		t.Fatal("wait returned nil for a pipeline that never finished")
	}
	for _, want := range []string{"gave up", "still", "spawn pipeline status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error %q does not mention %q, so the user cannot tell the pipeline is "+
				"still running or how to check it", err, want)
		}
	}
}

// The launch path must CALL the wait, not merely register the flag.
//
// TestEveryFlagVariableIsReadSomewhere counts references, and a reference inside
// dead code satisfies it: a revert to `if flagWait && false { _ = flagWaitTimeout }`
// left both variables with three references and passed. Reference counting
// detects a flag nobody looks at; it cannot detect one that is looked at and
// ignored. This asserts the wiring.
func TestPipelineLaunchActuallyWaits(t *testing.T) {
	root := repoRootFromCmd(t)
	b, err := os.ReadFile(filepath.Join(root, "cmd", "pipeline.go")) //nolint:gosec // fixed path
	if err != nil {
		t.Fatalf("read pipeline.go: %v", err)
	}
	src := string(b)

	i := strings.Index(src, "if flagWait {")
	if i < 0 {
		t.Fatal("no unconditional `if flagWait {` in the launch path — --wait is registered but not " +
			"acted on, which is the #807 defect")
	}
	// The guard must be plain. `if flagWait && false` and friends read as wiring
	// while disabling it.
	line := src[i:]
	if j := strings.Index(line, "\n"); j > 0 {
		line = line[:j]
	}
	if strings.Contains(line, "&&") || strings.Contains(line, "||") {
		t.Errorf("the --wait guard is compound (%q); it must be a plain `if flagWait {`", strings.TrimSpace(line))
	}
	if !strings.Contains(src[i:], "waitForPipeline(") {
		t.Error("the --wait branch does not call waitForPipeline")
	}
	// And the return must be propagated, or a failed pipeline still exits 0.
	if !strings.Contains(src[i:], "return waitForPipeline(") {
		t.Error("the result of waitForPipeline is not returned, so a failed pipeline would still exit 0")
	}
}

// --wait and --wait-timeout must both be registered on launch.
func TestPipelineLaunchRegistersWaitFlags(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"pipeline", "launch"})
	if err != nil {
		t.Fatalf("find pipeline launch: %v", err)
	}
	for _, name := range []string{"wait", "wait-timeout"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("pipeline launch has no --%s", name)
		}
	}
	if f := c.Flags().Lookup("wait-timeout"); f != nil && f.DefValue == "0s" {
		t.Error("--wait-timeout defaults to 0s, i.e. no bound; a wedged pipeline would hang CI forever")
	}
}

// ── small helpers (kept local so this file reads on its own) ─────────────────

func regexpMustCompile(expr string) *regexp.Regexp { return regexp.MustCompile(expr) }

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// countWholeWord counts identifier-boundary occurrences of name, so counting
// references to `flagWait` is not inflated by `flagWaitTimeout`.
func countWholeWord(haystack, name string) int {
	n := 0
	for i := 0; i+len(name) <= len(haystack); {
		j := strings.Index(haystack[i:], name)
		if j < 0 {
			break
		}
		at := i + j
		beforeOK := at == 0 || !isIdentByte(haystack[at-1])
		endAt := at + len(name)
		afterOK := endAt >= len(haystack) || !isIdentByte(haystack[endAt])
		if beforeOK && afterOK {
			n++
		}
		i = at + 1
	}
	return n
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
