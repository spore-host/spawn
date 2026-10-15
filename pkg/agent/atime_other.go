//go:build !linux && !darwin

package agent

import (
	"os"
	"time"
)

// fileAtime is unused off Linux (Windows counts sessions with quser); the
// modification time keeps ttyidle.go compiling.
func fileAtime(fi os.FileInfo) time.Time { return fi.ModTime() }
