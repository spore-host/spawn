package instancestate

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spore-host/spawn/pkg/userdata"
)

// RootAuthorizedKeys is root's authorized_keys, where spawn's MPI user data
// and control plane install the cohort's cluster key.
const RootAuthorizedKeys = "/root/.ssh/authorized_keys"

// StripClusterKeys removes, under root ("" on an instance), every line of
// RootAuthorizedKeys whose last field is exactly userdata.MPIClusterKeyComment,
// and returns how many it removed (#1343). Those lines are spawn's MPI
// cluster keys; on an instance that has not stripped them yet they came with
// the image, and authorize the source cohort's rank-0 key for root. Every
// other line is kept byte for byte; the file is rewritten in place (owner,
// mode and SELinux label kept) and only when a line goes.
//
// Once per instance, not per boot: userdata.MPIClusterKeyStripOwnerFile
// records the instance it ran for, and when that is instanceID nothing is
// touched. So a source whose bootstrap runs again ('cloud-init clean' and the
// CreateImage reboot) or whose spored restarts keeps its live cohort's key,
// while an instance launched from its image strips it. The MPI key install
// over SSM (pkg/mpicohort) writes the same file after adding this cohort's
// key, so a strip that comes after it leaves that key alone.
//
// A missing owner file means "not stripped yet". That is safe on a live
// instance because the release that tags cluster keys (#1338) is the release
// that writes the file: an instance holding a tagged line also has the file
// naming itself.
func StripClusterKeys(root, instanceID string) (int, error) {
	if instanceID == "" {
		return 0, errors.New("instancestate: no instance ID")
	}
	owner := filepath.Join(root, filepath.FromSlash(userdata.MPIClusterKeyStripOwnerFile))
	b, err := os.ReadFile(owner) //nolint:gosec // fixed path under the caller's root
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if strings.TrimSpace(string(b)) == instanceID {
		return 0, nil
	}
	ak := filepath.Join(root, filepath.FromSlash(RootAuthorizedKeys))
	n := 0
	data, err := os.ReadFile(ak) //nolint:gosec // fixed path under the caller's root
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return 0, err
	default:
		var kept []byte
		kept, n = withoutClusterKeys(data)
		if n > 0 {
			// In place, like the MPI user data's `cat new > file`: a rename
			// would drop the file's SELinux label (ssh_home_t) on EL.
			if err := os.WriteFile(ak, kept, 0o600); err != nil { //nolint:gosec // root's own file, rewritten as root
				return 0, err
			}
		}
	}
	return n, writeOwner(owner, instanceID)
}

// withoutClusterKeys returns data without the lines whose last field is
// userdata.MPIClusterKeyComment, every other byte unchanged, and how many
// lines it dropped.
func withoutClusterKeys(data []byte) ([]byte, int) {
	var out []byte
	n := 0
	for rest := data; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = nil
		}
		if f := strings.Fields(string(line)); len(f) > 0 && f[len(f)-1] == userdata.MPIClusterKeyComment {
			n++
			continue
		}
		out = append(out, line...)
	}
	if out == nil {
		out = []byte{}
	}
	return out, n
}

// StripClusterKeysHost is StripClusterKeys on the running instance.
func StripClusterKeysHost(instanceID string) (int, error) {
	return StripClusterKeys("", instanceID)
}
