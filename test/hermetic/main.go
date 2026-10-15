// Command hermetic is a `go test -exec` wrapper that runs each test binary in
// the hermetic environment of internal/hermetic and FAILS the package if any
// test reached the network (#968).
//
//	go build -o bin/hermetic-exec ./test/hermetic
//	go test -short -exec "$PWD/bin/hermetic-exec" ./...
//
// `make check`, CI and scripts/nested-modules.sh run every short test under it.
// The e2e and integration build tags, which exist to reach real AWS, are not.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spore-host/spawn/internal/hermetic"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hermetic <test-binary> [args...]  (use as go test -exec)")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Args[2:], os.Stdout, os.Stderr))
}

// run executes the test binary under the hermetic environment and returns the
// exit code: the binary's own, or 1 if it reached the recorder.
func run(bin string, args []string, stdout, stderr io.Writer) int {
	rec, err := hermetic.Start()
	if err != nil {
		fmt.Fprintf(stderr, "hermetic: listen: %v\n", err)
		return 2
	}
	defer rec.Close()

	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- bin is the test binary `go test -exec` hands this wrapper, and args are that binary's own test flags; the wrapper exists to run exactly that binary, and exec.Command runs no shell.
	cmd := exec.Command(bin, args...)
	cmd.Env = hermetic.Env(os.Environ(), rec.URL)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr

	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			fmt.Fprintf(stderr, "hermetic: run %s: %v\n", bin, err)
			return 2
		}
		code = ee.ExitCode()
	}
	if rec.Report(stderr, filepath.Base(bin)) {
		return 1
	}
	return code
}
