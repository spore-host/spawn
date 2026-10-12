package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A lambda directory must hold ONE implementation of its function (#811).
//
// `lambda/dns-updater/` held both: `main.go` (a Go module, deployed as
// `provided.al2023` with handler `bootstrap` — read off the live function) and
// `handler.py`, 472 lines of a complete Python implementation that was never
// packaged by anything.
//
// The language mismatch was the smaller half. `handler.py`'s own docstring
// described its security model as *"Validates AWS-signed instance identity
// document"* — the PKCS#7 identity-document scheme that #173 explicitly
// REPLACED with SigV4, and which `main.go` records as removed
// ("instance_identity_document / _signature were removed — the caller is now
// authenticated by SigV4"). It also hardcoded the production hosted zone ID. So
// a reader who found it had a plausible, deployable implementation of a
// superseded authentication design for a function whose whole security story is
// that the caller is SigV4-verified.
//
// A second implementation is not dead weight, it is a fork with no owner: it
// cannot be reviewed against the live one, nothing tests it, and nothing stops
// it being deployed.
func TestNoLambdaHasTwoImplementations(t *testing.T) {
	entries, err := os.ReadDir("lambda")
	if err != nil {
		t.Fatalf("read lambda/: %v", err)
	}
	// Rule 4: an empty discovery has broken rather than proved the tree clean.
	if len(entries) == 0 {
		t.Fatal("lambda/ listed no entries — the read has broken, not the directory emptied")
	}

	checked := 0
	var doubled []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join("lambda", e.Name())
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr != nil {
			continue // not a Go lambda; nothing to double up on
		}
		checked++

		// Only a file declaring a Lambda ENTRY POINT counts. A Go lambda may
		// legitimately ship Python helpers — ttl-reaper has
		// scripts/merge_deploy_params.py — and flagging those would be the
		// allowlist-of-obviously-fine-things signal (rule 6). The rule is about a
		// second handler for the same function, so it keys on `def lambda_handler`
		// at the start of a line, not on the presence of Python.
		py, gerr := filepath.Glob(filepath.Join(dir, "*.py"))
		if gerr != nil {
			t.Fatalf("glob %s: %v", dir, gerr)
		}
		for _, f := range py {
			b, rerr := os.ReadFile(f) //nolint:gosec // walking the repo's own lambda sources
			if rerr != nil {
				t.Fatalf("read %s: %v", f, rerr)
			}
			if pythonLambdaHandler.Match(b) {
				doubled = append(doubled, f)
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no Go lambda modules under lambda/ — the scan has broken, which is not " +
			"the same as there being none")
	}
	sort.Strings(doubled)
	if len(doubled) > 0 {
		t.Errorf("%d Go lambda(s) also ship a Python entry point: %s\n"+
			"  One function, two implementations. The second cannot be reviewed against the "+
			"live one, nothing tests it, and nothing stops someone deploying it — "+
			"lambda/dns-updater/handler.py was a deployable implementation of the PKCS#7 "+
			"identity-document auth that #173 replaced with SigV4. Delete it, or move it out "+
			"of the lambda directory if it is genuinely a reference.",
			len(doubled), strings.Join(doubled, ", "))
	}
}

// pythonLambdaHandler matches the handler DEFINITION at the start of a line, so
// a mention in a comment or a docstring is not read as an entry point (rule 3).
var pythonLambdaHandler = regexp.MustCompile(`(?m)^def lambda_handler\s*\(`)
