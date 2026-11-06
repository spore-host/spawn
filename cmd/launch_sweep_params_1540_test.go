package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// inlineSweepJSON is a sweep in the JSON param-file schema, with a grid: so
// the expansion is covered as well as explicit rows: 1 explicit row + 2x2
// grid rows = 5.
const inlineSweepJSON = `{
  "defaults": {"instance_type": "t4g.small", "ttl": "1h"},
  "params": [{"alpha": 0}],
  "grid": {"alpha": [1, 2], "beta": ["x", "y"]}
}`

// inlineSweepFlags is runModeLaunch's flag set for a sweep given by flag.
func inlineSweepFlags(flag, value string) map[string]string {
	return map[string]string{
		flag: value, "region": "us-east-1", "yes": "true", "no-dns": "true",
		"ami": "ami-0123456789abcdef0", "os": "linux", "sweep-name": "inl",
	}
}

func writeParamFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestInlineParamsSweepReachesTheSweepPath is #1540: an inline --params sweep
// used to fail at once with "inline --params not yet implemented". It must
// reach the sweep path (the detached one here, recognised by its look for
// SpawnSweepCrossAccountRole) with the document's rows.
func TestInlineParamsSweepReachesTheSweepPath(t *testing.T) {
	out, role, err := runModeLaunch(t, inlineSweepFlags("params", inlineSweepJSON))
	if err != nil && strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("inline --params is still unimplemented: %v", err)
	}
	if role.count() == 0 || !isRoleRefusal(err) {
		t.Fatalf("did not reach the sweep path: err = %v\n%s", err, out)
	}
	for _, want := range []string{"Parameters: 5", "Orchestration: Lambda (detached)", "PARAM_alpha, PARAM_beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("sweep summary lacks %q:\n%s", want, out)
		}
	}
}

// sweepIDPattern matches the generated sweep ID, which carries the time.
var sweepIDPattern = regexp.MustCompile(`-\d{8}-\d+`)

// TestInlineParamsMatchTheEquivalentJSONFile: --params is the JSON param-file
// schema read by the same parser, so the same document gives the same rows,
// and the same sweep, as a --param-file .json holding it.
func TestInlineParamsMatchTheEquivalentJSONFile(t *testing.T) {
	file := writeParamFile(t, "sweep.json", inlineSweepJSON)

	fromFile, err := loadSweepParams(file, "")
	if err != nil {
		t.Fatal(err)
	}
	inline, err := loadSweepParams("", inlineSweepJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromFile, inline) {
		t.Errorf("inline --params parsed differently from the same JSON file:\n file:   %#v\n inline: %#v", fromFile, inline)
	}
	if len(inline.Params) != 5 {
		t.Errorf("rows = %d, want 5 (1 explicit + 2x2 grid)", len(inline.Params))
	}

	// Each in its own subtest, whose cleanup resets the flags it set.
	run := func(flag, value string) (out string, err error) {
		t.Run(flag, func(t *testing.T) {
			out, _, err = runModeLaunch(t, inlineSweepFlags(flag, value))
		})
		return sweepIDPattern.ReplaceAllString(out, "-ID"), err
	}
	fileOut, fileErr := run("param-file", file)
	inlineOut, inlineErr := run("params", inlineSweepJSON)
	if fileOut != inlineOut {
		t.Errorf("the same sweep printed differently:\n--param-file:\n%s\n--params:\n%s", fileOut, inlineOut)
	}
	if (fileErr == nil) != (inlineErr == nil) || (fileErr != nil && fileErr.Error() != inlineErr.Error()) {
		t.Errorf("the same sweep ended differently: --param-file %v, --params %v", fileErr, inlineErr)
	}
}

// TestParamsWithParamFileIsRefused: the two are each a whole parameter
// document; given both, the launch is refused before any AWS call.
func TestParamsWithParamFileIsRefused(t *testing.T) {
	flags := inlineSweepFlags("params", inlineSweepJSON)
	flags["param-file"] = writeParamFile(t, "sweep.json", inlineSweepJSON)
	_, role, err := runModeLaunch(t, flags)
	if err == nil || !strings.Contains(err.Error(), "--params and --param-file cannot be used together") {
		t.Fatalf("err = %v, want the --params/--param-file refusal", err)
	}
	if role.count() != 0 {
		t.Error("a sweep with both --params and --param-file reached the sweep path")
	}
}

// TestParamsGivenAPathIsRedirectedToParamFile is the copy-paste mistake
// #1540 found in the examples: a file passed to --params. Whether or not the
// file exists, the error names the flag to use, and the file is never read
// (the existing one is not a valid param file, so a read would fail
// differently).
func TestParamsGivenAPathIsRedirectedToParamFile(t *testing.T) {
	existing := writeParamFile(t, "rows", "not: [a, valid, param file")
	for desc, path := range map[string]string{
		"existing file, no extension": existing,
		"missing .yaml":               filepath.Join(t.TempDir(), "sweep.yaml"),
		"missing .yml":                "examples/nope.yml",
		"missing .csv":                "rows.csv",
	} {
		t.Run(desc, func(t *testing.T) {
			want := "--params takes inline JSON; to use the file " + path + ", pass --param-file " + path
			_, role, err := runModeLaunch(t, inlineSweepFlags("params", path))
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
			if role.count() != 0 {
				t.Error("a path passed to --params reached the sweep path")
			}
		})
	}
}

// TestInlineParamsSyntaxErrorHasAPosition: a malformed document is reported
// as a JSON error with the line and column, not as a path.
func TestInlineParamsSyntaxErrorHasAPosition(t *testing.T) {
	cases := []struct{ desc, value, want string }{
		{"syntax error", "{\"params\": [\n  {\"a\": 1},\n  {\"a\": 2,}\n]}", "line 3, column 11"},
		{"type error", `{"params": {"a": 1}}`, "line 1, column 12"},
		{"bare word", "lr=0.1", "line 1, column 1"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := parseInlineParams(tc.value)
			if err == nil || !strings.Contains(err.Error(), "--params: failed to parse inline JSON parameters: "+tc.want) {
				t.Fatalf("err = %v, want a JSON error at %s", err, tc.want)
			}
		})
	}
	// And through runLaunch, before any AWS call.
	_, role, err := runModeLaunch(t, inlineSweepFlags("params", `{"params": [`))
	if err == nil || !strings.Contains(err.Error(), "line 1, column 12") {
		t.Fatalf("err = %v, want a JSON error with its position", err)
	}
	if role.count() != 0 {
		t.Error("malformed --params reached the sweep path")
	}
}

// TestInlineParamsCheckpointIsResumable: a --no-detach sweep's checkpoint is
// how spawn resume rebuilds it, and an inline sweep has no file to reload,
// so the checkpoint carries the document.
func TestInlineParamsCheckpointIsResumable(t *testing.T) {
	if got := inlineParamsFor("", inlineSweepJSON); got != inlineSweepJSON {
		t.Errorf("an inline sweep checkpoints %q, want its document", got)
	}
	if got := inlineParamsFor("p.json", inlineSweepJSON); got != "" {
		t.Errorf("a param-file sweep checkpoints inline params %q", got)
	}
	if got := sweepParamSource(""); got != inlineParamsSource {
		t.Errorf("sweepParamSource(\"\") = %q", got)
	}
}
