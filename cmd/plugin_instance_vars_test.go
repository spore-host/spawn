package cmd

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/plugin"
)

// documentedPluginInstanceKeys reads the {{ instance.<key> }} variables plugin
// templates are documented to have, from Render's doc comment in
// pkg/plugin/template.go. pkg/pluginruntime's
// TestRemoteInstanceVarsMatchTheDocumentedSet holds spored's remote-step context
// to the same comment, so local and remote steps get the same keys (#1225).
func documentedPluginInstanceKeys(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "pkg", "plugin", "template.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^//\s+\{\{\s*instance\.([a-z_]+)\s*\}\}`)
	var keys []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		keys = append(keys, m[1])
	}
	if len(keys) == 0 {
		t.Fatal("found no documented {{ instance.* }} variables in pkg/plugin/template.go: the discovery is broken")
	}
	sort.Strings(keys)
	return keys
}

// Local steps (provision, reconcile, deprovision) get every documented
// {{ instance.* }} variable that remote steps get. They lacked login_user, so
// spore-sync hardcoded ec2-user and broke on Ubuntu (#1225).
func TestPluginInstanceVarsMatchTheDocumentedSet(t *testing.T) {
	want := documentedPluginInstanceKeys(t)
	vars := pluginInstanceVars("box", &aws.InstanceInfo{
		InstanceID: "i-0123456789abcdef0", Name: "box", PublicIP: "203.0.113.7",
		Tags: map[string]string{"spawn:local-username": "ubuntu"},
	})
	var got []string
	for k := range vars {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("local steps get instance.{%s}; template.go documents (and remote steps get) instance.{%s}",
			strings.Join(got, ","), strings.Join(want, ","))
	}
}

// login_user is the user the instance was set up with, spawn:local-username,
// and ec2-user only when that is unknown, as spored resolves it.
func TestPluginInstanceVarsLoginUser(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst *aws.InstanceInfo
		want string
	}{
		{"tagged ubuntu", &aws.InstanceInfo{Tags: map[string]string{"spawn:local-username": "ubuntu"}}, "ubuntu"},
		{"untagged (older instance)", &aws.InstanceInfo{Tags: map[string]string{}}, "ec2-user"},
		{"not resolvable", nil, "ec2-user"},
	} {
		vars := pluginInstanceVars("box", tc.inst)
		got, err := plugin.Render("{{ instance.login_user }}@host", plugin.TemplateContext{Instance: vars})
		if err != nil || got != tc.want+"@host" {
			t.Errorf("%s: {{ instance.login_user }} rendered %q, %v; want %q", tc.name, got, err, tc.want+"@host")
		}
	}
}

// What local provision rendered against is what reconcile and deprovision
// replay against: `spawn plugin install` keeps the instance vars, login_user
// included, in the controller-side record those steps are run from.
func TestPluginInstallRecordsLoginUserForTeardown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	withPluginInstance(t, &aws.InstanceInfo{
		InstanceID: "i-0123456789abcdef0", Name: "box",
		Tags: map[string]string{"spawn:local-username": "ubuntu"},
	})
	t.Chdir(t.TempDir())
	// A local-only plugin with a teardown step and nothing to run at install:
	// no step runs, nothing contacts the instance, and the record is written.
	spec := "name: teardown-only\nversion: v1.0.0\ndescription: d\n" +
		"local:\n  deprovision:\n    - type: run\n      run: echo {{ instance.login_user }}\n"
	if err := os.WriteFile("teardown-only.yaml", []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPluginInstall(context.Background(), "./teardown-only.yaml", "box", nil); err != nil {
		t.Fatalf("install: %v", err)
	}

	store, err := plugin.DefaultLocalStore()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.Load("box", "teardown-only")
	if err != nil {
		t.Fatalf("local record: %v", err)
	}
	got, err := plugin.Render("{{ instance.login_user }}", templateContextFromRecord(rec))
	if err != nil || got != "ubuntu" {
		t.Errorf("deprovision renders {{ instance.login_user }} as %q, %v; want ubuntu (record: %+v)", got, err, rec.Instance)
	}
}
