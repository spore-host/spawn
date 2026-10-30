package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// buildSpec is a mechanism's `go build` line, reproduced exactly. It is parsed
// from the checklist's build.command, the line the mechanism itself runs, and
// any token this tool cannot reproduce refuses the deploy: dropping a flag
// silently would ship a binary the manual deploy would not have built.
type buildSpec struct {
	GOOS, GOARCH string
	Tags         []string
	LDFlags      string // after ${SPAWN_VERSION} is substituted; "" for none
}

var envPrefixRe = regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)=(.*)$`)

// shellFields splits a command line the way a shell would for the simple forms
// the deploy mechanisms use: whitespace-separated words, with '…' and "…"
// quoting (the quotes are removed), including inside a word (-ldflags="-s -w").
func shellFields(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote in %q", quote, s)
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// parseBuild reads the checklist's build for one Lambda. GOOS and GOARCH come
// from the checklist's resolved fields (the command may name them through a
// shell variable); everything else from the command, where the only accepted
// tokens are the env prefixes, `go build`, -tags, -ldflags, -o and the package
// `.`. ${SPAWN_VERSION} in -ldflags is the release version without the v, which
// is what deploy-sweep-orchestrator.sh stamps; any other unresolved variable
// refuses.
func parseBuild(b checklistBuild, version string) (buildSpec, error) {
	spec := buildSpec{GOOS: b.GOOS, GOARCH: b.GOARCH, Tags: b.Tags}
	if b.GOOS != "linux" {
		return spec, fmt.Errorf("build GOOS %q: a Lambda zip runs on linux", b.GOOS)
	}
	if _, err := lambdaArch(b.GOARCH); err != nil {
		return spec, err
	}
	words, err := shellFields(b.Command)
	if err != nil {
		return spec, err
	}
	i := 0
	for ; i < len(words); i++ {
		m := envPrefixRe.FindStringSubmatch(words[i])
		if m == nil {
			break
		}
		switch m[1] {
		case "GOOS", "GOARCH":
			// Resolved by the checklist, possibly through a variable.
		case "CGO_ENABLED":
			if m[2] != "0" {
				return spec, fmt.Errorf("build sets CGO_ENABLED=%s; this deployer builds with cgo off, as the checklist computed the file set", m[2])
			}
		default:
			return spec, fmt.Errorf("build sets %s, which this deployer does not reproduce: %q", m[1], b.Command)
		}
	}
	if i+1 >= len(words) || words[i] != "go" || words[i+1] != "build" {
		return spec, fmt.Errorf("not a `go build` line: %q", b.Command)
	}
	sawPkg := false
	var tags string
	for i += 2; i < len(words); i++ {
		w := words[i]
		flag, val, hasVal := strings.Cut(w, "=")
		switch flag {
		case "-tags", "-ldflags", "-o":
			if !hasVal {
				if i+1 >= len(words) {
					return spec, fmt.Errorf("%s with no value in %q", flag, b.Command)
				}
				i++
				val = words[i]
			}
			switch flag {
			case "-tags":
				tags = val
			case "-ldflags":
				spec.LDFlags = val
			case "-o":
				// The output goes to this tool's own directory, outside the checkout.
			}
		case ".":
			sawPkg = true
		default:
			return spec, fmt.Errorf("build token %q is not one this deployer reproduces (only -tags, -ldflags, -o and the package `.`): %q", w, b.Command)
		}
	}
	if !sawPkg {
		return spec, fmt.Errorf("build does not build the package `.`: %q", b.Command)
	}
	if tags != strings.Join(b.Tags, ",") {
		return spec, fmt.Errorf("build command -tags %q disagrees with the checklist's tags %v", tags, b.Tags)
	}
	spec.LDFlags = strings.NewReplacer("${SPAWN_VERSION}", version, "$SPAWN_VERSION", version).Replace(spec.LDFlags)
	if strings.Contains(spec.LDFlags, "$") {
		return spec, fmt.Errorf("-ldflags %q has a variable this deployer cannot resolve", spec.LDFlags)
	}
	return spec, nil
}

// lambdaArch is the Lambda architecture a GOARCH runs on.
func lambdaArch(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("GOARCH %q has no Lambda architecture", goarch)
}

// buildFunc builds one Lambda's package and returns the zip bytes; a seam for
// tests.
type buildFunc func(ctx context.Context, repo, pkgDir string, spec buildSpec, outDir string) ([]byte, error)

// goBuild runs the build from the clean tag checkout and zips the binary as
// `bootstrap` at the zip root, which is the layout every mechanism produces
// (`zip function.zip bootstrap`, `zip -j function.zip build/bootstrap`, a SAM
// CodeUri holding only bootstrap). The binary is written outside the checkout,
// so the tree stays clean and the build's vcs stamps match a clean tag.
//
// CGO_ENABLED=0 always: the checklist computes the compiled file set with cgo
// off, and the manual deploys cross-compile from macOS, where cgo is off by
// default for a linux target. On a linux runner it would otherwise be ON for a
// native amd64 build and link the runner's glibc.
func goBuild(ctx context.Context, repo, pkgDir string, spec buildSpec, outDir string) ([]byte, error) {
	bin := filepath.Join(outDir, "bootstrap")
	args := []string{"build"}
	if len(spec.Tags) > 0 {
		args = append(args, "-tags", strings.Join(spec.Tags, ","))
	}
	if spec.LDFlags != "" {
		args = append(args, "-ldflags", spec.LDFlags)
	}
	args = append(args, "-o", bin, ".")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = filepath.Join(repo, filepath.FromSlash(pkgDir))
	cmd.Env = append(os.Environ(), "GOOS="+spec.GOOS, "GOARCH="+spec.GOARCH, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=readonly")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go %s in %s: %v\n%s", strings.Join(args, " "), pkgDir, err, stderr.String())
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(bin); err != nil {
		return nil, err
	}
	return zipBootstrap(raw)
}

// zipBootstrap is a one-entry zip: `bootstrap`, mode 0755 (a custom runtime
// executes it), with a fixed timestamp so the same binary always gives the same
// CodeSha256.
func zipBootstrap(bin []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "bootstrap", Method: zip.Deflate, Modified: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.SetMode(0o755)
	w, err := zw.CreateHeader(h)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(bin); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// codeSha256 is the CodeSha256 Lambda reports for a zip: base64 of its sha256.
func codeSha256(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}
