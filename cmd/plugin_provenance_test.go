package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestPluginProvenanceTagValue(t *testing.T) {
	spec := &plugin.PluginSpec{Name: "rstudio-server", Version: "v1.0.1"}

	cases := []struct {
		name string
		prov *plugin.Provenance
		want []string // substrings that must appear
	}{
		{
			name: "signed official",
			prov: &plugin.Provenance{
				ContentSHA256:     strings.Repeat("a", 64),
				CommitSHA:         strings.Repeat("b", 40),
				ManifestVerified:  true,
				SignatureVerified: true,
			},
			want: []string{"version=v1.0.1", "sha256=aaaaaaaaaaaa", "commit=bbbbbbbbbbbb", "verify=signature"},
		},
		{
			name: "manifest only (unsigned)",
			prov: &plugin.Provenance{ContentSHA256: strings.Repeat("c", 64), ManifestVerified: true},
			want: []string{"verify=manifest"},
		},
		{
			name: "unverified",
			prov: &plugin.Provenance{ContentSHA256: strings.Repeat("d", 64)},
			want: []string{"verify=none"},
		},
		{
			name: "nil provenance still records version",
			prov: nil,
			want: []string{"version=v1.0.1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pluginProvenanceTagValue(spec, tc.prov)
			if len(got) > 256 {
				t.Errorf("tag value %d chars, exceeds EC2's 256-char limit: %q", len(got), got)
			}
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("tag value %q missing %q", got, sub)
				}
			}
		})
	}
}
