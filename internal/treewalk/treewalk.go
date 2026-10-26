// Package treewalk holds the one rule every test that walks the source tree
// must follow: an entry that disappears between being listed and being read is
// skipped, not reported (#1334).
//
// `go test ./...` runs packages in parallel, and the checkout is shared. A
// directory listing taken by filepath.Walk or filepath.WalkDir can name a
// file another package's test removes before the walk reaches it, and the walk
// then hands the callback an fs.ErrNotExist for it. A gate that returns that
// error fails for a reason that has nothing to do with what it checks.
package treewalk

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// Vanished reports whether err, as passed to a filepath.Walk or WalkDir
// callback for path in a walk started at root, means the entry went away
// mid-walk. Return nil from the callback when it does.
//
// The root itself is never vanished: a walk whose root is missing has found
// nothing, and the gate doing it has broken.
func Vanished(root, path string, err error) bool {
	return err != nil && errors.Is(err, fs.ErrNotExist) && filepath.Clean(path) != filepath.Clean(root)
}
