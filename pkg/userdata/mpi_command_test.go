package userdata

import (
	"strings"
	"testing"
)

// mpirunLine returns the actual mpirun INVOCATION line.
//
// Matching bare "mpirun" or the command text anywhere in the script is not
// enough: the template contains a `command -v mpirun` install probe and a
// comment block that quotes an example command, and both produced false
// positives while this was being written. The invocation is identifiable by its
// --mca flag.
func mpirunLine(t *testing.T, script string) string {
	t.Helper()
	i := strings.Index(script, "mpirun --mca")
	if i < 0 {
		t.Fatalf("no mpirun invocation found in:\n%s", relevant(script))
	}
	line := script[i:]
	if nl := strings.IndexByte(line, '\n'); nl > 0 {
		line = line[:nl]
	}
	return line
}

// commandFileBody returns the text between the here-doc markers that deliver
// --mpi-command, i.e. what actually lands in /etc/spawn/mpi-command.
func commandFileBody(t *testing.T, script string) string {
	t.Helper()
	const open = "<<'EOFMPICMD'\n"
	i := strings.Index(script, open)
	if i < 0 {
		t.Fatalf("no quoted here-doc delivering the command:\n%s", relevant(script))
	}
	rest := script[i+len(open):]
	j := strings.Index(rest, "\nEOFMPICMD")
	if j < 0 {
		t.Fatalf("unterminated command here-doc")
	}
	return rest[:j]
}

func mpiScriptWithCommand(t *testing.T, cmd string) string {
	t.Helper()
	s, err := GenerateMPIUserData(MPIConfig{
		Region:         "us-east-1",
		JobArrayID:     "arr-1",
		JobArraySize:   2,
		BinariesBucket: "b",
		MPICommand:     cmd,
	})
	if err != nil {
		t.Fatalf("GenerateMPIUserData: %v", err)
	}
	return s
}

// TestMPICommandPreservesArguments is spawn#660.
//
// The template rendered {{.MPICommand | shellEscape}}, and security.ShellEscape
// is strconv.Quote — which wraps the whole string in ONE pair of double quotes.
// So `--mpi-command "./gchp --flag x"` reached mpirun as a single argv word, and
// mpirun tried to exec a binary literally named `./gchp --flag x`. Only
// argument-free commands ever worked.
func TestMPICommandPreservesArguments(t *testing.T) {
	script := mpiScriptWithCommand(t, "./gchp --flag x")

	// The invocation must not carry the command as one double-quoted word.
	line := mpirunLine(t, script)
	if strings.Contains(line, `"./gchp --flag x"`) {
		t.Errorf("the command is still collapsed into a single double-quoted word, so "+
			"mpirun execs a binary of that literal name:\n%s", line)
	}

	// And the command must reach the instance byte-exact, via the file.
	if got := commandFileBody(t, script); got != "./gchp --flag x" {
		t.Errorf("command file body = %q, want the command verbatim", got)
	}
}

// TestMPICommandIsNotSubjectToWriteTimeExpansion is the second half of #660, and
// the reason strconv.Quote was the wrong tool rather than merely a clumsy one.
//
// strconv.Quote is Go/C escaping *inside double quotes* — not shell escaping. So
// $VAR, $(...) and backticks all still expand. It neither preserved argv nor
// neutralised metacharacters: it managed to fail at both jobs at once.
//
// A command line is meant to be interpreted by a shell at RUN time (that is what
// a user writing `--mpi-command` expects), so the fix is not to escape harder —
// it is to stop interpolating the command into the generated script at all.
func TestMPICommandIsNotSubjectToWriteTimeExpansion(t *testing.T) {
	for _, cmd := range []string{
		`./run $(whoami)`,
		"./run `id`",
		`./run $HOME/data`,
		`./run "quoted arg"`,
		`./run 'single'`,
	} {
		script := mpiScriptWithCommand(t, cmd)

		// The command must be delivered through a quoted here-doc, so the
		// generating step performs no expansion and the text lands byte-exact.
		if !strings.Contains(script, "<<'EOFMPICMD'") {
			t.Errorf("command %q: expected a single-quoted here-doc delimiter so nothing "+
				"expands while WRITING the file:\n%s", cmd, relevant(script))
			continue
		}
		if !strings.Contains(script, cmd) {
			t.Errorf("command %q did not survive verbatim:\n%s", cmd, relevant(script))
		}
	}
}

// TestMPICommandRunsViaBash: mpirun must be handed a shell, not the command
// text, or the argv problem just reappears in a different shape.
func TestMPICommandRunsViaBash(t *testing.T) {
	script := mpiScriptWithCommand(t, "./gchp --flag x")

	if !strings.Contains(script, "cat > /etc/spawn/mpi-command") {
		t.Fatalf("expected the command to be written to a file:\n%s", relevant(script))
	}
	line := mpirunLine(t, script)
	if !strings.Contains(line, "bash /etc/spawn/mpi-command") {
		t.Errorf("mpirun must run the command through bash so the user's arguments, "+
			"quoting and redirection are parsed by a shell; got:\n%s", line)
	}
}

// TestMPICommandOmittedWhenEmpty keeps the no-command case unchanged: a job array
// without --mpi-command must not grow an empty file or a bare mpirun.
func TestMPICommandOmittedWhenEmpty(t *testing.T) {
	script := mpiScriptWithCommand(t, "")
	if strings.Contains(script, "mpi-command") {
		t.Errorf("no --mpi-command was given, so nothing should reference the file:\n%s",
			relevant(script))
	}
	// "mpirun" on its own is not the signal — the template legitimately probes
	// for it with `command -v mpirun` to decide whether to install Open MPI.
	// Match the invocation.
	if strings.Contains(script, "mpirun --mca") {
		t.Errorf("no --mpi-command was given, so there is nothing to mpirun:\n%s",
			relevant(script))
	}
}

// relevant trims the script to the tail, where the command handling lives, so a
// failure message is readable.
func relevant(script string) string {
	if i := strings.Index(script, "mpi-hostfile"); i > 0 {
		return script[i:]
	}
	if len(script) > 800 {
		return script[len(script)-800:]
	}
	return script
}
