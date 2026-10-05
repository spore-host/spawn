package userdata

import (
	"os/exec"
	"strings"
	"testing"
)

// nastyValues are legal on Linux and each one broke the old template.
var nastyValues = []string{
	"/efs$(id -u)", // command substitution — EXECUTED at boot
	"/my efs",      // a space — corrupted the profile.d export
	"/efs$HOME",    // variable expansion
	"/efs`id`",     // backtick substitution
	`/efs"quoted"`, // double quotes
	"/efs'single'", // a single quote — the one single-quoting must escape
}

// TestStorageTemplateQuotesEveryValue is spawn#680 for the storage path.
//
// The template rendered values through `shellEscape`, which was
// security.ShellEscape = strconv.Quote — Go/C escaping inside DOUBLE quotes. A
// POSIX shell expands $VAR, $(...) and backticks inside double quotes, so none
// were neutralised, and the quotes collided with the template's own. Three
// renderings observed before the fix, for an ordinary EFS mount point:
//
//	mkdir -p "/efs$(id -u)"                 → the substitution RUNS
//	echo "export EFS_MOUNT="/efs""          → nested quotes; correct only by accident
//	echo "export EFS_MOUNT="/my efs""       → export EFS_MOUNT=/my, then `efs` as a command
//
// The third is the common case and needs nothing exotic: a mount point with a
// space corrupts /etc/profile.d/efs.sh for every login on the instance.
func TestStorageTemplateQuotesEveryValue(t *testing.T) {
	for _, mp := range nastyValues {
		t.Run(mp, func(t *testing.T) {
			script, err := GenerateStorageUserData(StorageConfig{
				EFSEnabled: true, EFSFilesystemDNS: "fs-1.efs.amazonaws.com",
				EFSMountPoint: mp, EFSMountOptions: "nfsvers=4.1",
			})
			if err != nil {
				t.Fatalf("GenerateStorageUserData: %v", err)
			}

			// Nothing may sit inside DOUBLE quotes where the shell would expand it.
			for _, line := range strings.Split(script, "\n") {
				if !strings.Contains(line, mp) {
					continue
				}
				if strings.Contains(line, `"`+mp) || strings.Contains(line, mp+`"`) {
					t.Errorf("mount point is inside double quotes, where the shell still "+
						"expands $VAR/$(...)/backticks:\n    %s", strings.TrimSpace(line))
				}
			}
		})
	}
}

// TestStorageTemplateIsValidShell runs the rendered script through `bash -n`.
//
// A quoting bug does not always look like an injection — the nested-quote
// rendering above was *syntactically* fine and silently produced the wrong
// value. Parsing catches the other half: a value containing a quote that
// terminates a string early.
func TestStorageTemplateIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, mp := range nastyValues {
		t.Run(mp, func(t *testing.T) {
			script, err := GenerateStorageUserData(StorageConfig{
				EFSEnabled: true, EFSFilesystemDNS: "fs-1.efs.amazonaws.com",
				EFSMountPoint: mp, EFSMountOptions: "nfsvers=4.1",
				FSxLustreEnabled: true, FSxFilesystemDNS: "fs-2.fsx.amazonaws.com",
				FSxMountName: "abc", FSxMountPoint: mp,
				AttachedVolumes: []AttachedVolume{{DeviceName: "/dev/sdf", MountPoint: mp}},
			})
			if err != nil {
				t.Fatalf("GenerateStorageUserData: %v", err)
			}
			// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("rendered script is not valid shell for mount point %q: %v\n%s",
					mp, err, out)
			}
		})
	}
}

// TestProfileExportSurvivesASpace pins the specific common-case regression: the
// profile.d line must set the whole path, not stop at the first space.
func TestProfileExportSurvivesASpace(t *testing.T) {
	script, err := GenerateStorageUserData(StorageConfig{
		EFSEnabled: true, EFSFilesystemDNS: "fs-1.efs.amazonaws.com",
		EFSMountPoint: "/my efs", EFSMountOptions: "nfsvers=4.1",
	})
	if err != nil {
		t.Fatalf("GenerateStorageUserData: %v", err)
	}

	var line string
	for _, l := range strings.Split(script, "\n") {
		if strings.Contains(l, "profile.d/efs.sh") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatal("no profile.d/efs.sh line rendered")
	}
	if !strings.Contains(line, `'/my efs'`) {
		t.Errorf("the mount point is not single-quoted in the profile export, so the "+
			"space splits it and `efs` is run as a command on every login:\n    %s", line)
	}
}
