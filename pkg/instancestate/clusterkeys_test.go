package instancestate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/userdata"
)

// #1343: an instance launched from an image of an MPI cohort member must not
// authorize that cohort's cluster key for root. Every line whose last field is
// exactly the spawn comment goes, once per instance; every other byte stays.

const (
	akUser     = "ssh-ed25519 AAAAC3NzaUSERKEY alice@laptop\n"
	akUserOpts = `from="10.0.0.0/8",no-pty ssh-ed25519 AAAAC3NzaUSEROPT bob` + "\n"
	akUserLook = "ssh-ed25519 AAAAC3NzaUSERTHREE " + userdata.MPIClusterKeyComment + "-notes\n"
	akUserMid  = "ssh-ed25519 AAAAC3NzaUSERFOUR " + userdata.MPIClusterKeyComment + " mine\n"
	akLegacy   = "ssh-rsa AAAAB3NzaRANKZERO root@ip-10-0-1-17\n" // pre-#1338: cannot be told from a user's
	akTagged   = "ssh-rsa AAAAB3NzaTAGGED " + userdata.MPIClusterKeyComment + "\n"
	akTaggedCR = "ssh-rsa AAAAB3NzaTAGGEDCR " + userdata.MPIClusterKeyComment + "\r\n"
	akComment  = "# my notes\n"
	akBlank    = "\n"
)

func writeAK(t *testing.T, root, content string) string {
	t.Helper()
	ak := filepath.Join(root, filepath.FromSlash(RootAuthorizedKeys))
	if err := os.MkdirAll(filepath.Dir(ak), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ak, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return ak
}

func TestStripClusterKeysRemovesTaggedLinesAndKeepsEveryOtherByte(t *testing.T) {
	root := t.TempDir()
	in := akUser + akTagged + akUserOpts + akComment + akUserLook + akBlank + akLegacy + akTaggedCR + akUserMid +
		"ssh-ed25519 AAAAC3NzaLAST no-newline"
	want := akUser + akUserOpts + akComment + akUserLook + akBlank + akLegacy + akUserMid +
		"ssh-ed25519 AAAAC3NzaLAST no-newline"
	ak := writeAK(t, root, in)

	n, err := StripClusterKeys(root, "i-0child")
	if err != nil || n != 2 {
		t.Fatalf("StripClusterKeys = %d, %v; want 2 lines removed", n, err)
	}
	b, _ := os.ReadFile(ak)
	if string(b) != want {
		t.Errorf("authorized_keys after the strip:\n%q\nwant every non-spawn line byte for byte:\n%q", b, want)
	}
	if fi, _ := os.Stat(ak); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	if o, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(userdata.MPIClusterKeyStripOwnerFile))); strings.TrimSpace(string(o)) != "i-0child" {
		t.Errorf("owner file = %q, want i-0child", o)
	}
}

// Once per instance: a second run for the same instance (a re-run bootstrap,
// a spored restart) removes nothing, so the cohort key installed since stays.
// A run for another instance (launched from an image of this one) strips.
func TestStripClusterKeysOncePerInstance(t *testing.T) {
	root := t.TempDir()
	ak := writeAK(t, root, akUser+akTagged)
	if n, err := StripClusterKeys(root, "i-0src"); err != nil || n != 1 {
		t.Fatalf("first run: %d, %v", n, err)
	}
	// This cohort's key installed after the strip.
	if err := os.WriteFile(ak, []byte(akUser+akTagged), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := StripClusterKeys(root, "i-0src"); err != nil || n != 0 {
		t.Fatalf("same instance again: %d, %v; want nothing removed", n, err)
	}
	if b, _ := os.ReadFile(ak); string(b) != akUser+akTagged {
		t.Errorf("same instance's own cluster key removed: %q", b)
	}
	// An image of i-0src, launched as i-0child.
	if n, err := StripClusterKeys(root, "i-0child"); err != nil || n != 1 {
		t.Fatalf("child: %d, %v; want the inherited line removed", n, err)
	}
	if b, _ := os.ReadFile(ak); string(b) != akUser {
		t.Errorf("child kept %q", b)
	}
}

func TestStripClusterKeysEdgeCases(t *testing.T) {
	t.Run("no authorized_keys", func(t *testing.T) {
		root := t.TempDir()
		if n, err := StripClusterKeys(root, "i-0a"); err != nil || n != 0 {
			t.Fatalf("%d, %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(RootAuthorizedKeys))); !os.IsNotExist(err) {
			t.Errorf("created authorized_keys: %v", err)
		}
	})
	t.Run("only cluster keys", func(t *testing.T) {
		root := t.TempDir()
		ak := writeAK(t, root, akTagged+akTaggedCR)
		if n, err := StripClusterKeys(root, "i-0a"); err != nil || n != 2 {
			t.Fatalf("%d, %v", n, err)
		}
		if b, _ := os.ReadFile(ak); len(b) != 0 {
			t.Errorf("left %q", b)
		}
	})
	t.Run("nothing to strip leaves the file alone", func(t *testing.T) {
		root := t.TempDir()
		ak := writeAK(t, root, akUser+akLegacy)
		if err := os.Chmod(ak, 0o644); err != nil {
			t.Fatal(err)
		}
		if n, err := StripClusterKeys(root, "i-0a"); err != nil || n != 0 {
			t.Fatalf("%d, %v", n, err)
		}
		if fi, _ := os.Stat(ak); fi.Mode().Perm() != 0o644 {
			t.Errorf("file rewritten (mode %v) with nothing to strip", fi.Mode().Perm())
		}
	})
	t.Run("no instance ID", func(t *testing.T) {
		if _, err := StripClusterKeys(t.TempDir(), ""); err == nil {
			t.Error("want an error")
		}
	})
}
