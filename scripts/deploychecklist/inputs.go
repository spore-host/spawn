package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Compiled is what goes into one Lambda's binary: the repo files `go list -deps`
// says are compiled (non-test Go, cgo/asm sources and //go:embed files, for the
// mechanism's GOOS/GOARCH/tags), the third-party module versions it links, and
// the main module's go directive.
//
// Third-party code is compared by module@version rather than by diffing go.mod
// and go.sum. Every nested module is tidy (scripts/check-tidy.sh), so any change
// that alters a linked dependency shows up here; one that does not (a bump to a
// module this Lambda never compiles, which is most Dependabot PRs against the
// root go.mod) correctly does not mark it changed.
type Compiled struct {
	Files     []string          // repo-relative, slash-separated
	Modules   map[string]string // path -> version
	GoVersion string
}

type listPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *struct {
		Path      string
		Version   string
		Main      bool
		GoVersion string
		Replace   *struct {
			Path    string
			Version string
		}
	}
	GoFiles, CgoFiles, CFiles, CXXFiles, HFiles, SFiles, SysoFiles, EmbedFiles []string
	Error                                                                      *struct{ Err string }
}

// goList runs `go list -deps` over a package directory. Any package error is
// fatal: `go list -e` would otherwise report a partial set, and a missing
// package would read as "unchanged", the one wrong answer this tool exists to
// prevent.
func goList(root, pkgDir string, b Build) (*Compiled, error) {
	args := []string{"list", "-e", "-deps", "-json=ImportPath,Dir,Standard,Module,GoFiles,CgoFiles,CFiles,CXXFiles,HFiles,SFiles,SysoFiles,EmbedFiles,Error"}
	if len(b.Tags) > 0 {
		args = append(args, "-tags", strings.Join(b.Tags, ","))
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join(root, filepath.FromSlash(pkgDir))
	cmd.Env = append(os.Environ(), "GOOS="+b.GOOS, "GOARCH="+b.GOARCH, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=readonly")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps in %s: %v\n%s", pkgDir, err, stderr.String())
	}
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	c := &Compiled{Modules: map[string]string{}}
	var errs []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list -deps in %s: decode: %v", pkgDir, err)
		}
		if p.Error != nil {
			errs = append(errs, p.ImportPath+": "+p.Error.Err)
			continue
		}
		if p.Standard {
			continue
		}
		dir, err := filepath.EvalSymlinks(p.Dir)
		if err != nil {
			return nil, err
		}
		if rel, err := filepath.Rel(absRoot, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// This module or its `replace ../..` parent: compare the files.
			for _, set := range [][]string{p.GoFiles, p.CgoFiles, p.CFiles, p.CXXFiles, p.HFiles, p.SFiles, p.SysoFiles, p.EmbedFiles} {
				for _, f := range set {
					c.Files = append(c.Files, filepath.ToSlash(filepath.Join(rel, f)))
				}
			}
			if p.Module != nil && p.Module.Main {
				c.GoVersion = p.Module.GoVersion
			}
			continue
		}
		if p.Module == nil {
			errs = append(errs, p.ImportPath+": outside the repo and in no module")
			continue
		}
		path, ver := p.Module.Path, p.Module.Version
		if r := p.Module.Replace; r != nil {
			path, ver = r.Path, r.Version
		}
		c.Modules[path] = ver
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("go list -deps in %s reported package errors (refusing a partial file set):\n  %s", pkgDir, strings.Join(errs, "\n  "))
	}
	if len(c.Files) == 0 {
		return nil, fmt.Errorf("go list -deps in %s found no compiled repo files: the discovery is broken", pkgDir)
	}
	sort.Strings(c.Files)
	c.Files = uniqSorted(c.Files)
	return c, nil
}
