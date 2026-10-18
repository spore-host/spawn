//go:build windows

package security

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func systemRoot(t *testing.T) string {
	t.Helper()
	if r := os.Getenv("SystemRoot"); r != "" {
		return r
	}
	return `C:\Windows`
}

// Windows host: before #1000 every one of the "true" rows was allowed, because
// the validator compared against /etc-style prefixes only.
func TestValidatePathForReading_WindowsHost(t *testing.T) {
	sr := systemRoot(t)
	drive := filepath.VolumeName(sr)
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"user profile file", `C:\Users\Administrator\results\out.txt`, false},
		{"temp file", filepath.Join(t.TempDir(), "x.txt"), false},
		{"forward slashes in a user path", `C:/Users/Administrator/out.txt`, false},

		{"SAM hive", sr + `\System32\config\SAM`, true},
		{"lower case", strings.ToLower(sr) + `\system32\config\sam`, true},
		{"upper case", strings.ToUpper(sr) + `\SYSTEM32\CONFIG\SAM`, true},
		{"forward slashes", filepath.ToSlash(sr) + `/System32/config/SAM`, true},
		{"mixed separators", sr + `/System32\config/SAM`, true},
		{"traversal", drive + `\Users\..\Windows\System32\config\SAM`, true},
		{"relative traversal", `..\..\Windows\win.ini`, true},
		{"ProgramData ssh keys", drive + `\ProgramData\ssh\administrators_authorized_keys`, true},
		{"EC2Launch state", drive + `\ProgramData\Amazon\EC2Launch\log\agent.log`, true},
		{"System Volume Information", drive + `\System Volume Information\x`, true},
		{"UNC loopback admin share", `\\localhost\` + strings.TrimSuffix(drive, ":") + `$\Windows\System32\config\SAM`, true},
		{"UNC with forward slashes", `//localhost/c$/Windows/win.ini`, true},
		{"UNC remote share", `\\server\share\file.txt`, true},
		{"extended-length prefix", `\\?\` + sr + `\System32\config\SAM`, true},
		{"extended UNC prefix", `\\?\UNC\localhost\c$\Windows\win.ini`, true},
		{"device namespace", `\\.\` + sr + `\win.ini`, true},
		{"physical drive", `\\.\PhysicalDrive0`, true},
		{"NT object namespace", `\??\` + sr + `\win.ini`, true},
		{"alternate data stream", filepath.Join(t.TempDir(), "x.txt") + `:hidden`, true},
		{"default data stream", filepath.Join(t.TempDir(), "x.txt") + `::$DATA`, true},
		{"NUL device", `NUL`, true},
		{"CON device in a directory", `C:\Users\Administrator\CON`, true},
		{"device with extension", `nul.txt`, true},
		{"COM port", `COM1`, true},
		{"trailing dot stripped by Windows", sr + `.\System32\config\SAM`, true},
		{"trailing space stripped by Windows", sr + ` \System32\config\SAM`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePathForReading(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePathForReading(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

// A path rooted at the current drive ("\Windows\...") resolves against it.
func TestValidatePathForReading_WindowsRootRelative(t *testing.T) {
	sr := systemRoot(t)
	t.Chdir(filepath.VolumeName(sr) + `\`)
	p := strings.TrimPrefix(sr, filepath.VolumeName(sr)) + `\System32\config\SAM`
	if err := ValidatePathForReading(p); err == nil {
		t.Errorf("ValidatePathForReading(%q) = nil from the system drive", p)
	}
}

// A directory junction (no privilege needed, unlike a symlink) under an
// allowed directory that points at the Windows directory must be refused.
func TestValidatePathForReading_WindowsJunction(t *testing.T) {
	sr := systemRoot(t)
	j := filepath.Join(t.TempDir(), "innocent")
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, sr).CombinedOutput(); err != nil { //nolint:gosec // fixed command; j is this test's temp dir, sr the system root
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	for _, p := range []string{j, filepath.Join(j, "win.ini"), filepath.Join(j, "System32", "config", "SAM"), filepath.Join(j, "nope", "*.log")} {
		if err := ValidatePathForReading(p); err == nil {
			t.Errorf("ValidatePathForReading(%q) = nil; %q is a junction to %q", p, j, sr)
		}
	}
}

// An 8.3 short name (C:\WINDOW~1) is a second spelling of the Windows
// directory that no prefix comparison on the written path can catch;
// resolution must.
func TestValidatePathForReading_WindowsShortName(t *testing.T) {
	sr := systemRoot(t)
	long, err := syscall.UTF16PtrFromString(sr)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 512)
	n, err := syscall.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Skipf("no short name for %s: %v", sr, err)
	}
	short := syscall.UTF16ToString(buf[:n])
	if strings.EqualFold(short, sr) {
		t.Skipf("8.3 names are disabled on %s's volume", sr)
	}
	p := short + `\win.ini`
	if err := ValidatePathForReading(p); err == nil {
		t.Errorf("ValidatePathForReading(%q) = nil; it is the short name of %s", p, sr)
	}
}

// Directory and file symlinks into the Windows directory. Creating a symlink
// needs SeCreateSymbolicLinkPrivilege or Developer Mode; CI's runner has it,
// elsewhere the test skips.
func TestValidatePathForReading_WindowsSymlink(t *testing.T) {
	sr := systemRoot(t)
	dir := t.TempDir()
	dlink := filepath.Join(dir, "dirlink")
	if err := os.Symlink(sr, dlink); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	flink := filepath.Join(dir, "filelink.txt")
	if err := os.Symlink(filepath.Join(sr, "win.ini"), flink); err != nil {
		t.Fatalf("file symlink: %v", err)
	}
	// A link to a link: the junction resolution must follow the whole chain.
	chain := filepath.Join(dir, "chain")
	if err := os.Symlink(dlink, chain); err != nil {
		t.Fatalf("chained symlink: %v", err)
	}
	// A dangling link cannot be resolved, so it is refused rather than
	// checked as the link's own name.
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(sr, "no-such-file-1000"), dangling); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	for _, p := range []string{
		dlink, filepath.Join(dlink, "win.ini"), filepath.Join(dlink, "System32", "config", "SAM"),
		filepath.Join(dlink, "nope", "*.log"), flink, chain, filepath.Join(chain, "win.ini"), dangling,
	} {
		if err := ValidatePathForReading(p); err == nil {
			t.Errorf("ValidatePathForReading(%q) = nil; it leads into %q", p, sr)
		}
	}
	// A link that stays outside the system directories is fine.
	ok := filepath.Join(dir, "ok")
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), ok); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ok, filepath.Join(ok, "out.txt"), filepath.Join(ok, "*.log")} {
		if err := ValidatePathForReading(p); err != nil {
			t.Errorf("ValidatePathForReading(%q) = %v; it leads to the temp dir", p, err)
		}
	}
}

// A junction to an ordinary directory is allowed; resolution must not turn
// every real path into a refusal.
func TestValidatePathForReading_WindowsJunctionAllowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(dir, "j")
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, target).CombinedOutput(); err != nil { //nolint:gosec // fixed command; both paths are this test's temp dir
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	for _, p := range []string{j, filepath.Join(j, "out.txt"), filepath.Join(j, "sub", "*.log")} {
		if err := ValidatePathForReading(p); err != nil {
			t.Errorf("ValidatePathForReading(%q) = %v; %q is a junction to a temp dir", p, err, j)
		}
	}
}

func TestNormaliseFinalPath(t *testing.T) {
	for in, want := range map[string]string{
		`\\?\C:\Windows\win.ini`:        `C:\Windows\win.ini`,
		`\\?\c:\x`:                      `c:\x`,
		`\\?\UNC\server\share\f.txt`:    `\\server\share\f.txt`,
		`\\?\Volume{0000-1111}\Windows`: `\\?\Volume{0000-1111}\Windows`,
		`C:\already\plain`:              `C:\already\plain`,
	} {
		if got := normaliseFinalPath(in); got != want {
			t.Errorf("normaliseFinalPath(%q) = %q, want %q", in, got, want)
		}
	}
	// The forms left alone must still be refused by the shape check.
	if windowsPathShapeError(`\\?\Volume{0000-1111}\Windows`) == nil {
		t.Error("a volume-GUID final path passed the shape check")
	}
}

// A path on a mapped network drive resolves to \\server\share form, which
// before #1107 was refused as UNC however legitimate it was. The share here is
// a loopback one (\\localhost\...) on a scratch volume attached for the test,
// because a loopback share on the system volume is refused by design; the
// mapping is real, so GetFinalPathNameByHandle sees exactly what it sees for
// a remote server.
//
// It needs an elevated shell (diskpart, New-SmbShare). The CI runner is one,
// so there a fixture that cannot be built fails the test; elsewhere it skips
// and says which step needed elevation.
func TestValidatePathForReading_WindowsMappedNetworkDrive(t *testing.T) {
	sr := systemRoot(t)
	vol := attachScratchVolume(t)
	dir := filepath.Join(vol+`\`, "share")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A junction inside the share into the Windows directory: the server
	// follows it, so the client's final path still names the share.
	j := filepath.Join(dir, "j")
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, sr).CombinedOutput(); err != nil { //nolint:gosec // fixed command; j is on this test's scratch volume, sr the system root
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	z := mapNetworkDrive(t, `\\localhost\`+smbShare(t, dir))

	// The fixture is the shape #1107 is about: the drive resolves to UNC.
	if r, _, err := finalPathName(z + `\data.txt`); err != nil || !isPlainUNC(r) {
		t.Fatalf("finalPathName(%s\\data.txt) = %q, %v; want a \\\\server\\share path", z, r, err)
	}
	for _, p := range []string{z + `\data.txt`, z + `\`, z + `\new\x.txt`, z + `\*.log`} {
		if err := ValidatePathForReading(p); err != nil {
			t.Errorf("ValidatePathForReading(%q) = %v; it is on a share mapped to %s", p, err, z)
		}
	}
	for _, p := range []string{j, z + `\j`, z + `\j\win.ini`, z + `\j\System32\config\SAM`, z + `\j\nope\*.log`} {
		if err := ValidatePathForReading(p); err == nil {
			t.Errorf("ValidatePathForReading(%q) = nil; it leads through a junction into %q", p, sr)
		}
	}

	// A drive mapped to a loopback share of the Windows directory reaches it
	// under a name no root matches; it must stay refused.
	y := mapNetworkDrive(t, `\\localhost\`+smbShare(t, sr))
	for _, p := range []string{y + `\win.ini`, y + `\System32\config\SAM`, y + `\nope\*.log`} {
		if err := ValidatePathForReading(p); err == nil {
			t.Errorf("ValidatePathForReading(%q) = nil; %s is a loopback share of %q", p, y, sr)
		}
	}
}

// A subst drive is a drive letter that is itself a link to a directory.
// GetFinalPathNameByHandle names the directory, so a subst drive pointing at
// the Windows directory must be refused and one pointing at an ordinary
// directory admitted: resolving the drive's root (#1107) must not translate
// the path back to the drive letter. subst needs no elevation.
func TestValidatePathForReading_WindowsSubstDrive(t *testing.T) {
	sr := systemRoot(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := substDrive(t, dir)
	for _, p := range []string{s + `\data.txt`, s + `\`, s + `\new\x.txt`, s + `\*.log`} {
		if err := ValidatePathForReading(p); err != nil {
			t.Errorf("ValidatePathForReading(%q) = %v; %s is subst for a temp dir", p, err, s)
		}
	}
	w := substDrive(t, sr)
	for _, p := range []string{w + `\`, w + `\win.ini`, w + `\System32\config\SAM`, w + `\nope\*.log`} {
		if err := ValidatePathForReading(p); err == nil {
			t.Errorf("ValidatePathForReading(%q) = nil; %s is subst for %q", p, w, sr)
		}
	}
}

// fixtureUnavailable fails the test on CI, where the runner is elevated and
// the fixture must be buildable, and otherwise skips naming the step.
func fixtureUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf("%s (the CI runner is elevated, so this fixture must be buildable)", msg)
	}
	t.Skipf("%s: this fixture needs an elevated shell", msg)
}

// freeDriveLetter returns an unused drive letter, as "X:".
func freeDriveLetter(t *testing.T) string {
	t.Helper()
	used, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	for c := 'Z'; c >= 'G'; c-- {
		if used&(1<<uint(c-'A')) == 0 {
			return string(c) + ":"
		}
	}
	t.Fatal("no free drive letter")
	return ""
}

// run runs a fixed fixture command and returns its combined output.
func run(name string, args ...string) (string, error) {
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // test fixture commands; arguments are this test's temp paths and generated names
	return string(out), err
}

// attachScratchVolume attaches a fresh 64 MB NTFS virtual disk and returns its
// drive letter. Its volume serial is new, so a loopback share on it is not on
// the system volume.
func attachScratchVolume(t *testing.T) string {
	t.Helper()
	// The long form: given a path through an 8.3 name (the runner's temp dir
	// is under C:\Users\RUNNER~1), diskpart attaches the disk but then reports
	// it "already detached" and leaves it attached.
	vhd := filepath.Join(longPath(t, t.TempDir()), "scratch.vhdx")
	letter := freeDriveLetter(t)
	script := filepath.Join(t.TempDir(), "attach.txt")
	body := fmt.Sprintf("create vdisk file=\"%s\" maximum=64 type=expandable\r\nselect vdisk file=\"%s\"\r\nattach vdisk\r\ncreate partition primary\r\nformat fs=ntfs quick label=spawn1107\r\nassign letter=%s\r\n", vhd, vhd, letter[:1])
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run("diskpart", "/s", script)
	t.Cleanup(func() {
		detach := filepath.Join(filepath.Dir(script), "detach.txt")
		if err := os.WriteFile(detach, []byte(fmt.Sprintf("select vdisk file=\"%s\"\r\ndetach vdisk\r\n", vhd)), 0o600); err == nil {
			if out, err := run("diskpart", "/s", detach); err != nil {
				t.Errorf("diskpart detach: %v: %s", err, out)
			}
		}
	})
	if err != nil {
		fixtureUnavailable(t, "diskpart could not attach a scratch volume: %v: %s", err, out)
	}
	if _, err := os.Stat(letter + `\`); err != nil {
		t.Fatalf("diskpart reported success but %s is not there: %v: %s", letter, err, out)
	}
	return letter
}

// shareSeq numbers the shares one test process creates.
var shareSeq int

// longPath expands any 8.3 short names in the existing path p.
func longPath(t *testing.T, p string) string {
	t.Helper()
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetLongPathName(in, &buf[0], uint32(len(buf)))
		if err != nil {
			t.Fatalf("GetLongPathName(%s): %v", p, err)
		}
		if n < uint32(len(buf)) {
			return windows.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n)
	}
}

// smbShare shares dir with the current user and returns the share name.
func smbShare(t *testing.T, dir string) string {
	t.Helper()
	shareSeq++
	name := fmt.Sprintf("spawn1107-%d-%d", os.Getpid(), shareSeq)
	out, err := run("powershell", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("New-SmbShare -Name '%s' -Path '%s' -FullAccess ([System.Security.Principal.WindowsIdentity]::GetCurrent().Name) | Out-Null", name, dir))
	if err != nil {
		fixtureUnavailable(t, "New-SmbShare %s: %v: %s", dir, err, out)
	}
	t.Cleanup(func() {
		if out, err := run("powershell", "-NoProfile", "-NonInteractive", "-Command",
			fmt.Sprintf("Remove-SmbShare -Name '%s' -Force", name)); err != nil {
			t.Logf("Remove-SmbShare %s: %v: %s", name, err, out)
		}
	})
	return name
}

// mapNetworkDrive maps a free drive letter to the UNC path unc.
func mapNetworkDrive(t *testing.T, unc string) string {
	t.Helper()
	letter := freeDriveLetter(t)
	if out, err := run("net", "use", letter, unc, "/persistent:no"); err != nil {
		fixtureUnavailable(t, "net use %s %s: %v: %s", letter, unc, err, out)
	}
	t.Cleanup(func() {
		if out, err := run("net", "use", letter, "/delete", "/y"); err != nil {
			t.Logf("net use %s /delete: %v: %s", letter, err, out)
		}
	})
	return letter
}

// substDrive maps a free drive letter to dir with subst.
func substDrive(t *testing.T, dir string) string {
	t.Helper()
	letter := freeDriveLetter(t)
	if out, err := run("subst", letter, dir); err != nil {
		t.Fatalf("subst %s %s: %v: %s", letter, dir, err, out)
	}
	t.Cleanup(func() {
		if out, err := run("subst", letter, "/d"); err != nil {
			t.Logf("subst %s /d: %v: %s", letter, err, out)
		}
	})
	return letter
}
