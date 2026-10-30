// Command deploychecklist says which spore.host-operated components changed
// between two git refs, and how each one deploys (#1444).
//
// A spawn release does not deploy the operated Lambdas; each is redeployed by
// hand. Deciding WHICH ones a release touched was also by hand, and on v0.151.0
// the first pass got it wrong: `git diff -- lambda/` showed only the #1434
// files, but both orchestrators compile pkg/aws and pkg/launcher, which #1433
// changed. Only `go list -deps` caught it. This tool is that `go list -deps`,
// for every operated Lambda, plus every non-code deploy input (role, policy,
// table, stream and mapping scripts, CloudFormation templates).
//
// Usage (from the repo root, or anywhere in it):
//
//	go -C scripts run ./deploychecklist --from v0.150.0 --to v0.151.0
//	go -C scripts run ./deploychecklist --from v0.150.0 --to v0.151.0 --json
//
// Both refs are read with `git archive`, never checked out, so the working tree
// is not touched. The report is read-only: it calls no AWS API.
package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "deploychecklist:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("deploychecklist", flag.ContinueOnError)
	from := fs.String("from", "", "the previous release ref (e.g. v0.150.0)")
	to := fs.String("to", "HEAD", "the new release ref")
	repo := fs.String("repo", ".", "any directory inside the git repository")
	asJSON := fs.Bool("json", false, "print JSON (the contract #1445 consumes) instead of markdown")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("--from is required")
	}
	rep, err := checklist(*repo, *from, *to, goListCompile)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	writeMarkdown(stdout, rep)
	return nil
}

func checklist(repo, from, to string, compile compileFunc) (*Report, error) {
	top, err := git(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	top = strings.TrimSpace(top)
	tmp, err := os.MkdirTemp("", "deploychecklist-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	trees := make([]*Tree, 2)
	invs := make([]*Inventory, 2)
	commits := make([]string, 2)
	for i, ref := range []string{from, to} {
		c, err := git(top, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return nil, err
		}
		commits[i] = strings.TrimSpace(c)
		dir := filepath.Join(tmp, fmt.Sprint(i))
		if err := extract(top, commits[i], dir); err != nil {
			return nil, err
		}
		if trees[i], err = newTree(dir); err != nil {
			return nil, err
		}
		if invs[i], err = discover(trees[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
	}
	rep, err := buildReport(trees[0], trees[1], invs[0], invs[1], compile)
	if err != nil {
		return nil, err
	}
	rep.From, rep.To, rep.FromCommit, rep.ToCommit = from, to, commits[0], commits[1]
	return rep, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// extract writes the tracked files of a commit into dir. `git archive` would
// drop export-ignore paths; the repo sets none (there is no .gitattributes), and
// if one appears the files it hides are not deploy inputs anyway.
func extract(repo, commit, dir string) error {
	cmd := exec.Command("git", "archive", "--format=tar", commit)
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git archive %s: %v: %s", commit, err, strings.TrimSpace(stderr.String()))
	}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("git archive %s: unsafe path %q", commit, h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				return err
			}
		}
		// Symlinks and git's pax header are skipped: neither is a deploy input.
	}
}
