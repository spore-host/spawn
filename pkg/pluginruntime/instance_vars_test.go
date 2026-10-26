package pluginruntime

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/provider"
)

// documentedInstanceKeys reads the {{ instance.<key> }} variables plugin
// templates are documented to have, from Render's doc comment in
// pkg/plugin/template.go. cmd's TestPluginInstanceVarsMatchTheDocumentedSet reads
// the same comment for the controller side, so the two contexts agree (#1225).
func documentedInstanceKeys(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "plugin", "template.go"))
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

// Remote steps get every documented {{ instance.* }} variable, and no
// undocumented one, so a plugin author can rely on the list.
func TestRemoteInstanceVarsMatchTheDocumentedSet(t *testing.T) {
	want := documentedInstanceKeys(t)
	rt := NewRuntimeWithStore(plugin.NewDiskStateStore(t.TempDir()),
		&provider.Identity{InstanceID: "i-0123456789abcdef0", Name: "box", PublicIP: "203.0.113.7"}, "ubuntu")
	ctx := rt.buildTemplateContext(&plugin.PluginState{})
	var got []string
	for k := range ctx.Instance {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("remote steps get instance.{%s}; template.go documents instance.{%s}",
			strings.Join(got, ","), strings.Join(want, ","))
	}
}
