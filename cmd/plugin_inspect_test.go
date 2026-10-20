package cmd

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

const inspectFixture = `
name: demo
version: v1.2.0
description: A demo plugin
local:
  env_passthrough: ["TS_API_CLIENT_SECRET"]
  provision:
    - type: run
      run: echo minting key
permissions:
  controller:
    env: ["TS_API_CLIENT_SECRET"]
    network: true
    commands: ["jq"]
  instance:
    root: true
    network: true
    ports: [8080]
    files: ["/etc/demo"]
remote:
  install:
    - type: fetch
      url: https://example.com/app.tgz
      dest: /opt/app.tgz
  start:
    - type: run
      as_user: true
      background: true
      run: /opt/app/serve --port 8080
  health:
    interval: 10s
    steps:
      - type: run
        run: curl -sf localhost:8080/health
outputs:
  url:
    source: local_capture
`

func TestRenderInspect_Sections(t *testing.T) {
	spec, err := plugin.ParseSpec([]byte(inspectFixture))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	var buf bytes.Buffer
	renderInspect(&buf, plugin.PluginRef{Host: "local", Name: "./demo.yaml"}, spec, &plugin.Provenance{Host: "local", ContentSHA256: "abc123def456abc123"}, false)
	got := buf.String()

	// Each of these must appear — proves the walk surfaces the security-relevant
	// facts a reviewer needs before installing.
	wants := []string{
		"demo v1.2.0",
		"local file ./demo.yaml",
		"Resolved:    local file · sha256 abc123def456",
		"Installing runs this plugin's code",
		"reads env: TS_API_CLIENT_SECRET",
		"download https://example.com/app.tgz → /opt/app.tgz",
		"run as login user (background): /opt/app/serve --port 8080",
		"Health check (every 10s)",
		"Outputs (printed by 'spawn plugin install' and shown by 'spawn plugin status')",
		"root=true network=true ports=[8080]",
		"commands=[jq]",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("inspect output missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func TestRenderInspect_DryRunBanner(t *testing.T) {
	spec, _ := plugin.ParseSpec([]byte(inspectFixture))
	var buf bytes.Buffer
	renderInspect(&buf, plugin.PluginRef{Host: "local", Name: "./demo.yaml"}, spec, &plugin.Provenance{Host: "local"}, true)
	if !strings.HasPrefix(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output should start with the DRY RUN banner, got:\n%s", buf.String())
	}
}

func TestRenderInspect_UnpinnedGitHubWarning(t *testing.T) {
	spec, _ := plugin.ParseSpec([]byte(inspectFixture))
	var buf bytes.Buffer
	renderInspect(&buf, plugin.PluginRef{Host: "github", Owner: "someone", Repo: "plugins", Name: "demo"}, spec, &plugin.Provenance{Host: "github", Owner: "someone", Repo: "plugins", ContentSHA256: "deadbeefcafe0000"}, false)
	got := buf.String()
	if !strings.Contains(got, "third-party source (someone/plugins)") {
		t.Errorf("expected third-party-source warning, got:\n%s", got)
	}
	if !strings.Contains(got, "No version pinned") {
		t.Errorf("expected unpinned-version warning, got:\n%s", got)
	}
}

func TestRunPluginInspect_ResolvesLocalFile(t *testing.T) {
	// End-to-end through the real resolver on a local fixture: no network, no
	// instance. Proves inspect works via DefaultResolver().Resolve for local refs.
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yaml")
	if err := os.WriteFile(path, []byte(inspectFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runPluginInspect(context.Background(), path, false); err != nil {
		t.Errorf("runPluginInspect on local fixture: %v", err)
	}
}

// "On removal" must say when each half runs (#1018): remote.stop runs on
// `spawn plugin remove` only (pluginruntime.Runtime.Remove), never at an
// instance stop (StopAll, see TestStopAllDoesNotRunTheStopSteps), and
// local.deprovision runs at remove and at terminate.
func TestRenderInspect_RemovalSaysWhenStepsRun(t *testing.T) {
	spec, err := plugin.ParseSpec([]byte(`
name: demo
version: v1.0.0
local:
  deprovision:
    - type: run
      run: delete-endpoint
remote:
  start:
    - type: run
      run: systemctl start demo
  stop:
    - type: run
      run: systemctl stop demo
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	renderCleanup(&buf, spec)
	got := buf.String()
	for _, w := range []string{
		"[instance 1] run as root: systemctl stop demo",
		"[controller 1]",
		"Instance steps run on `spawn plugin remove` only, not when the instance\n  stops, hibernates or terminates.",
		"Controller steps run on `spawn plugin remove` and `spawn terminate`.",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("removal section missing %q\n--- output ---\n%s", w, got)
		}
	}
	if strings.Contains(got, "instance stop") {
		t.Errorf("removal section still labels remote.stop as an instance stop:\n%s", got)
	}
}

func TestRemoveWarning(t *testing.T) {
	if got := removeWarning([]byte(`{"status":"removed","warning":"plugin x: stop steps failed: exit 3"}`)); got != "plugin x: stop steps failed: exit 3" {
		t.Errorf("removeWarning = %q", got)
	}
	// An older spored sends no warning; neither does a clean remove.
	if got := removeWarning([]byte(`{"status":"removed"}`)); got != "" {
		t.Errorf("removeWarning of a clean remove = %q, want empty", got)
	}
}

// #1038: spored answers 404 for a plugin that is not installed, and the CLI
// says so rather than reporting a server error.
func TestRemoveOutcome(t *testing.T) {
	if _, err := removeOutcome(http.StatusNotFound, []byte("not found\n"), "tailscale", "my-box"); err == nil ||
		err.Error() != `plugin "tailscale" not found on my-box` {
		t.Errorf("404: err = %v, want plugin \"tailscale\" not found on my-box", err)
	}
	if _, err := removeOutcome(http.StatusInternalServerError, []byte("internal error\n"), "tailscale", "my-box"); err == nil ||
		err.Error() != "remove API: HTTP 500 — internal error" {
		t.Errorf("500: err = %v", err)
	}
	w, err := removeOutcome(http.StatusOK, []byte(`{"status":"removed","warning":"stop steps failed"}`), "tailscale", "my-box")
	if err != nil || w != "stop steps failed" {
		t.Errorf("200: warning = %q, err = %v", w, err)
	}
}
