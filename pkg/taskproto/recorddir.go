package taskproto

import "path/filepath"

// localRecordDir is the directory the generated scripts use for their LOCAL
// copies of the task's terminal artifacts: the completion record, the plain
// exit-code file, and the start timestamp the flush hook reads back.
//
// In production this is always "/tmp", and that is load-bearing rather than
// incidental — spored shares the host /tmp (spawn#66: a systemd PrivateTmp=true
// hid /tmp/SPAWN_COMPLETE and silently killed every auto-shutdown path).
//
// It is a GENERATE-TIME value, deliberately not a runtime environment variable.
// The wrapper and the terminal-flush hook are generated as two separate scripts
// and run as two separate processes (the hook runs as root, from spored, while
// the wrapper is still blocked on the user's command). A runtime override could
// therefore be visible to one and not the other — and since the hook uses the
// existence of the wrapper's completion.json as its "the real record already
// exists" interlock, a disagreement about this path would make the hook
// overwrite the completion record of successfully finished tasks. Resolving it
// once, in Go, at generation time means the two scripts of a single launch can
// never disagree.
//
// Tests override it to stop the exec-based tests in pkg/taskproto and
// pkg/taskpool racing on one shared absolute path (#642): `go test ./...` runs
// packages in parallel, and each package's tests are a separate binary, so a
// per-process variable gives each one its own directory.
var localRecordDir = "/tmp"

// localPath resolves name against the configured local record directory.
func localPath(name string) string { return filepath.Join(localRecordDir, name) }

// Local artifact names, resolved through localPath at generation time.
const (
	completionFileName = "spawn-completion.json"
	// terminalFileName is the flush hook's own staging file. Deliberately a
	// DIFFERENT name from completionFileName: the hook must never create the file
	// whose absence is its own interlock, or a retried hook would see its own
	// previous output and conclude the wrapper had finished.
	terminalFileName = "spawn-completion-terminal.json"
	exitCodeFileName = "spawn.exitcode"
	startedAtName    = "spawn-task-started-at"
)

// CompletionRecordPath is where the generated wrapper writes its local copy of
// completion.json, and the file whose existence tells the terminal-flush hook
// that the wrapper got there first.
func CompletionRecordPath() string { return localPath(completionFileName) }

// StartedAtPath is where the wrapper persists STARTED_AT so the flush hook — a
// separate process that cannot see the wrapper's shell variables — reports the
// same start time the normal record would have carried.
func StartedAtPath() string { return localPath(startedAtName) }

// SetLocalRecordDirForTest points the generated scripts' local artifact paths at
// dir for the rest of this test binary's run, and returns a function restoring
// the previous value. Exported because pkg/taskpool's exec tests run generated
// scripts too and need the same isolation.
//
// Not safe for parallel use within one package; tests in a package run
// sequentially unless they call t.Parallel(), and none of these do.
func SetLocalRecordDirForTest(dir string) (restore func()) {
	prev := localRecordDir
	localRecordDir = dir
	return func() { localRecordDir = prev }
}
