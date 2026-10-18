//go:build windows

package security

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// GetFinalPathNameByHandle flags (fileapi.h); x/sys/windows does not name them.
const (
	fileNameNormalized = 0x0 // FILE_NAME_NORMALIZED
	volumeNameDOS      = 0x0 // VOLUME_NAME_DOS
)

// resolveExisting returns the final path of the longest prefix of the
// absolute path abs that exists, with the rest re-appended. A path that does
// not exist yet (or is a glob pattern) is still caught when a directory it
// sits under leads into a system directory.
//
// It asks Windows rather than filepath.EvalSymlinks: since Go 1.23
// (GODEBUG winsymlink=1) os.Lstat reports a directory junction or mount
// point as ModeIrregular rather than ModeSymlink, and EvalSymlinks no longer
// follows it, so a junction to C:\Windows resolved to itself (#1000). Opening
// the path (without FILE_FLAG_OPEN_REPARSE_POINT, so every symlink, junction
// and mount point is followed; with FILE_FLAG_BACKUP_SEMANTICS, so a
// directory opens) and calling GetFinalPathNameByHandle yields the path the
// I/O manager actually reached, with 8.3 short names expanded.
//
// It fails, so the caller refuses the path, when an element that exists is a
// reparse point that cannot be opened (a dangling link, or one whose target
// this process may not open): falling back to its parent would re-append the
// link's own name and miss where it leads.
func resolveExisting(abs string) (string, error) {
	cur, rest := abs, ""
	for {
		r, opened, err := finalPathName(cur)
		if err == nil {
			if rest == "" {
				return r, nil
			}
			return filepath.Join(r, rest), nil
		}
		if opened {
			// Opened, but Windows could not name it with a drive letter
			// (a mount point to a volume that has none, say).
			return "", err
		}
		if fi, lerr := os.Lstat(cur); lerr == nil && isReparsePoint(fi) {
			return "", errors.New("cannot resolve link " + cur + ": " + err.Error())
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = joinRest(filepath.Base(cur), rest)
		cur = parent
	}
}

// isReparsePoint reports whether Lstat saw a symlink, junction, mount point
// or other reparse point (Go 1.23+ reports the non-symlink ones as
// ModeIrregular).
func isReparsePoint(fi fs.FileInfo) bool {
	if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return true
	}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return false
}

// finalPathName opens p, following every reparse point, and returns the
// normalised final path. opened reports whether the open itself succeeded.
func finalPathName(p string) (s string, opened bool, err error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", false, err
	}
	// Access 0: no data access is requested, so neither the file's DACL
	// (beyond attribute reads) nor another process's share mode stops the
	// open; this only asks where the name leads.
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", false, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only query handle

	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", true, err
		}
		if n < uint32(len(buf)) {
			return normaliseFinalPath(windows.UTF16ToString(buf[:n])), true, nil
		}
		buf = make([]uint16, n)
	}
}

// normaliseFinalPath turns GetFinalPathNameByHandle's \\?\ form into the one
// the root comparison understands: \\?\C:\x becomes C:\x and \\?\UNC\srv\share
// becomes \\srv\share. Anything else (\\?\Volume{...}\x) is left as is, so
// windowsPathShapeError refuses it rather than it reading as a relative path.
func normaliseFinalPath(s string) string {
	const prefix = `\\?\`
	if !strings.HasPrefix(s, prefix) {
		return s
	}
	rest := s[len(prefix):]
	if len(rest) >= 3 && isASCIILetter(rest[0]) && rest[1] == ':' && rest[2] == '\\' {
		return rest
	}
	if len(rest) >= 4 && strings.EqualFold(rest[:4], `UNC\`) {
		return `\\` + rest[4:]
	}
	return s
}

// onSystemVolume reports whether the longest existing prefix of p is on the
// same volume as one of this machine's system directories, by volume serial
// number. For a UNC path the serial is the one the server reports for the
// volume the file is actually on, after the server has followed any junction,
// so a loopback share (\\localhost\C$, a share of C:\Windows, or a share
// holding a junction into it) matches the local system volume however it is
// spelled (#1107).
//
// It errs towards refusing: a share whose volume merely shares a serial with
// the system volume (a machine cloned from the same image) is refused too, and
// so is any path when no system directory's serial can be read.
func onSystemVolume(p string) (bool, error) {
	s, err := existingVolumeSerial(p)
	if err != nil {
		return false, err
	}
	found := false
	for _, r := range forbiddenReadRoots() {
		rs, err := volumeSerial(r)
		if err != nil {
			continue
		}
		found = true
		if rs == s {
			return true, nil
		}
	}
	if !found {
		return false, errors.New("cannot read the volume serial of any system directory")
	}
	return false, nil
}

// existingVolumeSerial is volumeSerial of the longest prefix of p that exists.
func existingVolumeSerial(p string) (uint32, error) {
	cur := p
	for {
		s, err := volumeSerial(cur)
		if err == nil {
			return s, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return 0, err
		}
		cur = parent
	}
}

// volumeSerial opens p as finalPathName does, following every reparse point,
// and returns the serial number of the volume the handle is on.
func volumeSerial(p string) (uint32, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only query handle
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return 0, err
	}
	return fi.VolumeSerialNumber, nil
}
