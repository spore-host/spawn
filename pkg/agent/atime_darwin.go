//go:build darwin

package agent

import (
	"os"
	"syscall"
	"time"
)

// fileAtime returns a file's access time. spored does not run on darwin; this
// exists so the terminal idle logic in ttyidle.go is tested where development
// happens.
func fileAtime(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
	}
	return fi.ModTime()
}
