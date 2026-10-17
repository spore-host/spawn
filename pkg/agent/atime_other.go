//go:build !linux && !darwin && !windows

package agent

import (
	"os"
	"time"
)

// fileAtime is unused on these platforms; the modification time keeps
// ttyidle.go compiling.
func fileAtime(fi os.FileInfo) time.Time { return fi.ModTime() }
