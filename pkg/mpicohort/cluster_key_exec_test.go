package mpicohort

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/instancestate"
	"github.com/spore-host/spawn/pkg/userdata"
)

// #1338, the control-plane half: what pkg/mpicohort runs over SSM on each
// member, executed under bash against a fake /root/.ssh.

func execBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs the commands SSM sends to Linux members")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	return bash
}

// relocate moves every absolute path the commands name under dir.
func relocate(cmd, dir string) string {
	cmd = strings.ReplaceAll(cmd, "/root/.ssh", filepath.Join(dir, "root", ".ssh"))
	cmd = strings.ReplaceAll(cmd, "/var/lib/spawn", filepath.Join(dir, "var", "lib", "spawn"))
	return strings.ReplaceAll(cmd, "/tmp/.spawn-cluster-key", filepath.Join(dir, "cluster-key"))
}

func runIn(t *testing.T, bash, dir, cmd string) (string, error) {
	t.Helper()
	// nosemgrep
	out, err := exec.Command(bash, "-c", relocate(cmd, dir)).CombinedOutput()
	return string(out), err
}

const (
	parentKey = "ssh-rsa AAAAPARENT " + userdata.MPIClusterKeyComment
	childKey  = "ssh-rsa AAAACHILD " + userdata.MPIClusterKeyComment
	userKey   = "ssh-ed25519 AAAAUSER someone@laptop"
)

// A member launched from an image of another cohort's member holds that
// cohort's key in authorized_keys. Installing this cohort's key must replace
// it, keep everything else, and stay one copy however often assembly retries.
func TestInstallPubKeyCommandReplacesTheClusterKey(t *testing.T) {
	bash := execBash(t)
	dir := t.TempDir()
	ssh := filepath.Join(dir, "root", ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	ak := filepath.Join(ssh, "authorized_keys")
	if err := os.WriteFile(ak, []byte(userKey+"\n"+parentKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if out, err := runIn(t, bash, dir, installPubKeyCommand(childKey, "i-0c41d")); err != nil {
			t.Fatalf("install (run %d): %v\n%s", i+1, err, out)
		}
	}
	b, _ := os.ReadFile(ak)
	if got, want := string(b), userKey+"\n"+childKey+"\n"; got != want {
		t.Errorf("authorized_keys after installing the cohort key twice:\n%s\nwant:\n%s\n"+
			"(the parent cohort's key removed, the user's kept, the new key once)", got, want)
	}

	// Into an empty file: grep finding no lines is not a failure.
	if err := os.WriteFile(ak, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runIn(t, bash, dir, installPubKeyCommand(childKey, "i-0c41d")); err != nil {
		t.Fatalf("install into an empty authorized_keys: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(ak); string(b) != childKey+"\n" {
		t.Errorf("authorized_keys = %q, want just the cohort key", b)
	}
}

// The read of rank 0's public key returns nothing until the MPI user data has
// recorded that THIS instance minted it. On an image boot id_rsa.pub is there
// from the start and is the parent cohort's.
func TestReadPubKeyCommandWaitsForThisInstancesKey(t *testing.T) {
	bash := execBash(t)
	dir := t.TempDir()
	ssh := filepath.Join(dir, "root", ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(ssh, filepath.Base(userdata.MPIClusterKeyOwnerFile))
	if err := os.WriteFile(filepath.Join(ssh, "id_rsa.pub"), []byte(parentKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, ownerContent := range []string{"", "i-0fa2e\n"} {
		if ownerContent != "" {
			if err := os.WriteFile(owner, []byte(ownerContent), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := runIn(t, bash, dir, readPubKeyCommand("i-0c41d")); err == nil || strings.Contains(out, "AAAA") {
			t.Errorf("owner %q: the read returned %q (err %v) before rank 0 minted its own key; "+
				"the parent cohort's key would be authorized on every member", ownerContent, out, err)
		}
	}

	if err := os.WriteFile(owner, []byte("i-0c41d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runIn(t, bash, dir, readPubKeyCommand("i-0c41d")); err != nil || strings.TrimSpace(out) != parentKey {
		t.Errorf("owner names this instance: read = %q, %v; want the key", out, err)
	}

	if got := readPubKeyCommand("i-0; rm -rf /"); got != "false" {
		t.Errorf("readPubKeyCommand interpolated a non-instance-ID: %q", got)
	}
}

// A key without the spawn comment is not distributed: it is not one this
// release's user data minted, and once installed it could never be replaced.
func TestReadRankZeroPubKeyRefusesAnUntaggedKey(t *testing.T) {
	f := newFakeLauncher()
	f.clusterKey = "ssh-rsa AAAAB3NzaC1yc2ETESTKEY root@rank0"
	t.Cleanup(SetKeyWaitForTest(50*time.Millisecond, 10*time.Millisecond))
	asm := NewSSMAssembler(f, "us-east-1", "", time.Millisecond, time.Millisecond)
	err := asm.Assemble(context.Background(), []cohort.Observation{{ID: "job-0", ProviderID: "i-0", Address: "10.0.0.1"}})
	if err == nil {
		t.Fatal("an untagged rank-0 key was distributed")
	}
}

// #1343: the boot-time strip of inherited cluster keys
// (instancestate.StripClusterKeys) and this install must not fight. Whichever
// order they run in on a member launched from another cohort's image, the
// parent's key is gone, the user's is kept, and THIS cohort's key survives.
// Strip first is the usual order (the bootstrap runs before enrollment); the
// install first happens on an image with MPI preinstalled, where enrollment
// can pass before the bootstrap reaches the strip.
func TestBootStripAndKeyInstallAgreeInEitherOrder(t *testing.T) {
	bash := execBash(t)
	const id = "i-0c41d"
	for _, order := range []string{"strip, install", "install, strip", "strip, install, strip"} {
		t.Run(order, func(t *testing.T) {
			dir := t.TempDir()
			ssh := filepath.Join(dir, "root", ".ssh")
			if err := os.MkdirAll(ssh, 0o700); err != nil {
				t.Fatal(err)
			}
			ak := filepath.Join(ssh, "authorized_keys")
			if err := os.WriteFile(ak, []byte(userKey+"\n"+parentKey+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The image's strip record names its source.
			rec := filepath.Join(dir, filepath.FromSlash(userdata.MPIClusterKeyStripOwnerFile))
			if err := os.MkdirAll(filepath.Dir(rec), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rec, []byte("i-0fa2e\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, step := range strings.Split(order, ", ") {
				switch step {
				case "strip":
					if _, err := instancestate.StripClusterKeys(dir, id); err != nil {
						t.Fatal(err)
					}
				case "install":
					if out, err := runIn(t, bash, dir, installPubKeyCommand(childKey, id)); err != nil {
						t.Fatalf("install: %v\n%s", err, out)
					}
				}
			}
			if b, _ := os.ReadFile(ak); string(b) != userKey+"\n"+childKey+"\n" {
				t.Errorf("authorized_keys after %s:\n%q\nwant the user's key and this cohort's, not the parent's", order, b)
			}
		})
	}
}
