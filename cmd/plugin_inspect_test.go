package cmd

import (
	"bytes"
	"context"
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
	renderInspect(&buf, plugin.PluginRef{Host: "local", Name: "./demo.yaml"}, spec, false)
	got := buf.String()

	// Each of these must appear — proves the walk surfaces the security-relevant
	// facts a reviewer needs before installing.
	wants := []string{
		"demo v1.2.0",
		"local file ./demo.yaml",
		"Installing runs this plugin's code",
		"reads env: TS_API_CLIENT_SECRET",
		"download https://example.com/app.tgz → /opt/app.tgz",
		"run as login user (background): /opt/app/serve --port 8080",
		"Health check (every 10s)",
		"Outputs (surfaced after install)",
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
	renderInspect(&buf, plugin.PluginRef{Host: "local", Name: "./demo.yaml"}, spec, true)
	if !strings.HasPrefix(buf.String(), "DRY RUN") {
		t.Errorf("dry-run output should start with the DRY RUN banner, got:\n%s", buf.String())
	}
}

func TestRenderInspect_UnpinnedGitHubWarning(t *testing.T) {
	spec, _ := plugin.ParseSpec([]byte(inspectFixture))
	var buf bytes.Buffer
	renderInspect(&buf, plugin.PluginRef{Host: "github", Owner: "someone", Repo: "plugins", Name: "demo"}, spec, false)
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
