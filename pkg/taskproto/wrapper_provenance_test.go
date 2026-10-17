package taskproto

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1076: the wrapper's provenance path, executed under real bash against a fake
// spored and a fake aws, both logging what they were asked to do.

func runProvenanceWrapper(t *testing.T, spored string) (record map[string]any, sporedLog, awsLog string) {
	t.Helper()
	skipOnWindows(t, "executes the bash wrapper against a fake spored and aws")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	restore := SetLocalRecordDirForTest(t.TempDir())
	t.Cleanup(restore)

	work := t.TempDir()
	out := filepath.Join(work, "out.txt")
	if err := os.WriteFile(out, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &TaskSpec{
		TaskID: "prov", Command: []string{"true"},
		Env:       map[string]string{"API_TOKEN": "s3cr3t"},
		Outputs:   []Manifest{{Source: out, Destination: "s3://out/out.txt"}},
		Lifecycle: Lifecycle{TTL: "1h"},
	}
	w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)

	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	_ = os.MkdirAll(logs, 0o755)
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	writeFakeExe(t, filepath.Join(bin, "aws"), "#!/bin/bash\necho \"$@\" >> "+filepath.Join(logs, "aws")+"\nexit 0\n")
	writeFakeSudo(t, bin)
	sp := filepath.Join(dir, "spored")
	writeFakeExe(t, sp, strings.ReplaceAll(spored, "LOG", filepath.Join(logs, "spored")))

	script := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(script, []byte(w), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script) //nolint:gosec // nosemgrep: the test's own generated script
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SPAWN_SPORED_BIN="+sp)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Logf("wrapper output: %s", o)
	}
	raw, err := os.ReadFile(CompletionRecordPath())
	if err != nil {
		t.Fatalf("no completion record: %v", err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("completion.json is not valid JSON: %v\n%s", err, raw)
	}
	s, _ := os.ReadFile(filepath.Join(logs, "spored"))
	a, _ := os.ReadFile(filepath.Join(logs, "aws"))
	// The spec spored was given must not carry env values.
	if b, err := os.ReadFile(filepath.Join("/tmp/spawn-provenance", testRunID, "spec.json")); err == nil {
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/tmp/spawn-provenance", testRunID)) })
		if strings.Contains(string(b), "s3cr3t") {
			t.Errorf("the provenance spec copy carries an env VALUE: %s", b)
		}
		if !strings.Contains(string(b), "API_TOKEN") {
			t.Errorf("the provenance spec copy lost the env NAME: %s", b)
		}
	}
	return record, string(s), string(a)
}

const goodSpored = `#!/bin/bash
echo "$@" >> LOG
case "$1 $2" in
  "provenance --help") exit 0 ;;
  "provenance cost") printf '0.123456' ;;
esac
exit 0
`

func TestWrapperRecordsProvenanceWithAProvenanceCapableSpored(t *testing.T) {
	rec, sporedLog, awsLog := runProvenanceWrapper(t, goodSpored)
	if rec["cost_estimate"] != 0.123456 {
		t.Errorf("cost_estimate = %v, want 0.123456 from `spored provenance cost`", rec["cost_estimate"])
	}
	if !strings.Contains(sporedLog, "provenance upload --state") || !strings.Contains(sporedLog, "s3://out/out.txt") {
		t.Errorf("the output was not uploaded through spored:\n%s", sporedLog)
	}
	if !strings.Contains(sporedLog, "--started") || !strings.Contains(sporedLog, "--ended") {
		t.Errorf("cost was not given the run's timestamps:\n%s", sporedLog)
	}
	for _, line := range strings.Split(awsLog, "\n") {
		if strings.Contains(line, "s3 cp") && strings.Contains(line, "s3://out/out.txt") {
			t.Errorf("the output was ALSO uploaded with aws s3 cp: %s", line)
		}
	}
}

// An older spored without the subcommand: `provenance --help` fails, so the
// wrapper must use plain `aws s3 cp`, exactly as before. Otherwise the output
// upload itself would fail with "unknown command".
func TestWrapperFallsBackWithASporedWithoutProvenance(t *testing.T) {
	old := "#!/bin/bash\necho \"$@\" >> LOG\nexit 1\n"
	rec, sporedLog, awsLog := runProvenanceWrapper(t, old)
	if _, has := rec["cost_estimate"]; has {
		t.Errorf("cost_estimate present without provenance support: %v", rec["cost_estimate"])
	}
	if strings.Contains(sporedLog, "upload") {
		t.Errorf("the wrapper called `spored provenance upload` on a spored without it:\n%s", sporedLog)
	}
	if !strings.Contains(awsLog, "s3 cp") || !strings.Contains(awsLog, "s3://out/out.txt") {
		t.Errorf("the output was not uploaded with aws s3 cp in the fallback:\n%s", awsLog)
	}
	if rec["state"] != string(StateCompleted) {
		t.Errorf("state = %v; the fallback must not fail the task", rec["state"])
	}
}

// Whatever `spored provenance cost` prints, completion.json stays valid JSON:
// only a plain number becomes cost_estimate.
func TestWrapperDiscardsANonNumericCost(t *testing.T) {
	bad := strings.Replace(goodSpored, "printf '0.123456'", `printf '1,2"; rm -rf /'`, 1)
	rec, _, _ := runProvenanceWrapper(t, bad)
	if _, has := rec["cost_estimate"]; has {
		t.Errorf("a non-numeric cost reached completion.json: %v", rec["cost_estimate"])
	}
}

// A spored whose provenance upload FAILS (as v0.132.0's did on every instance:
// its S3 client had no region, #1094) must not lose the output. The wrapper
// uploads it with plain `aws s3 cp`, untagged, and the task completes.
func TestWrapperFallsBackWhenTheProvenanceUploadFails(t *testing.T) {
	failing := strings.Replace(goodSpored, `"provenance cost") printf '0.123456' ;;`,
		`"provenance cost") printf '0.123456' ;;
  "provenance upload") exit 1 ;;`, 1)
	rec, sporedLog, awsLog := runProvenanceWrapper(t, failing)
	if !strings.Contains(sporedLog, "provenance upload") {
		t.Fatalf("the provenance upload was never attempted:\n%s", sporedLog)
	}
	uploaded := false
	for _, line := range strings.Split(awsLog, "\n") {
		if strings.Contains(line, "s3 cp") && strings.Contains(line, "s3://out/out.txt") {
			uploaded = true
		}
	}
	if !uploaded {
		t.Errorf("a failed provenance upload lost the output: no aws s3 cp fallback:\n%s", awsLog)
	}
	if rc, _ := rec["retry_class"].(string); rec["state"] != string(StateCompleted) || rc != "" {
		t.Errorf("state=%v retry_class=%v; a delivered output must not fail the task", rec["state"], rec["retry_class"])
	}
}
