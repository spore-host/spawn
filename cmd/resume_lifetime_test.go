package cmd

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// capturingLauncher is fakeSweepLauncher keeping each launched config.
type capturingLauncher struct {
	fakeSweepLauncher
	configs []aws.LaunchConfig
}

func (l *capturingLauncher) Launch(ctx context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error) {
	l.configs = append(l.configs, cfg)
	return l.fakeSweepLauncher.Launch(ctx, cfg)
}

// withLifetimeFlags sets the launch's lifetime flag globals for one test.
func withLifetimeFlags(t *testing.T, ttlV, idleV string, cost float64, onIdleV string, noTimeoutV bool) {
	t.Helper()
	pt, pi, pc, po, pn := ttl, idleTimeout, costLimit, onIdle, noTimeout
	t.Cleanup(func() { ttl, idleTimeout, costLimit, onIdle, noTimeout = pt, pi, pc, po, pn })
	ttl, idleTimeout, costLimit, onIdle, noTimeout = ttlV, idleV, cost, onIdleV, noTimeoutV
}

// withResumeFlags sets resume's own --ttl/--idle-timeout/--cost-limit.
func withResumeFlags(t *testing.T, f resumeLifetimeFlags) {
	t.Helper()
	pt, pi, pc := resumeTTL, resumeIdleTimeout, resumeCostLimit
	t.Cleanup(func() { resumeTTL, resumeIdleTimeout, resumeCostLimit = pt, pi, pc })
	resumeTTL, resumeIdleTimeout, resumeCostLimit = f.ttl, f.idleTimeout, f.costLimit
}

// lifetimeParamFile is a sweep whose file sets no bound of its own except on
// row 2, whose own ttl: must still win.
func lifetimeParamFile() *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "region": "us-west-2"},
		Params: []map[string]interface{}{
			{"alpha": 0.1}, {"alpha": 0.2}, {"alpha": 0.3, "ttl": "1h"},
		},
	}
}

// resumeConfigs is runResume's lifetime half: plan, then the pending members.
func resumeConfigs(t *testing.T, state *SweepState, pf *ParamFileFormat, pending []int) ([]*aws.LaunchConfig, error) {
	t.Helper()
	plan, err := planResumeLifetime(state.Lifetime, pf, resumeLifetimeFlagValues())
	if err != nil {
		return nil, err
	}
	return plan.members(pf, state, pending)
}

func checkpointedState(t *testing.T, id string, rec *SweepLifetime) *SweepState {
	t.Helper()
	fastRollingQueue(t)
	f := newFakeFleetSweep(0)
	g := newFleetGuard("sweep", io.Discard, f, sporedUp(), false)
	if err := runSweepLaunch(context.Background(), f, g, sweepRun{
		sweepID: id, name: "s", totalParams: 3, maxConcurrent: 1, launchDelay: "0s",
		configs: sweepConfigs("us-east-1", "us-east-1", "us-east-1"), lifetime: rec,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := loadSweepState(id)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// Gate 1: a sweep launched with --ttl 4h --idle-timeout 30m --cost-limit 5
// --on-idle hibernate records them in its checkpoint, and a resume relaunches
// every pending member with them, on its launch config and its tags, with no
// launch flag set (a resume has none). A row's own ttl: still wins.
func TestResume_MembersCarryTheSweepsLifetimeFlags(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)
	withResumeFlags(t, resumeLifetimeFlags{})

	withLifetimeFlags(t, "4h", "30m", 5, "hibernate", false)
	base, err := sweepBaseConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := checkpointedState(t, "life", newSweepLifetime(base, noTimeout))
	if state.Lifetime == nil {
		t.Fatal("the checkpoint records no lifetime settings")
	}
	withLifetimeFlags(t, "", "", 0, "", false) // the resume's process: no launch flags

	pf := lifetimeParamFile()
	pending := []int{0, 1, 2}
	configs, err := resumeConfigs(t, state, pf, pending)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range configs {
		c.Region, c.Name = "us-west-2", "m"
	}
	l := &capturingLauncher{}
	if _, _, n, err := relaunchPending(context.Background(), l, state, configs, pending, 3, unverifiedGuard(), &fakeProfileIAM{exists: map[string]bool{}}); err != nil || n != 3 {
		t.Fatalf("relaunchPending: n=%d err=%v", n, err)
	}
	for _, cfg := range l.configs {
		wantTTL := "4h"
		if cfg.SweepIndex == 2 {
			wantTTL = "1h" // the row's own ttl:
		}
		if cfg.TTL != wantTTL || cfg.IdleTimeout != "30m" || cfg.CostLimit != 5 || !cfg.HibernateOnIdle || !cfg.Hibernate {
			t.Errorf("member %d launched with ttl=%q idle=%q cost=%v hibernate-on-idle=%v hibernate=%v, want %s/30m/5/true/true",
				cfg.SweepIndex, cfg.TTL, cfg.IdleTimeout, cfg.CostLimit, cfg.HibernateOnIdle, cfg.Hibernate, wantTTL)
		}
		tags, err := testMemberTagList(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m := tagsAsMap(tags)
		if m["spawn:ttl"] != wantTTL || m["spawn:idle-timeout"] != "30m" || m["spawn:cost-limit"] != "5.0000" || m["spawn:hibernate-on-idle"] != "true" {
			t.Errorf("member %d tags: ttl=%q idle=%q cost=%q hibernate-on-idle=%q", cfg.SweepIndex,
				m["spawn:ttl"], m["spawn:idle-timeout"], m["spawn:cost-limit"], m["spawn:hibernate-on-idle"])
		}
	}
}

// Gate 2: a checkpoint written before #1394 records nothing. Its members get
// what a fresh launch gives them: the row's own bound, else the 1h idle
// default.
func TestResume_OldCheckpointGetsTheLaunchDefaults(t *testing.T) {
	setTestHome(t, t.TempDir())
	withResumeFlags(t, resumeLifetimeFlags{})
	withLifetimeFlags(t, "", "", 0, "", false)
	state := checkpointedState(t, "old", nil)
	if state.Lifetime != nil {
		t.Fatalf("lifetime = %+v, want none recorded", state.Lifetime)
	}
	configs, err := resumeConfigs(t, state, lifetimeParamFile(), []int{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ ttl, idle string }{{"", "1h"}, {"", "1h"}, {"1h", ""}} {
		if c := configs[i]; c.TTL != want.ttl || c.IdleTimeout != want.idle {
			t.Errorf("member %d: ttl=%q idle=%q, want ttl=%q idle=%q (the launch defaults)", i, c.TTL, c.IdleTimeout, want.ttl, want.idle)
		}
	}
}

// Gate 3: a member that would resume with no bound at all — the sweep was
// launched with --no-timeout and its row sets none — is refused with zero
// launches, and the message says how to pass --ttl.
func TestResume_UnboundedMemberIsRefusedWithZeroLaunches(t *testing.T) {
	setTestHome(t, t.TempDir())
	fastRollingQueue(t)
	withResumeFlags(t, resumeLifetimeFlags{})
	state := checkpointedState(t, "nt", &SweepLifetime{NoTimeout: true})

	_, err := resumeConfigs(t, state, lifetimeParamFile(), []int{0, 1, 2})
	if err == nil || !strings.Contains(err.Error(), "--ttl") || !strings.Contains(err.Error(), "parameter set 0") {
		t.Fatalf("err = %v, want a refusal of parameter set 0 naming --ttl", err)
	}

	// The backstop, at the queue: an unbounded config never launches.
	l := &capturingLauncher{}
	configs := []*aws.LaunchConfig{{Name: "s-0", Region: "us-west-2", TTL: "1h"}, {Name: "s-1", Region: "us-west-2"}}
	_, _, n, err := relaunchPending(context.Background(), l, state, configs, []int{0, 1}, 2, unverifiedGuard(), &fakeProfileIAM{exists: map[string]bool{}})
	if err == nil || n != 0 || len(l.configs) != 0 {
		t.Fatalf("relaunchPending: n=%d launched=%d err=%v, want a refusal and zero launches", n, len(l.configs), err)
	}
	if !strings.Contains(err.Error(), "--ttl") {
		t.Fatalf("err = %v, want it to say how to pass --ttl", err)
	}

	// With a resume --ttl the same sweep resumes.
	withResumeFlags(t, resumeLifetimeFlags{ttl: "2h"})
	got, err := resumeConfigs(t, state, lifetimeParamFile(), []int{0})
	if err != nil || got[0].TTL != "2h" {
		t.Fatalf("with --ttl 2h: err=%v configs=%+v", err, got)
	}
}

// Gate 4: resume's own flags override the recorded ones, and may tighten but
// never loosen them.
func TestResume_ExplicitFlagsOverrideButNeverLoosen(t *testing.T) {
	setTestHome(t, t.TempDir())
	rec := &SweepLifetime{TTL: "4h", IdleTimeout: "30m", CostLimit: 5}
	state := checkpointedState(t, "ov", rec)

	withResumeFlags(t, resumeLifetimeFlags{ttl: "2h", idleTimeout: "10m", costLimit: 3})
	got, err := resumeConfigs(t, state, lifetimeParamFile(), []int{0})
	if err != nil {
		t.Fatal(err)
	}
	if c := got[0]; c.TTL != "2h" || c.IdleTimeout != "10m" || c.CostLimit != 3 {
		t.Fatalf("ttl=%q idle=%q cost=%v, want the resume flags 2h/10m/3", c.TTL, c.IdleTimeout, c.CostLimit)
	}

	for _, loose := range []resumeLifetimeFlags{{ttl: "8h"}, {idleTimeout: "2h"}, {costLimit: 50}} {
		withResumeFlags(t, loose)
		if _, err := resumeConfigs(t, state, lifetimeParamFile(), []int{0}); err == nil || !strings.Contains(err.Error(), "may not loosen") {
			t.Errorf("%+v: err = %v, want a refusal to loosen the launch's bounds", loose, err)
		}
	}

	// A resume --ttl on a sweep that relied on the 1h idle default would drop
	// that default: refused too, unless the idle timeout is kept.
	idleOnly := checkpointedState(t, "idle", &SweepLifetime{})
	withResumeFlags(t, resumeLifetimeFlags{ttl: "8h"})
	if _, err := resumeConfigs(t, idleOnly, lifetimeParamFile(), []int{0}); err == nil || !strings.Contains(err.Error(), "--idle-timeout 1h") {
		t.Errorf("err = %v, want a refusal naming --idle-timeout 1h", err)
	}
	withResumeFlags(t, resumeLifetimeFlags{ttl: "8h", idleTimeout: "1h"})
	if _, err := resumeConfigs(t, idleOnly, lifetimeParamFile(), []int{0}); err != nil {
		t.Errorf("--ttl 8h --idle-timeout 1h: %v", err)
	}
}

// Gate 5, detached: the orchestrator relaunches from the params uploaded at
// launch, so the CLI bounds must be in each member's rendered tags there; and a
// resume --detach refuses flags it cannot deliver rather than ignore them.
func TestDetachedResume_OrchestratorParamsCarryTheLifetimeFlags(t *testing.T) {
	withLifetimeFlags(t, "4h", "30m", 5, "", false)
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0", "region": "us-east-1"},
		Params:   []map[string]interface{}{{"alpha": 0.1}, {"alpha": 0.2}},
	}
	applyCLISpendControlsToSweep(pf)
	if err := seedDetachedBootstrap(pf, aws.LaunchConfig{}, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
		t.Fatal(err)
	}
	for i := range pf.Params {
		m := tagsAsMap(shippedTags(t, pf, i))
		if m["spawn:ttl"] != "4h" || m["spawn:idle-timeout"] != "30m" || m["spawn:cost-limit"] != "5.0000" {
			t.Errorf("member %d's orchestrator tags: ttl=%q idle=%q cost=%q, want 4h/30m/5.0000", i,
				m["spawn:ttl"], m["spawn:idle-timeout"], m["spawn:cost-limit"])
		}
	}

	if err := refuseDetachedLifetimeFlags(resumeLifetimeFlags{ttl: "2h"}); err == nil {
		t.Error("resume --detach --ttl was accepted, and the orchestrator would ignore it")
	}
	if err := refuseDetachedLifetimeFlags(resumeLifetimeFlags{}); err != nil {
		t.Errorf("resume --detach with no lifetime flag: %v", err)
	}
}

// The paths reach the mechanism: the launch records the lifetime in the
// checkpoint, runResume plans it before the nested-role check and builds the
// members through it, and refuses lifetime flags before a detached resume.
func TestResumePathsReachTheLifetimeRecord(t *testing.T) {
	fset := token.NewFileSet()
	calls := map[string][]string{}
	for _, file := range []string{"resume.go", "launch_sweep.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					switch fn := x.Fun.(type) {
					case *ast.Ident:
						calls[fd.Name.Name] = append(calls[fd.Name.Name], fn.Name)
					case *ast.SelectorExpr:
						calls[fd.Name.Name] = append(calls[fd.Name.Name], fn.Sel.Name)
					}
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok {
						if c, ok := x.Value.(*ast.CallExpr); ok {
							if fn, ok := c.Fun.(*ast.Ident); ok {
								calls[fd.Name.Name] = append(calls[fd.Name.Name], k.Name+":"+fn.Name)
							}
						}
					}
				}
				return true
			})
		}
	}
	before := func(fn, first, then string) {
		t.Helper()
		i, j := -1, -1
		for k, c := range calls[fn] {
			if c == first && i < 0 {
				i = k
			}
			if c == then && j < 0 {
				j = k
			}
		}
		if i < 0 || j < 0 || i > j {
			t.Errorf("%s: want a call to %s before %s (%d, %d)", fn, first, then, i, j)
		}
	}
	if !slices.Contains(calls["launchParameterSweep"], "lifetime:newSweepLifetime") {
		t.Error("launchParameterSweep does not pass lifetime: newSweepLifetime(...) to the checkpoint")
	}
	before("runResume", "planResumeLifetime", "refuseNestedSweepRowsWithoutTTL")
	before("runResume", "members", "GetRecommendedAMI")
	before("runResume", "refuseDetachedLifetimeFlags", "resumeSweepDetached")
	before("relaunchPending", "refuseUnboundedResume", "launchWithRollingQueue")
	for _, c := range calls["runResume"] {
		if c == "buildLaunchConfigFromParams" {
			t.Error("runResume builds a member with buildLaunchConfigFromParams directly, bypassing the recorded lifetime")
		}
	}
}
