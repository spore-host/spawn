package pluginruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// onSpore makes the runtime believe it runs on goos, for platform conditions.
func onSpore(t *testing.T, goos string) {
	t.Helper()
	old := hostGOOS
	hostGOOS = goos
	t.Cleanup(func() { hostGOOS = old })
}

// needsSh skips a test whose steps or command conditions run sh.
func needsSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin steps and command conditions run sh")
	}
}

// remoteConditionCases is a passing and a failing conditions.remote entry for
// each condition type, on a Linux spore. Keyed by type and checked against
// plugin.ConditionTypes(), so a type the validator accepts without a spored
// test here fails the gate.
var remoteConditionCases = map[string]struct{ pass, fail plugin.Condition }{
	"command": {
		pass: plugin.Condition{Type: "command", Run: "true", Message: "docker must already be present"},
		fail: plugin.Condition{Type: "command", Run: "exit 3", Message: "docker must already be present"},
	},
	"platform": {
		pass: plugin.Condition{Type: "platform", OS: "linux", Message: "requires a Linux instance"},
		fail: plugin.Condition{Type: "platform", OS: "windows", Message: "requires a Windows instance"},
	},
}

// conditionSpec is a spec with one remote condition whose install step would
// create marker.
func conditionSpec(name string, c plugin.Condition, marker string) *plugin.PluginSpec {
	return &plugin.PluginSpec{
		Name: name, Version: "v1.0.0", Description: "d",
		Conditions: plugin.ConditionsBlock{Remote: []plugin.Condition{c}},
		Remote: plugin.RemoteBlock{
			Install: []plugin.Step{{Type: "run", Run: "echo ran > " + marker}},
		},
	}
}

// spored evaluates conditions.remote before the install phase (#1040): for every
// condition type, a passing condition lets the install run, and a failing one
// marks the plugin failed with the condition's message and runs no install
// step. Before, conditions.remote was validated and then ignored.
func TestInstall_EvaluatesEveryRemoteConditionType(t *testing.T) {
	needsSh(t)
	onSpore(t, "linux")
	types := plugin.ConditionTypes()
	if len(types) == 0 {
		t.Fatal("plugin.ConditionTypes() discovered nothing; discovery is broken")
	}
	for _, typ := range types {
		tc, ok := remoteConditionCases[typ]
		if !ok {
			t.Errorf("condition type %q has no spored pass/fail case", typ)
			continue
		}
		t.Run(typ+"/pass", func(t *testing.T) {
			rt := newTestRuntime(t)
			marker := filepath.Join(t.TempDir(), "ran")
			if err := rt.Install(context.Background(), conditionSpec("cond-pass", tc.pass, marker), nil); err != nil {
				t.Fatalf("Install with a passing %s condition: %v", typ, err)
			}
			st, _ := rt.store.Load("cond-pass")
			if st == nil || st.Status != plugin.StatusRunning {
				t.Errorf("state = %+v, want running", st)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("install step did not run after a passing condition: %v", err)
			}
		})
		t.Run(typ+"/fail", func(t *testing.T) {
			rt := newTestRuntime(t)
			marker := filepath.Join(t.TempDir(), "ran")
			err := rt.Install(context.Background(), conditionSpec("cond-fail", tc.fail, marker), nil)
			if err == nil {
				t.Fatalf("Install with a failing %s condition succeeded", typ)
			}
			if _, serr := os.Stat(marker); serr == nil {
				t.Error("an install step ran despite a failing remote condition")
			}
			st, lerr := rt.store.Load("cond-fail")
			if lerr != nil {
				t.Fatalf("no state recorded for the failed plugin: %v", lerr)
			}
			if st.Status != plugin.StatusFailed {
				t.Errorf("status = %q, want failed", st.Status)
			}
			if !strings.Contains(st.Error, tc.fail.Message) {
				t.Errorf("state error %q does not carry the condition's message %q", st.Error, tc.fail.Message)
			}
		})
	}
}

// A condition type spored does not know fails the install instead of being
// skipped. Specs from launch-time declarations reach Install without going
// through the push API's Validate, so the evaluator is the last line.
func TestInstall_UnknownRemoteConditionTypeFails(t *testing.T) {
	needsSh(t)
	onSpore(t, "linux")
	rt := newTestRuntime(t)
	marker := filepath.Join(t.TempDir(), "ran")
	err := rt.Install(context.Background(), conditionSpec("cond-unknown", plugin.Condition{Type: "kernel", Message: "needs a kernel"}, marker), nil)
	if err == nil || !strings.Contains(err.Error(), "unknown condition type") {
		t.Fatalf("err = %v, want an unknown condition type failure", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("an install step ran despite an unknown remote condition type")
	}
	if st, _ := rt.store.Load("cond-unknown"); st == nil || st.Status != plugin.StatusFailed {
		t.Errorf("state = %+v, want failed", st)
	}
}

// Every registry plugin declares conditions.remote: [{type: platform, os:
// linux}]. On a Windows spore that check fails, agreeing with the blanket
// Windows refusal (#1001); on a Linux spore it passes.
func TestInstall_RegistryPlatformConditionFollowsSporeOS(t *testing.T) {
	needsSh(t)
	linuxOnly := plugin.Condition{Type: "platform", OS: "linux", Message: "docker plugin requires a Linux instance"}
	for _, tc := range []struct {
		goos string
		ok   bool
	}{{"linux", true}, {"windows", false}, {"darwin", false}} {
		onSpore(t, tc.goos)
		rt := newTestRuntime(t)
		err := rt.Preflight(context.Background(), conditionSpec("docker", linuxOnly, filepath.Join(t.TempDir(), "m")), nil)
		if (err == nil) != tc.ok {
			t.Errorf("platform os=linux on a %s spore: err = %v, want ok=%v", tc.goos, err, tc.ok)
		}
	}
}

// postPreflight posts spec to the test server's preflight endpoint.
func postPreflight(t *testing.T, url string, spec *plugin.PluginSpec, cfg map[string]string) (int, PreflightResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"spec": mustMarshalSpec(t, spec), "config": cfg})
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/plugins/preflight", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var pr PreflightResponse
	_ = json.Unmarshal(raw, &pr)
	return resp.StatusCode, pr
}

// POST /v1/plugins/preflight reports each condition type's outcome through the
// plugin API — 200 when it passes, 412 with the condition's message when it
// fails — and installs nothing and records no state either way. This is what
// lets `spawn plugin install` stop before running local provision steps.
func TestHandlePreflight_ReportsEachConditionType(t *testing.T) {
	needsSh(t)
	onSpore(t, "linux")
	for _, typ := range plugin.ConditionTypes() {
		tc, ok := remoteConditionCases[typ]
		if !ok {
			t.Errorf("condition type %q has no preflight case", typ)
			continue
		}
		s, srv := newTestServer(t)
		marker := filepath.Join(t.TempDir(), "ran")

		code, pr := postPreflight(t, srv.URL, conditionSpec("pf-pass", tc.pass, marker), nil)
		if code != http.StatusOK || pr.Status != "ok" {
			t.Errorf("%s pass: HTTP %d %+v, want 200 ok", typ, code, pr)
		}
		code, pr = postPreflight(t, srv.URL, conditionSpec("pf-fail", tc.fail, marker), nil)
		if code != http.StatusPreconditionFailed || pr.Status != "failed" {
			t.Errorf("%s fail: HTTP %d %+v, want 412 failed", typ, code, pr)
		}
		if !strings.HasPrefix(pr.Error, tc.fail.Message+": ") {
			t.Errorf("%s fail: error %q does not lead with the condition's message", typ, pr.Error)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: preflight ran an install step", typ)
		}
		for _, name := range []string{"pf-pass", "pf-fail"} {
			if _, err := s.rt.store.Load(name); err == nil {
				t.Errorf("%s: preflight recorded state for %s", typ, name)
			}
		}
	}
}

// At preflight time pushed values do not exist yet, so a command condition
// that references one is deferred to the install-time check rather than
// failed; conditions after it are still evaluated. Config references render.
func TestPreflight_DefersPushedReferencesAndRendersConfig(t *testing.T) {
	needsSh(t)
	onSpore(t, "linux")
	rt := newTestRuntime(t)
	spec := &plugin.PluginSpec{
		Name: "pf-pushed", Version: "v1.0.0", Description: "d",
		Config: map[string]plugin.ConfigParam{"want": {Default: "yes"}},
		Conditions: plugin.ConditionsBlock{Remote: []plugin.Condition{
			{Type: "command", Run: "test -n {{ pushed.key }}"},
			{Type: "command", Run: `test {{ config.want }} = yes`, Message: "config must render"},
			{Type: "platform", OS: "windows", Message: "later condition still checked"},
		}},
	}
	err := rt.Preflight(context.Background(), spec, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "later condition still checked: ") {
		t.Fatalf("Preflight = %v, want the pushed condition deferred and the platform one to fail", err)
	}
	spec.Conditions.Remote = spec.Conditions.Remote[:2]
	if err := rt.Preflight(context.Background(), spec, nil); err != nil {
		t.Errorf("Preflight with only a deferred and a passing condition = %v", err)
	}

	// At install time the pushed value is seeded, so the same condition is
	// evaluated for real.
	spec.Conditions.Remote = []plugin.Condition{{Type: "command", Run: "test {{ pushed.key }} = ok", Message: "pushed key must be ok"}}
	if err := rt.InstallWithPushed(context.Background(), spec, nil, map[string]string{"key": "bad"}); err == nil ||
		!strings.Contains(err.Error(), "pushed key must be ok") {
		t.Errorf("Install with a failing pushed condition = %v", err)
	}
}
