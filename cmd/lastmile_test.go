package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
	fp "github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lastmile"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"

	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/lastmilespawn"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/reaperdeploy"
)

const (
	lmAccount = "123456789012"
	lmEast    = "us-east-1" // also the control region
	lmWest    = "us-west-2"
)

var lmT0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// lmWriteOps are every lifecycletest operation that changes a table.
var lmWriteOps = []string{"PutItem", "UpdateItem", "DeleteItem", "TransactWriteItems", "CreateTable", "DeleteTable"}

// lmWorld stands in for everything the last mile reaches: the account the
// command resolved, the enabled regions, a lifecycle table per region, the
// residue's IAM and Scheduler reads, the footprint, the clock and the state
// file. It counts the calls of lastMileRun, lastMileRegions and the
// footprint.
type lmWorld struct {
	t       *testing.T
	mu      sync.Mutex
	now     time.Time
	fakes   map[string]*lifecycletest.Fake
	api     func(region string) lifecycle.API // overrides fakes when set
	regions []string
	dir     string
	roles   map[string]map[string]string // IAM role name -> tags
	iamErr  error
	groups  map[string]*lmGroup // region -> the reaper's teardown schedule group
	report  *fp.Report
	fpErr   error
	planned []string // ARNs the plan deleter re-checked

	runs, regionCalls, fpCalls int
	opts                       lastmile.Options
	result                     lastmile.Result
}

type lmGroup struct {
	tags map[string]string
	jobs []string
}

func newLMWorld(t *testing.T) *lmWorld {
	t.Helper()
	withLogDir(t, false)
	w := &lmWorld{
		t: t, now: lmT0, fakes: map[string]*lifecycletest.Fake{},
		regions: []string{lmEast, lmWest}, dir: t.TempDir(),
		roles: map[string]map[string]string{}, groups: map[string]*lmGroup{},
	}
	w.report = &fp.Report{Schema: 1, Tool: tags.ToolSpawn, Account: lmAccount}

	restore := func(p any) {
		v := reflect.ValueOf(p).Elem()
		old := reflect.ValueOf(v.Interface())
		t.Cleanup(func() { v.Set(old) })
	}
	restore(&lastMileRun)
	restore(&lastMileBudget)
	restore(&lastMileAccount)
	restore(&lastMileStatePath)
	restore(&lastMileNow)
	restore(&lastMileConfig)
	restore(&lastMileRegions)
	restore(&lastMileTables)
	restore(&lastMileFootprintOf)
	restore(&lastMileDeleter)
	restore(&lastMileActivity)
	restore(&lastMileInfraReadable)
	t.Setenv(lastmile.EnvVar, "")

	lastMileAccount = func() string { return lmAccount }
	lastMileStatePath = func() (string, error) { return filepath.Join(w.dir, ".spore-host", "lifecycle.json"), nil }
	lastMileNow = w.clock
	lastMileConfig = func(context.Context) (sdkaws.Config, error) { return lmConfig(lmEast), nil }
	lastMileRegions = func(context.Context, sdkaws.Config) ([]string, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.regionCalls++
		return append([]string(nil), w.regions...), nil
	}
	lastMileTables = func(sdkaws.Config) lastmilespawn.TableAPI {
		return func(region string) (lifecycle.API, error) {
			if w.api != nil {
				return w.api(region), nil
			}
			return w.fake(region), nil
		}
	}
	lastMileFootprintOf = func(context.Context, sdkaws.Config, string, []string, time.Time) (*fp.Report, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.fpCalls++
		return w.report, w.fpErr
	}
	lastMileDeleter = func(sdkaws.Config, string, func() time.Time) fp.Deleter {
		return lastmilespawn.ReportOnlyDeleter{
			Residue: &lastmilespawn.ResidueChecker{Account: lmAccount, IAM: lmIAM{w}, Scheduler: func(r string) lastmilespawn.SchedulerAPI { return lmSched{w, r} }},
			Plan:    lmPlan{w},
		}
	}
	lastMileInfraReadable = func(context.Context) error { return errors.New("the infra probe must not run while Activity is nil") }
	lastMileRun = func(ctx context.Context, o lastmile.Options) lastmile.Result {
		res := lastmile.Run(ctx, o)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.runs++
		w.opts, w.result = o, res
		return res
	}
	return w
}

func lmConfig(region string) sdkaws.Config {
	return sdkaws.Config{Region: region, HTTPClient: lmRefusingHTTP{}, Credentials: sdkaws.AnonymousCredentials{}, RetryMaxAttempts: 1}
}

func (w *lmWorld) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

func (w *lmWorld) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

func (w *lmWorld) fake(region string) *lifecycletest.Fake {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, ok := w.fakes[region]
	if !ok {
		f = lifecycletest.New()
		w.fakes[region] = f
	}
	return f
}

func (w *lmWorld) runCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs
}

// participant is another spawn in the account (a creator, the reaper),
// writing to the same tables.
func (w *lmWorld) participant(owner string) *lifecycle.Client {
	w.t.Helper()
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(r string) (lifecycle.API, error) { return w.fake(r), nil },
		ControlRegion: lifecyclemark.ControlRegion,
		Owner:         owner,
		Now:           w.clock,
		Sleep:         func(ctx context.Context, d time.Duration) error { w.advance(d); return ctx.Err() },
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

// writes is how many writing calls every fake has seen.
func (w *lmWorld) writes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, f := range w.fakes {
		for _, op := range lmWriteOps {
			n += f.Calls(op)
		}
	}
	return n
}

// staleLease leaves a teardown in region whose holder died: claimed, then
// its heartbeat let go past the stale threshold.
func (w *lmWorld) staleLease(region string) {
	w.t.Helper()
	ctx := context.Background()
	c := w.participant("spawn-ttl-reaper")
	if err := c.MarkActive(ctx, tags.ToolSpawn, region); err != nil {
		w.t.Fatal(err)
	}
	if _, _, err := c.Observe(ctx, tags.ToolSpawn, region, false); err != nil {
		w.t.Fatal(err)
	}
	if _, err := c.Claim(ctx, tags.ToolSpawn, region, 0); err != nil {
		w.t.Fatal(err)
	}
	w.advance(lifecycle.DefaultStaleAfter + time.Minute)
}

// lmIAM, lmSched and lmPlan are the residue's and the plan's re-checks.
type lmIAM struct{ w *lmWorld }

func (f lmIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	if f.w.iamErr != nil {
		return nil, f.w.iamErr
	}
	t, ok := f.w.roles[sdkaws.ToString(in.RoleName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: sdkaws.String("no such role")}
	}
	var ts []iamtypes.Tag
	for k, v := range t {
		ts = append(ts, iamtypes.Tag{Key: sdkaws.String(k), Value: sdkaws.String(v)})
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Tags: ts}}, nil
}

type lmSched struct {
	w      *lmWorld
	region string
}

func (f lmSched) group() *lmGroup {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	return f.w.groups[f.region]
}

func (f lmSched) GetScheduleGroup(_ context.Context, in *scheduler.GetScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error) {
	if f.group() == nil {
		return nil, &schedulertypes.ResourceNotFoundException{Message: sdkaws.String("no group")}
	}
	return &scheduler.GetScheduleGroupOutput{Name: in.Name}, nil
}

func (f lmSched) ListTagsForResource(context.Context, *scheduler.ListTagsForResourceInput, ...func(*scheduler.Options)) (*scheduler.ListTagsForResourceOutput, error) {
	out := &scheduler.ListTagsForResourceOutput{}
	for k, v := range f.group().tags {
		out.Tags = append(out.Tags, schedulertypes.Tag{Key: sdkaws.String(k), Value: sdkaws.String(v)})
	}
	return out, nil
}

func (f lmSched) ListSchedules(context.Context, *scheduler.ListSchedulesInput, ...func(*scheduler.Options)) (*scheduler.ListSchedulesOutput, error) {
	out := &scheduler.ListSchedulesOutput{}
	for _, j := range f.group().jobs {
		out.Schedules = append(out.Schedules, schedulertypes.ScheduleSummary{Name: sdkaws.String(j)})
	}
	return out, nil
}

type lmPlan struct{ w *lmWorld }

func (p lmPlan) Describe(_ context.Context, r fp.Resource) (fp.Resource, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.planned = append(p.w.planned, r.ARN)
	return r, nil
}

func (lmPlan) Delete(context.Context, fp.Resource) error {
	return errors.New("the plan deleter's Delete was called")
}

func lmManaged() map[string]string { return map[string]string{tags.ManagedBy: string(tags.ToolSpawn)} }

// lmProbe adds a command to the real root for the test: it writes payload to
// its stdout and returns err.
func lmProbe(t *testing.T, name, payload string, err error) {
	t.Helper()
	c := &cobra.Command{Use: name, RunE: func(cmd *cobra.Command, _ []string) error {
		_, _ = io.WriteString(cmd.OutOrStdout(), payload)
		return err
	}}
	rootCmd.AddCommand(c)
	t.Cleanup(func() { rootCmd.RemoveCommand(c) })
}

// lmExec runs the real root command with args and returns what reached
// stdout and stderr, through the command's writers or the process's own file
// descriptors, and the error Execute would turn into the exit status.
func lmExec(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	capture := func(f **os.File) func() string {
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatal(perr)
		}
		old := *f
		*f = w
		var buf bytes.Buffer
		done := make(chan struct{})
		go func() { _, _ = io.Copy(&buf, r); close(done) }()
		return func() string {
			*f = old
			_ = w.Close()
			<-done
			_ = r.Close()
			return buf.String()
		}
	}
	spawnVerbose = false // a flag value outlives the run that set it
	if f := rootCmd.PersistentFlags().Lookup("verbose"); f != nil {
		f.Changed = false
	}
	stopOut, stopErr := capture(&os.Stdout), capture(&os.Stderr)
	var cmdOut, cmdErr bytes.Buffer
	rootCmd.SetOut(&cmdOut)
	rootCmd.SetErr(&cmdErr)
	rootCmd.SetArgs(args)
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		spawnVerbose = false
	}()
	_, err = rootCmd.ExecuteC()
	return cmdOut.String() + stopOut(), cmdErr.String() + stopErr(), err
}

func lmListCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c, _, err := rootCmd.Find([]string{"list"})
	if err != nil || c.Name() != "list" {
		t.Fatalf("find list: %v", err)
	}
	return c
}

// ---- when it runs ----------------------------------------------------------

// TestLastMileRunsAfterACommand: after a command succeeds, the root's
// post-run hook runs the last mile exactly once, for spawn in the account
// the command resolved, and prints nothing at default verbosity.
func TestLastMileRunsAfterACommand(t *testing.T) {
	w := newLMWorld(t)
	lmProbe(t, "lm-probe", "the command's output\n", nil)
	stdout, stderr, err := lmExec(t, "lm-probe")
	if err != nil {
		t.Fatal(err)
	}
	if n := w.runCount(); n != 1 {
		t.Fatalf("lastmile.Run called %d times, want 1", n)
	}
	if w.opts.Tool != tags.ToolSpawn || w.opts.Account != lmAccount {
		t.Errorf("Run for %s in %q, want spawn in %s", w.opts.Tool, w.opts.Account, lmAccount)
	}
	if !w.result.Ran {
		t.Errorf("the run did not run: %s", w.result.Summary())
	}
	if stdout != "the command's output\n" {
		t.Errorf("stdout = %q, want only the command's own output", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr at default verbosity = %q, want nothing", stderr)
	}
}

// TestLastMileRunsOnceUnderAGroupWithItsOwnPostRun: root.go runs every
// ancestor's hooks (cobra.EnableTraverseRunHooks), so a group that sets its
// own persistent post-run neither hides the last mile nor runs it twice.
func TestLastMileRunsOnceUnderAGroupWithItsOwnPostRun(t *testing.T) {
	w := newLMWorld(t)
	groupRan := 0
	group := &cobra.Command{Use: "lm-group", PersistentPostRunE: func(*cobra.Command, []string) error { groupRan++; return nil }}
	group.AddCommand(&cobra.Command{Use: "leaf", RunE: func(*cobra.Command, []string) error { return nil }})
	rootCmd.AddCommand(group)
	t.Cleanup(func() { rootCmd.RemoveCommand(group) })
	if _, _, err := lmExec(t, "lm-group", "leaf"); err != nil {
		t.Fatal(err)
	}
	if groupRan != 1 || w.runCount() != 1 {
		t.Errorf("group post-run ran %d, last mile %d; want 1 and 1", groupRan, w.runCount())
	}
	if rootCmd.PersistentPostRunE == nil || reflect.ValueOf(rootCmd.PersistentPostRunE).Pointer() != reflect.ValueOf(lastMileHook).Pointer() {
		t.Error("the root's PersistentPostRunE is not the last mile")
	}
}

// TestLastMileSkippedCommands: completion, help, version, footprint,
// teardown, doctor, cleanup, every reaper subcommand and the hidden
// top-level plumbing (gen-docs and the rest) never run it; any other
// command does.
func TestLastMileSkippedCommands(t *testing.T) {
	w := newLMWorld(t)
	for name := range lastMileSkip {
		switch name {
		case "completion", "help", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			continue // cobra adds these when it executes
		}
		c, _, err := rootCmd.Find([]string{name})
		if err != nil || c == rootCmd || c.Name() != name {
			t.Errorf("lastMileSkip names %q, which is not a spawn command (renamed?)", name)
			continue
		}
		runLastMile(c)
	}
	for _, want := range []string{"version", "footprint", "teardown", "doctor", "cleanup", "reaper", "gen-docs"} {
		if _, ok := lastMileSkip[want]; !ok {
			t.Errorf("lastMileSkip lacks %s", want)
		}
	}
	// Every reaper subcommand, and every hidden top-level command, found
	// from the tree rather than listed.
	reaper, _, err := rootCmd.Find([]string{"reaper"})
	if err != nil || len(reaper.Commands()) == 0 {
		t.Fatalf("no reaper subcommands: %v", err)
	}
	for _, sub := range reaper.Commands() {
		runLastMile(sub)
	}
	hidden := 0
	for _, c := range rootCmd.Commands() {
		if c.Hidden {
			hidden++
			runLastMile(c)
		}
	}
	if hidden == 0 {
		t.Fatal("found no hidden top-level command; the discovery has broken")
	}
	if g, _, _ := rootCmd.Find([]string{"gen-docs"}); g == nil || !g.Hidden {
		t.Error("gen-docs is not a hidden top-level command")
	}
	if n := w.runCount(); n != 0 {
		t.Fatalf("a skipped command ran the last mile %d time(s)", n)
	}

	// Through the real root, which builds the help and completion commands.
	for _, args := range [][]string{{"help"}, {"completion", "bash"}, {cobra.ShellCompRequestCmd, "wat"}} {
		if _, _, err := lmExec(t, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if n := w.runCount(); n != 0 {
		t.Fatalf("help/completion ran the last mile %d time(s)", n)
	}

	// Any other command runs it.
	runLastMile(lmListCmd(t))
	if n := w.runCount(); n != 1 {
		t.Fatalf("after `spawn list` the last mile ran %d time(s), want 1", n)
	}
}

// TestLastMileOff: SPORE_LASTMILE=off (any case) skips it before anything.
func TestLastMileOff(t *testing.T) {
	for _, v := range []string{"off", "OFF", " Off "} {
		t.Run(v, func(t *testing.T) {
			w := newLMWorld(t)
			t.Setenv(lastmile.EnvVar, v)
			lmProbe(t, "lm-probe", "", nil)
			if _, _, err := lmExec(t, "lm-probe"); err != nil {
				t.Fatal(err)
			}
			if w.runCount() != 0 || w.regionCalls != 0 {
				t.Errorf("%s=%q: ran %d time(s), listed regions %d time(s); want neither", lastmile.EnvVar, v, w.runCount(), w.regionCalls)
			}
		})
	}
}

// TestLastMileModesAllReport: unset, report, reap and an unknown value all
// run as report: nothing is written, nothing is done, and stdout and the
// exit status are the command's.
func TestLastMileModesAllReport(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want lastmile.Mode
	}{{"", lastmile.ModeReport}, {"report", lastmile.ModeReport}, {"reap", lastmile.ModeReap}, {"bogus", lastmile.ModeReport}} {
		t.Run("mode="+tc.env, func(t *testing.T) {
			w := newLMWorld(t)
			w.staleLease(lmWest)
			before := w.writes()
			t.Setenv(lastmile.EnvVar, tc.env)
			lmProbe(t, "lm-probe", "out\n", nil)
			stdout, stderr, err := lmExec(t, "lm-probe")
			if err != nil || stdout != "out\n" || stderr != "" {
				t.Fatalf("the command became %q / %q, %v", stdout, stderr, err)
			}
			if w.opts.Mode != tc.want || w.result.Mode != lastmile.ModeReport {
				t.Errorf("passed %q, ran as %q; want %q passed, run as report", w.opts.Mode, w.result.Mode, tc.want)
			}
			if len(w.result.Did) != 0 || w.writes() != before {
				t.Errorf("did %v, %d write(s); want none", w.result.Did, w.writes()-before)
			}
			for _, wn := range w.result.Warnings {
				if strings.Contains(wn, "report-only") {
					t.Errorf("lastmile tried a write the guard refused: %s", wn)
				}
			}
		})
	}
}

// TestLastMileNoAccountNoCall: a command that resolved no account (made no
// GetCallerIdentity) skips the last mile, which makes no STS call of its
// own; and in spore.host's infra account it never runs.
func TestLastMileNoAccountNoCall(t *testing.T) {
	for _, acct := range []string{"", fs.InfraAccountID} {
		t.Run("account="+acct, func(t *testing.T) {
			w := newLMWorld(t)
			lastMileAccount = func() string { return acct }
			lmProbe(t, "lm-probe", "", nil)
			_, stderr, err := lmExec(t, "lm-probe", "-v")
			if err != nil {
				t.Fatal(err)
			}
			if w.runCount() != 0 || w.regionCalls != 0 {
				t.Errorf("ran %d, listed regions %d; want neither", w.runCount(), w.regionCalls)
			}
			if !strings.Contains(stderr, "debug: lastmile: skipped: ") {
				t.Errorf("no skip line under --verbose:\n%s", stderr)
			}
		})
	}
}

// TestLastMileAccountIsTheCommandsOwn: the production account seam is what
// the observer Execute installs records from the command's own
// GetCallerIdentity, with no request of its own.
func TestLastMileAccountIsTheCommandsOwn(t *testing.T) {
	callerAccounts.Reset()
	t.Cleanup(callerAccounts.Reset)
	if callerAccounts.Account() != "" {
		t.Fatal("an account before any call")
	}
	h := &lmSTSHTTP{}
	cfg := lmConfig(lmEast)
	cfg.HTTPClient = h
	cfg.APIOptions = append(cfg.APIOptions, callerAccounts.Middleware)
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err != nil {
		t.Fatal(err)
	}
	if got := callerAccounts.Account(); got != lmAccount || h.n != 1 {
		t.Errorf("recorded %q after %d request(s), want %s after 1", got, h.n, lmAccount)
	}
}

// TestExecuteInstallsTheAccountObserver: Execute, and nothing in pkg/, hands
// the observer to the user-account loaders.
func TestExecuteInstallsTheAccountObserver(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "root.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Execute" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetCLIAPIOptions" || len(call.Args) != 1 {
				return true
			}
			if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "Middleware" {
				if id, ok := arg.X.(*ast.Ident); ok && id.Name == "callerAccounts" {
					found = true
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Error("Execute does not call spawnconfig.SetCLIAPIOptions(callerAccounts.Middleware): the last mile would never learn the account")
	}
	if reflect.ValueOf(lmDefaultAccount).Pointer() != reflect.ValueOf(callerAccounts.Account).Pointer() {
		t.Error("lastMileAccount is not callerAccounts.Account")
	}
}

// ---- never in the way ------------------------------------------------------

// TestLastMileNeverTouchesStdoutOrExitStatus: whatever the last mile does
// (finds things, warns, errors, panics), stdout holds exactly the command's
// own output and the command's error (its exit status) is unchanged. At
// default verbosity stderr is empty; under --verbose every line is a debug
// line on stderr.
func TestLastMileNeverTouchesStdoutOrExitStatus(t *testing.T) {
	cases := map[string]func(w *lmWorld){
		"finds a stale lease and residue": func(w *lmWorld) {
			w.staleLease(lmWest)
			w.roles[reaperdeploy.TeardownInvokeRoleName] = lmManaged()
		},
		"lifecycle reads fail": func(w *lmWorld) {
			w.api = func(string) lifecycle.API { return lmFailingTable{err: errors.New("dynamodb is down")} }
		},
		"Run panics": func(*lmWorld) {
			lastMileRun = func(context.Context, lastmile.Options) lastmile.Result { panic("boom") }
		},
		"region listing panics": func(*lmWorld) {
			lastMileRegions = func(context.Context, sdkaws.Config) ([]string, error) { panic("boom") }
		},
		"footprint panics": func(w *lmWorld) {
			w.staleLease(lmWest)
			lastMileFootprintOf = func(context.Context, sdkaws.Config, string, []string, time.Time) (*fp.Report, error) { panic("boom") }
		},
		"Run returns warnings and would-dos": func(*lmWorld) {
			lastMileRun = func(_ context.Context, o lastmile.Options) lastmile.Result {
				o.Logf("lastmile: warning: %s", "something")
				return lastmile.Result{Ran: true, Tool: o.Tool, Account: o.Account, Warnings: []string{"w"},
					WouldDo: []lastmile.Action{{Kind: lastmile.KindDelete, Target: "arn:x"}}}
			}
		},
	}
	for name, setup := range cases {
		for _, v := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/verbose=%v", name, v), func(t *testing.T) {
				w := newLMWorld(t)
				setup(w)
				lmProbe(t, "lm-probe", "payload\n", nil)
				args := []string{"lm-probe"}
				if v {
					args = append(args, "--verbose")
				}
				stdout, stderr, err := lmExec(t, args...)
				if err != nil {
					t.Fatalf("the command's error became %v", err)
				}
				if stdout != "payload\n" {
					t.Errorf("stdout = %q, want exactly the command's output", stdout)
				}
				if !v && stderr != "" {
					t.Errorf("stderr at default verbosity = %q, want nothing", stderr)
				}
				if v && stderr == "" {
					t.Error("nothing at all under --verbose")
				}
				for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
					if v && line != "" && !strings.HasPrefix(line, "debug: lastmile") {
						t.Errorf("verbose stderr line %q is not a last-mile debug line", line)
					}
				}
			})
		}
	}

	// A failed command keeps its own exit status, and runs no last mile.
	for _, tc := range []struct {
		err  error
		code int
	}{{&remoteExitError{code: 3}, 3}, {newAborted("declined"), ExitCodeAborted}, {errors.New("plain"), 1}} {
		w := newLMWorld(t)
		lmProbe(t, "lm-fail", "partial\n", tc.err)
		stdout, _, err := lmExec(t, "lm-fail")
		if code := exitForError(err); code != tc.code || !errors.Is(err, tc.err) {
			t.Errorf("exit status %d (%v), want the command's own %d", code, err, tc.code)
		}
		if stdout != "partial\n" {
			t.Errorf("stdout = %q", stdout)
		}
		if w.runCount() != 0 {
			t.Errorf("the last mile ran after a failed command")
		}
		rootCmd.RemoveCommand(func() *cobra.Command { c, _, _ := rootCmd.Find([]string{"lm-fail"}); return c }())
	}
}

// TestLastMileVerboseSummary: under --verbose the summary and each
// would-action reach stderr, as debug lines, never stdout.
func TestLastMileVerboseSummary(t *testing.T) {
	w := newLMWorld(t)
	w.staleLease(lmWest)
	lmProbe(t, "lm-probe", "", nil)
	stdout, stderr, err := lmExec(t, "lm-probe", "-v")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	for _, want := range []string{
		"debug: lastmile REPORT tool=spawn account=" + lmAccount,
		"debug: lastmile: would resume teardown of tool#spawn#us-west-2",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("verbose stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// TestLastMileBudget: a last mile that overruns (here one that ignores its
// context entirely) holds the command no longer than its budget plus the
// slack, and so does a lifecycle table that never answers.
func TestLastMileBudget(t *testing.T) {
	newLMWorld(t)
	lastMileBudget = 300 * time.Millisecond
	release, returned := make(chan struct{}), make(chan struct{})
	lastMileRun = func(context.Context, lastmile.Options) lastmile.Result {
		defer close(returned)
		<-release // ignores ctx
		return lastmile.Result{}
	}
	start := time.Now()
	runLastMile(lmListCmd(t))
	if took := time.Since(start); took > lastMileBudget+lastMileSlack+500*time.Millisecond {
		t.Errorf("the hook took %s with a %s budget", took, lastMileBudget)
	}
	// Let the abandoned run end before the seams change under it.
	close(release)
	<-returned

	release = make(chan struct{})
	t.Cleanup(func() { close(release) })
	w := newLMWorld(t)
	lastMileBudget = 300 * time.Millisecond
	w.api = func(string) lifecycle.API { return lmSlowTable{release: release} }
	start = time.Now()
	runLastMile(lmListCmd(t))
	if took := time.Since(start); took > lastMileBudget+lastMileSlack+500*time.Millisecond {
		t.Errorf("the hook took %s with a %s budget and a table that never answers", took, lastMileBudget)
	}
}

// TestLastMileHookRecovers: the hook returns nil even when it is handed
// nothing to work with.
func TestLastMileHookRecovers(t *testing.T) {
	w := newLMWorld(t)
	if err := lastMileHook(nil, nil); err != nil {
		t.Errorf("lastMileHook(nil) = %v, want nil", err)
	}
	if w.runCount() != 0 {
		t.Error("ran with no command")
	}
}

// ---- what it passes and reports --------------------------------------------

// TestLastMileOptionsWiring pins the options: spawn, every enabled region, a
// lifecycle client reading the global scope where spawn's creators write
// it, spawn's footprint, report-only deleter and patterns, the reaper's
// teardown residue, and FullVisibility false.
func TestLastMileOptionsWiring(t *testing.T) {
	w := newLMWorld(t)
	w.regions = []string{"eu-west-1", lmEast, lmWest}
	lmProbe(t, "lm-probe", "", nil)
	if _, _, err := lmExec(t, "lm-probe"); err != nil {
		t.Fatal(err)
	}
	o := w.opts
	if w.regionCalls != 1 {
		t.Errorf("listed the regions %d times, want 1", w.regionCalls)
	}
	if !reflect.DeepEqual(o.Regions, w.regions) {
		t.Errorf("Regions = %v, want every enabled region %v", o.Regions, w.regions)
	}
	if o.FullVisibility || o.Activity != nil {
		t.Errorf("FullVisibility = %v, Activity set = %v; want false and nil (lastMileVisibility)", o.FullVisibility, o.Activity != nil)
	}
	if o.Lifecycle == nil {
		t.Fatal("no Lifecycle client")
	}
	if r, err := o.Lifecycle.RegionFor(lifecycle.ScopeGlobal); err != nil || r != lifecyclemark.ControlRegion {
		t.Errorf("control region = %q, %v; want %s", r, err, lifecyclemark.ControlRegion)
	}
	if !reflect.DeepEqual(o.Patterns, lastmilespawn.Patterns()) {
		t.Error("Patterns are not lastmilespawn.Patterns (spawn's teardown patterns and the residue)")
	}
	if o.Footprint == nil || o.Deleter == nil {
		t.Fatal("no Footprint or Deleter")
	}
	if o.Budget <= 0 || o.Budget > lastmile.DefaultBudget || o.StatePath == "" || o.Logf == nil || o.Now == nil {
		t.Errorf("Budget %s, StatePath %q, Logf set %v", o.Budget, o.StatePath, o.Logf != nil)
	}
	if !reflect.DeepEqual(o.Residue, lastmilespawn.Residue(lmAccount, w.regions)) {
		t.Errorf("Residue = %+v, want the reaper's teardown residue in every region", o.Residue)
	}
	roles, groups := 0, 0
	for _, r := range o.Residue {
		switch {
		case r.Type == fs.TypeRole && r.Name == reaperdeploy.TeardownInvokeRoleName:
			roles++
		case r.Type == lastmilespawn.TypeScheduleGroup && r.Name == reaperdeploy.TeardownScheduleGroup:
			groups++
		}
		if tags.IsService(r.Tags) {
			t.Errorf("%s is tagged as the service", r.ARN)
		}
	}
	if roles != 1 || groups != len(w.regions) {
		t.Errorf("%d invoke role(s) and %d schedule group(s), want 1 and %d", roles, groups, len(w.regions))
	}

	// The Deleter refuses every delete; the client refuses every write.
	if err := o.Deleter.Delete(context.Background(), o.Residue[0]); !errors.Is(err, lastmilespawn.ErrReportOnly) {
		t.Errorf("Deleter.Delete = %v, want the report-only refusal", err)
	}
	if err := o.Lifecycle.MarkActive(context.Background(), tags.ToolSpawn, lmWest); !errors.Is(err, lastmilespawn.ErrReportOnly) {
		t.Errorf("a write through the last mile's lifecycle client = %v, want the report-only refusal", err)
	}
}

// TestLastMileVisibilityDecision: FullVisibility is set only with an
// activity reader AND a readable infra account, and then the reader is
// what lastmile is given. Production has no reader, so it is false and
// the infra account is not even read.
func TestLastMileVisibilityDecision(t *testing.T) {
	scopes := []string{}
	activity := func(sdkaws.Config, string) func(context.Context, string) (bool, error) {
		return func(_ context.Context, scope string) (bool, error) { scopes = append(scopes, scope); return true, nil }
	}
	for _, tc := range []struct {
		name     string
		activity func(sdkaws.Config, string) func(context.Context, string) (bool, error)
		infra    error
		want     bool
		probed   bool
	}{
		{"production: no activity reader", nil, nil, false, false},
		{"infra unreadable", activity, errors.New("AccessDenied: dynamodb:Scan on spawn-schedules"), false, true},
		{"infra readable", activity, nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newLMWorld(t)
			scopes = nil
			probed := false
			lastMileActivity = tc.activity
			lastMileInfraReadable = func(context.Context) error { probed = true; return tc.infra }
			runLastMile(lmListCmd(t))
			if w.opts.FullVisibility != tc.want || (w.opts.Activity != nil) != tc.want {
				t.Errorf("FullVisibility = %v, Activity set = %v; want %v", w.opts.FullVisibility, w.opts.Activity != nil, tc.want)
			}
			if probed != tc.probed {
				t.Errorf("infra probed = %v, want %v", probed, tc.probed)
			}
			if tc.want && len(scopes) == 0 {
				t.Error("lastmile was given the reader but never asked it")
			}
			if !tc.want && len(scopes) != 0 {
				t.Errorf("the reader was asked %v without FullVisibility", scopes)
			}
		})
	}
	if lmDefaultActivity != nil {
		t.Error("lastMileActivity is set in production: the reaper's verdict is not reusable from cmd (lastMileVisibility)")
	}
}

// TestLastMileReportsStaleLeaseWithoutWriting: a teardown whose holder died
// is reported, with what it would still delete re-checked by the plan
// deleter, and nothing is written.
func TestLastMileReportsStaleLeaseWithoutWriting(t *testing.T) {
	w := newLMWorld(t)
	w.staleLease(lmWest)
	sg := fp.Resource{ARN: "arn:aws:ec2:us-west-2:" + lmAccount + ":security-group/sg-0123456789abcdef0", Type: fs.TypeSecurityGroup,
		Region: lmWest, Name: "spawn-sg-x", Ownership: fp.OwnershipTagged, Class: fp.ClassLitter, Tags: lmManaged()}
	w.report.Resources = []fp.Resource{sg}
	before := w.writes()
	item := w.fake(lmWest).Item(lifecycle.TableName, lifecycle.ItemKey(tags.ToolSpawn, lmWest))
	lmProbe(t, "lm-probe", "", nil)
	if _, _, err := lmExec(t, "lm-probe"); err != nil {
		t.Fatal(err)
	}
	var resume, del bool
	for _, a := range w.result.WouldDo {
		resume = resume || (a.Kind == lastmile.KindResumeTeardown && a.Target == lifecycle.ItemKey(tags.ToolSpawn, lmWest))
		del = del || (a.Kind == lastmile.KindDelete && a.Target == sg.ARN)
	}
	if !resume || !del {
		t.Errorf("resume %v, would delete the group %v: %+v (warnings %v)", resume, del, w.result.WouldDo, w.result.Warnings)
	}
	if len(w.planned) != 1 || w.planned[0] != sg.ARN || w.fpCalls != 1 {
		t.Errorf("re-checked %v with %d footprint(s), want the group once from one footprint", w.planned, w.fpCalls)
	}
	if w.writes() != before {
		t.Errorf("%d write(s) to the lifecycle tables; report mode writes none", w.writes()-before)
	}
	if after := w.fake(lmWest).Item(lifecycle.TableName, lifecycle.ItemKey(tags.ToolSpawn, lmWest)); !reflect.DeepEqual(after, item) {
		t.Error("the stale item changed")
	}
}

// TestLastMileResidue: with no spawn item left anywhere, the reaper's
// teardown invoke role is reported removable once the reaper's own role is
// gone; a schedule group still holding a job keeps itself and the role; any
// spawn item keeps both; an empty lifecycle table is reported to be closed.
// None of it reads the footprint.
func TestLastMileResidue(t *testing.T) {
	role := reaperdeploy.TeardownInvokeRoleARN(lmAccount)
	wouldDelete := func(w *lmWorld, arn string) bool {
		for _, a := range w.result.WouldDo {
			if a.Kind == lastmile.KindDelete && a.Target == arn {
				return true
			}
		}
		return false
	}
	run := func(t *testing.T, setup func(w *lmWorld)) *lmWorld {
		t.Helper()
		w := newLMWorld(t)
		w.roles[reaperdeploy.TeardownInvokeRoleName] = lmManaged()
		setup(w)
		before := w.writes()
		runLastMile(lmListCmd(t))
		if w.writes() != before {
			t.Errorf("%d write(s); report mode writes none", w.writes()-before)
		}
		if w.fpCalls != 0 {
			t.Errorf("the residue check read the footprint %d time(s)", w.fpCalls)
		}
		return w
	}

	t.Run("removable", func(t *testing.T) {
		w := run(t, func(w *lmWorld) {
			f := w.fake(lmWest)
			if _, err := f.CreateTable(context.Background(), &dynamodb.CreateTableInput{
				TableName:            sdkaws.String(lifecycle.TableName),
				KeySchema:            []types.KeySchemaElement{{AttributeName: sdkaws.String("pk"), KeyType: types.KeyTypeHash}},
				AttributeDefinitions: []types.AttributeDefinition{{AttributeName: sdkaws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
				BillingMode:          types.BillingModePayPerRequest,
			}); err != nil {
				t.Fatal(err)
			}
			f.SetStatus(lifecycle.TableName, types.TableStatusActive, 0)
		})
		var table bool
		for _, a := range w.result.WouldDo {
			table = table || (a.Kind == lastmile.KindCloseTable && a.Region == lmWest)
		}
		if !wouldDelete(w, role) || !table {
			t.Errorf("would delete the invoke role: %v, close the empty table: %v; got %+v (warnings %v)",
				wouldDelete(w, role), table, w.result.WouldDo, w.result.Warnings)
		}
	})
	t.Run("the reaper is still deployed", func(t *testing.T) {
		w := run(t, func(w *lmWorld) { w.roles[reaperdeploy.RoleName] = lmManaged() })
		if wouldDelete(w, role) {
			t.Error("would delete the invoke role while the reaper's own role exists")
		}
	})
	t.Run("a group still holds a job", func(t *testing.T) {
		group := reaperdeploy.TeardownScheduleGroupARN(lmWest, lmAccount)
		w := run(t, func(w *lmWorld) {
			w.groups[lmWest] = &lmGroup{tags: lmManaged(), jobs: []string{reaperdeploy.JobLogGroup}}
		})
		if wouldDelete(w, role) || wouldDelete(w, group) {
			t.Errorf("would delete the role %v / the busy group %v", wouldDelete(w, role), wouldDelete(w, group))
		}
	})
	t.Run("an empty group", func(t *testing.T) {
		group := reaperdeploy.TeardownScheduleGroupARN(lmWest, lmAccount)
		w := run(t, func(w *lmWorld) { w.groups[lmWest] = &lmGroup{tags: lmManaged()} })
		if !wouldDelete(w, role) || !wouldDelete(w, group) {
			t.Errorf("would delete the role %v / the empty group %v: %+v", wouldDelete(w, role), wouldDelete(w, group), w.result.WouldDo)
		}
	})
	t.Run("a spawn item remains", func(t *testing.T) {
		w := run(t, func(w *lmWorld) {
			if err := w.participant("spawn-cli").MarkActive(context.Background(), tags.ToolSpawn, lmWest); err != nil {
				t.Fatal(err)
			}
		})
		if wouldDelete(w, role) {
			t.Error("would delete the invoke role while a spawn item remains")
		}
		var kept bool
		for _, n := range w.result.Notes {
			kept = kept || strings.Contains(n, "residue kept: tool#spawn#us-west-2 remains")
		}
		if !kept {
			t.Errorf("the item does not keep the residue: %v", w.result.Notes)
		}
	})
}

// TestLastMileAccessDeniedIsSilent: a caller without the lifecycle or IAM
// reads is the normal case. The refusal is silent at default verbosity, one
// debug line (never a warning) under --verbose, and costs one denied
// request per run, not one per region.
func TestLastMileAccessDeniedIsSilent(t *testing.T) {
	for _, v := range []bool{false, true} {
		t.Run(fmt.Sprintf("verbose=%v", v), func(t *testing.T) {
			w := newLMWorld(t)
			w.regions = []string{"eu-west-1", lmEast, lmWest, "ap-south-1"}
			denied := &lmDeniedTable{}
			w.api = func(string) lifecycle.API { return denied }
			lmProbe(t, "lm-probe", "out\n", nil)
			args := []string{"lm-probe"}
			if v {
				args = append(args, "-v")
			}
			stdout, stderr, err := lmExec(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			if stdout != "out\n" {
				t.Errorf("stdout = %q", stdout)
			}
			if !v && stderr != "" {
				t.Errorf("stderr at default verbosity = %q", stderr)
			}
			if strings.Contains(stderr, "lastmile: warning") {
				t.Errorf("a refused read was reported as a warning:\n%s", stderr)
			}
			if v && !strings.Contains(stderr, "debug: lastmile: not permitted, skipped: ") {
				t.Errorf("verbose stderr lacks the not-permitted debug line:\n%s", stderr)
			}
			if n := strings.Count(stderr, "not permitted"); v && n != 1 {
				t.Errorf("%d not-permitted line(s), want 1:\n%s", n, stderr)
			}
			if n := denied.requests(); n != 1 {
				t.Errorf("%d denied request(s) reached DynamoDB, want 1", n)
			}
		})
	}
}

// TestLastMileRateLimitCostsNoAWSCall: once a run has checked every scope,
// the next invocations inside 6 h make no AWS call (not even the region
// listing) and do not run; after 6 h they do. This also holds the state-file
// reader (lastMileMaybeDue) to the file lastmile.Run really writes.
func TestLastMileRateLimitCostsNoAWSCall(t *testing.T) {
	w := newLMWorld(t)
	lmProbe(t, "lm-probe", "", nil)
	if _, _, err := lmExec(t, "lm-probe"); err != nil {
		t.Fatal(err)
	}
	if w.runCount() != 1 || w.regionCalls != 1 || !w.result.Ran {
		t.Fatalf("first run: runs %d, region listings %d, ran %v", w.runCount(), w.regionCalls, w.result.Ran)
	}
	w.advance(lastmile.DefaultMinInterval - time.Minute)
	if _, _, err := lmExec(t, "lm-probe"); err != nil {
		t.Fatal(err)
	}
	if w.runCount() != 1 || w.regionCalls != 1 {
		t.Errorf("inside 6 h: runs %d, region listings %d; want no new call", w.runCount(), w.regionCalls)
	}
	w.advance(2 * time.Minute)
	if _, _, err := lmExec(t, "lm-probe"); err != nil {
		t.Fatal(err)
	}
	if w.runCount() != 2 || !w.result.Ran {
		t.Errorf("after 6 h: runs %d, ran %v; want a second run", w.runCount(), w.result.Ran)
	}
}

// TestLastMileCutOffBacksOff: a run the budget cut off is not retried on the
// next invocation (lastmile.Run alone would retry it, and an account whose
// check never fits would hold every command for the whole budget), but is
// after lastMileRetryAfter.
func TestLastMileCutOffBacksOff(t *testing.T) {
	w := newLMWorld(t)
	lastMileBudget = 300 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := lmSlowTable{release: release}
	w.api = func(region string) lifecycle.API {
		if region == lmWest {
			return slow // never answers: us-west-2 is cut off
		}
		return w.fake(region)
	}
	runLastMile(lmListCmd(t))
	if w.runCount() != 1 || len(w.result.Warnings) == 0 {
		t.Fatalf("first run: %d run(s), warnings %v; want one run cut off by its budget", w.runCount(), w.result.Warnings)
	}
	w.advance(30 * time.Minute)
	runLastMile(lmListCmd(t))
	if w.runCount() != 1 {
		t.Errorf("retried %s after a cut-off run; want a wait of %s", 30*time.Minute, lastMileRetryAfter)
	}
	w.advance(lastMileRetryAfter)
	runLastMile(lmListCmd(t))
	if w.runCount() != 2 {
		t.Errorf("not retried %s after a cut-off run", lastMileRetryAfter+30*time.Minute)
	}
}

// TestLastMileMaybeDue pins the pre-check's reading of the state file.
func TestLastMileMaybeDue(t *testing.T) {
	dir := t.TempDir()
	now := lmT0
	write := func(body string) string {
		p := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+".json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rec := func(key, lastRun, incomplete string) string {
		s := `"` + key + `":{"last_run":"` + lastRun + `"`
		if incomplete != "" {
			s += `,"incomplete":"` + incomplete + `"`
		}
		return s + "}"
	}
	cut := func(key, lastAttempt string) string {
		return `"` + key + `":{"last_attempt":"` + lastAttempt + `","incomplete":"budget exceeded"}`
	}
	global := "spawn/" + lmAccount + "/global"
	fresh := now.Add(-time.Hour).Format(time.RFC3339)
	stale := now.Add(-7 * time.Hour).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)
	cases := []struct {
		name, body string
		due        bool
	}{
		{"fresh", `{"version":1,"runs":{` + rec(global, fresh, "") + "," + rec("spawn/"+lmAccount+"/us-east-1", fresh, "") + `}}`, false},
		{"stale", `{"version":1,"runs":{` + rec(global, stale, "") + `}}`, true},
		{"incomplete", `{"version":1,"runs":{` + rec(global, fresh, "budget exceeded") + `}}`, true},
		{"future", `{"version":1,"runs":{` + rec(global, future, "") + `}}`, true},
		{"another account", `{"version":1,"runs":{` + rec("spawn/999999999999/global", fresh, "") + `}}`, true},
		{"another tool", `{"version":1,"runs":{` + rec("lagotto/"+lmAccount+"/global", fresh, "") + `}}`, true},
		{"unknown version", `{"version":2,"runs":{` + rec(global, fresh, "") + `}}`, true},
		{"corrupt", `{not json`, true},
		{"cut off just now", `{"version":1,"runs":{` + cut("spawn/"+lmAccount+"/us-west-2", now.Add(-10*time.Minute).Format(time.RFC3339)) + `}}`, false},
		{"cut off over an hour ago", `{"version":1,"runs":{` + cut("spawn/"+lmAccount+"/us-west-2", now.Add(-61*time.Minute).Format(time.RFC3339)) + `}}`, true},
		{"another account cut off", `{"version":1,"runs":{` + cut("spawn/999999999999/us-west-2", now.Add(-10*time.Minute).Format(time.RFC3339)) + `}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastMileMaybeDue(write(tc.body), lmAccount, now); got != tc.due {
				t.Errorf("due = %v, want %v", got, tc.due)
			}
		})
	}
	if !lastMileMaybeDue(filepath.Join(dir, "missing.json"), lmAccount, now) {
		t.Error("a missing state file is not due")
	}
	if !lastMileMaybeDue(t.TempDir(), lmAccount, now) {
		t.Error("an unreadable state file (a directory) is not due")
	}
}

// TestLastMileSkipsWhenAStepFails: each step before lastmile.Run that cannot
// go on (no state path, no AWS config, no region listing, no budget left)
// skips the run with one debug line, prints nothing at default verbosity,
// and leaves the command alone.
func TestLastMileSkipsWhenAStepFails(t *testing.T) {
	cases := map[string]struct {
		setup func(w *lmWorld)
		line  string
	}{
		"no state path": {func(*lmWorld) {
			lastMileStatePath = func() (string, error) { return "", errors.New("no home directory") }
		}, "debug: lastmile: skipped: no home directory"},
		"no AWS config": {func(*lmWorld) {
			lastMileConfig = func(context.Context) (sdkaws.Config, error) { return sdkaws.Config{}, errors.New("no credentials") }
		}, "debug: lastmile: skipped: load AWS config: no credentials"},
		"region listing refused": {func(*lmWorld) {
			lastMileRegions = func(context.Context, sdkaws.Config) ([]string, error) {
				return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "not authorized to perform: ec2:DescribeRegions"}
			}
		}, "debug: lastmile: skipped: list the account's regions: api error UnauthorizedOperation"},
		"no budget left": {func(*lmWorld) { lastMileBudget = time.Nanosecond }, "debug: lastmile: skipped: no budget left"},
	}
	for name, tc := range cases {
		for _, v := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/verbose=%v", name, v), func(t *testing.T) {
				w := newLMWorld(t)
				tc.setup(w)
				lmProbe(t, "lm-probe", "out\n", nil)
				args := []string{"lm-probe"}
				if v {
					args = append(args, "-v")
				}
				stdout, stderr, err := lmExec(t, args...)
				if err != nil || stdout != "out\n" {
					t.Fatalf("the command became %q, %v", stdout, err)
				}
				if w.runCount() != 0 {
					t.Errorf("lastmile.Run ran %d time(s) after the step failed", w.runCount())
				}
				switch {
				case !v && stderr != "":
					t.Errorf("stderr at default verbosity = %q", stderr)
				case v && !strings.Contains(stderr, tc.line):
					t.Errorf("verbose stderr lacks %q:\n%s", tc.line, stderr)
				}
			})
		}
	}
}

// TestLastMileNoRegionUsesControlRegion: with no region configured the
// regions are listed from the control region.
func TestLastMileNoRegionUsesControlRegion(t *testing.T) {
	newLMWorld(t)
	lastMileConfig = func(context.Context) (sdkaws.Config, error) { return lmConfig(""), nil }
	var listedFrom string
	lastMileRegions = func(_ context.Context, cfg sdkaws.Config) ([]string, error) {
		listedFrom = cfg.Region
		return []string{lmEast}, nil
	}
	runLastMile(lmListCmd(t))
	if listedFrom != lifecyclemark.ControlRegion {
		t.Errorf("regions listed from %q, want the control region %s", listedFrom, lifecyclemark.ControlRegion)
	}
}

// TestLastMileFootprintFails: when the footprint a stale lease's plan needs
// cannot be read, nothing in it is reported removable, and the failure is a
// warning.
func TestLastMileFootprintFails(t *testing.T) {
	w := newLMWorld(t)
	w.staleLease(lmWest)
	w.fpErr = errors.New("cannot list the account's buckets")
	runLastMile(lmListCmd(t))
	for _, a := range w.result.WouldDo {
		if a.Kind == lastmile.KindDelete {
			t.Errorf("would delete %s without a footprint", a.Target)
		}
	}
	var failed bool
	for _, wn := range w.result.Warnings {
		failed = failed || strings.Contains(wn, "cannot list the account's buckets")
	}
	if !failed {
		t.Errorf("the footprint failure is not in the warnings: %v", w.result.Warnings)
	}
}

// TestLastMileLogDropsLateAndRepeatedLines: after the hook has stopped
// waiting nothing more is written, and a refusal the latch answered is not
// logged again.
func TestLastMileLogDropsLateAndRepeatedLines(t *testing.T) {
	var b strings.Builder
	l := &lastMileLog{w: &b}
	l.printf("lastmile: warning: read x: %s", "AccessDeniedException: no")
	l.printf("lastmile: warning: read y: %s already", lastmilespawn.NotAsked)
	l.printf("lastmile: a note\n")
	l.close()
	l.printf("lastmile: after close")
	want := "debug: lastmile: not permitted, skipped: read x: AccessDeniedException: no\ndebug: lastmile: a note\n"
	if b.String() != want {
		t.Errorf("log = %q, want %q", b.String(), want)
	}
	(&lastMileLog{}).printf("nowhere to write")
}

// The production seams, as built, before any test replaces them.
var (
	lmDefaultAccount        = lastMileAccount
	lmDefaultConfig         = lastMileConfig
	lmDefaultRegions        = lastMileRegions
	lmDefaultTables         = lastMileTables
	lmDefaultFootprintOf    = lastMileFootprintOf
	lmDefaultDeleter        = lastMileDeleter
	lmDefaultActivity       = lastMileActivity
	lmDefaultInfraReadable  = lastMileInfraReadable
	lmDefaultFootprintTimer = 10 * time.Second
)

// TestLastMileDefaultSeams: the production readers, against a config whose
// HTTP client refuses every request, fail fast with an error; the deleter
// is report-only; the table client is regional; and the infra probe fails
// without a profile for the infra account.
func TestLastMileDefaultSeams(t *testing.T) {
	isolateAWSEnv(t)
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), lmDefaultFootprintTimer)
	defer cancel()
	cfg := lmConfig(lmEast)
	if _, err := lmDefaultRegions(ctx, cfg); err == nil {
		t.Error("listing regions through a refusing client succeeded")
	}
	api, err := lmDefaultTables(cfg)(lmWest)
	if c, ok := api.(*dynamodb.Client); err != nil || !ok || c.Options().Region != lmWest {
		t.Errorf("table API = %T, %v; want a DynamoDB client in %s", api, err, lmWest)
	}
	d, ok := lmDefaultDeleter(cfg, lmAccount, time.Now).(lastmilespawn.ReportOnlyDeleter)
	if !ok || d.Residue == nil || d.Plan == nil {
		t.Fatalf("deleter = %T, want lastmilespawn.ReportOnlyDeleter", d)
	}
	if err := d.Delete(ctx, fp.Resource{ARN: "arn:x"}); !errors.Is(err, lastmilespawn.ErrReportOnly) {
		t.Errorf("Delete = %v", err)
	}
	rep, err := lmDefaultFootprintOf(ctx, cfg, lmAccount, []string{lmWest}, lmT0)
	if err != nil || rep == nil || rep.Tool != tags.ToolSpawn || rep.Account != lmAccount {
		t.Errorf("footprint = %+v, %v; want spawn's report for %s", rep, err, lmAccount)
	}
	if err := lmDefaultInfraReadable(ctx); err == nil {
		t.Error("the infra tables read with no infra profile")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if c, err := lmDefaultConfig(ctx); err != nil {
		t.Errorf("the caller's config: %v", err)
	} else if c.Credentials == nil {
		t.Error("the caller's config has no credentials")
	}
}

// ---- fakes -----------------------------------------------------------------

type lmRefusingHTTP struct{}

func (lmRefusingHTTP) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("refused: no network in tests")
}

// lmSTSHTTP answers a GetCallerIdentity for lmAccount, with no network.
type lmSTSHTTP struct{ n int }

func (s *lmSTSHTTP) Do(*http.Request) (*http.Response, error) {
	s.n++
	body := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
		`<Arn>arn:aws:iam::` + lmAccount + `:user/u</Arn><UserId>AIDAEXAMPLE</UserId><Account>` + lmAccount + `</Account>` +
		`</GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/xml"}},
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

// lmFailingTable fails every read with err.
type lmFailingTable struct {
	lifecycle.API
	err error
}

func (f lmFailingTable) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return nil, f.err
}

func (f lmFailingTable) Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return nil, f.err
}

func (f lmFailingTable) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return nil, f.err
}

// lmDeniedTable refuses every read as IAM would, counting the requests.
type lmDeniedTable struct {
	lifecycle.API
	mu sync.Mutex
	n  int
}

func (d *lmDeniedTable) deny() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	return &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "User: arn:aws:sts::123456789012:assumed-role/u/s is not authorized to perform: dynamodb:GetItem"}
}

func (d *lmDeniedTable) requests() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func (d *lmDeniedTable) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return nil, d.deny()
}

func (d *lmDeniedTable) Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return nil, d.deny()
}

func (d *lmDeniedTable) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return nil, d.deny()
}

// lmSlowTable never answers until release is closed, whatever its context
// says.
type lmSlowTable struct {
	lifecycle.API
	release chan struct{}
}

func (s lmSlowTable) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	<-s.release
	return &dynamodb.GetItemOutput{}, nil
}

func (s lmSlowTable) Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	<-s.release
	return &dynamodb.ScanOutput{}, nil
}

func (s lmSlowTable) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	<-s.release
	return &dynamodb.DescribeTableOutput{}, nil
}
