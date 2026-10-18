package security

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Host-independent cases for ValidatePathForReading. The host-specific tables
// (POSIX system directories, Windows drive letters / UNC / ADS / device names)
// are in path_unix_test.go and path_windows_test.go (#1000).
func TestValidatePathForReading(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "data.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid relative path", "userdata.txt", false},
		{"existing temp file", file, false},
		{"not-yet-existing temp file", filepath.Join(tmp, "new", "x.txt"), false},
		{"dots inside a name are not traversal", "notes..v2.txt", false},
		{"empty path", "", true},
		{"NUL byte", "data\x00.txt", true},
		{"relative traversal", "../../secret", true},
		{"traversal that cleans to an allowed shape", filepath.Join(tmp, "a") + string(filepath.Separator) + ".." + string(filepath.Separator) + "data.txt", true},
		{"traversal through forward slashes", "a/../../b", true},
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

// A symlink under an allowed directory that points into a system directory
// must be refused, whether the link is the file itself, a directory above it,
// or a directory above a file that does not exist (a glob pattern's shape).
func TestValidatePathForReading_FollowsSymlinksIntoSystemDirs(t *testing.T) {
	sys := existingForbiddenRoot(t)
	tmp := t.TempDir()
	link := filepath.Join(tmp, "innocent")
	if err := os.Symlink(sys, link); err != nil {
		t.Skipf("cannot create a symlink here (%v); the Windows junction test covers this there", err)
	}
	for _, p := range []string{
		link,
		filepath.Join(link, "anything"),
		filepath.Join(link, "does-not-exist", "*.txt"),
	} {
		if err := ValidatePathForReading(p); err == nil {
			t.Errorf("ValidatePathForReading(%q) = nil; %q links to system dir %q", p, link, sys)
		}
	}
	// And a symlink to an allowed place stays allowed.
	ok := filepath.Join(tmp, "ok")
	if err := os.Symlink(tmp, ok); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePathForReading(filepath.Join(ok, "data.txt")); err != nil {
		t.Errorf("symlink to the temp dir was refused: %v", err)
	}
}

// existingForbiddenRoot returns a forbidden root that exists on this host.
func existingForbiddenRoot(t *testing.T) string {
	t.Helper()
	for _, r := range forbiddenReadRoots() {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() {
			return r
		}
	}
	t.Fatalf("none of %v exists on this host; the forbidden-root list is wrong for it", forbiddenReadRoots())
	return ""
}

// Every forbidden root must itself be refused, in its own and in swapped case:
// the list is what the validator is for, so a root that slipped through the
// comparison would be the bug #1000 describes.
func TestValidatePathForReading_RefusesEveryForbiddenRoot(t *testing.T) {
	roots := forbiddenReadRoots()
	if len(roots) == 0 {
		t.Fatal("forbiddenReadRoots() is empty")
	}
	for _, r := range roots {
		for _, p := range []string{r, filepath.Join(r, "x"), strings.ToUpper(r) + string(filepath.Separator) + "X", strings.ToLower(r) + string(filepath.Separator) + "x"} {
			if err := ValidatePathForReading(p); err == nil {
				t.Errorf("ValidatePathForReading(%q) = nil; it is under forbidden root %q", p, r)
			}
		}
	}
}

// The Windows path rules are plain string handling, so they are checked here on
// every OS as well as end to end on Windows (path_windows_test.go).
func TestWindowsPathShapeError(t *testing.T) {
	tests := []struct {
		path    string
		wantErr bool
	}{
		{`C:\Users\Administrator\results\out.txt`, false},
		{`c:/users/administrator/out.txt`, false},
		{`relative\dir\file.txt`, false},
		{`C:file.txt`, false},
		{`\Users\x\file.txt`, false},
		{`report.v1.txt`, false},
		{`CONFIG.txt`, false},
		{`COM10`, false},
		{`console`, false},

		{`\\server\share\file.txt`, true},
		{`//server/share/file.txt`, true},
		{`\\localhost\C$\Windows\System32\config\SAM`, true},
		{`\\?\C:\Windows\System32\config\SAM`, true},
		{`\\?\UNC\server\share\x`, true},
		{`\\.\C:\Windows\x`, true},
		{`\\.\PhysicalDrive0`, true},
		{`\??\C:\Windows\x`, true},
		{`C:\Users\x\file.txt:hidden`, true},
		{`file.txt::$DATA`, true},
		{`C:\Users\x\dir:stream\f`, true},
		{`NUL`, true},
		{`nul.txt`, true},
		{`C:\Users\x\CON`, true},
		{`C:\Users\x\con.log`, true},
		{`aux`, true},
		{`PRN `, true},
		{`COM1`, true},
		{`lpt9.dat`, true},
		{`COM¹`, true},
		{`CONIN$`, true},
		{`CONOUT$`, true},
		{`C:\Windows.\System32\config\SAM`, true},
		{`C:\Windows \System32`, true},
		{`C:\Users\x\file.txt.`, true},
	}
	for _, tt := range tests {
		err := windowsPathShapeError(tt.path)
		if (err != nil) != tt.wantErr {
			t.Errorf("windowsPathShapeError(%q) = %v, wantErr %v", tt.path, err, tt.wantErr)
		}
	}
}

func TestUnderAnyRoot(t *testing.T) {
	win := windowsForbiddenReadRoots(func(k string) string {
		return map[string]string{"SystemDrive": "C:", "SystemRoot": `C:\WINDOWS`, "ProgramData": `C:\ProgramData`}[k]
	})
	unix := unixForbiddenReadRoots()
	tests := []struct {
		path    string
		windows bool
		want    bool
	}{
		{`C:\Windows\System32\config\SAM`, true, true},
		{`c:\windows\system32\config\sam`, true, true},
		{`C:/Windows/System32`, true, true},
		{`C:\WINDOWS`, true, true},
		{`C:\Windows\`, true, true},
		{`C:\ProgramData\ssh\ssh_host_ed25519_key`, true, true},
		{`C:\System Volume Information\x`, true, true},
		{`C:\WindowsApps\x`, true, false},
		{`C:\Users\Administrator\out.txt`, true, false},
		{`D:\Windows\x`, true, false},

		{"/etc/passwd", false, true},
		{"/etc", false, true},
		{"/ETC/shadow", false, true},
		{"/private/etc/passwd", false, true},
		{"/var/root/.ssh/id_rsa", false, true},
		{"/proc/self/mem", false, true},
		{"/etcetera/x", false, false},
		{"/home/user/etc/passwd", false, false},
		{"/var/library", false, false},
		{"/tmp/x", false, false},
	}
	for _, tt := range tests {
		roots := unix
		if tt.windows {
			roots = win
		}
		if got := underAnyRoot(tt.path, roots, tt.windows); got != tt.want {
			t.Errorf("underAnyRoot(%q, windows=%v) = %v, want %v", tt.path, tt.windows, got, tt.want)
		}
	}
}

// Windows roots fall back to C:\Windows and C:\ProgramData when the
// environment is empty (a service started with a stripped environment).
func TestWindowsForbiddenReadRoots_Fallbacks(t *testing.T) {
	roots := windowsForbiddenReadRoots(func(string) string { return "" })
	for _, want := range []string{`C:\Windows`, `C:\ProgramData`} {
		if !underAnyRoot(want+`\x`, roots, true) {
			t.Errorf("with an empty environment %q is not forbidden (roots %v)", want, roots)
		}
	}
	roots = windowsForbiddenReadRoots(func(k string) string {
		return map[string]string{"SystemDrive": "E:", "SystemRoot": `E:\WinNT`}[k]
	})
	for _, want := range []string{`E:\WinNT\x`, `E:\Windows\x`, `E:\ProgramData\x`} {
		if !underAnyRoot(want, roots, true) {
			t.Errorf("with SystemDrive=E: %q is not forbidden (roots %v)", want, roots)
		}
	}
}

// ValidateMountPath describes a path on a Linux instance, so the answers are
// the same on every host OS (#1000): /mnt/data was rejected as "not absolute"
// on Windows.
func TestValidateMountPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid mnt path", "/mnt/data", false},
		{"valid data path", "/data/scratch", false},
		{"valid scratch path", "/scratch/temp", false},
		{"empty path", "", true},
		{"relative path", "mnt/data", true},
		{"path traversal in mnt", "/mnt/../etc/passwd", true},
		{"invalid prefix /tmp", "/tmp/mount", true},
		{"invalid prefix /home", "/home/user/mount", true},
		{"root path", "/", true},
		{"etc path", "/etc/mnt", true},
		{"bare /mnt", "/mnt", true},
		{"Windows drive path", `C:\mnt\data`, true},
		{"backslashes", `\mnt\data`, true},
		{"backslash smuggled after /mnt", `/mnt/..\etc`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMountPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMountPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

// SanitizePath's output is forward-slashed on every OS (#1000).
func TestSanitizePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{"clean path", "/home/user/data.txt", "/home/user/data.txt"},
		{"path with traversal", "/home/user/../../etc/passwd", "/etc/passwd"},
		{"path with double dots", "/home/../user/../data.txt", "/data.txt"},
		{"relative path", "../../etc/passwd", "/etc/passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result := SanitizePath(tt.path); result != tt.expected {
				t.Errorf("SanitizePath(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

// A path on a mapped network drive resolves to the share's UNC form; it is
// admitted when the drive's root resolves to that same share, and only then
// (#1107). The resolver and the volume check are fakes, so the rule is tested
// on every OS; path_windows_test.go maps a real share.
func TestMappedDriveError(t *testing.T) {
	drives := map[string]string{
		`Z:\`: `\\server\share\`,
		`Y:\`: `\\server\share\sub`,
		`S:\`: `C:\Users\x\substituted`, // subst: a local directory
		`V:\`: `\\?\Volume{0000-1111}\`,
		`D:\`: `\\.\PhysicalDrive0`,
		`Q:\`: `\\?\UNC`, // an extended form normaliseFinalPath could not shorten
	}
	resolve := func(p string) (string, error) { // drive letters ignore case
		if r, ok := drives[strings.ToUpper(p)]; ok {
			return r, nil
		}
		return "", errors.New("no such drive")
	}
	notSystem := func(string) (bool, error) { return false, nil }
	tests := []struct {
		name, abs, resolved string
		wantErr             bool
	}{
		{"file on the mapped share", `Z:\data\x.txt`, `\\server\share\data\x.txt`, false},
		{"the share's root", `Z:\`, `\\server\share\`, false},
		{"case differs from the drive's root", `Z:\data`, `\\SERVER\Share\data`, false},
		{"drive mapped to a folder in a share", `Y:\x`, `\\server\share\sub\x`, false},
		{"lower-case drive letter", `z:\data\x`, `\\server\share\data\x`, false},

		{"link on the drive to another share", `Z:\link\x`, `\\other\share\x`, true},
		{"link on the drive to a sibling share", `Z:\link\x`, `\\server\share2\x`, true},
		{"link out of the mapped folder", `Y:\link\x`, `\\server\share\secret\x`, true},
		{"a local drive's link to a share", `C:\Users\x\link\f`, `\\server\share\f`, true},
		{"subst drive", `S:\x`, `\\server\share\x`, true},
		{"drive resolving to a volume GUID", `V:\x`, `\\server\share\x`, true},
		{"drive resolving to a device", `D:\x`, `\\server\share\x`, true},
		{"malformed UNC root", `Q:\x`, `\\?\UNC\x`, true},
		{"unresolvable drive", `W:\x`, `\\server\share\x`, true},
		{"not a drive path", `\\server\share\x`, `\\server\share\x`, true},
		{"relative path", `x\y`, `\\server\share\x`, true},
		{"stream on the share", `Z:\x`, `\\server\share\x:hidden`, true},
		{"device name on the share", `Z:\nul`, `\\server\share\nul`, true},
		{"trailing dot on the share", `Z:\x.`, `\\server\share\x.`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mappedDriveError(tt.abs, tt.resolved, resolve, notSystem)
			if (err != nil) != tt.wantErr {
				t.Errorf("mappedDriveError(%q, %q) = %v, wantErr %v", tt.abs, tt.resolved, err, tt.wantErr)
			}
		})
	}

	// A share on one of this machine's system volumes (a loopback mapping
	// such as \\localhost\C$) is refused, and so is a share whose volume
	// cannot be identified: both were refused before #1107.
	ok := `\\server\share\data\x`
	if err := mappedDriveError(`Z:\data\x`, ok, resolve, func(string) (bool, error) { return true, nil }); err == nil {
		t.Error("a mapped drive on a system volume was admitted")
	}
	if err := mappedDriveError(`Z:\data\x`, ok, resolve, func(string) (bool, error) { return false, errors.New("boom") }); err == nil {
		t.Error("a mapped drive whose volume could not be identified was admitted")
	}
	// The volume check is asked about the resolved path, not the drive
	// letter: that is where a junction inside the share would have led.
	var asked string
	_ = mappedDriveError(`Z:\data\x`, ok, resolve, func(p string) (bool, error) { asked = p; return false, nil })
	if asked != ok {
		t.Errorf("onSystemVolume asked about %q, want the resolved path %q", asked, ok)
	}
}

func TestIsPlainUNC(t *testing.T) {
	for p, want := range map[string]bool{
		`\\server\share`:       true,
		`\\server\share\`:      true,
		`\\server\share\x\y`:   true,
		`\\server`:             false,
		`\\server\`:            false,
		`\\?\C:\x`:             false,
		`\\?\UNC\server\share`: false,
		`\\.\PhysicalDrive0`:   false,
		`\\.\pipe\x`:           false,
		`C:\x`:                 false,
		`\x`:                   false,
		``:                     false,
	} {
		if got := isPlainUNC(p); got != want {
			t.Errorf("isPlainUNC(%q) = %v, want %v", p, got, want)
		}
	}
}
