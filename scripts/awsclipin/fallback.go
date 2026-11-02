package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// When GitHub will not let GITHUB_TOKEN open the bump PR, the workflow opens
// (or updates) an issue instead, holding what the owner needs to open the PR
// from the pushed branch. This file is that decision and that issue's text,
// in Go so they are tested offline.
//
//	gh pr create ... 2>&1 | go -C scripts run ./awsclipin pr-refused
//	go -C scripts run ./awsclipin issue-title --from 2.39.3 --version 2.40.0
//	go -C scripts run ./awsclipin issue-body  --from ... --version ... \
//	    --sha256-x86_64 <hex> --sha256-aarch64 <hex> --compare <url>

// errNotRefused is pr-refused's "no": the failure was something else, which
// must fail the run. main prints nothing for it; the caller already has gh's
// own message.
var errNotRefused = errors.New("not GitHub's refusal to let Actions create pull requests")

// prRefusal matches GitHub's message when the repository (or its org) has
// "Allow GitHub Actions to create and approve pull requests" off:
// "GitHub Actions is not permitted to create or approve pull requests."
var prRefusal = regexp.MustCompile(`(?i)\bnot permitted to create or approve pull requests\b`)

// prRefused reports whether gh's output is that refusal and nothing else can
// be read into it: a failed push, a missing branch, an auth or network error
// all say something different, and all of them must fail the run.
func prRefused(ghOutput string) bool { return prRefusal.MatchString(ghOutput) }

func issueTitle(from, version string) string {
	return fmt.Sprintf("AWS CLI pin bump available: %s → %s", from, version)
}

func issueBody(from, version, x8664, aarch64, compare string) string {
	return strings.Join([]string{
		fmt.Sprintf("A newer AWS CLI v2 exists for the Linux bootstrap to install: **%s**, pinned now at %s (#1495).", version, from),
		"",
		"The weekly `AWS CLI pin` workflow verified it and pushed the bump, but GitHub would not let `GITHUB_TOKEN` open the PR (\"Allow GitHub Actions to create and approve pull requests\" is off). **Open the PR from this link:**",
		"",
		compare,
		"",
		"| | |",
		"|---|---|",
		"| Version | `" + version + "` |",
		"| sha256, `awscli-exe-linux-x86_64-" + version + ".zip` | `" + x8664 + "` |",
		"| sha256, `awscli-exe-linux-aarch64-" + version + ".zip` | `" + aarch64 + "` |",
		"| Signing key fingerprint | `" + KeyFingerprint + "` |",
		"",
		"Both zips' detached signatures verified with gpg as made by that key (`scripts/awsclipin/aws-cli-team.asc`) before the sha256s were pinned; the workflow run's log has the output. The bump changes the published bootstrap body, so it ships in a release and the sweep orchestrator is redeployed from it. Close this issue once the PR is open.",
		"",
	}, "\n")
}

func runFallback(args []string, stdin io.Reader, stdout io.Writer) error {
	if args[0] == "pr-refused" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		if !prRefused(string(b)) {
			return errNotRefused
		}
		return nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	from := fs.String("from", "", "the version pinned now")
	version := fs.String("version", "", "the verified new version")
	x8664 := fs.String("sha256-x86_64", "", "its x86_64 zip's sha256")
	aarch64 := fs.String("sha256-aarch64", "", "its aarch64 zip's sha256")
	compare := fs.String("compare", "", "the compare URL of the pushed branch")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !versionRe.MatchString(*from) || !versionRe.MatchString(*version) {
		return fmt.Errorf("--from %q / --version %q: want 2.N.N", *from, *version)
	}
	if args[0] == "issue-title" {
		_, err := fmt.Fprintln(stdout, issueTitle(*from, *version))
		return err
	}
	if err := validate(map[string]string{constVersion: *version, constX8664: *x8664, constAarch64: *aarch64}); err != nil {
		return err
	}
	if !strings.HasPrefix(*compare, "https://github.com/") {
		return fmt.Errorf("--compare %q is not a github.com URL", *compare)
	}
	_, err := fmt.Fprint(stdout, issueBody(*from, *version, *x8664, *aarch64, *compare))
	return err
}
