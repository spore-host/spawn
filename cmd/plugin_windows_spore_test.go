package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/plugin"
)

func withPluginInstance(t *testing.T, inst *aws.InstanceInfo) {
	t.Helper()
	old := pluginInstanceLookup
	pluginInstanceLookup = func(context.Context, string) *aws.InstanceInfo { return inst }
	t.Cleanup(func() { pluginInstanceLookup = old })
}

// `spawn plugin install` on a Windows spore fails up front with the reason,
// before the plugin is resolved or any local step runs (#1001). The ref is a
// local file that does not exist: if the guard were missing or ran later, the
// error would be the resolve failure instead.
func TestRunPluginInstall_RefusesAWindowsSpore(t *testing.T) {
	withPluginInstance(t, &aws.InstanceInfo{InstanceID: "i-0123456789abcdef0", Tags: map[string]string{"spawn:os": "windows"}})
	t.Chdir(t.TempDir())
	ref := "./absent-plugin.yaml" // "./" is a local file on every OS, so no registry fetch

	err := runPluginInstall(context.Background(), ref, "win-box", nil)
	if !errors.Is(err, plugin.ErrWindowsSporeUnsupported) {
		t.Fatalf("runPluginInstall on a Windows spore = %v, want ErrWindowsSporeUnsupported", err)
	}
	if !strings.Contains(err.Error(), "win-box") {
		t.Errorf("error %q does not name the instance", err)
	}
}

// A Linux spore (spawn:os=linux or absent) and an instance spawn cannot
// resolve both get past the guard, to the resolve step.
func TestRunPluginInstall_LinuxSporeIsNotRefused(t *testing.T) {
	for _, inst := range []*aws.InstanceInfo{
		{Tags: map[string]string{"spawn:os": "linux"}},
		{Tags: map[string]string{}},
		nil,
	} {
		withPluginInstance(t, inst)
		t.Chdir(t.TempDir())
		ref := "./absent-plugin.yaml" // "./" is a local file on every OS, so no registry fetch
		err := runPluginInstall(context.Background(), ref, "box", nil)
		if err == nil || errors.Is(err, plugin.ErrWindowsSporeUnsupported) {
			t.Errorf("instance %+v: err = %v, want the resolve failure for the absent ref", inst, err)
		}
	}
}

// `spawn launch --plugin` on a Windows target is refused: the Windows bootstrap
// carries no plugin declarations, so the flag was silently dropped (#1001).
func TestWindowsPluginGuard(t *testing.T) {
	decls := []plugin.Declaration{{Ref: "docker"}, {Ref: "rclone@v1.0.0"}}
	err := windowsPluginGuard("windows", decls)
	if !errors.Is(err, plugin.ErrWindowsSporeUnsupported) {
		t.Fatalf("windowsPluginGuard(windows, plugins) = %v, want ErrWindowsSporeUnsupported", err)
	}
	if !strings.Contains(err.Error(), "docker, rclone@v1.0.0") {
		t.Errorf("error %q does not name the refused plugins", err)
	}
	if err := windowsPluginGuard("windows", nil); err != nil {
		t.Errorf("a Windows launch with no plugins was refused: %v", err)
	}
	for _, osName := range []string{"linux", ""} {
		if err := windowsPluginGuard(osName, decls); err != nil {
			t.Errorf("windowsPluginGuard(%q, plugins) = %v, want nil", osName, err)
		}
	}
}

// A spec that fails validation is refused before any local step runs (#1130):
// spored would refuse it on receipt, so running local provision first would
// leave a controller-side footprint for an install that cannot happen.
func TestRunPluginInstall_InvalidSpecRunsNoLocalStep(t *testing.T) {
	withPluginInstance(t, &aws.InstanceInfo{Tags: map[string]string{"spawn:os": "linux"}})
	dir := t.TempDir()
	t.Chdir(dir)
	marker := filepath.Join(dir, "provisioned")
	spec := "name: bad\nversion: v1.0.0\ndescription: d\n" +
		"local:\n  provision:\n    - type: run\n      run: touch " + marker + "\n" +
		"remote:\n  install:\n    - type: run\n      run: \"true\"\n      capture:\n        x: .y\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runPluginInstall(context.Background(), "./bad.yaml", "box", nil)
	if err == nil || !strings.Contains(err.Error(), "capture") {
		t.Fatalf("runPluginInstall = %v, want the validation error for capture on a remote step", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("the local provision step ran before the spec was validated")
	}
}
