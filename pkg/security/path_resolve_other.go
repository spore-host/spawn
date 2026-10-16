//go:build !windows

package security

import "path/filepath"

// resolveExisting resolves symlinks in the longest prefix of the absolute path
// abs that exists, and re-appends the rest. A path that does not exist yet (or
// is a glob pattern) is still caught when a directory it sits under links into
// a system directory. On POSIX filepath.EvalSymlinks follows every link, so
// this never fails; the error is for the Windows implementation.
func resolveExisting(abs string) (string, error) {
	cur, rest := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r, nil
			}
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = joinRest(filepath.Base(cur), rest)
		cur = parent
	}
}
