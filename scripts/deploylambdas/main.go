// Command deploylambdas deploys the code-only Lambda changes of a spawn release
// (#1445). It is run by .github/workflows/deploy-lambdas.yaml, in the
// approval-gated `production` environment, as the role
// scripts/setup-lambda-deploy-ci-role.sh creates.
//
// Its input is scripts/deploychecklist's JSON for <previous tag>..<tag>
// (#1444). It deploys ONLY the Lambdas that checklist marks status "changed"
// and deploy_kind "code-only"; everything else is listed as not deployed. Per
// function, in order:
//
//	a. pre-flight: snapshot get-function-configuration; refuse unless the live
//	   Architectures, Runtime, Handler and PackageType are what this build
//	   produces (the architecture from the checklist's build, the runtime and
//	   handler from the component's own deploy mechanisms at the tag), the
//	   function is in the expected account and region, and it is settled.
//	   deploy-sweep-orchestrator.sh's published-bootstrap-body check runs for
//	   any component whose mechanism runs it.
//	b. build: exactly the checklist's `go build` line, from the clean tag
//	   checkout, zipped as `bootstrap` at the root.
//	c. save the live code (get-function's Code.Location) and check its sha.
//	d. update-function-code, wait, and check the remote CodeSha256.
//	e. diff the configuration against the snapshot: it must be empty.
//	f. stamp spawn:version and read it back.
//	g. on any failure after (d), roll back to the saved code, verify it, stop.
//
// Steps a to c run for every function before any is changed. --dry-run (the
// default) stops there and prints what it would do.
//
// Usage:
//
//	go -C scripts run ./deploylambdas --checklist checklist.json --tag v0.152.0 [--dry-run=false] \
//	    [--record-md record.md] [--record-json record.json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "deploylambdas:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("deploylambdas", flag.ContinueOnError)
	checklistPath := fs.String("checklist", "", "scripts/deploychecklist --json output for <previous tag>..<tag> (required)")
	tag := fs.String("tag", "", "the release tag being deployed, which HEAD must be (required)")
	repo := fs.String("repo", ".", "any directory inside the git checkout of the tag")
	region := fs.String("region", "us-east-1", "region of the operated functions")
	account := fs.String("account", "966362334030", "account of the operated functions")
	dryRun := fs.Bool("dry-run", true, "pre-flight, build and save only; change nothing")
	recordMD := fs.String("record-md", "", "write the deploy record as markdown here")
	recordJSON := fs.String("record-json", "", "write the deploy record as JSON here")
	bodyURL := fs.String("body-url", "https://spawn-binaries-us-east-1.s3.amazonaws.com/spawn/versions/%s/bootstrap.sh", "published bootstrap body, %s is the version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *checklistPath == "" || *tag == "" {
		return fmt.Errorf("--checklist and --tag are required")
	}
	c, err := readChecklist(*checklistPath)
	if err != nil {
		return err
	}
	top, err := exec.Command("git", "-C", *repo, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse --show-toplevel in %s: %w", *repo, err)
	}
	root := strings.TrimSpace(string(top))
	work, err := os.MkdirTemp("", "deploylambdas-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	d := &deployer{
		aws: awsCLI{region: *region}, fetch: httpFetch, git: realGit(root), build: goBuild, renderBody: goRunBootstrapBody,
		sleep: time.Sleep, log: os.Stderr,
		repo: root, region: *region, account: *account, tag: *tag, version: strings.TrimPrefix(*tag, "v"),
		dryRun: *dryRun, workDir: work, bodyURL: *bodyURL,
		pollAttempts: 120, pollInterval: 5 * time.Second,
	}
	rec, runErr := d.run(ctx, c)
	if runErr != nil {
		rec.Error = runErr.Error()
	}
	if werr := writeRecord(rec, stdout, *recordMD, *recordJSON); werr != nil && runErr == nil {
		runErr = werr
	}
	return runErr
}

func writeRecord(rec *record, stdout io.Writer, mdPath, jsonPath string) error {
	rec.writeMarkdown(stdout)
	if mdPath != "" {
		f, err := os.Create(mdPath)
		if err != nil {
			return err
		}
		rec.writeMarkdown(f)
		if err := f.Close(); err != nil {
			return err
		}
	}
	if jsonPath != "" {
		b, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(jsonPath, append(b, '\n'), 0o644)
	}
	return nil
}
