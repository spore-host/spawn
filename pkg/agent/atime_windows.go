//go:build windows

package agent

import (
	"os"
	"syscall"
	"time"
)

// fileAtime returns a file's last-access time. spored counts Windows sessions
// with quser, not terminal device nodes; this exists so the terminal idle logic
// in ttyidle.go is tested on the Windows runner too, instead of reading mtime
// there and failing every fixture that sets input and output times apart.
func fileAtime(fi os.FileInfo) time.Time {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, d.LastAccessTime.Nanoseconds())
	}
	return fi.ModTime()
}
