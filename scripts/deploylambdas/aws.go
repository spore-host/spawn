package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// lambdaCLI runs one `aws lambda <subcommand>`; the seam tests replace.
//
// Every AWS call this tool makes goes through deployer.callLambda with the
// subcommand as a string literal, and the AWS CLI is invoked in exactly one
// place (awsCLI below). TestEveryLambdaCallIsGrantedByTheCIRole relies on both:
// it reads the subcommands from the source and checks each against the
// policy in scripts/setup-lambda-deploy-ci-role.sh.
type lambdaCLI interface {
	Lambda(ctx context.Context, subcommand string, args ...string) ([]byte, error)
}

// awsCLI is the real AWS CLI. The region is always explicit, and the pager off,
// so a call can neither hit the wrong region nor block on less.
type awsCLI struct{ region string }

func (a awsCLI) Lambda(ctx context.Context, subcommand string, args ...string) ([]byte, error) {
	full := append([]string{"lambda", subcommand}, args...)
	full = append(full, "--region", a.region, "--output", "json", "--no-cli-pager")
	cmd := exec.CommandContext(ctx, "aws", full...)
	cmd.Env = append(os.Environ(), "AWS_PAGER=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("aws lambda %s: %v: %s", subcommand, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// fetchFunc GETs a URL; a seam for tests. Used for the presigned Code.Location
// get-function returns and for the published bootstrap body, both plain HTTPS,
// so neither needs an S3 permission.
type fetchFunc func(ctx context.Context, url string) ([]byte, error)

// maxFetch bounds a download: Lambda's own zip limit is 250 MB unzipped, and a
// direct --zip-file upload is at most 50 MB.
const maxFetch = 256 << 20

func httpFetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", redact(url), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetch+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFetch {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", redact(url), maxFetch)
	}
	return b, nil
}

// redact drops a URL's query, which for a presigned URL is the credential.
func redact(url string) string {
	if i := strings.IndexByte(url, '?'); i >= 0 {
		return url[:i] + "?…"
	}
	return url
}

// gitFunc runs git in the repo; a seam for tests.
type gitFunc func(ctx context.Context, args ...string) (string, error)

func realGit(repo string) gitFunc {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(out)), nil
	}
}

// renderBodyFunc prints the spored bootstrap body a release compiles in; a seam
// for tests.
type renderBodyFunc func(ctx context.Context, repo, version string) ([]byte, error)

// bodyVersionLDFlag is the version ldflag the release workflow renders the
// published body with ("Publish the bootstrap body to S3" in release.yaml).
// TestBodyRenderMatchesTheRelease keeps the two the same.
const bodyVersionLDFlag = "-X github.com/spore-host/spawn/cmd.Version="

func goRunBootstrapBody(ctx context.Context, repo, version string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", "run", "-ldflags", bodyVersionLDFlag+version, ".", "bootstrap-body")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "SPORE_NO_UPDATE_CHECK=1", "CGO_ENABLED=0", "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("spawn bootstrap-body: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("spawn bootstrap-body printed nothing")
	}
	return out, nil
}
