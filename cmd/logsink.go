package cmd

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/spore-host/spawn/pkg/audit"
)

// Where spawn's machine-oriented output goes (#895).
//
// stdout and the terminal are for the human output — progress lines, summaries,
// tables — and `-o json` documents. Two other streams used to share the terminal
// with them:
//
//   - the audit trail: one JSON object per security-sensitive operation, written
//     to stderr. Each carries the AWS account ID as user_id, so it ended up in
//     every screenshot and screen recording of a launch.
//   - Go `log` diagnostics from library code ("pricing: m8a.4xlarge in us-west-2
//     = $0.9738/hr ..."), timestamped and on stderr by default.
//
// Both are now written to files under ~/.spawn, and copied to stderr only when
// the user asks with --verbose. The audit trail is the one that must survive:
// it is appended to ~/.spawn/audit.log on every run, verbose or not. If that file
// cannot be opened, the audit trail falls back to stderr rather than being
// dropped — a lost audit record is worse than an untidy terminal.

const (
	auditLogName      = "audit.log"
	diagnosticLogName = "spawn.log"
)

// spawnLogDir is where audit.log and spawn.log live. A var so tests can point it
// at a temp dir instead of the real home directory.
var spawnLogDir = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".spawn"), nil
}

var (
	logFilesMu sync.Mutex
	logFiles   = map[string]*os.File{}
)

// openSpawnLog opens (once per process) ~/.spawn/<name> for append, owner-only:
// the audit trail names the account and the resources it touched.
func openSpawnLog(name string) (io.Writer, error) {
	logFilesMu.Lock()
	defer logFilesMu.Unlock()
	dir, err := spawnLogDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	if f, ok := logFiles[path]; ok {
		return f, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	logFiles[path] = f
	return f, nil
}

// lazyLog appends to ~/.spawn/<name>, opening it on the first write so a run
// that logs nothing leaves no file behind. If the file can't be opened, writes
// go to fallback instead.
type lazyLog struct {
	name     string
	fallback io.Writer
}

func (l lazyLog) Write(p []byte) (int, error) {
	f, err := openSpawnLog(l.name)
	if err != nil {
		return l.fallback.Write(p)
	}
	return f.Write(p)
}

// auditSink is where audit events are written: always the durable audit file,
// plus stderr under --verbose. If the file can't be opened the events go to
// stderr rather than nowhere — exactly once, verbose or not.
func auditSink(verbose bool, stderr io.Writer) io.Writer {
	if verbose {
		return io.MultiWriter(lazyLog{auditLogName, io.Discard}, stderr)
	}
	return lazyLog{auditLogName, stderr}
}

// diagnosticSink is where Go `log` output goes: the diagnostics file, plus
// stderr under --verbose. Dropped when the file can't be opened and --verbose is
// off — these are debugging aids, not records.
func diagnosticSink(verbose bool, stderr io.Writer) io.Writer {
	if verbose {
		return io.MultiWriter(lazyLog{diagnosticLogName, io.Discard}, stderr)
	}
	return lazyLog{diagnosticLogName, io.Discard}
}

// newAuditLogger is the only way spawn's commands should build an audit logger:
// it routes the events to auditSink rather than to the terminal (#895).
func newAuditLogger(userID, correlationID string) *audit.AuditLogger {
	return audit.NewLogger(auditSink(spawnVerbose, os.Stderr), userID, correlationID)
}

// configureDiagnosticLog points the standard logger at diagnosticSink. Called
// via cobra.OnInitialize, i.e. once flags are parsed, for every command.
func configureDiagnosticLog() {
	log.SetOutput(diagnosticSink(spawnVerbose, os.Stderr))
}

// goLogLine matches a line written by Go's standard logger with its default
// flags ("2006/01/02 15:04:05 message"). spored versions before #895 write their
// agent diagnostics ("Not idle: CPU usage could not be measured ...") to stderr
// in exactly this form during `spored status`.
var goLogLine = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)

// relaySporedStderr forwards a remote `spored status`'s stderr to ours, minus its
// Go log diagnostics unless verbose. Anything else spored wrote to stderr — an
// "Error: ..." from a failed status — is always forwarded.
func relaySporedStderr(w io.Writer, sporedErr string, verbose bool) {
	if sporedErr == "" {
		return
	}
	if verbose {
		_, _ = io.WriteString(w, sporedErr)
		return
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(sporedErr, "\n") {
		if line == "" || goLogLine.MatchString(line) {
			continue
		}
		b.WriteString(line)
	}
	_, _ = io.WriteString(w, b.String())
}
