//go:build linux

package agent

import (
	"os"
	"syscall"
	"time"
)

// fileAtime returns a file's access time. For a terminal device node that is
// the time it last read input (see ttyidle.go).
func fileAtime(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return fi.ModTime()
}
