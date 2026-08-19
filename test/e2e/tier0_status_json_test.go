//go:build e2e_tier0

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// TestTier0_StatusJSONOutput is the direct regression test for spawn#540:
// `spawn status <id> -o json` used to print the human table (spored's SSH
// output was never routed through a JSON path at all) preceded by spored's own
// log.Printf lines, which a remote-side `2>&1` merged onto the same stream —
// so the ENTIRE stdout capture failed json.Unmarshal with "invalid
// character... looking for beginning of value" / "Extra data".
//
// Substrate has no real SSH or booted instance, so this drives the keyless
// SSM path (runStatusOverSSM in cmd/status.go) — the same path a
// lagotto/cohort-launched instance uses (#222) — by launching an instance with
// no KeyName and seeding Substrate's SSM Run Command emulation
// (POST /v1/ssm/command-invocation) with a canned `spored status --output
// json` response. That's the seam Substrate actually emulates; it exercises
// spawn's own stdout/stderr handling (the part #540 broke) rather than
// spored's internals (covered separately in cmd/spored).
func TestTier0_StatusJSONOutput(t *testing.T) {
	env := startSpawnSubstrate(t)

	id := launchKeylessInstance(t, env, "status-json-540")

	// Seed the SSM Run Command outcome: what a real spored --output json call
	// would produce on stdout/stderr, on the instance this test's `spawn
	// status` will target. ParamMatch scopes it to the actual command
	// runStatusOverSSM sends (so this seed can't accidentally satisfy some
	// other RunShellScript call in the same test binary run).
	sporedStdout := `{"instance_id":"` + id + `","name":"status-json-540","region":"us-east-1","spored_version":"9.9.9","ttl":{"configured":false},"idle":{"configured":false,"is_idle":false},"on_complete":{"sentinel_present":false},"cpu_percent":1.5,"network_bytes_per_min":0}` + "\n"
	sporedStderr := "2026/08/19 00:00:00 Agent initialized for instance " + id + " in us-east-1 (account: 123456789012, provider: ec2)\n" +
		"2026/08/19 00:00:00 Not idle: CPU usage 1.50% >= 5.00%\n"
	seedSSMCommandInvocation(t, env, ssmSeed{
		DocumentName: "AWS-RunShellScript",
		ParamMatch:   "spored status --output json",
		Status:       "Success",
		Stdout:       sporedStdout,
		Stderr:       sporedStderr,
	})

	stdout, stderr, code := env.run("status", id, "-o", "json")
	if code != 0 {
		t.Fatalf("spawn status -o json: expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	// The core regression: the ENTIRE stdout capture must parse as JSON — not
	// just a substring of it. Before the fix this failed with "invalid
	// character '2' looking for beginning of value" (no JSON support: the
	// human table's leading log lines were literal text) or, after adding
	// --output json to spored alone but not fixing the leak, "invalid
	// character... after top-level value" / "Extra data" once a merged
	// log-then-JSON stream reached spawn's stdout.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout did not parse as JSON: %v\nstdout:\n%q\nstderr:\n%q", err, stdout, stderr)
	}
	if decoded["instance_id"] != id {
		t.Errorf("instance_id = %v, want %q", decoded["instance_id"], id)
	}

	// The log lines are fine on stderr — that's the intended destination.
	if !strings.Contains(stderr, "Agent initialized for instance") {
		t.Errorf("expected spored's diagnostic lines on stderr, got:\n%s", stderr)
	}
	// ...but they must NOT be on stdout, in front of or inside the JSON.
	if strings.Contains(stdout, "Agent initialized") || strings.Contains(stdout, "Not idle") {
		t.Errorf("spored's log lines leaked onto stdout (the #540 bug):\n%s", stdout)
	}
}

// TestTier0_StatusTableOutput confirms the default (-o table, unset --output)
// path is unaffected by the #540 fix: it still prints the human-readable
// status output rather than JSON.
func TestTier0_StatusTableOutput(t *testing.T) {
	env := startSpawnSubstrate(t)
	id := launchKeylessInstance(t, env, "status-table-540")

	sporedStdout := "\n  status-table-540  (" + id + ")\n  spored:           v9.9.9\n  TTL:              none — instance will not auto-terminate\n\n  CPU:              1.5%\n"
	sporedStderr := "2026/08/19 00:00:00 Agent initialized for instance " + id + " in us-east-1 (account: 123456789012, provider: ec2)\n"
	seedSSMCommandInvocation(t, env, ssmSeed{
		DocumentName: "AWS-RunShellScript",
		ParamMatch:   "/usr/local/bin/spored status",
		Status:       "Success",
		Stdout:       sporedStdout,
		Stderr:       sporedStderr,
	})

	stdout, stderr, code := env.run("status", id)
	if code != 0 {
		t.Fatalf("spawn status: expected exit 0, got %d\nstdout:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "spored:") || !strings.Contains(stdout, "TTL:") {
		t.Errorf("expected the human table, got:\n%s", stdout)
	}
	// Table output is prose, not data — confirm it does NOT happen to parse as
	// JSON (sanity check that the two modes are actually distinct).
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err == nil {
		t.Errorf("table output unexpectedly parsed as JSON:\n%s", stdout)
	}
	// The stdout/stderr split holds in table mode too, not just JSON mode
	// (#540 fixed the transport-level `2>&1`, which affected every output
	// mode, not only -o json).
	if strings.Contains(stdout, "Agent initialized") {
		t.Errorf("spored's log lines leaked onto stdout in table mode:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Agent initialized") {
		t.Errorf("expected spored's diagnostic lines on stderr, got:\n%s", stderr)
	}
}

// launchKeylessInstance creates a spawn-managed, running EC2 instance directly
// via the SDK (bypassing `spawn launch`, which always provisions/imports an
// SSH key) with NO KeyName — the keyless/SSM-only shape (#222) that routes
// `spawn status` through runStatusOverSSM instead of SSH. Returns the instance
// ID.
func launchKeylessInstance(t *testing.T, env *spawnEnv, name string) string {
	t.Helper()
	out, err := env.EC2Client().RunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: ec2types.InstanceTypeT3Small,
		ImageId:      aws.String("ami-12345678"),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: aws.String("Name"), Value: aws.String(name)},
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RunInstances (keyless): %v", err)
	}
	if len(out.Instances) != 1 || out.Instances[0].InstanceId == nil {
		t.Fatalf("RunInstances (keyless): unexpected response %+v", out)
	}
	return *out.Instances[0].InstanceId
}

// ssmSeed mirrors substrate's ssmSeededInvocation POST body
// (POST /v1/ssm/command-invocation): it sets the canned outcome
// GetCommandInvocation returns for a SendCommand matching DocumentName (and,
// if set, containing ParamMatch in its flattened parameter values). Substrate
// never executes the command; this is purely an observable-result seed.
type ssmSeed struct {
	DocumentName string `json:"documentName"`
	ParamMatch   string `json:"paramMatch"`
	Status       string `json:"status"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	ExitCode     int    `json:"exitCode"`
}

// seedSSMCommandInvocation POSTs an ssmSeed to Substrate's SSM control-plane
// endpoint so the next matching SendCommand (from client.RunShellScript, used
// by the keyless status path) resolves to it.
func seedSSMCommandInvocation(t *testing.T, env *spawnEnv, seed ssmSeed) {
	t.Helper()
	body, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal ssm seed: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, env.URL+"/v1/ssm/command-invocation", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build ssm seed request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seed ssm command invocation: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed ssm command invocation: got HTTP %d", resp.StatusCode)
	}
}
