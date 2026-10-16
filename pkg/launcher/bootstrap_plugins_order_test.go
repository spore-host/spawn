package launcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/plugin"
)

// TestPluginsJSONExistsWhenSporedStarts is #1023, run under bash.
//
// spored reads /etc/spawn/plugins.json once, at startup. `systemctl start
// spored` returns as soon as the Type=simple unit is forked, so a file written
// after that line is read only if the bootstrap wins a race against spored.
// The property is therefore "the file is on disk at the moment spored is
// started", and that is what this observes: it runs the real generated
// bootstrap with every external command stubbed, and the systemctl stub records
// whether the file exists when it is asked to start spored.
//
// The harness:
//   - PATH is an empty directory, so every external command reaches bash's
//     command_not_found_handle, which logs it and succeeds. mkdir, cat and
//     chmod are passed through to the real binaries so the file is actually
//     written; uname answers x86_64 so the architecture check passes.
//   - /etc/spawn is rewritten into a temp dir.
//   - The verified spored download is replaced with a function that succeeds:
//     it is a network fetch, and the bootstrap rightly aborts when it fails.
//   - errexit is turned off, because writes to other root-owned paths (/etc/motd,
//     /var/log/...) fail in a test and are irrelevant here.
func TestPluginsJSONExistsWhenSporedStarts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: executes the Linux user-data bootstrap (/etc/spawn) under bash (#1002)")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	// command_not_found_handle, which the harness relies on, is bash 4+.
	// macOS ships 3.2 as /bin/bash; skipping there beats a misleading failure.
	v, err := exec.Command(bash, "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if major, convErr := strconv.Atoi(strings.TrimSpace(string(v))); err != nil || convErr != nil || major < 4 {
		t.Skipf("bash major version %q lacks command_not_found_handle", strings.TrimSpace(string(v)))
	}
	script, err := BuildLinuxBootstrap(BootstrapConfig{
		Username: "tester",
		Plugins:  []plugin.Declaration{{Ref: "spore-host/jupyter"}},
	})
	if err != nil {
		t.Fatalf("BuildLinuxBootstrap: %v", err)
	}

	root := t.TempDir()
	empty := filepath.Join(root, "empty-path")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	pluginsFile := filepath.Join(root, "etc", "spawn", "plugins.json")
	atStart := filepath.Join(root, "at-spored-start")

	mustReplace := func(old, repl string) {
		t.Helper()
		if !strings.Contains(script, old) {
			t.Fatalf("harness anchor %q is gone from the bootstrap; update the harness", old)
		}
		script = strings.ReplaceAll(script, old, repl)
	}
	mustReplace("/etc/spawn/", root+"/etc/spawn/")
	mustReplace("/etc/spawn\n", root+"/etc/spawn\n")
	mustReplace("\nif ! spore_install_verified_spored ",
		"\nspore_install_verified_spored() { return 0; }\nif ! spore_install_verified_spored ")
	if !strings.HasPrefix(script, "#!/bin/bash\nset -e\n") {
		t.Fatalf("the bootstrap no longer opens with set -e; update the harness:\n%.80s", script)
	}
	script = strings.Replace(script, "set -e\n", "set +e\n", 1)

	prelude := `
command_not_found_handle() {
  case "$1" in
    mkdir|cat|chmod) "/bin/$1" "${@:2}"; return $? ;;
    uname) echo x86_64 ;;
    systemctl)
      if [ "$2" = start ] && [ "$3" = spored ]; then
        if [ -s "$SPAWN_TEST_PLUGINS" ]; then echo present >> "$SPAWN_TEST_AT_START"
        else echo absent >> "$SPAWN_TEST_AT_START"; fi
      fi ;;
  esac
  return 0
}
`
	path := filepath.Join(root, "bootstrap.sh")
	if err := os.WriteFile(path, []byte(prelude+script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, path)
	cmd.Env = []string{
		"PATH=" + empty,
		"HOME=" + root,
		"SPAWN_TEST_PLUGINS=" + pluginsFile,
		"SPAWN_TEST_AT_START=" + atStart,
	}
	out, _ := cmd.CombinedOutput() // stubbed writes to root-owned paths fail; judged by the record below
	if ctx.Err() != nil {
		t.Fatalf("the stubbed bootstrap did not finish in 60s:\n%s", tail(out))
	}

	got, err := os.ReadFile(atStart)
	if err != nil {
		t.Fatalf("`systemctl start spored` was never reached, so this test observed nothing:\n%s", tail(out))
	}
	if strings.TrimSpace(string(got)) != "present" {
		t.Fatalf("spored was started before /etc/spawn/plugins.json was written (recorded %q). "+
			"spored reads it once at startup, so the plugins load only if the bootstrap wins "+
			"a race (#1023)", strings.TrimSpace(string(got)))
	}
	decl, err := os.ReadFile(pluginsFile)
	if err != nil || !strings.Contains(string(decl), "spore-host/jupyter") {
		t.Fatalf("plugins.json does not hold the declaration (err=%v): %q", err, decl)
	}
}

func tail(b []byte) string {
	const n = 3000
	if len(b) > n {
		return "..." + string(b[len(b)-n:])
	}
	return string(b)
}
