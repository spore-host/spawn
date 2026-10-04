package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestStatusJSONStdoutIsPureJSON is the guard spawn#540 asked for.
//
// `spawn status <id> -o json` printed the human table on stdout, preceded by
// four spored agent log lines. The exit code was 0, so a caller doing
// json.loads(check_output(...)) failed at character 4 with "Extra data" — and
// misleadingly, because the leading "2026" of the log timestamp parses as a
// valid JSON number, so the decoder's error pointed at the REST of the output
// rather than at "this is not JSON".
//
// spored's own side has TestRenderStatusJSON_StdoutIsPureJSON. spawn's relay —
// which adds six human annotations of its own — had no test at all, so nothing
// stopped one of them being written to stdout and corrupting the document.
func TestStatusJSONStdoutIsPureJSON(t *testing.T) {
	// A realistic spored JSON document. The exact schema is spored's business;
	// what matters here is that spawn relays it unmodified and alone.
	const sporedJSON = `{"instance_id":"i-0abc","spored":"v0.118.0","ttl_remaining_s":14340}` + "\n"

	// Diagnostics on spored's stderr — the #540 lines, which must never reach
	// stdout no matter the format.
	const sporedStderr = "2026/08/19 21:30:10 Agent initialized for instance i-0abc\n" +
		"2026/08/19 21:30:10 Config: TTL=4h0m0s, IdleTimeout=0s\n"

	// Non-empty annotations, asserted below. The first version of this test let
	// emitStatus compute them from an *InstanceInfo, and passed with the routing
	// bug deliberately reintroduced — every notice helper returns "" for any
	// instance constructible without AWS.
	annotations := []string{
		"\n  ⚠️  TTL reconciliation: tag says 4h, spored says 3h59m\n",
		"\n  Billable: 1 x gp3 30GiB\n",
		"\n  spored v0.118.0 — upgrade available (v0.119.0)\n",
	}
	for i, a := range annotations {
		if a == "" {
			t.Fatalf("annotation %d is empty; this test only proves anything with "+
				"non-empty annotations to misroute", i)
		}
	}

	var stdout, stderr bytes.Buffer
	emitStatus(&stdout, &stderr, sporedJSON, sporedStderr, annotations, true)

	// The whole point: stdout must parse.
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n\nstdout was:\n%s\n\n"+
			"A caller doing json.loads(check_output(...)) gets a decode error with exit "+
			"code 0 — the #540 symptom.", err, stdout.String())
	}
	if doc["instance_id"] != "i-0abc" {
		t.Errorf("relayed document lost its content: %v", doc)
	}

	// Not one agent log line on stdout.
	if strings.Contains(stdout.String(), "Agent initialized") {
		t.Error("spored's log.Printf diagnostics reached stdout; these are exactly the four " +
			"lines that broke #540's reporter")
	}
	// And they must not be silently dropped either — a diagnostic nobody sees is
	// its own problem.
	if !strings.Contains(stderr.String(), "Agent initialized") {
		t.Error("spored's diagnostics were dropped rather than routed to stderr")
	}
}

// TestStatusTableModeKeepsAnnotationsOnStdout: the human path must not regress
// into writing its annotations to stderr, which would hide them from anyone
// reading a terminal — the notices are the reason they exist.
func TestStatusTableModeKeepsAnnotationsOnStdout(t *testing.T) {
	const table = "  my-spore  (i-0abc)\n  spored: v0.118.0\n"
	annotations := []string{"\n  Billable: 1 x gp3 30GiB\n"}

	var stdout, stderr bytes.Buffer
	emitStatus(&stdout, &stderr, table, "", annotations, false)

	if !strings.Contains(stdout.String(), "my-spore") {
		t.Errorf("the table itself must be on stdout, got:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Billable") {
		t.Errorf("annotations must stay on stdout for a human reader, got:\n%s", stdout.String())
	}
	// stderr should carry nothing when spored reported no diagnostics.
	if stderr.Len() != 0 {
		t.Errorf("table mode wrote to stderr with no spored diagnostics:\n%s", stderr.String())
	}
}

// TestStatusJSONAnnotationsGoToStderr pins the specific hazard: spawn's own
// annotations are plain prose, so any one of them on stdout breaks the document.
// Asserted by construction rather than by listing them, so a seventh annotation
// added later is covered too.
func TestStatusJSONAnnotationsGoToStderr(t *testing.T) {
	const sporedJSON = `{"instance_id":"i-0abc"}` + "\n"
	annotations := []string{"\n  DNS: my-spore.spore.host\n", "\n  Cost limit: $5.00\n"}

	var stdout, stderr bytes.Buffer
	emitStatus(&stdout, &stderr, sporedJSON, "", annotations, true)

	if got := strings.TrimSpace(stdout.String()); got != strings.TrimSpace(sporedJSON) {
		t.Errorf("stdout carried more than spored's document:\n  got:  %q\n  want: %q\n\n"+
			"In --output json, stdout must contain the document and nothing else; every "+
			"annotation spawn adds is prose and breaks json.Unmarshal.",
			got, strings.TrimSpace(sporedJSON))
	}
}
