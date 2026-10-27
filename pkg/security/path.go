package security

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// hostIsWindows selects the path rules ValidatePathForReading applies. It is a
// variable, not runtime.GOOS inline, only so the rules are named in one place.
var hostIsWindows = runtime.GOOS == "windows"

// ValidatePathForReading validates a path on THIS machine before it is read.
//
// Its callers read a file the caller is about to send somewhere else: `spawn
// launch --user-data-file` / `--user-data @file` (the file becomes the instance's
// user data) and spored's queue runner (each job's result files are uploaded to
// S3, on Linux and Windows spores alike). The guard keeps those paths from
// reaching the machine's secrets and system state, so it uses the host OS's path
// rules: POSIX on Linux and macOS, Windows on Windows (#1000).
//
// It rejects:
//   - an empty path, or one containing a NUL byte;
//   - any ".." element (before cleaning, so "a/../../etc" cannot clean itself
//     into an allowed shape);
//   - a path at or under a system directory (forbiddenReadRoots), compared
//     case-insensitively, both as written and after resolving symlinks and
//     junctions in the longest existing prefix (resolveExisting), so a link
//     pointing into /etc or C:\Windows is caught;
//   - on Windows, shapes that bypass or confuse that comparison: UNC paths and
//     the \\?\, \\.\ and \??\ prefixes, alternate data streams ("file:stream"),
//     reserved device names (CON, NUL, COM1, ...), and elements ending in a dot
//     or space (which Windows silently strips, so "C:\Windows.\x" opens
//     C:\Windows\x).
//
// The check is advisory against a racing local attacker: the path is resolved
// here and opened later by the caller.
func ValidatePathForReading(p string) error {
	if p == "" {
		return errors.New("path cannot be empty")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return errors.New("path contains a NUL byte")
	}
	if hostIsWindows {
		if err := windowsPathShapeError(p); err != nil {
			return err
		}
	}
	if hasDotDotElement(p, hostIsWindows) {
		return errors.New("path traversal not allowed")
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		// If we can't get absolute path, be conservative and reject
		return errors.New("cannot validate path")
	}

	roots := forbiddenReadRoots()
	if underAnyRoot(abs, roots, hostIsWindows) {
		return errors.New("access to system paths not allowed")
	}
	resolved, err := resolveExisting(abs)
	if err != nil {
		return errors.New("cannot validate path: " + err.Error())
	}
	if resolved != abs {
		if hostIsWindows {
			// A junction or mount point can resolve to a device or UNC form
			// the prefix comparison would not recognise. The one UNC form
			// admitted is a path on a mapped network drive that stays on the
			// share the drive is mapped to (#1107).
			if err := windowsPathShapeError(resolved); err != nil {
				if merr := mappedDriveError(abs, resolved, resolveExisting, onSystemVolume); merr != nil {
					return errors.New("path resolves to a disallowed location: " + err.Error() + " (" + merr.Error() + ")")
				}
			}
		}
		if underAnyRoot(resolved, roots, hostIsWindows) {
			return errors.New("access to system paths not allowed (path resolves through a link into one)")
		}
	}
	return nil
}

// joinRest prepends elem to the unresolved tail rest.
func joinRest(elem, rest string) string {
	if rest == "" {
		return elem
	}
	return filepath.Join(elem, rest)
}

// isSep reports whether c separates path elements under the given rules.
func isSep(c byte, windows bool) bool {
	return c == '/' || (windows && c == '\\')
}

// splitElements splits p into its elements under the given rules, dropping
// empty ones (from leading, trailing or repeated separators).
func splitElements(p string, windows bool) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r < 0x80 && isSep(byte(r), windows) })
}

// hasDotDotElement reports whether any element of p is "..".
func hasDotDotElement(p string, windows bool) bool {
	for _, e := range splitElements(p, windows) {
		if e == ".." {
			return true
		}
	}
	return false
}

// mappedDriveError reports why resolved, the final path of abs, does not
// qualify as a path on a mapped network drive, or nil if it does (#1107).
//
// A mapped drive Z: resolves to \\server\share, so every existing path on it
// resolves to a UNC form that windowsPathShapeError refuses and that no
// drive-letter root matches. The drive's ROOT is resolved through the same
// resolution (resolve, which is resolveExisting outside tests), so both sides
// are in \\server\share form, and resolved is admitted only when:
//
//   - abs is on a drive letter whose root resolves to a plain \\server\share
//     (not \\?\ or \\.\, and never a drive form: a subst drive resolves to
//     the local directory it names, and its paths are compared against the
//     system directories as that directory, unchanged);
//   - resolved is at or under that share, so no link on the drive led
//     anywhere else, and the part after the share passes
//     windowsPathShapeError;
//   - resolved is not on a volume holding one of this machine's system
//     directories (onSystemVolume). The prefix comparison cannot see through
//     a loopback share: Z: mapped to \\localhost\C$, or to a share of
//     C:\Windows, or a share holding a junction into it, reaches the system
//     directories under a name no root matches, and before #1107 those were
//     refused for being UNC. This keeps them refused.
//
// Nothing is translated back to the drive letter: resolved is checked as the
// place it reaches, so a drive that is itself a link cannot admit more than
// its target would. It is plain string handling apart from the two
// functions, so it is tested on every OS with fakes.
func mappedDriveError(abs, resolved string, resolve func(string) (string, error), onSystemVolume func(string) (bool, error)) error {
	if len(abs) < 3 || !isASCIILetter(abs[0]) || abs[1] != ':' || !isSep(abs[2], true) {
		return errors.New("not on a drive letter")
	}
	root, err := resolve(abs[:2] + `\`)
	if err != nil {
		return errors.New("cannot resolve drive " + abs[:2] + ": " + err.Error())
	}
	if !isPlainUNC(root) {
		return errors.New("drive " + abs[:2] + " is not mapped to a network share")
	}
	if !underAnyRoot(resolved, []string{root}, true) {
		return errors.New("it leaves the share drive " + abs[:2] + " is mapped to")
	}
	share := strings.TrimRight(root, `\`)
	if err := windowsPathShapeError(resolved[len(share):]); err != nil {
		return err
	}
	sys, err := onSystemVolume(resolved)
	if err != nil {
		return errors.New("cannot identify its volume: " + err.Error())
	}
	if sys {
		return errors.New("it is on a volume holding this machine's system directories")
	}
	return nil
}

// isPlainUNC reports whether p is \\server\share or a path under it: not a
// device (\\.\) or extended-length (\\?\) form, which normaliseFinalPath
// leaves in place for what it cannot name.
func isPlainUNC(p string) bool {
	if len(p) < 2 || p[0] != '\\' || p[1] != '\\' {
		return false
	}
	e := strings.Split(p[2:], `\`)
	if len(e) < 2 || e[0] == "" || e[1] == "" || e[0] == "?" || e[0] == "." {
		return false
	}
	return true
}

// windowsPathShapeError rejects Windows path forms that can reach a file the
// system-directory comparison would not recognise. It is pure string handling,
// so it is tested on every OS.
func windowsPathShapeError(p string) error {
	if len(p) >= 2 && isSep(p[0], true) && isSep(p[1], true) {
		// \\server\share, //server/share, \\?\C:\..., \\.\PhysicalDrive0,
		// \\?\UNC\... — all bypass the drive-letter root comparison.
		return errors.New("UNC and device-namespace paths (\\\\server\\share, \\\\?\\, \\\\.\\) are not allowed")
	}
	if strings.HasPrefix(p, `\??\`) || strings.HasPrefix(p, `/??/`) {
		return errors.New("NT object-namespace paths (\\??\\) are not allowed")
	}
	rest := p
	if len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		rest = p[2:]
	}
	if strings.IndexByte(rest, ':') >= 0 {
		// Anything after the drive letter: an alternate data stream
		// (file.txt:hidden, file::$DATA) or a device name like CON:.
		return errors.New("alternate data streams and colons in Windows paths are not allowed")
	}
	for _, e := range splitElements(rest, true) {
		if e == "." || e == ".." {
			continue
		}
		if isWindowsReservedName(e) {
			return errors.New("Windows reserved device names (CON, NUL, COM1, ...) are not allowed: " + e)
		}
		if last := e[len(e)-1]; last == '.' || last == ' ' {
			return errors.New("Windows path elements ending in a dot or space are not allowed: " + e)
		}
	}
	return nil
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// isWindowsReservedName reports whether the element names a DOS device. The
// name is reserved with any extension and trailing spaces ("nul.txt", "CON ")
// on the Windows versions spores run, so this is deliberately broader than
// Go's internal check, which defers "CON.txt" to the running OS.
func isWindowsReservedName(elem string) bool {
	base := elem
	if i := strings.IndexAny(base, ".:"); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimRight(base, " ")
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
		return true
	}
	if len(base) >= 4 {
		prefix := strings.ToUpper(base[:3])
		if prefix == "COM" || prefix == "LPT" {
			switch base[3:] {
			case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
				return true
			}
		}
	}
	return false
}

// underAnyRoot reports whether p is one of roots or sits under one.
//
// The comparison is case-insensitive everywhere: on Windows and on macOS's
// default filesystem "/ETC/passwd" and "c:\windows" open the same files as
// their canonical spellings, and on Linux the cost is only that a directory
// such as "/Etc" is refused too.
func underAnyRoot(p string, roots []string, windows bool) bool {
	norm := func(s string) string {
		if windows {
			s = strings.ReplaceAll(s, "/", `\`)
			for strings.HasSuffix(s, `\`) && len(s) > 3 {
				s = strings.TrimSuffix(s, `\`)
			}
		} else {
			for strings.HasSuffix(s, "/") && len(s) > 1 {
				s = strings.TrimSuffix(s, "/")
			}
		}
		return s
	}
	sep := "/"
	if windows {
		sep = `\`
	}
	np := norm(p)
	for _, r := range roots {
		nr := norm(r)
		if nr == "" {
			continue
		}
		if strings.EqualFold(np, nr) {
			return true
		}
		if len(np) > len(nr) && strings.EqualFold(np[:len(nr)], nr) &&
			(strings.HasSuffix(nr, sep) || np[len(nr):len(nr)+1] == sep) {
			return true
		}
	}
	return false
}

// forbiddenReadRoots are the system directories of the machine spawn or spored
// is running on.
func forbiddenReadRoots() []string {
	if hostIsWindows {
		return windowsForbiddenReadRoots(os.Getenv)
	}
	return unixForbiddenReadRoots()
}

// unixForbiddenReadRoots are the directories ValidatePathForReading refuses on
// Linux and macOS. The /private and /var/root entries are macOS, where /etc and
// /var are symlinks into /private (so a resolved path lands there) and /var/root
// is root's home.
func unixForbiddenReadRoots() []string {
	return []string{
		"/etc",
		"/sys",
		"/proc",
		"/root",
		"/boot",
		"/dev",
		"/var/lib",
		"/run",
		"/private/etc",
		"/private/var/root",
		"/var/root",
	}
}

// windowsForbiddenReadRoots are the Windows counterparts, from the environment
// with fixed fallbacks: the Windows directory (registry hives and the SAM under
// System32\config, the LocalSystem profile spored runs as), ProgramData
// (machine-wide state: EC2Launch's logs and config, OpenSSH host keys and
// administrators_authorized_keys, spored's own log), and System Volume
// Information. getenv is a parameter so the list is tested on every OS.
func windowsForbiddenReadRoots(getenv func(string) string) []string {
	drive := getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	roots := []string{
		drive + `\Windows`,
		drive + `\ProgramData`,
		drive + `\System Volume Information`,
		`C:\Windows`,
		`C:\ProgramData`,
	}
	for _, v := range []string{"SystemRoot", "windir", "ProgramData", "ALLUSERSPROFILE"} {
		if s := getenv(v); s != "" {
			roots = append(roots, s)
		}
	}
	return roots
}

// SanitizePath sanitizes a path for safe logging and display.
// It removes any path traversal sequences. The result uses forward slashes on
// every OS (#1000), so a log line reads the same wherever spawn ran; a Windows
// host's backslashes are converted first so they are cleaned like slashes.
//
// No production caller, kept on purpose (#868): it is display-only and safe to
// use, but nothing logs an untrusted path today. It is not a validator; use
// ValidatePathForReading before opening a path.
func SanitizePath(p string) string {
	cleaned := path.Clean(filepath.ToSlash(p))

	// Remove any remaining .. sequences and clean up double slashes
	cleaned = strings.ReplaceAll(cleaned, "..", "")
	return path.Clean(cleaned)
}
