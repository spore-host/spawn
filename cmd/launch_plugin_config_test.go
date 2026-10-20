package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setLaunchConfigFile points --config at path for one test.
func setLaunchConfigFile(t *testing.T, path string) {
	t.Helper()
	old, oldPlugins := launchConfigFile, launchPlugins
	launchConfigFile, launchPlugins = path, nil
	t.Cleanup(func() { launchConfigFile, launchPlugins = old, oldPlugins })
}

func writeLaunchConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "launch.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A --config file that cannot be loaded must fail the launch (#1116). The
// error used to be dropped in collectPluginDeclarations, so a typo'd path, an
// unknown key or a plugins entry written with name: instead of ref: launched an
// instance with none of the plugins the file named.
func TestCollectPluginDeclarations_BadConfigFileIsAnError(t *testing.T) {
	cases := []struct {
		name, body, wantSub string
		missing             bool
	}{
		{name: "missing file", missing: true, wantSub: "read launch config"},
		{name: "unparseable", body: "plugins: [\n", wantSub: "parse launch config"},
		// The shape four spore-plugins READMEs used.
		{name: "name instead of ref", body: "plugins:\n  - name: rstudio-server\n", wantSub: "field name not found"},
		// The docs site's guides/plugins.md example.
		{name: "unknown top-level key", body: "instance_type: m7i.large\nttl: 4h\nplugins:\n  - ref: rstudio-server\n", wantSub: "field instance_type not found"},
		{name: "misspelled config key", body: "plugins:\n  - ref: rstudio-server\n    confg:\n      port: \"8787\"\n", wantSub: "field confg not found"},
		{name: "empty ref", body: "plugins:\n  - ref: tailscale\n  - config:\n      port: \"8787\"\n", wantSub: "plugins[1] has no ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
			if !tc.missing {
				path = writeLaunchConfig(t, tc.body)
			}
			setLaunchConfigFile(t, path)

			decls, err := collectPluginDeclarations()
			if err == nil {
				t.Fatalf("collectPluginDeclarations() = %+v, nil; want an error containing %q", decls, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestCollectPluginDeclarations_ValidConfigFile(t *testing.T) {
	setLaunchConfigFile(t, writeLaunchConfig(t, "plugins:\n  - ref: rstudio-server\n    config:\n      port: 8787\n  - ref: tailscale\n"))
	launchPlugins = []string{"docker"}

	decls, err := collectPluginDeclarations()
	if err != nil {
		t.Fatalf("collectPluginDeclarations: %v", err)
	}
	var refs []string
	for _, d := range decls {
		refs = append(refs, d.Ref)
	}
	if got := strings.Join(refs, ","); got != "rstudio-server,tailscale,docker" {
		t.Errorf("refs = %s, want rstudio-server,tailscale,docker", got)
	}
	if decls[0].Config["port"] != "8787" {
		t.Errorf("rstudio-server port = %q, want 8787", decls[0].Config["port"])
	}
}

// An empty file declares no plugins; it is not an error.
func TestCollectPluginDeclarations_EmptyConfigFile(t *testing.T) {
	setLaunchConfigFile(t, writeLaunchConfig(t, ""))
	if decls, err := collectPluginDeclarations(); err != nil || len(decls) != 0 {
		t.Fatalf("collectPluginDeclarations() = %+v, %v; want none, nil", decls, err)
	}
}

// buildLaunchConfig is what every launch path (single, sweep, pipe and
// --dry-run) builds its config through, so the bad file is refused there, before
// any AWS work, and not first at user-data assembly.
func TestBuildLaunchConfig_RefusesBadLaunchConfigFile(t *testing.T) {
	setLaunchConfigFile(t, filepath.Join(t.TempDir(), "typo.yaml"))
	if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("buildLaunchConfig() err = %v, want the --config error", err)
	}
}
