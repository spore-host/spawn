//go:build windows

package security

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
