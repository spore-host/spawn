package sporedinstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/curlcompat"
)

// Executable gates for the two Enterprise Linux failures that left an instance
// running with no spored — so no TTL and no cost limit:
//
//   - #1056: EL8 ships curl 7.61.1, which rejects --retry-all-errors (7.71+).
//   - #1052: on SELinux-enforcing EL9 the binary kept its /tmp label after mv,
//     and systemd could not exec it.
//
// Neither is caught by running the installer on a developer machine or a CI
// runner, which have a new curl and no SELinux. So these put shims on PATH that
// behave like the EL hosts in the one respect that matters.

// shimDir writes executable shims into a fresh directory and returns a PATH
// value that puts them first.
func shimDir(t *testing.T, shims map[string]string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	for name, body := range shims {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil { //nolint:gosec // test shim must be executable
			t.Fatalf("write shim %s: %v", name, err)
		}
	}
	return dir, "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// el8CurlShim returns a curl that accepts exactly curl 7.61.1's options —
// parsed from that release's real --help, see internal/curlcompat — and exits 2
// the way 7.61.1 does on anything else. Otherwise it runs the real curl.
func el8CurlShim(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not available")
	}
	o := curlcompat.EL8()
	var long []string
	for k := range o.Long {
		long = append(long, k)
	}
	sort.Strings(long)
	var short strings.Builder
	for c := range o.Short {
		short.WriteByte(c)
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
# Fake curl 7.61.1 (EL8): reject any option that release does not have.
long=" %s "
short=%q
for arg in "$@"; do
    case "$arg" in
        --) break ;;
        --*)
            name="${arg%%%%=*}"
            case "$long" in *" $name "*) ;; *)
                echo "curl: option $name: is unknown" >&2
                echo "curl: try 'curl --help' or 'curl --manual' for more information" >&2
                exit 2 ;;
            esac ;;
        -?*)
            rest="${arg#-}"
            while [ -n "$rest" ]; do
                c="${rest:0:1}"; rest="${rest:1}"
                case "$c" in [A-Za-z0-9#:]) ;; *) break ;; esac
                case "$short" in *"$c"*) ;; *)
                    echo "curl: option -$c: is unknown" >&2
                    exit 2 ;;
                esac
            done ;;
    esac
done
exec %q "$@"
`, strings.Join(long, " "), short.String(), real)
}

// The shim must actually bite, or the test below passes vacuously.
func TestEL8CurlShimRejectsANewerOption(t *testing.T) {
	requireBash(t)
	dir, _ := shimDir(t, map[string]string{"curl": el8CurlShim(t)})
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.Command(filepath.Join(dir, "curl"), "--retry-all-errors", "-V").CombinedOutput() //nolint:gosec // test shim
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "--retry-all-errors: is unknown") {
		t.Fatalf("the EL8 curl shim accepted --retry-all-errors (err=%v):\n%s", err, out)
	}
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if out, err := exec.Command(filepath.Join(dir, "curl"), "-fsS", "--retry", "1", "-V").CombinedOutput(); err != nil { //nolint:gosec // test shim
		t.Fatalf("the EL8 curl shim rejected options 7.61.1 has: %v\n%s", err, out)
	}
}

// The installer must work with curl 7.61.1 (#1056).
//
// Before the fix every curl here carried --retry-all-errors, the shim (like EL8)
// exits 2 on it, and the installer reported "Failed to download ... from any
// source" for a source that was serving the artifact perfectly well.
func TestInstallsWithEL8Curl(t *testing.T) {
	requireBash(t)
	f := newFixture(t)
	_, path := shimDir(t, map[string]string{"curl": el8CurlShim(t)})
	f.env = append(f.env, path)

	code, out, installed := f.run(true)
	if code != 0 || !installed {
		t.Fatalf("the installer cannot install spored with EL8's curl 7.61.1 (exit %d, installed=%v) — "+
			"on Rocky/RHEL 8 the bootstrap aborts and the instance has no TTL or cost limit (#1056):\n%s",
			code, installed, out)
	}
}

// A transient failure on any fetch is retried, not fatal (#462) — and still is
// with curl 7.61.1, which has no --retry-all-errors (#1056).
//
// The server closes the connection without replying, which is curl exit 52,
// "Empty reply from server": the failure #462 observed from S3. curl's own
// --retry does not treat that as transient; only --retry-all-errors did, which
// is why the retry has to be a loop now.
func TestRetriesATransientEmptyReplyOnEveryFetch(t *testing.T) {
	requireBash(t)
	f := newFixture(t)
	_, path := shimDir(t, map[string]string{"curl": el8CurlShim(t)})
	f.env = append(f.env, path)
	f.drops["/"+f.binary] = 2
	f.drops["/"+f.binary+".sha256"] = 1
	f.drops["/"+f.binary+".sig"] = 2 // the HEAD probe, then the GET

	code, out, installed := f.run(true)
	if code != 0 || !installed {
		t.Fatalf("a transient empty reply aborted the install instead of being retried (exit %d, installed=%v):\n%s",
			code, installed, out)
	}
	for p, n := range f.drops {
		if n != 0 {
			t.Errorf("%s: %d planned failures never happened, so this test did not exercise the retry", p, n)
		}
	}
}

// The installed binary gets its policy-default SELinux label (#1052).
//
// The fake restorecon records what it was asked to relabel and whether that
// file was already the installed, executable binary at the time — so a relabel
// before the rename (which would fix the temp file and then be undone by mv
// keeping its label) fails this as surely as no relabel at all.
func TestRelabelsTheInstalledBinaryForSELinux(t *testing.T) {
	requireBash(t)

	restorecon := func(rc int) string {
		return fmt.Sprintf(`#!/usr/bin/env bash
for a in "$@"; do last="$a"; done
state=missing
[ -x "$last" ] && state=installed
echo "$* $state" >> "$(dirname "$0")/restorecon.log"
exit %d
`, rc)
	}
	selinuxenabled := func(rc int) string { return fmt.Sprintf("#!/bin/sh\nexit %d\n", rc) }
	readLog := func(dir string) string {
		b, err := os.ReadFile(filepath.Join(dir, "restorecon.log")) //nolint:gosec // test temp dir
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}

	t.Run("enforcing host: relabels the binary after installing it", func(t *testing.T) {
		f := newFixture(t)
		dir, path := shimDir(t, map[string]string{"restorecon": restorecon(0), "selinuxenabled": selinuxenabled(0)})
		f.env = append(f.env, path)
		code, out, installed := f.run(true)
		if code != 0 || !installed {
			t.Fatalf("install failed (exit %d):\n%s", code, out)
		}
		if got, want := readLog(dir), "-Fv "+f.destPath+" installed"; got != want {
			t.Errorf("restorecon calls = %q, want %q: on an SELinux-enforcing host the binary keeps "+
				"the label of the /tmp file it was renamed from, and systemd cannot exec it (#1052)", got, want)
		}
	})

	t.Run("SELinux disabled: restorecon is not run", func(t *testing.T) {
		f := newFixture(t)
		dir, path := shimDir(t, map[string]string{"restorecon": restorecon(0), "selinuxenabled": selinuxenabled(1)})
		f.env = append(f.env, path)
		code, out, installed := f.run(true)
		if code != 0 || !installed {
			t.Fatalf("install failed (exit %d):\n%s", code, out)
		}
		if got := readLog(dir); got != "" {
			t.Errorf("ran restorecon with SELinux disabled: %q", got)
		}
	})

	t.Run("relabel failure is reported, not fatal", func(t *testing.T) {
		f := newFixture(t)
		_, path := shimDir(t, map[string]string{"restorecon": restorecon(1), "selinuxenabled": selinuxenabled(0)})
		f.env = append(f.env, path)
		code, out, installed := f.run(true)
		if code != 0 || !installed {
			t.Fatalf("a failed relabel failed the install (exit %d); the verified binary is in place "+
				"and runs on a permissive host:\n%s", code, out)
		}
		if !strings.Contains(out, "WARNING: restorecon failed") {
			t.Errorf("a failed relabel was silent:\n%s", out)
		}
	})

	t.Run("no restorecon, as on Ubuntu: a no-op", func(t *testing.T) {
		if _, err := exec.LookPath("restorecon"); err == nil {
			t.Skip("this host has a real restorecon; cannot model its absence by PATH alone")
		}
		f := newFixture(t)
		code, out, installed := f.run(true)
		if code != 0 || !installed {
			t.Fatalf("install failed without restorecon (exit %d):\n%s", code, out)
		}
	})
}
