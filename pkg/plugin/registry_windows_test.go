//go:build windows

package plugin_test

import (
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// TestParseRef_WindowsLocalPaths: on a Windows controller a local plugin file is
// written the Windows way. Every one of these used to fall through to the
// official-registry branch and be looked up in spore-host/spore-plugins over the
// network, so a Windows user could not use a local plugin file at all (#997).
func TestParseRef_WindowsLocalPaths(t *testing.T) {
	for _, ref := range []string{
		`C:\Users\me\demo.yaml`,
		`c:\plugins\demo.yaml`,
		`\\server\share\demo.yaml`,
		`.\demo.yaml`,
		`..\plugins\demo.yaml`,
		`C:/Users/me/demo.yaml`,
	} {
		if got := plugin.ParseRef(ref); got.Host != "local" || got.Name != ref {
			t.Errorf("ParseRef(%q) = %+v, want a local ref named %q", ref, got, ref)
		}
	}
	// And a bare name is still a registry name.
	if got := plugin.ParseRef("demo@v1.0.0"); got.Host == "local" {
		t.Errorf("ParseRef(\"demo@v1.0.0\") = %+v, want a registry ref", got)
	}
}
