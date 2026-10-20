package launcher

import (
	"fmt"

	"github.com/spore-host/spawn/pkg/security"
)

// CommandRanMarkerDir holds the once-per-instance --command marker,
// command-ran-<instance-id>, that the Linux bootstrap writes before it starts
// --command and checks before starting it again (#1057).
const CommandRanMarkerDir = "/var/lib/spawn"

// ImageCleanOK is the line ImageStateCleanupScript prints, last, when every
// run-state file is gone. A caller treats any other outcome as a failed clean.
const ImageCleanOK = "spawn-image-clean: ok"

// ImageCleanCommandRunning is the exit status ImageStateCleanupScript uses
// when --command is still running, so its completion record would be written
// after the clean and baked into the image anyway.
const ImageCleanCommandRunning = 3

// ImageStateCleanupScript returns the shell script `spawn ami create` runs on
// the source instance, as root over SSM, before CreateImage (#1057).
//
// It removes the files that record or signal a --command run, so an instance
// launched from the image does not start with them: the completion file (the
// launch's --completion-file, which spored watches, and the default), every
// /tmp/SPAWN_* sentinel (the exit code), the rendered command script, and
// the inputs the bootstrap would read as "there is a command to run"
// (/etc/spawn/command, the sweep environment, the ready-gate list). On a
// distro whose /tmp is on the root volume rather than a tmpfs (Rocky 9), all
// of these were baked in, and a later launch from the image started with a
// stale SPAWN_COMPLETE — so --on-complete fired at boot.
//
// The command-ran marker of THIS instance is kept: the CreateImage reboot
// runs the bootstrap again after a 'cloud-init clean', and the marker is what
// stops it re-running the command. Markers for other instance IDs are
// inherited from a parent image and removed.
//
// POSIX sh, not bash: AWS-RunShellScript does not promise bash. Paths go
// through "$@", never an unquoted variable, so a --completion-file with a
// space in it is one path.
//
// It refuses (exit ImageCleanCommandRunning) while --command is still
// running, and exits non-zero if any file survives. On success the last line
// it prints is ImageCleanOK.
//
// SPAWN_IMAGE_ROOT, if set, prefixes every path. Nothing sets it on an
// instance; it is how the tests run the script against a scratch tree.
func ImageStateCleanupScript(instanceID, completionFile string) string {
	return fmt.Sprintf(`set -u
R="${SPAWN_IMAGE_ROOT:-}"
ID=%s
CF=%s
MARKER="$R%s/command-ran-$ID"
if [ -e "$MARKER" ] && [ ! -e "$R/tmp/SPAWN_EXITCODE" ] && [ ! -e "$R$CF" ]; then
    echo "spawn: --command is still running on $ID; its completion record would be written after this clean and baked into the image" >&2
    exit %d
fi
set -- "$R$CF" "$R%s" "$R/tmp/spawn-command.sh" "$R/etc/spawn/command" "$R/etc/profile.d/spawn-params.sh" "$R%s"
for f in "$@" "$R"/tmp/SPAWN_*; do
    rm -f -- "$f"
done
for m in "$R%s"/command-ran-*; do
    [ -e "$m" ] || continue
    [ "$m" = "$MARKER" ] && continue
    rm -f -- "$m"
done
LEFT=""
for f in "$@" "$R"/tmp/SPAWN_* "$R%s"/command-ran-*; do
    [ -e "$f" ] || continue
    [ "$f" = "$MARKER" ] && continue
    LEFT="$LEFT $f"
done
if [ -n "$LEFT" ]; then
    echo "spawn: could not remove:$LEFT" >&2
    exit 1
fi
echo %s
`,
		security.ShellQuote(instanceID),
		security.ShellQuote(completionFileOrDefault(completionFile)),
		CommandRanMarkerDir,
		ImageCleanCommandRunning,
		defaultCompletionFile,
		RequiredGatesFile,
		CommandRanMarkerDir,
		CommandRanMarkerDir,
		security.ShellQuote(ImageCleanOK),
	)
}
