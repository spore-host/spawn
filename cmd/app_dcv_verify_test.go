//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// #1598: Amazon DCV installs only once its signing key imports and every RPM's
// signature verifies; otherwise nothing is installed. Runs dcvInstallBlock in
// bash with rpm, curl, tar, dnf, systemctl and uname stood in for.
func TestDCVInstallsOnlyVerifiedPackages(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for name, tc := range map[string]struct {
		importOK bool
		checks   string // rpm -K output
		checkRC  int
		install  bool
	}{
		"verified":       {true, "/x/nice-dcv-server.rpm: digests signatures OK\n/x/nice-xdcv.rpm: digests signatures OK\n", 0, true},
		"key not import": {false, "/x/a.rpm: digests signatures OK\n", 0, false},
		"bad signature":  {true, "/x/a.rpm: digests signatures OK\n/x/b.rpm: digests SIGNATURES NOT OK\n", 1, false},
		"no packages":    {true, "", 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			rc := "1"
			if tc.importOK {
				rc = "0"
			}
			stubs := map[string]string{
				"rpm": "#!/bin/bash\nif [ \"$1\" = --import ]; then exit " + rc + "; fi\nif [ \"$1\" = -K ]; then printf '%s' '" +
					tc.checks + "'; exit " + strconv.Itoa(tc.checkRC) + "; fi\nexit 0\n",
				"curl":      "#!/bin/bash\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && : > \"$2\"; shift; done\nexit 0\n",
				"tar":       "#!/bin/bash\nexit 0\n",
				"dnf":       "#!/bin/bash\necho \"$*\" >> \"" + dir + "/dnf-calls\"\nexit 0\n",
				"systemctl": "#!/bin/bash\nexit 0\n",
				"uname":     "#!/bin/bash\necho x86_64\n",
			}
			for n, body := range stubs {
				if err := os.WriteFile(filepath.Join(bin, n), []byte(body), 0o755); err != nil { //nolint:gosec // test stand-ins must be executable
					t.Fatal(err)
				}
			}
			script := strings.ReplaceAll(dcvInstallBlock(), "/tmp/", dir+"/")
			// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
			cmd := exec.Command("bash", "-c", script) //nolint:gosec // this package's own generated script
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "dnf-calls"))
			installed := strings.Contains(string(calls), "nice-dcv-server")
			if installed != tc.install {
				t.Fatalf("DCV installed=%t, want %t\n--- output ---\n%s\n--- dnf ---\n%s", installed, tc.install, out, calls)
			}
			if installed && !strings.Contains(string(calls), "--setopt=localpkg_gpgcheck=1") {
				t.Errorf("dnf installed the local DCV packages without checking their signatures: %s", calls)
			}
		})
	}
}
