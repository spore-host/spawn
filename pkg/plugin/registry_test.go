package plugin_test

import (
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		ref  string
		want plugin.PluginRef
	}{
		{
			ref:  "globus-personal-endpoint",
			want: plugin.PluginRef{Host: "official", Owner: "spore-host", Repo: "spore-plugins", Name: "globus-personal-endpoint"},
		},
		{
			ref:  "globus-personal-endpoint@v1.2.0",
			want: plugin.PluginRef{Host: "official", Owner: "spore-host", Repo: "spore-plugins", Name: "globus-personal-endpoint", Version: "v1.2.0"},
		},
		{
			ref:  "github:user/repo/myplugin",
			want: plugin.PluginRef{Host: "github", Owner: "user", Repo: "repo", Name: "myplugin"},
		},
		{
			ref:  "github:user/repo/myplugin@v2.0.0",
			want: plugin.PluginRef{Host: "github", Owner: "user", Repo: "repo", Name: "myplugin", Version: "v2.0.0"},
		},
		{
			ref:  "./path/to/plugin.yaml",
			want: plugin.PluginRef{Host: "local", Name: "./path/to/plugin.yaml"},
		},
		{
			ref:  "/abs/path/plugin.yaml",
			want: plugin.PluginRef{Host: "local", Name: "/abs/path/plugin.yaml"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			got := plugin.ParseRef(tc.ref)
			if got != tc.want {
				t.Errorf("ParseRef(%q) = %+v, want %+v", tc.ref, got, tc.want)
			}
		})
	}
}
