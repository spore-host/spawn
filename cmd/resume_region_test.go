package cmd

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #495: `spawn resume` read the sweep's region with an unchecked
// paramFormat.Defaults["region"].(string). A parameter file whose defaults
// carry no region — every sweep launched with --region alone, since launch
// seeds that default only in memory — panicked as soon as one instance had
// launched. The `region == ""` fallback beneath it was us-east-1, so a sweep
// that survived the assertion could be reconciled against, and continue
// launching into, a region it never used. The issue asks for no panic, and a
// missing region to be a clear error rather than a guess.

func TestResolveResumeRegions_NoRegionAnywhereIsAnErrorNotAPanic(t *testing.T) {
	state := &SweepState{Instances: []InstanceState{{Index: 0, InstanceID: "i-0aaa"}}}
	for name, defaults := range map[string]map[string]interface{}{
		"no region key":     {"instance_type": "t3.micro"},
		"nil defaults":      nil,
		"non-string region": {"region": 42},
	} {
		t.Run(name, func(t *testing.T) {
			pf := &ParamFileFormat{Defaults: defaults, Params: []map[string]interface{}{{"alpha": 1}}}
			_, err := resolveResumeRegions(state, pf, "", "")
			if err == nil {
				t.Fatal("no region from the state, the parameter file or config must be an error, not a guessed region")
			}
			if !strings.Contains(err.Error(), "--region") || !strings.Contains(err.Error(), "i-0aaa") {
				t.Errorf("error should name the instance and the way out (--region); got %q", err)
			}
		})
	}
}

func TestResolveResumeRegions_PrefersTheRecordedRegion(t *testing.T) {
	state := &SweepState{Instances: []InstanceState{
		{Index: 0, InstanceID: "i-recorded", Region: "eu-west-1"},
		{Index: 1, InstanceID: "i-perset"},
		{Index: 2, InstanceID: "i-default"},
		{Index: 3, InstanceID: ""}, // never launched: nothing to reconcile
	}}
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"region": "us-west-2"},
		Params: []map[string]interface{}{
			{"region": "ap-south-1"}, // ignored: i-recorded's own record wins
			{"region": "ca-central-1"},
			{},
			{},
		},
	}
	got, err := resolveResumeRegions(state, pf, "", "sa-east-1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"i-recorded": "eu-west-1", "i-perset": "ca-central-1", "i-default": "us-west-2"}
	for id, reg := range want {
		if got.byInstance[id] != reg {
			t.Errorf("%s: region %q, want %q", id, got.byInstance[id], reg)
		}
	}
	if len(got.byInstance) != len(want) {
		t.Errorf("byInstance = %v, want exactly %v", got.byInstance, want)
	}
	if d := got.distinct(); strings.Join(d, ",") != "ca-central-1,eu-west-1,us-west-2" {
		t.Errorf("distinct regions to reconcile = %v", d)
	}
}

func TestResolveResumeRegions_PendingFallbackNeverDefaultsToUSEast1(t *testing.T) {
	// Nothing launched, nothing configured: there is no fallback, and the
	// caller refuses to launch a set with no region.
	got, err := resolveResumeRegions(&SweepState{}, &ParamFileFormat{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.fallback != "" {
		t.Errorf("fallback = %q with no region known anywhere; want empty (no hard-coded default)", got.fallback)
	}

	recorded := &SweepState{Instances: []InstanceState{{Index: 0, InstanceID: "i-1", Region: "eu-north-1"}}}
	if got, _ := resolveResumeRegions(recorded, &ParamFileFormat{}, "", "us-east-2"); got.fallback != "eu-north-1" {
		t.Errorf("fallback = %q; the sweep's one recorded region beats the ambient config", got.fallback)
	}
	if got, _ := resolveResumeRegions(recorded, &ParamFileFormat{}, "ap-east-1", "ap-east-1"); got.fallback != "ap-east-1" {
		t.Errorf("fallback = %q; an explicit --region beats the recorded region", got.fallback)
	}
}

// TestRunResume_ParamFileWithoutRegionDefaultDoesNotPanic drives the command
// itself with the issue's exact input: an interrupted sweep with one launched
// instance and a parameter file whose defaults have no region. It must return
// an error naming --region, before any AWS client is built.
func TestRunResume_ParamFileWithoutRegionDefaultDoesNotPanic(t *testing.T) {
	resetResumeFlags(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, k := range []string{"SPORE_REGION", "AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Setenv(k, "")
	}
	origRegion := spawnRegion
	spawnRegion = ""
	t.Cleanup(func() { spawnRegion = origRegion })

	paramFile := filepath.Join(home, "params.json")
	pf := map[string]interface{}{
		"defaults": map[string]interface{}{"instance_type": "t3.micro"},
		"params":   []map[string]interface{}{{"alpha": 1}, {"alpha": 2}},
	}
	writeResumeTestJSON(t, paramFile, pf)
	state := &SweepState{
		SweepID: "sweep-495", SweepName: "s", ParamFile: paramFile, TotalParams: 2, MaxConcurrent: 1,
		Instances: []InstanceState{{Index: 0, InstanceID: "i-0launched", State: "running"}},
	}
	if err := saveSweepState(state); err != nil {
		t.Fatal(err)
	}
	resumeSweepID = state.SweepID

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("runResume panicked on a parameter file with no region default: %v", r)
			}
		}()
		err = runResume(resumeCmd, nil)
	}()
	if err == nil || !strings.Contains(err.Error(), "--region") {
		t.Fatalf("want a clear error naming --region, got %v", err)
	}
}

func writeResumeTestJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEverySweepInstanceRecordSetsItsRegion: resume trusts the recorded region
// over every other source, so every place a sweep records a launched instance
// must record where it went. Discovered from the source: every InstanceState
// composite literal in a non-test cmd file.
func TestEverySweepInstanceRecordSetsItsRegion(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "InstanceState" {
				return true
			}
			found++
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Region" {
						return true
					}
				}
			}
			t.Errorf("%s: InstanceState literal records a launched instance without its Region (#495)", fset.Position(lit.Pos()))
			return true
		})
	}
	if found == 0 {
		t.Fatal("discovered no InstanceState literals: the discovery is broken, not the repo clean")
	}
}
