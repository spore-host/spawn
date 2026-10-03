package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// samCodeUriRe finds a `CodeUri:` property in a SAM template. Deliberately a
// regex over the raw YAML rather than a parse: these templates use CFN
// shorthand (!Sub, !Ref, !GetAtt), which a plain YAML unmarshaller rejects, and
// pulling in a CFN-aware parser to assert one string is not worth it.
var samCodeUriRe = regexp.MustCompile(`(?m)^\s*CodeUri:\s*(\S+)\s*$`)

// samTemplates returns every lambda/*/template.yaml that declares a serverless
// function, i.e. the ones `sam deploy` will package.
func samTemplates(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	matches, err := filepath.Glob("lambda/*/template.yaml")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		body := string(b)
		if !strings.Contains(body, "AWS::Serverless::Function") {
			continue
		}
		found[p] = body
	}
	if len(found) == 0 {
		t.Skip("no SAM templates found")
	}
	return found
}

// TestSamCodeUriIsADedicatedBuildDir guards spawn#645.
//
// `sam deploy` zips the ENTIRE CodeUri directory with no filtering whatsoever —
// samcli/lib/package/utils.py walks it with os.walk and adds every file,
// dotfiles included. There is no `.samignore` feature (no such string anywhere in
// SAM CLI 1.166.2) and `.gitignore` is not consulted, so #637's gitignore work
// does nothing for deployment.
//
// With `CodeUri: .` that shipped the stray dev binary, a nested function.zip
// containing a second copy of bootstrap, and every source file: the ttl-reaper
// deployed at 75.7 MiB against a 25.2 MiB real artifact.
//
// A missing CodeUri is worse, not better: ServerlessFunctionResource inherits
// PACKAGE_NULL_PROPERTY = True, and upload_local_artifacts' own docstring says
// "If path is omitted, this method will zip the current working folder and
// upload" — so it packaged whatever cwd happened to be.
func TestSamCodeUriIsADedicatedBuildDir(t *testing.T) {
	for path, body := range samTemplates(t) {
		m := samCodeUriRe.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s declares a serverless function but sets no CodeUri.\n"+
				"That is not a safe default: SAM packages the CURRENT WORKING DIRECTORY when "+
				"CodeUri is omitted, so what ships depends on where make was run. Set "+
				"`CodeUri: build/`.", path)
			continue
		}
		switch got := m[1]; got {
		case ".", "./":
			t.Errorf("%s sets CodeUri: %s — SAM zips that whole directory with no filtering "+
				"(dotfiles included, no .samignore), so the dev binary, function.zip and every "+
				"source file ship with it. Point it at a dedicated build/ dir holding only "+
				"bootstrap.", path, got)
		case "build/", "build":
			// Correct.
		default:
			t.Errorf("%s sets CodeUri: %s — expected build/. If this is intentional, the "+
				"directory must still contain nothing but the Lambda artifact.", path, got)
		}
	}
}

// TestSamCodeUriDirHoldsOnlyBootstrap is the assertion that actually bounds the
// upload. The CodeUri value only says WHERE SAM looks; this says what it will
// find. It is skipped when the directory has not been built, so it is advisory
// in a clean checkout and binding on any machine that has run `make build` —
// which is every machine that could deploy.
func TestSamCodeUriDirHoldsOnlyBootstrap(t *testing.T) {
	for path := range samTemplates(t) {
		dir := filepath.Join(filepath.Dir(path), "build")
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // not built here; nothing to bound
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if len(names) != 1 || names[0] != "bootstrap" {
			t.Errorf("%s contains %v, want exactly [bootstrap].\n"+
				"Every file here is uploaded into the Lambda deployment package.", dir, names)
		}
	}
}

// TestDeployParamsNeverInsideCodeUri is the security half, and the one most
// likely to outlive anyone's memory of why it exists.
//
// lambda/ttl-reaper/Makefile generates .deploy-params.yaml immediately before
// `sam deploy`, holding every deploy parameter including NotifyUrl — which that
// Makefile's own comment calls "a Slack webhook secret". SAM's walk includes
// dotfiles, so had that file been inside the CodeUri directory it would be
// uploaded into the deployment package and readable by anyone with
// lambda:GetFunction.
//
// It must stay in the module root, NOT in build/.
func TestDeployParamsNeverInsideCodeUri(t *testing.T) {
	for path := range samTemplates(t) {
		moduleDir := filepath.Dir(path)
		buildDir := filepath.Join(moduleDir, "build")

		if _, err := os.Stat(filepath.Join(buildDir, ".deploy-params.yaml")); err == nil {
			t.Errorf("%s/.deploy-params.yaml is inside the CodeUri directory — it holds deploy "+
				"parameters including a Slack webhook (NotifyUrl) and SAM uploads dotfiles, so "+
				"this would ship the secret in the deployment package.", buildDir)
		}

		// The Makefile's PARAMS_FILE must not be pointed into build/ either —
		// catching the regression at its source rather than after a deploy.
		mk, err := os.ReadFile(filepath.Join(moduleDir, "Makefile"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(mk), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "PARAMS_FILE") && strings.Contains(trimmed, "build/") {
				t.Errorf("%s/Makefile points PARAMS_FILE into build/ (%q). That directory is "+
					"uploaded to Lambda; the params file holds a webhook secret.", moduleDir, trimmed)
			}
		}
	}
}
