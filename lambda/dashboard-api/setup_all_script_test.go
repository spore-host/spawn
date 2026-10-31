package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scripts/setup-all.sh, the documented one-shot setup, run end to end from
// several working directories (#1447). It assumed two different starting
// directories at once (./scripts/... for step 1, `cd ..; ./deploy.sh` for step
// 2) and could not complete from either. Everything AWS is the role script's
// fake plus the few answers the other steps need; `go` and `zip` are faked
// too, so nothing is built or sent anywhere.

const setupAllScript = "scripts/setup-all.sh"

// setupAllFakeAWS answers what deploy.sh, the API Gateway and the domain
// scripts need to reach their end, and hands everything else to the role
// script's fake.
const setupAllFakeAWS = `#!/bin/bash
case "$1 $2" in
  "iam get-role") echo arn:aws:iam::966362334030:role/SpawnDashboardLambdaRole ;;
  "route53 list-hosted-zones") echo /hostedzone/ZFAKE ;;
  "acm list-certificates") echo arn:aws:acm:us-east-1:966362334030:certificate/fake ;;
  "apigatewayv2 get-domain-name") printf 'd-fake.execute-api.us-east-1.amazonaws.com\tZGW\n' ;;
  *) exec "$FAKE_DIR/aws-role" "$@" ;;
esac
`

// fakeBuild records where `go build` ran, and makes the file it was asked for
// so zip and the cleanup trap have something to act on.
const fakeGo = `#!/bin/bash
echo "$PWD" >> "$FAKE_DIR/go-pwd"
while [ $# -gt 0 ]; do [ "$1" = -o ] && { : > "$2"; break; }; shift; done
`

const fakeZip = `#!/bin/bash
: > "$2"
`

// traceScripts is BASH_ENV: bash sources it at the start of every
// non-interactive shell, so it logs each script setup-all.sh runs, however it
// was invoked. $0 is still the interpreter at that point, so the command line
// comes from ps.
const traceScripts = `echo "$PWD|$(ps -o args= -p $$)" >> "$FAKE_DIR/scripts"
`

// scriptArgRe is the script operand of a traced `bash <script> ...` line.
var scriptArgRe = regexp.MustCompile(`(?:^|\s)(\S+\.sh)(?:\s|$)`)

// invokedScriptRe matches a line of setup-all.sh that runs a script by a
// relative path. Anchored on the start of the line so the echoed banners and
// comments never match.
var invokedScriptRe = regexp.MustCompile(`(?m)^\s*(?:\(cd \S+ && )?(\.{1,2}/[\w./-]+\.sh)\b`)

func realpath(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSetupAllRunsEveryStepFromAnyDirectory(t *testing.T) {
	raw, err := os.ReadFile(setupAllScript)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, m := range invokedScriptRe.FindAllStringSubmatch(string(raw), -1) {
		want = append(want, filepath.Base(m[1]))
	}
	if len(want) == 0 {
		t.Fatalf("found no script invocation in %s; the pattern is broken, not the script clean", setupAllScript)
	}

	pkgDir := realpath(t, ".")
	script := filepath.Join(pkgDir, setupAllScript)
	cwds := map[string]string{
		"its own directory":            filepath.Join(pkgDir, "scripts"),
		"the Lambda directory":         pkgDir,
		"a directory outside the repo": t.TempDir(),
	}
	for name, cwd := range cwds {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			for f, body := range map[string]string{
				filepath.Join(bin, "aws"):      setupAllFakeAWS,
				filepath.Join(bin, "go"):       fakeGo,
				filepath.Join(bin, "zip"):      fakeZip,
				filepath.Join(dir, "aws-role"): fakeAWS,
			} {
				if err := os.WriteFile(f, []byte(body), 0o755); err != nil { //nolint:gosec // executable test fakes
					t.Fatal(err)
				}
			}
			for f, body := range map[string]string{"versions": "", "trace.sh": traceScripts} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command("bash", script, "test-profile")
			cmd.Dir = cwd
			cmd.Stdin = strings.NewReader("y")
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_DIR="+dir,
				"BASH_ENV="+filepath.Join(dir, "trace.sh"),
				"SLACK_SECRETS_ARN=", "WS_API_ID=", "SPAWN_LAMBDA_ACCOUNT=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("setup-all.sh failed from %s: %v\n%s", cwd, err, out)
			}

			// Every script it names ran, and each one that ran is a real file
			// in this Lambda's tree.
			trace, err := os.ReadFile(filepath.Join(dir, "scripts"))
			if err != nil {
				t.Fatal(err)
			}
			ran := map[string]bool{}
			for _, l := range strings.Split(strings.TrimSpace(string(trace)), "\n") {
				pwd, args, _ := strings.Cut(l, "|")
				m := scriptArgRe.FindStringSubmatch(args)
				if m == nil {
					continue
				}
				p := m[1]
				if !filepath.IsAbs(p) {
					p = filepath.Join(pwd, p)
				}
				p = realpath(t, p)
				if p == realpath(t, script) {
					continue
				}
				if !strings.HasPrefix(p, pkgDir+string(filepath.Separator)) {
					t.Errorf("ran a script outside lambda/dashboard-api: %s", p)
				}
				ran[filepath.Base(p)] = true
			}
			for _, w := range want {
				if !ran[w] {
					t.Errorf("setup-all.sh names %s, but it never ran; ran %v\n%s", w, ran, out)
				}
			}
			if len(ran) != len(want) {
				t.Errorf("ran %d scripts, setup-all.sh names %d: %v vs %v", len(ran), len(want), ran, want)
			}

			// deploy.sh built this Lambda, not whatever package the caller was in.
			gopwd, err := os.ReadFile(filepath.Join(dir, "go-pwd"))
			if err != nil {
				t.Fatalf("go build never ran: %v\n%s", err, out)
			}
			for _, d := range strings.Fields(string(gopwd)) {
				if realpath(t, d) != pkgDir {
					t.Errorf("go build ran in %s, want %s", d, pkgDir)
				}
			}
			for _, f := range []string{"bootstrap", "function.zip"} {
				if _, err := os.Stat(filepath.Join(pkgDir, f)); err == nil {
					t.Errorf("deploy.sh left %s behind", f)
				}
			}
		})
	}
}
