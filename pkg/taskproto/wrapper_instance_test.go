package taskproto

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// spawn#1260: completion.json carries the instance type and purchase the
// instance reported through IMDS, so a reader can price a record with no
// cost_estimate after EC2 has forgotten the instance. These run the generated
// wrapper under real bash with a fake `curl` standing in for IMDS (and the
// usual fake aws/sudo), and read the record it wrote.

// fakeIMDSCurl answers the three IMDSv2 requests the wrapper makes. The
// metadata answers are only given to a request carrying the token, as real
// IMDSv2 does; lifeCycle "" makes instance-life-cycle a 404 (curl -f exits 22).
func fakeIMDSCurl(instanceType, lifeCycle string) string {
	return `#!/bin/bash
url=''
for a in "$@"; do case "$a" in http://*) url="$a" ;; esac; done
hdrs="$*"
case "$url" in
  */api/token) printf 'TOK123'; exit 0 ;;
esac
case "$hdrs" in *"X-aws-ec2-metadata-token: TOK123"*) ;; *) exit 22 ;; esac
case "$url" in
  */meta-data/instance-type) v=` + shQuote(instanceType) + ` ;;
  */meta-data/instance-life-cycle) v=` + shQuote(lifeCycle) + ` ;;
  *) exit 22 ;;
esac
[ -n "$v" ] || exit 22
printf '%s' "$v"
`
}

// noIMDSCurl is curl off EC2: the connection is refused.
const noIMDSCurl = "#!/bin/bash\nexit 7\n"

func runWrapperWithCurl(t *testing.T, curl string) map[string]any {
	t.Helper()
	skipOnWindows(t, "executes the bash wrapper against fake curl/aws on a POSIX PATH")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	restore := SetLocalRecordDirForTest(t.TempDir())
	t.Cleanup(restore)

	spec := &TaskSpec{TaskID: "imds", Command: []string{"true"}, Lifecycle: Lifecycle{TTL: "1h"}}
	w := genWrapper(spec, "results-bucket", "us-east-1", false, testRunID)

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeExe(t, filepath.Join(bin, "aws"), "#!/bin/bash\nexit 0\n")
	writeFakeExe(t, filepath.Join(bin, "curl"), curl)
	writeFakeSudo(t, bin)

	script := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(script, []byte(w), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script) //nolint:gosec // nosemgrep: the test's own generated script
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SPAWN_SPORED_BIN="+filepath.Join(dir, "no-spored"))
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Logf("wrapper output: %s", o)
	}
	raw, err := os.ReadFile(CompletionRecordPath())
	if err != nil {
		t.Fatalf("no completion record: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("completion.json is not valid JSON: %v\n%s", err, raw)
	}
	if _, err := ParseCompletionRecord(raw); err != nil {
		t.Fatalf("completion.json does not parse as a CompletionRecord: %v\n%s", err, raw)
	}
	return rec
}

func TestWrapperRecordsInstanceTypeAndPurchase(t *testing.T) {
	cases := []struct {
		name, lifeCycle, wantPurchase string
	}{
		{"spot", "spot", PurchaseSpot},
		{"on-demand", "on-demand", PurchaseOnDemand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := runWrapperWithCurl(t, fakeIMDSCurl("m7i.large", tc.lifeCycle))
			if rec["instance_type"] != "m7i.large" {
				t.Errorf("instance_type = %v, want m7i.large from IMDS", rec["instance_type"])
			}
			if rec["purchase"] != tc.wantPurchase {
				t.Errorf("purchase = %v, want %q for instance-life-cycle %q", rec["purchase"], tc.wantPurchase, tc.lifeCycle)
			}
			if rec["state"] != string(StateCompleted) {
				t.Errorf("state = %v; the IMDS lookup must not affect the task", rec["state"])
			}
		})
	}
}

// Off EC2, or with IMDS unreachable, both fields are left out rather than
// guessed, and the record is still written and valid.
func TestWrapperOmitsInstanceFactsWithoutIMDS(t *testing.T) {
	rec := runWrapperWithCurl(t, noIMDSCurl)
	for _, k := range []string{"instance_type", "purchase"} {
		if v, has := rec[k]; has {
			t.Errorf("%s = %v with no IMDS; it must be omitted", k, v)
		}
	}
	if rec["state"] != string(StateCompleted) {
		t.Errorf("state = %v; a missing IMDS must not fail the task", rec["state"])
	}
}

// Whatever IMDS (or a fake of it) answers, completion.json stays valid JSON:
// only a plain instance type and a known life cycle reach the record.
func TestWrapperDropsUnexpectedInstanceFacts(t *testing.T) {
	rec := runWrapperWithCurl(t, fakeIMDSCurl(`m7i.large", "state": "completed`, "scheduled"))
	for _, k := range []string{"instance_type", "purchase"} {
		if v, has := rec[k]; has {
			t.Errorf("%s = %v reached completion.json from an unexpected IMDS answer", k, v)
		}
	}
}

// The terminal-flush hook's record carries the same two fields, read from the
// file the wrapper wrote, so a task cut short by its TTL is priceable too.
func TestFlushRecordCarriesTheWrappersInstanceFacts(t *testing.T) {
	restore := SetLocalRecordDirForTest(t.TempDir())
	t.Cleanup(restore)
	if err := os.WriteFile(instanceFactsPath(), []byte(`"instance_type": "c7g.xlarge", "purchase": "spot",`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := runFlushHook(t, genFlush(flushSpec(), "results-bucket", "us-east-1", testRunID), string(ExitTTLExpired))
	raw, err := os.ReadFile(localPath(terminalFileName))
	if err != nil {
		t.Fatalf("no terminal record: %v\n%s", err, out)
	}
	rec, err := ParseCompletionRecord(raw)
	if err != nil {
		t.Fatalf("terminal record does not parse: %v\n%s", err, raw)
	}
	if rec.InstanceType != "c7g.xlarge" || rec.Purchase != PurchaseSpot {
		t.Errorf("instance_type/purchase = %q/%q, want c7g.xlarge/spot\n%s", rec.InstanceType, rec.Purchase, raw)
	}
	if strings.Contains(string(raw), "on_demand") {
		t.Errorf("unexpected purchase in %s", raw)
	}
}
