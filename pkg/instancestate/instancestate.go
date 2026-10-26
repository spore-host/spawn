// Package instancestate discards the per-instance state an instance inherits
// from the image it was launched from (#1241).
//
// The bootstrap and spored write state on the root volume that describes one
// instance: its plugin declarations and their state, its job-array identity and
// peer list, its batch queue and resume point, its --command and terminal-flush
// hook, the push API token. `spawn ami create` (or any CreateImage) snapshots
// that volume, so an instance launched from the image started with all of it:
// spored loaded the parent's plugins, the MPI user data found the parent's
// peers file and built its hostfile from the parent cohort's addresses, and the
// flush hook would have written to the parent's results prefix.
//
// The rule: OwnerFile records the instance that wrote the state. At boot, when
// it names a different instance, every file under Paths last modified BEFORE
// this boot is removed, and OwnerFile is rewritten with this instance's ID.
//
// Two properties follow from the "before this boot" half, and both matter:
//
//   - Nothing this instance wrote is lost, whoever runs first. The bootstrap
//     writes plugins.json and the command before spored is installed, the MPI
//     control plane pushes the peers file over SSM as soon as the agent is
//     online, and an image's own spored may start before the bootstrap does.
//     Every one of those files is newer than the boot, so the order of the
//     discard and the writes does not matter and no lock is needed.
//   - The source instance keeps its state. Its own reboots (the CreateImage
//     reboot included) find OwnerFile naming itself and remove nothing.
//
// An image whose OwnerFile is missing (a source launched before this existed,
// imaged outside `spawn ami create`) cannot be told apart from an instance that
// simply has not recorded itself yet, so nothing is removed: removing on a
// missing owner would wipe the live state of every older instance whose spored
// is upgraded in place. `spawn ami create` writes OwnerFile on the source
// before the snapshot (launcher.ImageStateCleanupScript) so its images always
// carry one.
package instancestate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// OwnerFile holds the ID of the instance whose state is on this volume.
const OwnerFile = "/var/lib/spawn/instance-owner"

// Paths is every per-instance state file or directory spawn writes on the
// root volume of a Linux instance. A directory covers everything beneath it.
// TestEveryWrittenStatePathIsCovered discovers the paths spawn's source
// names and fails on one that is neither here nor in notInstanceState.
var Paths = []string{
	// Plugins: declarations spored loads at startup, their state, and the
	// loopback push API's bearer token (re-generated per start, but a secret
	// the image would otherwise carry).
	"/etc/spawn/plugins.json",
	"/var/lib/spored/plugins",
	"/var/lib/spored/push.token",
	// Batch queue: the definition and the runner's resume point.
	"/var/lib/spored/queue.json",
	"/var/lib/spored/queue-state.json",
	// Already keyed on instance_id and ignored on a mismatch; removed anyway.
	"/var/lib/spored/compute-checkpoint.json",
	// Job array / pipeline identity and peers. The MPI user data waits for the
	// peers file to appear, so an inherited one released it at once with the
	// parent cohort's addresses.
	"/etc/profile.d/job-array.sh",
	"/etc/spawn/job-array-peers.json",
	"/etc/spawn/pipeline-peers.json",
	// The launch's parameters and session settings.
	"/etc/profile.d/spawn-params.sh",
	"/etc/profile.d/session-timeout.sh",
	// Mount points of the source's file systems, appended to on each launch.
	"/etc/profile.d/efs.sh",
	"/etc/profile.d/fsx.sh",
	// The launch's workload and the hook spored runs on any lifecycle exit,
	// which flushes to the SOURCE's results prefix.
	"/etc/spawn/command",
	"/etc/spawn/mpi-command",
	"/etc/spawn/task-flush.sh",
	// What the bootstrap stub fetched for the launch.
	"/etc/spawn/staged-files.sh",
	"/etc/spawn/bootstrap-body.sh",
}

// Reconcile applies the rule to the tree under root ("" on an instance) for
// the instance instanceID, whose current boot began at boot. It returns the
// files it removed and the instance that owned them ("" when nothing was
// inherited).
//
// It is idempotent and safe to run concurrently with itself and with the
// bootstrap: a second run finds OwnerFile naming this instance.
func Reconcile(root, instanceID string, boot time.Time) (removed []string, from string, err error) {
	if instanceID == "" {
		return nil, "", errors.New("instancestate: no instance ID")
	}
	if boot.IsZero() {
		// Without the boot time a file this instance wrote cannot be told from an
		// inherited one; recording ownership now would adopt the inherited state.
		return nil, "", errors.New("instancestate: boot time unknown")
	}
	owner := filepath.Join(root, filepath.FromSlash(OwnerFile))
	b, rerr := os.ReadFile(owner) //nolint:gosec // fixed path under the caller's root
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return nil, "", rerr
	}
	prev := strings.TrimSpace(string(b))
	var errs []error
	if prev != "" && prev != instanceID {
		from = prev
		for _, p := range Paths {
			r, e := removeOlder(filepath.Join(root, filepath.FromSlash(p)), boot)
			removed = append(removed, r...)
			if e != nil {
				errs = append(errs, e)
			}
		}
	}
	if prev != instanceID {
		if err := writeOwner(owner, instanceID); err != nil {
			errs = append(errs, err)
		}
	}
	sort.Strings(removed)
	return removed, from, errors.Join(errs...)
}

// removeOlder removes every non-directory entry at or under p whose mtime is
// not after boot. Directories are left (empty is the same as absent to every
// reader), as is anything that vanishes mid-walk.
func removeOlder(p string, boot time.Time) ([]string, error) {
	var removed []string
	err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.ModTime().After(boot) {
			return nil // written during this boot: this instance's own
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed = append(removed, path)
		return nil
	})
	return removed, err
}

func writeOwner(owner, instanceID string) error {
	if err := os.MkdirAll(filepath.Dir(owner), 0o755); err != nil { //nolint:gosec // root-owned state dir, world-readable like /var/lib/spawn
		return err
	}
	tmp := owner + ".tmp"
	if err := os.WriteFile(tmp, []byte(instanceID+"\n"), 0o644); err != nil { //nolint:gosec // an instance ID, not a secret
		return err
	}
	if err := os.Rename(tmp, owner); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// BootTime is when the running kernel booted, from /proc/stat's btime.
func BootTime() (time.Time, error) {
	return bootTimeFrom("/proc/stat")
}

func bootTimeFrom(path string) (time.Time, error) {
	b, err := os.ReadFile(path) //nolint:gosec // /proc/stat, or a test fixture
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			var secs int64
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &secs); err != nil || secs <= 0 {
				return time.Time{}, fmt.Errorf("instancestate: bad btime %q in %s", v, path)
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("instancestate: no btime in %s", path)
}

// ReconcileHost is Reconcile on the running instance: the real root and the
// running kernel's boot time.
func ReconcileHost(instanceID string) (removed []string, from string, err error) {
	boot, err := BootTime()
	if err != nil {
		return nil, "", err
	}
	return Reconcile("", instanceID, boot)
}
