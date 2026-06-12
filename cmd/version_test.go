package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/libs/update"
)

func TestRenderUpdateNotice(t *testing.T) {
	cases := []struct {
		name     string
		res      *update.Result
		contains string
	}{
		{"nil → couldn't check", nil, "couldn't check"},
		{
			"newer available → upgrade line",
			&update.Result{CurrentVersion: "0.42.0", LatestVersion: "0.43.0", UpdateURL: "https://example.test/v0.43.0"},
			"A newer version is available: 0.42.0 → 0.43.0",
		},
		{
			"on latest → reassurance",
			&update.Result{CurrentVersion: "0.43.0", LatestVersion: "0.43.0"},
			"latest version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderUpdateNotice(tc.res)
			if !strings.Contains(got, tc.contains) {
				t.Errorf("renderUpdateNotice() = %q, want it to contain %q", got, tc.contains)
			}
		})
	}
}
