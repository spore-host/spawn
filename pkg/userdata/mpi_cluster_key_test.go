package userdata

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// #1338: an image of an MPI rank-0 instance carries the cohort's root SSH key.
// The key was minted only when /root/.ssh/id_rsa was missing, and every
// authorized_keys write appended, so a cohort launched from the image reused
// the parent cohort's private key and kept authorizing the parent's public key.
//
// These run the key section of the rendered rank-0 script under bash against a
// fake /root/.ssh holding what an image would: the parent's key pair, its line
// in authorized_keys, and an owner file naming the parent instance.

// clusterKeySection is the rendered script from the key comment through the
// ssh config chmod, with /root/.ssh moved under root. Fails when the markers
// are gone, rather than running nothing.
func clusterKeySection(t *testing.T, index int, root string) string {
	t.Helper()
	script := renderMPI(t, MPIConfig{Region: "us-east-1", JobArrayID: "arr", JobArrayIndex: index, JobArraySize: 2})
	start := strings.Index(script, "# Cluster SSH key:")
	const tail = "chmod 600 /root/.ssh/config\n"
	end := strings.Index(script, tail)
	if start < 0 || end < start {
		t.Fatalf("cluster-key section not found in the MPI script (start %d, end %d)", start, end)
	}
	return strings.ReplaceAll(script[start:end+len(tail)], "/root/.ssh", filepath.Join(root, ".ssh"))
}

type keyHarness struct {
	t      *testing.T
	bash   string
	root   string
	sshDir string
}

func newKeyHarness(t *testing.T) *keyHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs the Linux MPI user data under bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	root := t.TempDir()
	h := &keyHarness{t: t, bash: bash, root: root, sshDir: filepath.Join(root, ".ssh")}
	if err := os.MkdirAll(h.sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *keyHarness) path(name string) string { return filepath.Join(h.sshDir, name) }

func (h *keyHarness) read(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.path(name))
	if err != nil {
		h.t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func (h *keyHarness) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(h.path(name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// boot runs the key section as instance iid at rank index, under errexit as
// the bootstrap does.
func (h *keyHarness) boot(iid string, index int) {
	h.t.Helper()
	// nosemgrep
	cmd := exec.Command(h.bash, "-e", "-c", clusterKeySection(h.t, index, h.root))
	cmd.Env = append(os.Environ(), "INSTANCE_ID="+iid)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("key section as %s (rank %d): %v\n%s", iid, index, err, out)
	}
}

// mintParent makes the key pair an image of a rank-0 instance carries, with
// the given comment, and returns its public line.
func (h *keyHarness) mintParent(comment string) string {
	h.t.Helper()
	if out, err := exec.Command("ssh-keygen", "-t", "rsa", "-N", "", "-C", comment, "-f", h.path("id_rsa"), "-q").CombinedOutput(); err != nil {
		h.t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	return strings.TrimSpace(h.read("id_rsa.pub"))
}

const userKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIUSERKEY someone@laptop"

func countLine(content, line string) int {
	n := 0
	for _, l := range strings.Split(content, "\n") {
		if l == line {
			n++
		}
	}
	return n
}

// The gate: a simulated image boot with a parent's key gets a NEW key, the
// parent's public key leaves authorized_keys, a key the user put there stays,
// and a reboot of the same instance keeps the key it minted.
func TestMPIClusterKeyIsMintedPerInstanceNotInheritedFromAnImage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		comment string
		owner   string
	}{
		// An image made by this release: tagged key, owner file naming the parent.
		{"tagged parent key", MPIClusterKeyComment, "i-parent\n"},
		// An image made before #1338: ssh-keygen's default comment, no owner file.
		{"legacy untagged parent key", "root@ip-10-0-0-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newKeyHarness(t)
			parentPub := h.mintParent(tc.comment)
			parentPriv := h.read("id_rsa")
			h.write("authorized_keys", userKeyLine+"\n"+parentPub+"\n")
			if tc.owner != "" {
				h.write(filepath.Base(MPIClusterKeyOwnerFile), tc.owner)
			}

			h.boot("i-child", 0)

			childPub := strings.TrimSpace(h.read("id_rsa.pub"))
			if childPub == parentPub || h.read("id_rsa") == parentPriv {
				t.Fatal("rank 0 of a cohort launched from the image kept the parent cohort's key; " +
					"whoever holds the parent's private key can ssh in as root (#1338)")
			}
			if !strings.HasSuffix(childPub, " "+MPIClusterKeyComment) {
				t.Errorf("new key %q does not carry the %q comment, so the next image cannot replace it", childPub, MPIClusterKeyComment)
			}
			ak := h.read("authorized_keys")
			if countLine(ak, parentPub) != 0 {
				t.Errorf("authorized_keys still authorizes the parent cohort's key:\n%s", ak)
			}
			if countLine(ak, childPub) != 1 {
				t.Errorf("authorized_keys holds the new key %d times, want 1:\n%s", countLine(ak, childPub), ak)
			}
			if countLine(ak, userKeyLine) != 1 {
				t.Errorf("a key spawn did not write was removed from authorized_keys:\n%s", ak)
			}
			if got := strings.TrimSpace(h.read(filepath.Base(MPIClusterKeyOwnerFile))); got != "i-child" {
				t.Errorf("owner file = %q, want i-child", got)
			}

			// Same instance, script runs again (cloud-init clean + reboot, the
			// CreateImage reboot): the cohort keeps its key, nothing accumulates.
			childPriv := h.read("id_rsa")
			h.boot("i-child", 0)
			if h.read("id_rsa") != childPriv || strings.TrimSpace(h.read("id_rsa.pub")) != childPub {
				t.Error("a reboot of the same instance replaced its cluster key; the peers authorize the old one")
			}
			ak = h.read("authorized_keys")
			if countLine(ak, childPub) != 1 || countLine(ak, userKeyLine) != 1 {
				t.Errorf("after a reboot authorized_keys is:\n%s\nwant the cluster key and the user's key once each", ak)
			}
			if n := strings.Count(h.read("config"), "StrictHostKeyChecking no"); n != 1 {
				t.Errorf("ssh config holds the spawn block %d times after two runs, want 1", n)
			}
		})
	}
}

// A non-zero rank never mints or touches a key; the control plane installs
// rank 0's (pkg/mpicohort installPubKeyCommand).
func TestMPIClusterKeyPeerMintsNothing(t *testing.T) {
	h := newKeyHarness(t)
	h.write("authorized_keys", userKeyLine+"\n")
	h.boot("i-peer", 1)
	if _, err := os.Stat(h.path("id_rsa")); err == nil {
		t.Error("a peer minted a cluster key")
	}
	if ak := h.read("authorized_keys"); ak != userKeyLine+"\n" {
		t.Errorf("a peer changed authorized_keys to:\n%s", ak)
	}
}
