package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestRecognizedRowKeysMatchesSwitch is the reason recognizedRowKeys is allowed to
// exist as a hand-written duplicate of buildLaunchConfigFromParams' case labels.
// It reads the switch out of cmd/sweep.go with go/ast and requires exact equality
// in both directions:
//
//   - a case label missing from the map means a REAL spawn key is treated as a
//     near-miss candidate and possibly rejected — a working param file breaks
//   - a map entry with no case label means a key spawn ignores is advertised as
//     recognised, which is the #526 bug wearing a different hat
//
// Without this test the map would drift the first time someone adds a field, and
// the drift would be silent in exactly the direction that costs money.
func TestRecognizedRowKeysMatchesSwitch(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sweep.go", nil, 0)
	if err != nil {
		t.Fatalf("parse sweep.go: %v", err)
	}

	fn := findFuncDecl(file, "buildLaunchConfigFromParams")
	if fn == nil {
		t.Fatal("buildLaunchConfigFromParams not found in sweep.go — this test's premise is gone, " +
			"not its subject: find where the param keys are switched on now and re-point it")
	}

	labels := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		// Only the switch over the merged param key, not any inner switch.
		if id, ok := sw.Tag.(*ast.Ident); !ok || id.Name != "key" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("non-literal case in the param-key switch: %T — this test can only "+
						"see string literals", expr)
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Errorf("unquote case %s: %v", lit.Value, err)
					continue
				}
				labels[s] = true
			}
		}
		return true
	})

	if len(labels) == 0 {
		t.Fatal("found no case labels in the param-key switch — the AST walk is broken, and a " +
			"broken walk here would make every assertion below pass vacuously")
	}

	for k := range labels {
		if !recognizedRowKeys[k] {
			t.Errorf("case %q exists in buildLaunchConfigFromParams but is missing from "+
				"recognizedRowKeys — spawn acts on that key, so it must not be treated as an "+
				"unknown one", k)
		}
	}
	for k := range recognizedRowKeys {
		if !labels[k] {
			t.Errorf("recognizedRowKeys has %q but buildLaunchConfigFromParams has no case for it "+
				"— that key is advertised as recognised and silently does nothing", k)
		}
	}
}

func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// TestClassifyRowKeyRejects covers each rule with the cases from the #526 table,
// and asserts on the guidance text rather than just "an error happened" — the
// whole value of this check is that the message tells the user the right spelling.
func TestClassifyRowKeyRejects(t *testing.T) {
	tests := []struct {
		key      string
		wantHint string // substring the message must contain
		why      string
	}{
		{"ttl_hours", "ttl:", "a TTL the user believes they set — the expensive case"},
		{"ttl_minutes", "ttl:", "same, other unit"},
		{"max_runtime", "ttl:", "plausible name from other schedulers"},
		{"walltime", "ttl:", "the HPC spelling"},
		{"max_cost", "cost_limit:", "a dollar cap that capped nothing"},
		{"budget", "cost_limit:", "no such key; point at what does work"},
		{"max_concurrent", "command-line flag", "CLI-only"},
		{"launch_delay", "command-line flag", "CLI-only"},
		{"instance_types", "grid:", "plural means rows or a grid, not a list in one row"},
		{"image_id", "ami:", "wrong name for the same thing"},
		{"spot_price", "spot_max_price:", "near-miss on a real key"},
		// #545: this used to point at "--disk-size", a flag that has never
		// existed in this codebase — only --volume-size does. The message must
		// name the real, working key (volume_size:), not a nonexistent flag.
		{"disk_size", "volume_size:", "near-miss spelling redirected at the real key, not a dead flag name"},
		{"on-complete", "on_complete", "hyphen instead of underscore — rule B"},
		{"instance-type", "instance_type", "hyphen on the key that decides what you pay for"},
		{"cost-limit", "cost_limit", "hyphen on the dollar cap"},
		{"TTL", "ttl", "wrong case — rule B"},
		{"Instance_Type", "instance_type", "inherited capitalisation"},
		{"my-workload-flag", "not a valid shell identifier", "not a spawn key, but cannot be an env var either — rule A"},
		{"2fast", "not a valid shell identifier", "leading digit"},
		{"has space", "not a valid shell identifier", "space"},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			if recognizedRowKeys[tc.key] {
				t.Fatalf("%q is a recognised key, so classifyRowKey is never called on it — "+
					"this case is testing nothing", tc.key)
			}
			err := classifyRowKey(tc.key)
			if err == nil {
				t.Fatalf("classifyRowKey(%q) = nil, want an error (%s)", tc.key, tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("classifyRowKey(%q) message does not contain %q, so it does not tell the "+
					"user what to write instead:\n  %v", tc.key, tc.wantHint, err)
			}
		})
	}
}

// TestVolumeSizeIsNoLongerRejected is the #544/#545 regression guard:
// volume_size: used to be on reservedRowKeys (rejected, pointing at a
// nonexistent "--disk-size" flag) and is now a real, working key in
// recognizedRowKeys instead. Both halves of that swap are asserted, because
// leaving it in both maps would make the reserved entry dead code (see
// TestNoRecognizedKeyIsAlsoReserved) rather than actually fixed.
func TestVolumeSizeIsNoLongerRejected(t *testing.T) {
	if !recognizedRowKeys["volume_size"] {
		t.Error(`"volume_size" must be in recognizedRowKeys — it is now a real ` +
			"param-file key (#544), not an unknown one")
	}
	if _, reserved := reservedRowKeys["volume_size"]; reserved {
		t.Error(`"volume_size" must not be in reservedRowKeys — that would make it ` +
			"rejected again, the exact bug #544/#545 fixed")
	}
	// classifyRowKey itself doesn't consult recognizedRowKeys (that gate lives in
	// the caller — buildLaunchConfigFromParams's switch handles a recognized key
	// before classifyRowKey is ever reached), so calling it directly on
	// "volume_size" exercises only rules A/B/C and must find none of them fire:
	// it is not a near-miss, not reserved, and a valid shell identifier.
	if err := classifyRowKey("volume_size"); err != nil {
		t.Errorf(`classifyRowKey("volume_size") = %v, want nil — it is no longer reserved`, err)
	}
}

// TestDiskSizeMessageNamesTheRealFlag is the #545 fail-without/pass-with case
// on its own: the message must name a flag/key that actually exists in this
// codebase. Pre-fix it named "--disk-size", which `grep -rn "disk-size"`
// across the whole repo turns up nowhere else — a rejection with no working
// spelling to try.
func TestDiskSizeMessageNamesTheRealFlag(t *testing.T) {
	err := classifyRowKey("disk_size")
	if err == nil {
		t.Fatal(`classifyRowKey("disk_size") = nil, want an error (it is a near-miss for volume_size:)`)
	}
	msg := err.Error()
	if strings.Contains(msg, "--disk-size") {
		t.Errorf("message still names the nonexistent --disk-size flag: %v", msg)
	}
	if !strings.Contains(msg, "volume_size") {
		t.Errorf("message does not name the real key (volume_size:): %v", msg)
	}
}

// TestClassifyRowKeyAllowsWorkloadParams is the half of this feature that must NOT
// regress. Every key here is a real parameter someone sweeps over, and several
// deliberately resemble a spawn setting — `image` is a container image, `steps` is
// an MD step count next to spawn's `step`. A denylist that eats these is a worse
// bug than the one it fixes, so they are pinned as allowed.
func TestClassifyRowKeyAllowsWorkloadParams(t *testing.T) {
	allowed := []string{
		"alpha", "beta", "learning_rate", "batch_size", "epochs", "seed",
		"image", "container", "type", "count", "steps", "nsteps", "threads",
		"dataset", "optimizer", "temperature", "cutoff", "n_gpu", "mdp",
		"TEMPERATURE", "N_REPLICAS", "_private", "x2", "gmx_bin",
		// timeout is documented vocabulary on the workflow path
		// (examples/workflow-ci-pipeline.yaml, pkg/queue.JobConfig.Timeout), not a
		// near-miss. The first draft of the denylist rejected it and
		// TestBuildLaunchConfigFromParams_WorkflowStep failed — that test is the
		// reason this entry is pinned here.
		"timeout", "runtime", "instance", "instances", "idle",
	}
	for _, key := range allowed {
		if recognizedRowKeys[key] {
			t.Errorf("%q is in recognizedRowKeys, so it is not a passthrough parameter at all — "+
				"this list is asserting the wrong thing about it", key)
			continue
		}
		if err := classifyRowKey(key); err != nil {
			t.Errorf("classifyRowKey(%q) rejected a legitimate workload parameter: %v", key, err)
		}
	}
}

// TestNoRecognizedKeyIsAlsoReserved: an entry in both maps would be unreachable
// (the switch handles it before classifyRowKey is called), so it is dead guidance
// that reads as active. The overlap is easy to introduce by adding a real key
// whose name someone already listed as a near-miss.
func TestNoRecognizedKeyIsAlsoReserved(t *testing.T) {
	for k := range reservedRowKeys {
		if recognizedRowKeys[k] {
			t.Errorf("%q is both recognised and reserved — the reserved entry can never fire", k)
		}
	}
}

// TestReservedKeysAreNormalized: lookups happen on the normalized key, so a
// reserved entry written with a hyphen or a capital could never match. That would
// be a check that cannot fail, which is the failure mode this repo keeps hitting.
func TestReservedKeysAreNormalized(t *testing.T) {
	for k := range reservedRowKeys {
		if norm := normalizeKey(k); norm != k {
			t.Errorf("reserved key %q is not in normalized form (%q), so it can never be matched", k, norm)
		}
	}
}

func TestValidateSweepParamKeysReportsEveryProblem(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{
			"on_complete": "terminate",
			"budget":      50,
		},
		Params: []map[string]interface{}{
			{"instance_type": "c7g.16xlarge", "ttl_hours": 4},
			{"instance_type": "c6i.32xlarge", "alpha": 0.5},
			{"instance_type": "g5.xlarge", "max_cost": 20},
		},
	}
	err := validateSweepParamKeys(pf)
	if err != nil {
		msg := err.Error()
		// One error naming all three, not one round trip per typo.
		for _, want := range []string{"budget", "ttl_hours", "max_cost", "defaults:", "c7g.16xlarge", "g5.xlarge"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error does not mention %q:\n%s", want, msg)
			}
		}
		if strings.Contains(msg, "alpha") {
			t.Errorf("error names alpha, which is a legitimate workload parameter:\n%s", msg)
		}
		if strings.Contains(msg, "c6i.32xlarge") {
			t.Errorf("error names the row whose only extra key is legitimate:\n%s", msg)
		}
	} else {
		t.Error("validateSweepParamKeys accepted a file with budget:, ttl_hours: and max_cost:")
	}
}

func TestValidateSweepParamKeysAcceptsCleanFile(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"ttl": "1h", "on_complete": "terminate", "cost_limit": 8},
		Params: []map[string]interface{}{
			{"instance_type": "c7g.16xlarge", "isa": "neon", "nsteps": 500000},
			{"instance_type": "c8g.16xlarge", "isa": "sve", "nsteps": 500000, "ttl": "30m"},
		},
	}
	if err := validateSweepParamKeys(pf); err != nil {
		t.Errorf("rejected a valid file: %v", err)
	}
}

// TestBuildLaunchConfigRejectsDangerousKeyAtTheSeam: the check also lives in
// buildLaunchConfigFromParams, because resume and the quota preflight call that
// directly and never pass through launchParameterSweep's early validation.
func TestBuildLaunchConfigRejectsDangerousKeyAtTheSeam(t *testing.T) {
	_, err := buildLaunchConfigFromParams(
		map[string]interface{}{"ttl_hours": 4},
		map[string]interface{}{"instance_type": "c5.large"},
		"sweep-1", "bench", 0, 1,
	)
	if err == nil {
		t.Fatal("buildLaunchConfigFromParams accepted ttl_hours: — resume would rebuild an " +
			"unbounded instance from a file the launch path had rejected")
	}
	if !strings.Contains(err.Error(), "ttl") {
		t.Errorf("error does not point at the right spelling: %v", err)
	}
}

func TestBuildLaunchConfigStillPassesThroughParams(t *testing.T) {
	config, err := buildLaunchConfigFromParams(
		map[string]interface{}{"ttl": "1h"},
		map[string]interface{}{"instance_type": "c5.large", "alpha": 0.1, "nsteps": 1000},
		"sweep-1", "bench", 0, 1,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.Parameters["alpha"] != "0.1" {
		t.Errorf("alpha = %q, want \"0.1\" — passthrough is the feature", config.Parameters["alpha"])
	}
	if config.Parameters["nsteps"] != "1000" {
		t.Errorf("nsteps = %q, want \"1000\"", config.Parameters["nsteps"])
	}
	if config.TTL != "1h" {
		t.Errorf("TTL = %q, want 1h", config.TTL)
	}
}

func TestPassthroughKeysExcludesRecognized(t *testing.T) {
	got := passthroughKeys(map[string]interface{}{
		"instance_type": "c5.large",
		"ttl":           "1h",
		"cost_limit":    8,
		"beta":          2,
		"alpha":         1,
	})
	want := []string{"alpha", "beta"}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("passthroughKeys = %v, want %v", got, want)
	}
}

// TestExplicitParamPrefixEscapesTheDenylist: every reserved name must have a way
// through, or the check has replaced a silent misconfiguration with a hard block on
// a legitimate parameter — `budget` is an optimiser's evaluation budget as often as
// it is a dollar figure, and `time_limit` is a standard solver option.
func TestExplicitParamPrefixEscapesTheDenylist(t *testing.T) {
	for _, key := range []string{"param:budget", "param:ttl_hours", "param:time_limit", "param:max_cost"} {
		name, err := resolveParamName(key)
		if err != nil {
			t.Errorf("resolveParamName(%q) = %v, want it to pass through — a denylist with no "+
				"escape hatch blocks the feature it is protecting", key, err)
			continue
		}
		want := strings.TrimPrefix(key, explicitParamPrefix)
		if name != want {
			t.Errorf("resolveParamName(%q) = %q, want %q (the prefix must not reach the env var)",
				key, name, want)
		}
	}
}

// TestExplicitParamPrefixStillEnforcesRuleA: the prefix opts out of the near-miss
// and reserved checks, not out of "this has to be a legal env var name".
func TestExplicitParamPrefixStillEnforcesRuleA(t *testing.T) {
	for _, key := range []string{"param:on-complete", "param:2fast", "param:has space", "param:"} {
		if _, err := resolveParamName(key); err == nil {
			t.Errorf("resolveParamName(%q) = nil, want an error: the name after the prefix still "+
				"becomes PARAM_<name> in /etc/profile.d", key)
		}
	}
}

// TestReservedKeyErrorNamesTheEscapeHatch: the message has to carry the remedy,
// otherwise the user's only option is to guess.
func TestReservedKeyErrorNamesTheEscapeHatch(t *testing.T) {
	err := classifyRowKey("budget")
	if err == nil {
		t.Fatal("budget: was accepted")
	}
	for _, want := range []string{"param:budget", "PARAM_budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so the rejection is a dead end:\n  %v", want, err)
		}
	}
}

// TestClassifyTopLevelKeyErrorsOnDangerousKeys covers #530: a top-level key
// that IS a recognized row-level setting must be a hard error, because
// silently ignoring ttl:/idle_timeout:/cost_limit: at this level produces an
// unbounded instance with no warning at all — the same failure #526 fixed one
// level down, one level up.
func TestClassifyTopLevelKeyErrorsOnDangerousKeys(t *testing.T) {
	tests := []struct {
		key      string
		wantHint string
	}{
		{"ttl", "defaults:"},
		{"idle_timeout", "defaults:"},
		{"cost_limit", "defaults:"},
		{"instance_type", "defaults:"},
		{"on_complete", "defaults:"},
		// A recognized key misspelled at the top level: still an error, and the
		// message should still carry the correct spelling.
		{"TTL", "ttl"},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			isError, msg := classifyTopLevelKey(tc.key)
			if !isError {
				t.Fatalf("classifyTopLevelKey(%q) = warning, want error: %s", tc.key, msg)
			}
			if !strings.Contains(msg, tc.wantHint) {
				t.Errorf("classifyTopLevelKey(%q) message %q does not contain %q", tc.key, msg, tc.wantHint)
			}
		})
	}
}

// TestClassifyTopLevelKeyErrorsOnReservedKeys: the reserved-name table (CLI-only
// flags and near-misses) must also be a hard error at the top level — this is
// exactly the mistake examples/schedule-params.yaml shipped with
// (sweep_name:/max_concurrent:/launch_delay: at the top level, read by nothing).
func TestClassifyTopLevelKeyErrorsOnReservedKeys(t *testing.T) {
	for _, key := range []string{"sweep_name", "max_concurrent", "launch_delay", "ttl_hours", "budget"} {
		t.Run(key, func(t *testing.T) {
			isError, msg := classifyTopLevelKey(key)
			if !isError {
				t.Fatalf("classifyTopLevelKey(%q) = warning, want error: %s", key, msg)
			}
			if !strings.Contains(msg, key) {
				t.Errorf("classifyTopLevelKey(%q) message does not name the key: %s", key, msg)
			}
		})
	}
}

// TestClassifyTopLevelKeyWarnsOnHarmlessKeys: anything that is neither a
// recognized row-level key nor on the reserved list is a warning, not an
// error — most top-level clutter (description:, version:, author:) is
// harmless metadata, and hard-failing on it would break files that already
// carry it.
func TestClassifyTopLevelKeyWarnsOnHarmlessKeys(t *testing.T) {
	for _, key := range []string{"description", "version", "author", "notes"} {
		t.Run(key, func(t *testing.T) {
			isError, msg := classifyTopLevelKey(key)
			if isError {
				t.Errorf("classifyTopLevelKey(%q) = error, want warning: %s", key, msg)
			}
			if !strings.Contains(msg, key) {
				t.Errorf("classifyTopLevelKey(%q) message does not name the key: %s", key, msg)
			}
		})
	}
}

// TestValidateTopLevelParamKeysErrors: a dangerous top-level key fails
// validateTopLevelParamKeys with a message naming both the key and the file.
func TestValidateTopLevelParamKeysErrors(t *testing.T) {
	err := validateTopLevelParamKeys("sweep.yaml", []string{"ttl", "description"})
	if err == nil {
		t.Fatal("validateTopLevelParamKeys accepted a file with a top-level ttl:")
	}
	for _, want := range []string{"ttl", "sweep.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	// The harmless key must not have been escalated into the error too.
	if strings.Contains(err.Error(), "description") {
		t.Errorf("error names the harmless key, which should only warn:\n%v", err)
	}
}

// TestValidateTopLevelParamKeysWarnOnlyPasses: a file with only harmless
// unknown top-level keys must not fail validation — the check is a warning
// for these, not a hard stop.
func TestValidateTopLevelParamKeysWarnOnlyPasses(t *testing.T) {
	if err := validateTopLevelParamKeys("sweep.yaml", []string{"description", "version"}); err != nil {
		t.Errorf("validateTopLevelParamKeys rejected a file with only harmless top-level keys: %v", err)
	}
}

// TestValidateTopLevelParamKeysAcceptsCleanFile: no unknown top-level keys at
// all must not fail or warn.
func TestValidateTopLevelParamKeysAcceptsCleanFile(t *testing.T) {
	if err := validateTopLevelParamKeys("sweep.yaml", nil); err != nil {
		t.Errorf("validateTopLevelParamKeys rejected a file with no unknown top-level keys: %v", err)
	}
}

// TestExplicitParamPrefixReachesParameters is the end-to-end of the escape hatch
// through the real merge function: prefix stripped, value intact, and no spawn
// setting touched by a key that merely looked like one.
func TestExplicitParamPrefixReachesParameters(t *testing.T) {
	config, err := buildLaunchConfigFromParams(
		map[string]interface{}{"ttl": "1h"},
		map[string]interface{}{"instance_type": "c5.large", "param:budget": 50, "param:time_limit": 300},
		"sweep-1", "bench", 0, 1,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.Parameters["budget"] != "50" {
		t.Errorf("PARAM_budget = %q, want \"50\"", config.Parameters["budget"])
	}
	if config.Parameters["time_limit"] != "300" {
		t.Errorf("PARAM_time_limit = %q, want \"300\"", config.Parameters["time_limit"])
	}
	if _, leaked := config.Parameters["param:budget"]; leaked {
		t.Error("the param: prefix leaked into the parameter name")
	}
	if config.TTL != "1h" {
		t.Errorf("TTL = %q, want 1h — an escaped key must not disturb the real settings", config.TTL)
	}
}

// TestValidateSweepParamKeysRejectsNewlineInValue is the value-side half of
// #531: a passthrough parameter value containing a literal newline cannot
// survive as an EC2 tag round-trip through pkg/launcher/bootstrap.go's
// one-tag-per-line `read -r key value` loop, so it must be rejected here,
// before anything is launched or priced — the same "fail before AWS is
// touched" seam #526 used for the key side of this exact line.
func TestValidateSweepParamKeysRejectsNewlineInValue(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"ttl": "1h", "on_complete": "terminate"},
		Params: []map[string]interface{}{
			{"instance_type": "c5.large", "label": "line one\nline two"},
		},
	}
	err := validateSweepParamKeys(pf)
	if err == nil {
		t.Fatal("validateSweepParamKeys accepted a value containing a newline")
	}
	if !strings.Contains(err.Error(), "label") {
		t.Errorf("error does not name the offending key:\n%v", err)
	}
}

// TestValidateSweepParamKeysAcceptsNewlineFreeValue is the pass-with half: a
// value that merely looks unusual (quotes, dollar signs, backticks) but has
// no newline must still be accepted at this seam — that content is exactly
// what the bootstrap.go quoting fix now makes safe, so the key-validation
// seam must not start rejecting it too.
func TestValidateSweepParamKeysAcceptsNewlineFreeValue(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"ttl": "1h", "on_complete": "terminate"},
		Params: []map[string]interface{}{
			{"instance_type": "c5.large", "label": `run "A"`, "prefix": "$HOME/out", "cmd": "`hostname`"},
		},
	}
	if err := validateSweepParamKeys(pf); err != nil {
		t.Errorf("rejected a value with no newline: %v", err)
	}
}

// TestBuildLaunchConfigRejectsNewlineValueAtTheSeam mirrors
// TestBuildLaunchConfigRejectsDangerousKeyAtTheSeam for the value side: resume
// and the quota preflight call buildLaunchConfigFromParams directly and never
// go through launchParameterSweep's early validation, so the same newline
// check must also live here.
func TestBuildLaunchConfigRejectsNewlineValueAtTheSeam(t *testing.T) {
	_, err := buildLaunchConfigFromParams(
		map[string]interface{}{"ttl": "1h"},
		map[string]interface{}{"instance_type": "c5.large", "label": "a\nb"},
		"sweep-1", "bench", 0, 1,
	)
	if err == nil {
		t.Fatal("buildLaunchConfigFromParams accepted a parameter value containing a newline")
	}
	if !strings.Contains(err.Error(), "label") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}
